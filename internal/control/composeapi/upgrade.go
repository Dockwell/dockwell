// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/linked"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
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
	// Companion says it changes which of the install's companions run, or what meets
	// one of its needs, at the release it's on: no data moves, so it takes no backup and
	// says nothing of one.
	Companion bool
	// Keeps are services that leave the file and keep running as they are, with their
	// data, as the database that met a need before it's met another way.
	Keeps []string
	// FromRelease and Release are the releases an upgrade leaves and goes to, which
	// its plan's intent says (ADR 0080). A companion's change has none.
	FromRelease, Release string
	// Old is the file the install has now, with what it used and what stood for its
	// secrets, which nothing but reading it needs.
	Old          []byte
	OldVariables map[string]string
	OldStands    map[string]string
	// Notes are what else the plan says, as Install's are.
	Notes []revision.Note
	// Needs are what its needs set, and how each is met, as Install's are.
	Needs Needs
	// Hold closes the app to strangers as it restarts, as one that starts on an
	// empty database whose first visitor makes its owner (ADR 0084): each port its
	// services publish listens on the server alone (PortMapping.held), beside the
	// routes Needs.Extra holds. It's refused with cant_hold where nothing can be
	// held, or a service runs on the server's own network.
	Hold bool
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
	useValues(next, in.Needs.Stands, variable.VariableKind)
	useValues(old, in.Needs.OldStands, variable.VariableKind)
	joinLinks(next, in.Needs.Joins)
	joinLinks(old, in.Needs.Joins)
	showPlain(next)
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
	deletes = slices.DeleteFunc(deletes, func(r resource.Ref) bool { return r.Kind == service.Kind && slices.Contains(in.Keeps, r.Name) })
	if in.Hold {
		held, err := h.holdWays(ctx, place.environment, next, &in)
		if err != nil {
			return nil, err
		}
		in.Notes = append(in.Notes, held...)
	}
	project := resource.Ref{Kind: scope.ProjectKind, Scope: place.organization, Name: place.project}
	for i := range in.Notes {
		n := in.Notes[i]
		if n.Ref == (resource.Ref{}) {
			n.Ref = project
		}
		notes = append(notes, n)
	}
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
	intent := &revision.Intent{Kind: revision.IntentUpgrade, Resource: in.Project, FromRelease: in.FromRelease, Release: in.Release}
	// The app's own database on a server another project runs is dumped alone first,
	// as its volumes are backed up, for its app (ADR 0084).
	var ones []revision.OneBackup
	if !in.Companion && !in.SkipBackup {
		if ones, err = h.linkedDatabases(ctx, place.environment); err != nil {
			return nil, h.internal(ctx, err)
		}
		if len(ones) > 0 {
			say.Message = "Dockwell backs up this app's volumes, and dumps its database alone where another project runs it, before it changes anything. If a backup fails, the upgrade stops there, with nothing else done. Undoing this puts the old version back, but not what the new one changed in its data: restore the backup for that"
		}
	}
	switch {
	case in.Companion:
		return h.plan(ctx, req, next, place, planning{needs: in.Needs, secrets: in.Secrets, upgrade: true, deletes: deletes, notes: notes})
	case in.SkipBackup:
		backUp = nil
		say = revision.Note{
			Reason:  "no_backup",
			Message: "This upgrade takes no backup, as you asked. Undoing it puts the old version back, but not what the new one changed in its data, such as a database it migrated. Back up what you can't lose first",
			Ref:     project,
		}
	case len(backUp) == 0 && len(ones) == 0:
		return h.plan(ctx, req, next, place, planning{needs: in.Needs, secrets: in.Secrets, upgrade: true, deletes: deletes, notes: notes, intent: intent})
	}
	return h.plan(ctx, req, next, place, planning{needs: in.Needs, secrets: in.Secrets, upgrade: true, deletes: deletes, backUp: backUp, backUpOne: ones, skipBackup: in.SkipBackup, notes: append([]revision.Note{say}, notes...), intent: intent})
}

// linkedDatabases are the databases an app in env has alone on servers other
// environments run, through its links, each to dump first, made for its link.
func (h *Handler) linkedDatabases(ctx context.Context, env string) ([]revision.OneBackup, error) {
	var out []revision.OneBackup
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		out, err = linked.Databases(ctx, tx, h.keeper, env, nil)
		return err
	})
	return out, err
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
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}, Variables: variables, Secrets: secrets, ShownEnv: mergeShown(app, ours, next)}, clashes
}

// mergeShown is which of a merged app's own values a read shows: each that has the
// value what it came from showed, what runs or the new file. A value that's neither
// is hidden, as nothing says what it is (ADR 0081).
func mergeShown(app *resourcev1.Container, sides ...*resourcev1.Service) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(app.GetEnv())) {
		for _, side := range sides {
			if v, ok := side.GetApp().GetEnv()[name]; ok && v == app.GetEnv()[name] && slices.Contains(side.GetShownEnv(), name) {
				out = append(out, name)
				break
			}
		}
	}
	return out
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
	"extra_hosts":       word("host "),
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

