// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Upgrade is an install to move to a newer release of its entry.
type Upgrade struct {
	Project     string // by ID
	Environment string // by ID
	Summary     string
	Name        string // how the project shows
	File        []byte
	// Variables are values for what the file uses.
	Variables map[string]string
	// Secrets are new secrets to keep, by name.
	Secrets map[string]string
	// Stands is what stands for each secret among Variables, which a service uses
	// by reference.
	Stands map[string]string
	Pin    *resourcev1.CatalogPin
	// SkipBackup leaves out the backup of its volumes that comes first.
	SkipBackup bool
	// Old is the file the install has now, with what it used and what stood for its
	// secrets, which nothing but reading it needs.
	Old          []byte
	OldVariables map[string]string
	OldStands    map[string]string
}

// PlanUpgrade plans an install's newer release into the environment it runs in, with
// its pin in the same change, so undoing it takes both back. It's a three-way merge of
// the old file, the new one and what runs (ADR 0029), so what the release changes
// follows it, what it dropped goes, and what a person did stays. Nothing the old file
// didn't name is touched. It backs up the volumes the release keeps first, as steps
// that come before what uses them, unless it's asked not to, and its plan says which.
func (h *Handler) PlanUpgrade(ctx context.Context, in Upgrade) (*dockwellv1.Change, error) {
	next, err := h.read(ctx, compose.Input{File: in.File, Variables: in.Variables})
	if err != nil {
		return nil, err
	}
	old, err := h.read(ctx, compose.Input{File: in.Old, Variables: in.OldVariables})
	if err != nil {
		return nil, err
	}
	useSecrets(next, in.Stands)
	useSecrets(old, in.OldStands)
	h.normalize(old)
	h.normalize(next)
	place, err := h.destination(ctx, in.Project, in.Environment, "")
	if err != nil {
		return nil, err
	}
	now, err := h.running(ctx, place.environment)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	deletes, notes, err := mergeStack(place.environment, old, now, next)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, err, "upgrade_clash")
	}
	project := resource.Ref{Kind: scope.ProjectKind, Scope: place.organization, Name: place.project}
	place.made = []revision.Edit{{Ref: project, Spec: &resourcev1.Project{DisplayName: in.Name, Catalog: in.Pin}}}
	req := &dockwellv1.ImportComposeRequest{Summary: in.Summary, Variables: in.Variables}
	var backUp []resource.Ref
	for _, v := range next.Volumes {
		if now.volumes[v.Name] {
			backUp = append(backUp, resource.Ref{Kind: volume.Kind, Scope: place.environment, Name: v.Name})
		}
	}
	say := revision.Note{
		Reason:  "backup_first",
		Message: "Dockwell backs up this app's volumes before it changes anything. If a backup fails, the upgrade stops there, with nothing else done. Undoing this puts the old version back, but not what the new one changed in its data: restore the backup for that",
		Ref:     project,
	}
	switch {
	case in.SkipBackup:
		backUp = nil
		say = revision.Note{
			Reason:  "no_backup",
			Message: "This upgrade takes no backup, as you asked. Undoing it puts the old version back, but not what the new one changed in its data, such as a database it migrated. Back up what you can't lose first",
			Ref:     project,
		}
	case len(backUp) == 0:
		return h.plan(ctx, req, next, place, planning{secrets: in.Secrets, upgrade: true, deletes: deletes, notes: notes})
	}
	return h.plan(ctx, req, next, place, planning{secrets: in.Secrets, upgrade: true, deletes: deletes, backUp: backUp, notes: append([]revision.Note{say}, notes...)})
}

// normalize gives what a file says the shape the store keeps it in, so it compares
// with what runs. One that isn't valid stays as it is, for the plan to refuse.
func (h *Handler) normalize(stack *compose.Stack) {
	for _, s := range stack.Services {
		_ = h.engine.Kinds().Validate(service.Kind, s.Spec)
	}
}

// runs is what an environment has now.
type runs struct {
	services map[string]*resourcev1.Service
	volumes  map[string]bool
	networks map[string]bool
}

