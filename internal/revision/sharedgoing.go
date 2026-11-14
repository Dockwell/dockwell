// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Deleting a project, or an environment, that shares a database server with apps of
// other projects keeps those apps working by default: the server moves, as a move
// takes it, with its volumes, its users and their secrets, to a project of its own
// named for it, or the one the deletion names (Edit.KeepSharedIn), and the links that
// meet it follow it, all in the deletion's one plan. Edit.DeleteShared deletes it
// anyway (ADR 0084, #572).

// keepShared adds to each deletion of a scope with its contents the moves of the
// servers apps elsewhere link to.
func (e *Engine) keepShared(ctx context.Context, edits []Edit) ([]Edit, []Note, error) {
	out := slices.Clone(edits)
	var notes []Note
	for i := range edits {
		ed := &edits[i]
		if !ed.WithContents || ed.Spec != nil {
			continue
		}
		if ed.DeleteShared && ed.KeepSharedIn != "" {
			return nil, nil, fault.New(ErrInvalid, "%s both keeps what other projects' apps use and deletes it. Choose one", ed.Ref)
		}
		if ed.DeleteShared {
			continue
		}
		shared, to, err := e.sharedIn(ctx, *ed)
		if err != nil {
			return nil, nil, err
		}
		for _, s := range shared {
			ask := AppMove{From: s.Scope, Services: []string{s.Name}, Leaving: true}
			if to != "" {
				ask.To = to
			} else {
				ask.NewProject = s.Name
			}
			moved, report, err := e.PlanAppMove(ctx, ask)
			if err != nil {
				return nil, nil, err
			}
			if len(report.Blocks) > 0 {
				return nil, nil, fault.New(ErrInvalid, "%s can't keep %s, which apps in other projects use: %s", ed.Ref, s.Name, MoveWords(report.Blocks))
			}
			for k := range moved {
				m := &moved[k]
				// What the move takes is the move's, not the deletion's.
				if j := slices.IndexFunc(out, func(o Edit) bool { return o.Ref == m.Ref }); j >= 0 {
					out[j] = *m
					continue
				}
				out = append(out, *m)
			}
			where := "a project of its own, " + s.Name
			if to != "" {
				where = "the project " + ed.KeepSharedIn
			}
			notes = append(notes, Note{
				Reason: "keeps_shared", Ref: s, Names: []string{s.Name}, Named: true,
				Message: fmt.Sprintf("%s moves to %s, with its data, as apps in other projects use it, and their links follow it", s.Name, where),
			})
		}
	}
	return out, notes, nil
}

// sharedIn lists the services a deletion's scope runs that a link of another
// environment, outside it, meets, and the environment the deletion keeps them in,
// or "" for projects of their own.
func (e *Engine) sharedIn(ctx context.Context, ed Edit) ([]resource.Ref, string, error) {
	shared, _, to, err := e.sharedWith(ctx, ed)
	return shared, to, err
}

// sharedWith is sharedIn with the links outside that meet what's shared, and the
// deletion's environments.
func (e *Engine) sharedWith(ctx context.Context, ed Edit) (shared []resource.Ref, meeting []sharedLink, to string, err error) {
	err = e.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		var envs []string
		for _, p := range tree.Projects {
			for _, env := range p.Environments {
				if ed.Ref.Kind == scope.ProjectKind && p.Name == ed.Ref.Name || ed.Ref.Kind == scope.EnvironmentKind && p.ID == ed.Ref.Scope && env.Name == ed.Ref.Name {
					envs = append(envs, env.ID)
				}
			}
		}
		if ed.KeepSharedIn != "" {
			p, env, err := tree.Resolve(ed.KeepSharedIn, "")
			if err != nil {
				return err
			}
			if slices.Contains(envs, env.ID) {
				return fault.New(ErrInvalid, "%s is what's being deleted, so it can't keep what other projects' apps use. Choose another project", p.Name)
			}
			to = env.ID
		}
		if t, _ := e.kinds.Type(linkKind); t == nil {
			return nil
		}
		links, err := tx.ListKind(ctx, string(linkKind))
		if err != nil {
			return err
		}
		for _, r := range links {
			if r.Spec == nil || slices.Contains(envs, r.Scope) {
				continue
			}
			it, err := e.load(ctx, tx, resource.Ref{Kind: linkKind, Scope: r.Scope, Name: r.Name})
			if err != nil {
				return err
			}
			l, ok := it.current.(*resourcev1.Link)
			if !ok || !slices.Contains(envs, l.GetMeetsEnvironment()) {
				continue
			}
			ref := resource.Ref{Kind: serviceKind, Scope: l.GetMeetsEnvironment(), Name: l.GetService()}
			if !slices.Contains(shared, ref) {
				shared = append(shared, ref)
			}
			project := r.Scope
			if p := projectOf(tree, r.Scope); p != nil {
				project = p.Name
			}
			meeting = append(meeting, sharedLink{ref: resource.Ref{Kind: linkKind, Scope: r.Scope, Name: r.Name}, link: l, server: ref, project: project})
		}
		return nil
	})
	sortRefs(shared)
	return shared, meeting, to, err
}

