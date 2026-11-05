// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// live is a resource as it stands once a change applies.
type live struct {
	ref  resource.Ref
	spec proto.Message // read only for a kind that names others
	// servers are where it runs, less removed servers.
	servers []string
}

// checkReferences refuses a change that would leave a resource naming one
// that isn't there once it applies (resource.Referrer): a service that
// mounts a volume, or joins a network, its environment won't have, or
// won't have on a server the service runs on. So a volume or network in
// use can't be deleted, or taken off a server where it is used.
//
// It looks at what is stored and what the change makes, deletes and
// moves, but only at the pairs the change touches: a resource it makes or
// changes, and one it deletes or moves that a live one names. So what was
// stored before the check existed holds up only a change to it. The agent
// checks again as it applies, for what was made outside Dockwell.
func (e *Engine) checkReferences(ctx context.Context, tx *store.Tx, items []item) error {
	if !e.kinds.HasReferrers() {
		return nil
	}
	inChange := make(map[resource.Ref]*item, len(items))
	var scopes []string
	for i := range items {
		inChange[items[i].ref] = &items[i]
		if !slices.Contains(scopes, items[i].ref.Scope) {
			scopes = append(scopes, items[i].ref.Scope)
		}
	}
	names := map[string]string{} // server ID → name
	serverName := func(id string) (string, error) {
		if n, ok := names[id]; ok {
			return n, nil
		}
		s, err := tx.GetServer(ctx, id)
		if err != nil {
			return "", err
		}
		names[id] = s.Name
		return s.Name, nil
	}
	for _, scopeID := range scopes {
		all, err := e.standing(ctx, tx, items, scopeID)
		if err != nil {
			return err
		}
		byRef := make(map[resource.Ref]*live, len(all))
		for i := range all {
			byRef[all[i].ref] = &all[i]
		}
		for i := range all {
			r := &all[i]
			_, changed := inChange[r.ref]
			for _, use := range e.kinds.References(r.ref.Kind, r.spec) {
				target := resource.Ref{Kind: use.Kind, Scope: scopeID, Name: use.Name}
				ti, touched := inChange[target]
				if !changed && !touched {
					continue
				}
				t, ok := byRef[target]
				switch {
				case !ok && touched && ti.spec == nil && changed:
					return fault.New(ErrInvalid, "this change deletes %s %s, but %s %s %ss it. Keep %s, or change %s so it doesn't %s it",
						use.Kind, use.Name, r.ref.Kind, r.ref.Name, use.Verb, use.Name, r.ref.Name, use.Verb)
				case !ok && touched && ti.spec == nil:
					return fault.New(ErrInvalid, "%s %s can't go while %s %s %ss it. Change %s so it doesn't, or delete it, first, in this change or an earlier one",
						use.Kind, use.Name, r.ref.Kind, r.ref.Name, use.Verb, r.ref.Name)
				case !ok:
					return fault.New(ErrInvalid, "%s %s %ss %s %s, which isn't in its environment. Add %s to it, in this change or an earlier one",
						r.ref.Kind, r.ref.Name, use.Verb, use.Kind, use.Name, use.Name)
				}
				for _, s := range r.servers {
					if slices.Contains(t.servers, s) {
						continue
					}
					name, err := serverName(s)
					if err != nil {
						return err
					}
					return fault.New(ErrInvalid, "%s %s runs on %s, but %s %s isn't placed there, so %s can't %s it. Place %s on %s too, or %s where %s is",
						r.ref.Kind, r.ref.Name, name, use.Kind, use.Name, r.ref.Name, use.Verb, use.Name, name, r.ref.Name, use.Name)
				}
			}
		}
	}
	return nil
}

// standing lists what lives in a scope once the change applies, with
// where each runs: what is stored there, less what the change deletes,
// with what it makes and changes.
func (e *Engine) standing(ctx context.Context, tx *store.Tx, items []item, scopeID string) ([]live, error) {
	stored, err := tx.ListResources(ctx, scopeID, "")
	if err != nil {
		return nil, err
	}
	inChange := map[resource.Ref]*item{}
	for i := range items {
		if items[i].ref.Scope == scopeID {
			inChange[items[i].ref] = &items[i]
		}
	}
	// What is stored and not in the change matters only for what it names,
	// and only if the change touches a kind it can name. Otherwise its spec
	// stays sealed: a rotation opens the services it pins again, and no
	// others (ADR 0022).
	named := false
	for ref := range inChange {
		named = named || e.kinds.Named(ref.Kind)
	}
	var out []live
	for _, r := range stored {
		ref := resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}
		if _, ok := inChange[ref]; ok {
			continue
		}
		l := live{ref: ref}
		if named && e.isReferrer(ref.Kind) {
			if l.spec, err = e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec); err != nil {
				return nil, err
			}
		}
		placed, err := tx.Placement(ctx, r.ID, r.Revision)
		if err != nil {
			return nil, err
		}
		for _, s := range placed {
			gone, err := serverGone(ctx, tx, s)
			if err != nil {
				return nil, err
			}
			if !gone {
				l.servers = append(l.servers, s)
			}
		}
		out = append(out, l)
	}
	for i := range items {
		it := &items[i]
		if it.ref.Scope == scopeID && it.spec != nil {
			out = append(out, live{ref: it.ref, spec: it.spec, servers: it.servers})
		}
	}
	return out, nil
}

// isReferrer reports a kind whose specs name others.
func (e *Engine) isReferrer(kind resource.Kind) bool {
	t, err := e.kinds.Type(kind)
	if err != nil {
		return false
	}
	_, ok := t.(resource.Referrer)
	return ok
}

// orderByReference puts what a resource names, in the change, before it:
// so what it needs is made first, and taken down last, as deletes run last
// first. Otherwise the proposal's order stays.
func (e *Engine) orderByReference(items []item) []item {
	at := make(map[resource.Ref]int, len(items))
	for i := range items {
		at[items[i].ref] = i
	}
	const visiting, done = 1, 2
	state := make([]int, len(items))
	out := make([]item, 0, len(items))
	var visit func(i int)
	visit = func(i int) {
		if state[i] != 0 {
			return // done, or a cycle, which keeps the proposal's order
		}
		state[i] = visiting
		it := &items[i]
		for _, spec := range []proto.Message{it.spec, it.current} {
			for _, use := range e.kinds.References(it.ref.Kind, spec) {
				if j, ok := at[resource.Ref{Kind: use.Kind, Scope: it.ref.Scope, Name: use.Name}]; ok {
					visit(j)
				}
			}
		}
		state[i] = done
		out = append(out, *it)
	}
	for i := range items {
		visit(i)
	}
	return out
}