// running reads what an environment has. A spec stays here, in the process that read
// it, as it may hold a person's values.
func (h *Handler) running(ctx context.Context, env string) (runs, error) {
	out := runs{services: map[string]*resourcev1.Service{}, volumes: map[string]bool{}, networks: map[string]bool{}}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListResources(ctx, env, string(service.Kind))
		if err != nil {
			return err
		}
		for i := range list {
			r := list[i]
			spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			if s, ok := spec.(*resourcev1.Service); ok {
				out.services[r.Name] = s
			}
		}
		for kind, names := range map[resource.Kind]map[string]bool{volume.Kind: out.volumes, network.Kind: out.networks} {
			list, err := tx.ListResources(ctx, env, string(kind))
			if err != nil {
				return err
			}
			for i := range list {
				names[list[i].Name] = true
			}
		}
		return nil
	})
	return out, err
}

// mergeStack turns next, what the new file says, into what the install should run,
// and says what goes and what a person should hear. What's in old and next but no
// longer here, a person took away, so it stays away. What only next names is new, unless
// something already has its name. What only old names goes, for a service, and for a
// network once nothing uses it. A volume stays, with its data, as anywhere else.
func mergeStack(env string, old *compose.Stack, now runs, next *compose.Stack) ([]resource.Ref, []revision.Note, error) {
	var notes []revision.Note
	kept := next.Services[:0]
	for _, s := range next.Services {
		was := find(old.Services, s.Name)
		have := now.services[s.Name]
		switch {
		case was != nil && have == nil:
			continue
		case was == nil && have != nil:
			return nil, nil, fmt.Errorf("this release adds a service named %s, and you run one of that name. Rename yours, then upgrade", s.Name)
		case was != nil:
			merged, clashes := mergeService(was.Spec, have, s.Spec)
			s.Spec = merged
			notes = append(notes, clashNotes(env, s.Name, clashes)...)
		}
		kept = append(kept, s)
	}
	next.Services = kept
	next.Volumes = slices.DeleteFunc(next.Volumes, func(v compose.Item[*resourcev1.Volume]) bool {
		return find(old.Volumes, v.Name) != nil && !now.volumes[v.Name]
	})
	next.Networks = slices.DeleteFunc(next.Networks, func(n compose.Item[*resourcev1.Network]) bool {
		return find(old.Networks, n.Name) != nil && !now.networks[n.Name]
	})

	var deletes []resource.Ref
	stays := maps.Clone(now.services)
	for _, s := range next.Services {
		stays[s.Name] = s.Spec
	}
	for _, s := range old.Services {
		if find(next.Services, s.Name) == nil && now.services[s.Name] != nil {
			deletes = append(deletes, resource.Ref{Kind: service.Kind, Scope: env, Name: s.Name})
			delete(stays, s.Name)
		}
	}
	for _, n := range old.Networks {
		if find(next.Networks, n.Name) != nil || !now.networks[n.Name] {
			continue
		}
		joined := false
		for s := range maps.Values(stays) {
			joined = joined || slices.Contains(s.GetApp().GetNetworks(), n.Name)
		}
		if !joined {
			deletes = append(deletes, resource.Ref{Kind: network.Kind, Scope: env, Name: n.Name})
		}
	}
	return deletes, notes, nil
}

func find[T any](items []compose.Item[T], name string) *compose.Item[T] {
	i := slices.IndexFunc(items, func(item compose.Item[T]) bool { return item.Name == name })
	if i < 0 {
		return nil
	}
	return &items[i]
}

// clash is one thing a person changed that a release changed too, by what a person
// reads. It went if the release dropped it, and otherwise it stayed.
type clash struct {
	what string
	gone bool
}