// sharedLink is a link of another project that meets a server a deletion takes.
type sharedLink struct {
	ref     resource.Ref
	link    *resourcev1.Link
	server  resource.Ref
	project string // the link's, by name
}

// breaksShared, for a deletion that takes what other projects' apps use anyway
// (Edit.DeleteShared), backs up each database their links read, alone, before
// anything else in the change, as an update does (ADR 0080), and recorded as each
// app's; holds each value of theirs that reads one the deletion takes at what it
// holds now; and says which apps it breaks (ADR 0084, #572).
func (e *Engine) breaksShared(ctx context.Context, edits []Edit) ([]Edit, []Note, error) {
	out := slices.Clone(edits)
	var notes []Note
	for i := range edits {
		ed := &edits[i]
		if !ed.WithContents || ed.Spec != nil || !ed.DeleteShared {
			continue
		}
		_, meeting, _, err := e.sharedWith(ctx, *ed)
		if err != nil {
			return nil, nil, err
		}
		if len(meeting) == 0 {
			continue
		}
		var held []Edit
		dumps := map[resource.Ref][]OneBackup{} // by the server's data volume
		err = e.db.View(ctx, func(tx *store.Tx) error {
			for _, m := range meeting {
				svc, err := e.load(ctx, tx, m.server)
				if err != nil {
					return err
				}
				spec, _ := svc.current.(*resourcev1.Service)
				f, ok := database.FromSpec(spec)
				volume := resource.Ref{Kind: volumeKind, Scope: m.server.Scope, Name: f.Volume}
				switch {
				case !ok || f.Volume == "" || f.External:
				case m.link.GetDatabaseName() != "" && f.Engine.SQL():
					dumps[volume] = append(dumps[volume], OneBackup{Volume: volume, Database: m.link.GetDatabaseName(), For: m.ref})
				case len(dumps[volume]) == 0:
					// An engine that dumps no database alone: the server whole, once.
					dumps[volume] = nil
				}
				notes = append(notes, Note{
					Reason: "breaks_shared", Ref: m.ref, Names: []string{m.project}, Named: true,
					Message: fmt.Sprintf("the app in %s loses %s, which its link %s reads, and stops working", m.project, m.server.Name, m.ref.Name),
				})
			}
			// What reads a value the deletion takes, from outside it, holds what it reads now.
			for _, kind := range []resource.Kind{"secret", "variable"} {
				list, err := tx.ListKind(ctx, string(kind))
				if err != nil {
					return err
				}
				for i := range list {
					r := &list[i]
					if r.Spec == nil {
						continue
					}
					ref := resource.Ref{Kind: kind, Scope: r.Scope, Name: r.Name}
					it, err := e.load(ctx, tx, ref)
					if err != nil {
						return err
					}
					v := variable.FromOf(it.current).GetValue()
					if v == nil || !slices.ContainsFunc(meeting, func(m sharedLink) bool { return m.server.Scope == v.GetEnvironment() }) {
						continue
					}
					fromKind := resource.Kind("variable")
					if v.GetSecret() {
						fromKind = "secret"
					}
					from, err := e.load(ctx, tx, resource.Ref{Kind: fromKind, Scope: v.GetEnvironment(), Name: v.GetName()})
					if err != nil {
						return err
					}
					value := ""
					switch s := from.current.(type) {
					case *resourcev1.Secret:
						value = s.GetValue()
					case *resourcev1.Variable:
						value = s.GetValue()
					}
					var spec proto.Message = &resourcev1.Variable{Value: value}
					if kind == "secret" {
						spec = &resourcev1.Secret{Value: value}
					}
					held = append(held, Edit{Ref: ref, Spec: spec, Base: it.base})
				}
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		out = append(out, held...)
		for _, volume := range slices.SortedFunc(maps.Keys(dumps), func(a, b resource.Ref) int { return strings.Compare(a.String(), b.String()) }) {
			ones := dumps[volume]
			backup := Edit{Ref: volume, Backup: true}
			if len(ones) > 0 {
				backup.BackupOne, backup.For, backup.BackupAlso = ones[0].Database, ones[0].For, ones[1:]
			}
			if j := slices.IndexFunc(out, func(o Edit) bool { return o.Ref == volume }); j >= 0 {
				out[j].Backup, out[j].BackupOne, out[j].For, out[j].BackupAlso = backup.Backup, backup.BackupOne, backup.For, backup.BackupAlso
				continue
			}
			out = append(out, backup)
		}
	}
	return out, notes, nil
}
