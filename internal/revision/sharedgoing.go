// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/id"
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
	err = e.db.View(ctx, func(tx *store.Tx) (err error) {
		shared, meeting, to, err = e.sharedWithIn(ctx, tx, ed)
		return err
	})
	return shared, meeting, to, err
}

// sharedWithIn is sharedWith, read with tx.
func (e *Engine) sharedWithIn(ctx context.Context, tx *store.Tx, ed Edit) (shared []resource.Ref, meeting []sharedLink, to string, err error) {
	err = func() error {
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
			// A project with one environment, or an environment by ID.
			project, environment := ed.KeepSharedIn, ""
			if strings.HasPrefix(project, id.Environment+"_") {
				project, environment = "", project
			}
			// Among the caller's own places, so a name elsewhere reads as one that
			// isn't there, and no answer names another's project.
			p, env, err := place.Reachable(ctx, tree).Resolve(project, environment)
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
	}()
	sortRefs(shared)
	return shared, meeting, to, err
}

// maxWritten bounds how far writtenNow follows values written from others, as the
// keeper does where a change runs.
const maxWritten = 4

// writtenNow is what the variable or secret ref holds now, written out: what it
// holds, or what what it comes from holds, read the same way.
func (e *Engine) writtenNow(ctx context.Context, tx *store.Tx, ref resource.Ref, depth int) (string, error) {
	it, err := e.load(ctx, tx, ref)
	if err != nil {
		return "", err
	}
	t, err := e.kinds.Type(ref.Kind)
	if err != nil {
		return "", err
	}
	valuer, ok := t.(resource.Valuer)
	if !ok || it.current == nil {
		return "", fault.New(ErrInvalid, "%s has no value to hold", ref)
	}
	from := variable.FromOf(it.current)
	if from == nil {
		return valuer.Value(it.current), nil
	}
	if depth >= maxWritten {
		return "", fault.New(ErrInvalid, "%s is written from others more than %d deep, as one names itself through another. Write one of them out as a value of its own", ref, maxWritten)
	}
	uses := resource.Uses(t, it.current)
	read := func(u resource.Use) (string, error) {
		return e.writtenNow(ctx, tx, resource.Ref{Kind: u.Kind, Scope: cmp.Or(u.Scope, ref.Scope), Name: u.Name}, depth+1)
	}
	switch src := from.GetSource().(type) {
	case *resourcev1.ValueFrom_Template:
		values := make(map[string]string, len(uses))
		for _, u := range uses {
			v, err := read(u)
			if err != nil {
				return "", err
			}
			values[u.Name] = v
		}
		return variable.Write(src.Template, values)
	case *resourcev1.ValueFrom_Value:
		if len(uses) != 1 {
			return "", fault.New(ErrInvalid, "%s's value from another environment names none", ref)
		}
		return read(uses[0])
	}
	return "", fault.New(ErrInvalid, "%s says it comes from somewhere, and not where", ref)
}

// breakingRoles refuses deleting what other projects' apps use too (Edit.DeleteShared)
// to someone without a developer's role, or more, where each link it breaks is: it
// stops those apps, which is theirs to do, and in a protected environment an
// admin's to apply (the review of #572, its M3). Where the caller holds nothing,
// the refusal names no app nor project.
func (e *Engine) breakingRoles(ctx context.Context, tx *store.Tx, grants []store.Grant, it *item, a act) error {
	if !it.deleteShared {
		return nil
	}
	_, meeting, _, err := e.sharedWithIn(ctx, tx, Edit{Ref: it.ref, WithContents: true, DeleteShared: true})
	if err != nil {
		return err
	}
	for _, m := range meeting {
		at, err := place.Of(ctx, tx, m.ref.Scope)
		if err != nil || len(at) != 1 {
			return cmp.Or(err, fault.New(ErrInvalid, "can't tell where link %s is", m.ref.Name))
		}
		held := place.Holds(grants, at[0])
		needs, why := store.RoleDeveloper, "it deletes what "+m.ref.Name+" in "+m.project+" uses, which stops that app"
		if a != planning && at[0].Protected {
			needs, why = store.RoleAdmin, why+", in a protected environment"
		}
		if held == "" {
			why = "it deletes what apps in projects you don't hold use, which stops them"
		}
		if store.RoleRank(held) < store.RoleRank(needs) {
			return &Refusal{act: a, Held: held, Needs: needs, Why: why}
		}
	}
	return nil
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
					// What it reads now, written out, so a value that's written from others is
					// held at what it comes to (the review of #572, its L7).
					value, err := e.writtenNow(ctx, tx, resource.Ref{Kind: fromKind, Scope: v.GetEnvironment(), Name: v.GetName()}, 0)
					if err != nil {
						return err
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