// clashNotes say what a person should check, as a plan's diff shows what changes and
// not what a person's change stood in the way of.
func clashNotes(env, name string, clashes []clash) []revision.Note {
	var kept, gone []string
	for _, c := range clashes {
		if c.gone {
			gone = append(gone, c.what)
		} else {
			kept = append(kept, c.what)
		}
	}
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: name}
	var out []revision.Note
	if len(kept) > 0 {
		out = append(out, revision.Note{
			Reason: "upgrade_kept", Ref: ref, Names: kept,
			Message: fmt.Sprintf("You changed %s on %s, and this release changes that too, so yours stays. Check the new version runs with it", human.List(kept), name),
		})
	}
	if len(gone) > 0 {
		out = append(out, revision.Note{
			Reason: "upgrade_dropped", Ref: ref, Names: gone,
			Message: fmt.Sprintf("You changed %s on %s, and this release drops it, so it goes", human.List(gone), name),
		})
	}
	return out
}

// three is the merge of one thing, absent as nil: base is what the old file gave, ours
// what runs, and next what the new file gives. What a person left follows the file, and
// what the file left stays as a person has it. Where both changed it, the release
// dropping it wins, as closing a port must close it, and otherwise a person's stays:
// taking the file's could start an app on the wrong data, or open what they closed.
func three[T any](base, ours, next *T, same func(a, b T) bool) (*T, bool) {
	eq := func(a, b *T) bool {
		if a == nil || b == nil {
			return a == b
		}
		return same(*a, *b)
	}
	switch {
	case eq(base, ours):
		return next, false
	case eq(base, next):
		return ours, false
	case eq(ours, next):
		return next, false
	case next == nil:
		return nil, true
	default:
		return ours, true
	}
}

// mergeService merges a service, what it runs and what it uses. Its revisions are the
// store's to set.
func mergeService(base, ours, next *resourcev1.Service) (*resourcev1.Service, []clash) {
	app, clashes := mergeContainer(base.GetApp(), ours.GetApp(), next.GetApp())
	variables, c := mergeStrings("variable ", base.GetVariables(), ours.GetVariables(), next.GetVariables())
	clashes = append(clashes, c...)
	secrets, c := mergeStrings("secret ", base.GetSecrets(), ours.GetSecrets(), next.GetSecrets())
	clashes = append(clashes, c...)
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}, Variables: variables, Secrets: secrets}, clashes
}

// mergeStrings merges a map by its keys. A note names a key, and never what it holds,
// as an app's environment may hold a secret.
func mergeStrings(what string, base, ours, next map[string]string) (map[string]string, []clash) {
	at := func(m map[string]string, k string) *string {
		if v, ok := m[k]; ok {
			return &v
		}
		return nil
	}
	var out map[string]string
	var clashes []clash
	for _, k := range slices.Sorted(maps.Keys(union(base, ours, next))) {
		got, clashed := three(at(base, k), at(ours, k), at(next, k), func(a, b string) bool { return a == b })
		if clashed {
			clashes = append(clashes, clash{what: what + k, gone: got == nil})
		}
		if got != nil {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = *got
		}
	}
	return out, clashes
}

func union(all ...map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, m := range all {
		for k := range m {
			out[k] = true
		}
	}
	return out
}

// keyedLists are a container's lists that hold things of their own, by what makes one
// the same thing from one version to the next. A port is the app's port, not the
// server's address for it, so limiting one to 127.0.0.1 changes that port, and
// doesn't add another. Any other list reads whole, as its order means something.
var keyedLists = map[protoreflect.Name]func(protoreflect.Value) string{
	"ports": func(v protoreflect.Value) string {
		p, _ := v.Message().Interface().(*resourcev1.PortMapping)
		if p.GetProtocol() == resourcev1.Protocol_PROTOCOL_UDP {
			return fmt.Sprintf("port %d/udp", p.GetContainerPort())
		}
		return fmt.Sprintf("port %d/tcp", p.GetContainerPort())
	},
	"mounts": func(v protoreflect.Value) string {
		m, _ := v.Message().Interface().(*resourcev1.Mount)
		return "mount " + m.GetTarget()
	},
	"tmpfs": func(v protoreflect.Value) string {
		t, _ := v.Message().Interface().(*resourcev1.Tmpfs)
		return "tmpfs " + t.GetTarget()
	},
	"networks":          word("network "),
	"external_networks": word("network "),
	"aliases":           word("alias "),
	"cap_add":           word("capability "),
	"cap_drop":          word("capability "),
	"group_add":         word("group "),
}