// holdWays holds what reaches the app's published ports from the server itself, a
// route of its environment that sends to one of those ports on the server, which
// dials 127.0.0.1, then each port (hold). A route of another environment that sends
// to one on the app's server can't be held from here, so it's refused with
// cant_hold. Where the server's Docker is older than 28, which lets machines on its
// local network reach a port bound to 127.0.0.1, the plan says so.
func (h *Handler) holdWays(ctx context.Context, env string, next *compose.Stack, in *Upgrade) ([]revision.Note, error) {
	ports := map[uint32]bool{}
	for _, s := range next.Services {
		for _, p := range s.Spec.GetApp().GetPorts() {
			if p.GetHostPort() != 0 {
				ports[p.GetHostPort()] = true
			}
		}
	}
	var notes []revision.Note
	err := h.db.View(ctx, func(tx *store.Tx) error {
		servers := map[string]bool{}
		services, err := tx.ListResources(ctx, env, string(service.Kind))
		if err != nil {
			return err
		}
		for _, r := range services {
			on, err := tx.Placement(ctx, r.ID, r.Revision)
			if err != nil {
				return err
			}
			for _, s := range on {
				servers[s] = true
			}
		}
		routes, err := tx.ListKind(ctx, "route")
		if err != nil {
			return err
		}
		for _, r := range routes {
			if r.Spec == nil {
				continue
			}
			// A route is sealed since it could hold users' password hashes (ADR 0087).
			plain, err := tx.Open(ctx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			spec := &resourcev1.Route{}
			if proto.Unmarshal(plain, spec) != nil || spec.GetService() != "" || !ports[spec.GetHostPort()] || spec.GetHeld() {
				continue
			}
			if r.Scope == env {
				spec.Held = true
				in.Needs.Extra = append(in.Needs.Extra, &dockwellv1.Edit{
					Resource: &dockwellv1.ResourceRef{Kind: "route", Scope: env, Name: r.Name},
					Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: spec}},
				})
				continue
			}
			on, err := tx.Placement(ctx, r.ID, r.Revision)
			if err != nil {
				return err
			}
			if slices.ContainsFunc(on, func(s string) bool { return servers[s] }) {
				return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("a domain of another environment sends to port %d on %s's server, which Dockwell can't hold from here while %s starts empty, so whoever reaches it first could take it. Take that domain off the port first, or copy its database across instead", spec.GetHostPort(), in.Name, in.Name), "cant_hold")
			}
		}
		if len(ports) == 0 {
			return nil
		}
		reports, err := tx.ServerReports(ctx)
		if err != nil {
			return err
		}
		all, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		for i := range all {
			sv := &all[i]
			var inv dockwellv1.ServerInventory
			if !servers[sv.ID] || proto.Unmarshal(reports[sv.ID].Inventory, &inv) != nil || !beforeDocker28(inv.GetDockerVersion()) {
				continue
			}
			notes = append(notes, revision.Note{
				Reason: "hold_local_network",
				Message: fmt.Sprintf("Docker %s on %s lets machines on its local network reach a port bound to 127.0.0.1, which Docker 28 closed, so while %s is held its ports are closed to the internet but not to that network. Update Docker on %s to 28 or later to close them there too",
					inv.GetDockerVersion(), sv.Name, in.Name, sv.Name),
				Names: []string{sv.Name},
			})
		}
		return nil
	})
	if err != nil {
		var reason *connect.Error
		if errors.As(err, &reason) {
			return nil, err
		}
		return nil, h.internal(ctx, err)
	}
	return notes, hold(next, in.Name, in.Needs.Extra)
}

// beforeDocker28 reports a Docker Engine release before 28, as its version says.
func beforeDocker28(version string) bool {
	major, _, ok := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	return ok && err == nil && n < 28
}

// hold has each port the file's services publish listen on the server alone, and
// refuses an app that would still be open to strangers: one with a service on the
// server's own network, or with nothing to hold, no route among routes nor a port.
func hold(next *compose.Stack, app string, routes []*dockwellv1.Edit) error {
	held := 0
	for _, e := range routes {
		if e.GetSpec().GetRoute().GetHeld() {
			held++
		}
	}
	for _, s := range next.Services {
		c := s.Spec.GetApp()
		if c.GetHostNetwork() {
			return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s runs on the server's own network, which Dockwell can't close to strangers while %s starts empty and its first visitor makes its owner. Copy its database across instead", s.Name, app), "cant_hold")
		}
		for _, p := range c.GetPorts() {
			p.Held = true
			held++
		}
	}
	if held == 0 {
		return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s's first visitor makes its owner, and it has no domain or published port Dockwell can hold while it starts empty, so whoever reaches it first could take it. Give it a domain first, or copy its database across instead", app), "cant_hold")
	}
	return nil
}
