// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"path"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// A mount that can write the folder another mount's source is in can turn that source
// into a link to anywhere on the server, and Docker follows it the next time what
// mounts it starts. The agent checks every start it asks for, but Docker's own
// restarts ask nobody, after a crash or a reboot. So at plan time, a read-write mount
// that holds another mount's source on the same server counts as reaching whatever
// that source could be made to lead to, which is as good as root.
//
// It goes by what the specs say, as the rest of what a plan gains does, and by where
// the agent says a path leads for what's in the plan. What isn't in the plan is
// compared as it's written.

// pathHold is one mount able to move another's source.
type pathHold struct {
	holder, source resource.Ref
	// in is the holder's path, and at the source's.
	in, at string
}

// accessFor is what the hold is to ref, on whichever side of it ref is, naming the
// other side. A change on either side can make one, and each person approving it has
// to see who can move what, and that the holder asks from now on.
func (h pathHold) accessFor(ref resource.Ref) resource.Access {
	if h.holder == ref {
		a := resource.Access{Kind: resource.AccessPath, Path: h.in, Holds: h.at, SameAsRoot: true}
		if h.source != h.holder {
			a.HoldsFor = h.source.Name
		}
		return a
	}
	return resource.Access{Kind: resource.AccessPath, Path: h.at, HeldIn: h.in, HeldBy: h.holder.Name, SameAsRoot: true}
}

// pathHolds is what's so of a server as a change finds it and as it leaves it.
type pathHolds struct {
	before, after []pathHold
}

// serverMount is a server path a resource mounts.
type serverMount struct {
	ref    resource.Ref
	access resource.Access
	// root says the resource is as good as root already, by what else it reaches.
	root bool
	// unmanaged says Dockwell didn't make it, so it's only ever what's held: its code
	// isn't what a token or a plan here chooses.
	unmanaged bool
}

// unmanagedKind names a container Dockwell didn't make, on the held side of a hold.
const unmanagedKind resource.Kind = "unmanaged"

// holdingsOn works out what holds what on server. Most changes mount no server path,
// and those read nothing more.
func (e *Engine) holdingsOn(ctx context.Context, server string, its []*item, before, after map[resource.Ref]proto.Message, leads map[string]string, alongside []resource.Unmanaged) (pathHolds, error) {
	var was, is []serverMount
	for _, it := range its {
		was = append(was, mountsOf(it.ref, e.reach(it.ref, it.currentOn(server), before), leads)...)
		is = append(is, mountsOf(it.ref, e.reach(it.ref, it.desiredOn(server), after), leads)...)
	}
	if len(was)+len(is) == 0 {
		return pathHolds{}, nil
	}
	others, err := e.othersOn(ctx, server, its)
	if err != nil {
		return pathHolds{}, err
	}
	loaded := make([]item, 0, len(others)+len(its))
	loaded = append(loaded, others...)
	for _, it := range its {
		loaded = append(loaded, *it)
	}
	namedBefore, namedAfter, err := e.named(ctx, loaded)
	if err != nil {
		return pathHolds{}, err
	}
	for i := range others {
		o := &others[i]
		was = append(was, mountsOf(o.ref, e.reach(o.ref, o.currentOn(server), namedBefore), leads)...)
		is = append(is, mountsOf(o.ref, e.reach(o.ref, o.desiredOn(server), namedAfter), leads)...)
	}
	// What Dockwell didn't make is as the change finds it and as it leaves it.
	for _, u := range alongside {
		ref := resource.Ref{Kind: unmanagedKind, Scope: server, Name: u.Name}
		for _, p := range u.Paths {
			m := serverMount{ref: ref, access: resource.Access{Kind: resource.AccessPath, Path: p}, root: u.Root, unmanaged: true}
			was, is = append(was, m), append(is, m)
		}
	}
	return pathHolds{before: holdingsIn(was, leads), after: holdingsIn(is, leads)}, nil
}

// othersOn reads every service and container on server that the change leaves alone.
func (e *Engine) othersOn(ctx context.Context, server string, its []*item) ([]item, error) {
	var out []item
	err := e.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		var found []store.Resource
		for i := range tree.Projects {
			for _, env := range tree.Projects[i].Environments {
				services, err := tx.ListResources(ctx, env.ID, "service")
				if err != nil {
					return err
				}
				found = append(found, services...)
			}
		}
		containers, err := tx.ListResources(ctx, server, "container")
		if err != nil {
			return err
		}
		for _, r := range append(found, containers...) {
			ref := resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}
			if slices.ContainsFunc(its, func(it *item) bool { return it.ref == ref }) {
				continue
			}
			it, err := e.load(ctx, tx, ref)
			if err != nil {
				return err
			}
			if slices.Contains(it.placed, server) {
				it.spec, it.servers = it.current, it.placed
				out = append(out, it)
			}
		}
		return nil
	})
	return out, err
}

// mountsOf is the server paths in what ref reaches, and whether it's as good as root.
func mountsOf(ref resource.Ref, list []resource.Access, leads map[string]string) []serverMount {
	root := slices.ContainsFunc(list, func(a resource.Access) bool { return a.Leading(leads[a.Path]).AsGoodAsRoot() })
	var out []serverMount
	for i := range list {
		if list[i].Kind == resource.AccessPath {
			out = append(out, serverMount{ref: ref, access: list[i], root: root})
		}
	}
	return out
}

// holdingsIn pairs each read-write mount with the sources strictly inside it. A mount
// point can't be replaced from inside, so a source at the very path is no pair. A
// holder that's as good as root already asks for everything, and what's held that is
// gains nothing by being moved, so neither makes one.
func holdingsIn(list []serverMount, leads map[string]string) []pathHold {
	var out []pathHold
	for i := range list {
		h := &list[i]
		if h.unmanaged || h.access.ReadOnly || h.access.Leading(leads[h.access.Path]).AsGoodAsRoot() {
			continue
		}
		for j := range list {
			s := &list[j]
			if !s.root && strictlyInside(h.access.Path, s.access.Path, leads) {
				out = append(out, pathHold{holder: h.ref, source: s.ref, in: h.access.Path, at: s.access.Path})
			}
		}
	}
	return out
}

// strictlyInside reports source inside holder, as either is written or where it leads.
// What a holder can replace is the source's last part, in whatever folder that's in,
// even where the source itself leads elsewhere already.
func strictlyInside(holder, source string, leads map[string]string) bool {
	sources := []string{source, leads[source]}
	if dir := leads[path.Dir(path.Clean(source))]; dir != "" {
		sources = append(sources, path.Join(dir, path.Base(source)))
	}
	for _, h := range []string{holder, leads[holder]} {
		for _, s := range sources {
			if h != "" && s != "" && path.Clean(h) != path.Clean(s) && resource.Within(path.Clean(s), path.Clean(h)) {
				return true
			}
		}
	}
	return false
}

// holderOf is what ref holds as the change leaves it, which is what it reaches.
func (h pathHolds) holderOf(ref resource.Ref) []resource.Access {
	var out []resource.Access
	for i := range h.after {
		if h.after[i].holder == ref {
			out = append(out, h.after[i].accessFor(ref))
		}
	}
	return out
}

// gained is what the change newly lets ref move or be moved by. A volume's path can't
// change once it's made, so one side of what's new is always in the change.
func (h pathHolds) gained(ref resource.Ref) []resource.Access {
	var out []resource.Access
	for i := range h.after {
		a := &h.after[i]
		if (a.holder == ref || a.source == ref) && !slices.Contains(h.before, *a) {
			out = append(out, a.accessFor(ref))
		}
	}
	return out
}