func word(what string) func(protoreflect.Value) string {
	return func(v protoreflect.Value) string { return what + v.String() }
}

// mergeContainer merges each field of a container, so a field added to it later is
// merged with no one thinking of it here. A person's image gives way to a release's, as
// that's what an upgrade is for.
func mergeContainer(base, ours, next *resourcev1.Container) (*resourcev1.Container, []clash) {
	out := &resourcev1.Container{}
	b, o, n, to := base.ProtoReflect(), ours.ProtoReflect(), next.ProtoReflect(), out.ProtoReflect()
	var clashes []clash
	fields := to.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		key := keyedLists[fd.Name()]
		switch {
		case fd.IsMap():
			what := strings.TrimSuffix(string(fd.Name()), "s") + " "
			got, c := mergeStrings(what, stringsOf(b, fd), stringsOf(o, fd), stringsOf(n, fd))
			clashes = append(clashes, c...)
			if len(got) > 0 {
				m := to.Mutable(fd).Map()
				for k, v := range got {
					m.Set(protoreflect.ValueOfString(k).MapKey(), protoreflect.ValueOfString(v))
				}
			}
		case fd.IsList() && key != nil:
			clashes = append(clashes, mergeList(to.Mutable(fd).List(), key, b.Get(fd).List(), o.Get(fd).List(), n.Get(fd).List())...)
		default:
			got, clashed := three(value(b, fd), value(o, fd), value(n, fd), protoreflect.Value.Equal)
			if clashed && fd.Name() == "image" {
				got = value(n, fd)
			} else if clashed {
				clashes = append(clashes, clash{what: string(fd.Name()), gone: got == nil})
			}
			if got != nil {
				to.Set(fd, *got)
			}
		}
	}
	return out, clashes
}

func value(m protoreflect.Message, fd protoreflect.FieldDescriptor) *protoreflect.Value {
	if !m.Has(fd) {
		return nil
	}
	v := m.Get(fd)
	return &v
}

func stringsOf(m protoreflect.Message, fd protoreflect.FieldDescriptor) map[string]string {
	out := map[string]string{}
	m.Get(fd).Map().Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		out[k.String()] = v.String()
		return true
	})
	return out
}

// mergeList merges a list by each thing's key, as all a key's entries together, as one
// app port can be published twice. It writes them in next's order, then what only a
// person has.
func mergeList(to protoreflect.List, key func(protoreflect.Value) string, base, ours, next protoreflect.List) []clash {
	type group = []protoreflect.Value
	groups := func(l protoreflect.List) (map[string]group, []string) {
		out, order := map[string]group{}, []string(nil)
		for i := range l.Len() {
			k := key(l.Get(i))
			if _, seen := out[k]; !seen {
				order = append(order, k)
			}
			out[k] = append(out[k], l.Get(i))
		}
		return out, order
	}
	at := func(m map[string]group, k string) *group {
		if g, ok := m[k]; ok {
			return &g
		}
		return nil
	}
	b, _ := groups(base)
	o, oursOrder := groups(ours)
	n, nextOrder := groups(next)
	var clashes []clash
	seen := map[string]bool{}
	for _, k := range slices.Concat(nextOrder, oursOrder, sortedKeys(b)) {
		if seen[k] {
			continue
		}
		seen[k] = true
		got, clashed := three(at(b, k), at(o, k), at(n, k), func(x, y group) bool { return slices.EqualFunc(x, y, protoreflect.Value.Equal) })
		if clashed {
			clashes = append(clashes, clash{what: k, gone: got == nil})
		}
		if got != nil {
			for _, v := range *got {
				to.Append(v)
			}
		}
	}
	return clashes
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }
