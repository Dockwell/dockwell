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
	access         resource.Access
}

// pathHolds is what's so of a server as a change finds it and as it leaves it.
type pathHolds struct {
	before, after []pathHold
}

// serverMount is a server path a resource mounts.
type serverMount struct {
	ref    resource.Ref
	access resource.Access
}

// holdingsOn works out what holds what on server. Most changes mount no server path,
// and those read nothing more.
func (e *Engine) holdingsOn(ctx context.Context, server string, its []*item, before, after map[resource.Ref]proto.Message, leads map[string]string) (pathHolds, error) {
	var was, is []serverMount
	for _, it := range its {
		was = append(was, mountsOf(it.ref, e.reach(it.ref, it.currentOn(server), before))...)
		is = append(is, mountsOf(it.ref, e.reach(it.ref, it.desiredOn(server), after))...)
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
		was = append(was, mountsOf(o.ref, e.reach(o.ref, o.currentOn(server), namedBefore))...)
		is = append(is, mountsOf(o.ref, e.reach(o.ref, o.desiredOn(server), namedAfter))...)
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

// mountsOf is the server paths in what ref reaches.
func mountsOf(ref resource.Ref, list []resource.Access) []serverMount {
	var out []serverMount
	for i := range list {
		if list[i].Kind == resource.AccessPath {
			out = append(out, serverMount{ref: ref, access: list[i]})
		}
	}
	return out
}

// holdingsIn pairs each read-write mount with the sources strictly inside it. One that's as
// good as root already has nothing to add, and a mount point can't be replaced from
// inside, so a source at the very path is no pair.
func holdingsIn(list []serverMount, leads map[string]string) []pathHold {
	var out []pathHold
	for i := range list {
		h := &list[i]
		if h.access.ReadOnly || h.access.Leading(leads[h.access.Path]).AsGoodAsRoot() {
			continue
		}
		for j := range list {
			s := &list[j]
			if strictlyInside(h.access.Path, s.access.Path, leads) {
				out = append(out, pathHold{holder: h.ref, source: s.ref, access: resource.Access{
					Kind: resource.AccessPath, Path: h.access.Path, Holds: s.access.Path, SameAsRoot: true,
				}})
			}
		}
	}
	return out
}

// strictlyInside reports source inside holder, as either is written or where it leads.
func strictlyInside(holder, source string, leads map[string]string) bool {
	for _, h := range []string{holder, leads[holder]} {
		for _, s := range []string{source, leads[source]} {
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
			out = append(out, h.after[i].access)
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
			out = append(out, a.access)
		}
	}
	return out
}
