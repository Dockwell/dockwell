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

// held is one claim of a resource as it stands once a change applies.
type held struct {
	claim resource.Claim
	ref   resource.Ref
}

// checkClaims refuses a change that would have two resources hold one thing
// that only one may, across every scope, such as a domain and a path; or two
// scopes hold what only one may, such as a domain. It looks at what is stored
// and what the change makes and deletes, so a change can move a claim from
// one resource to another. It checks only what the change makes or edits, so
// what was stored before a rule existed holds up only a change to it.
func (e *Engine) checkClaims(ctx context.Context, tx *store.Tx, items []item) error {
	inChange := make(map[resource.Ref]bool, len(items))
	wanted := false
	for i := range items {
		inChange[items[i].ref] = true
		wanted = wanted || len(e.kinds.Claims(items[i].ref.Kind, items[i].spec)) > 0
	}
	if !wanted {
		return nil
	}
	var all []held
	for _, kind := range e.kinds.Claimers() {
		stored, err := tx.ListKind(ctx, string(kind))
		if err != nil {
			return err
		}
		for _, r := range stored {
			ref := resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}
			if inChange[ref] {
				continue
			}
			spec, err := e.kinds.Decode(ref.Kind, r.Spec)
			if err != nil {
				return err
			}
			for _, c := range e.kinds.Claims(ref.Kind, spec) {
				all = append(all, held{claim: c, ref: ref})
			}
		}
	}
	// Each item is checked against what is stored, and the items before it,
	// so two items that claim one thing are found once, by the second.
	for i := range items {
		it := &items[i]
		for _, c := range e.kinds.Claims(it.ref.Kind, it.spec) {
			for _, other := range all {
				if err := e.conflict(ctx, tx, it, c, other, inChange[other.ref]); err != nil {
					return err
				}
			}
		}
		for _, c := range e.kinds.Claims(it.ref.Kind, it.spec) {
			all = append(all, held{claim: c, ref: it.ref})
		}
	}
	return nil
}

// conflict says why a claim of it can't stand beside what other holds.
func (e *Engine) conflict(ctx context.Context, tx *store.Tx, it *item, c resource.Claim, other held, sameChange bool) error {
	switch {
	case c.Key == other.claim.Key && sameChange:
		return fault.New(ErrInvalid, "this change has %s %s and %s %s both claim %s, and only one can. Keep one of them",
			other.ref.Kind, other.ref.Name, it.ref.Kind, it.ref.Name, c.Words)
	case c.Key == other.claim.Key:
		return fault.New(ErrInvalid, "%s is already served by %s %s in the environment %s, so %s %s can't claim it. Delete %s first, or in this change, or choose another path",
			c.Words, other.ref.Kind, other.ref.Name, scopeName(ctx, tx, other.ref.Scope), it.ref.Kind, it.ref.Name, other.ref.Name)
	case c.Group == other.claim.Group && it.ref.Scope != other.ref.Scope:
		return fault.New(ErrInvalid, "%s belongs to the environment %s, where %s %s serves it, so %s %s in the environment %s can't serve a path under it: a domain lives in one environment. Move the domain's routes first, or in this change, or use another domain",
			c.GroupWords, scopeName(ctx, tx, other.ref.Scope), other.ref.Kind, other.ref.Name, it.ref.Kind, it.ref.Name, scopeName(ctx, tx, it.ref.Scope))
	}
	return nil
}

// scopeName is what a person calls a scope, or its ID if it can't be read.
func scopeName(ctx context.Context, tx *store.Tx, scope string) string {
	if r, err := tx.GetResourceByID(ctx, scope); err == nil {
		return r.Name
	}
	return scope
}

// checkOneServer refuses a resource of a kind that runs on one server at a
// time, placed on several.
func (e *Engine) checkOneServer(ctx context.Context, tx *store.Tx, it *item) error {
	if len(it.servers) < 2 {
		return nil
	}
	t, err := e.kinds.Type(it.ref.Kind)
	if err != nil {
		return err
	}
	one, ok := t.(resource.SingleServer)
	if !ok {
		return nil
	}
	names := make([]string, len(it.servers))
	for i, s := range it.servers {
		names[i] = s
		if srv, err := tx.GetServer(ctx, s); err == nil {
			names[i] = srv.Name
		}
	}
	return fault.New(ErrInvalid, "%s %s can run on one server at a time, as %s. Name one of %s", it.ref.Kind, it.ref.Name, one.OneServer(), oneOf(names))
}

func oneOf(names []string) string {
	out := ""
	for i, n := range names {
		switch {
		case i == 0:
			out = n
		case i == len(names)-1:
			out += " and " + n
		default:
			out += ", " + n
		}
	}
	return out
}

// takeovers is what each item takes from another scope: a Group it claims
// that its own scope doesn't hold, which another scope held within the undo
// window. A token could otherwise delete a route in one change and claim its
// domain from elsewhere in the next, so a person approves it (ADR 0024). A
// plan's fingerprint covers it, and it counts as host access.
func (e *Engine) takeovers(ctx context.Context, items []item) (map[resource.Ref][]resource.Access, error) {
	wanted := false
	for i := range items {
		wanted = wanted || len(e.kinds.Claims(items[i].ref.Kind, items[i].spec)) > 0
	}
	if !wanted {
		return nil, nil
	}
	out := map[resource.Ref][]resource.Access{}
	err := e.db.View(ctx, func(tx *store.Tx) error {
		since := e.now().Add(-UndoWindow)
		for _, kind := range e.kinds.Claimers() {
			past, err := tx.SupersededSince(ctx, string(kind), since)
			if err != nil {
				return err
			}
			held := map[string][]string{} // group → the scopes that held it
			add := func(scope string, spec proto.Message) {
				for _, c := range e.kinds.Claims(kind, spec) {
					if !slices.Contains(held[c.Group], scope) {
						held[c.Group] = append(held[c.Group], scope)
					}
				}
			}
			for _, p := range past {
				spec, err := e.kinds.Decode(kind, p.Spec)
				if err != nil {
					return err
				}
				add(p.Scope, spec)
			}
			// What the change itself takes from a resource counts as much as
			// what a change did a day ago: a move in one change is a takeover
			// too.
			for i := range items {
				if items[i].ref.Kind == kind {
					add(items[i].ref.Scope, items[i].current)
				}
			}
			for i := range items {
				it := &items[i]
				if it.ref.Kind != kind {
					continue
				}
				if err := e.takeover(ctx, tx, it, held, out); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return out, err
}

// takeover adds what one item takes from another scope.
func (e *Engine) takeover(ctx context.Context, tx *store.Tx, it *item, past map[string][]string, out map[resource.Ref][]resource.Access) error {
	t, err := e.kinds.Type(it.ref.Kind)
	if err != nil {
		return err
	}
	takes, ok := t.(resource.TakesOver)
	if !ok {
		return nil
	}
	for _, c := range e.kinds.Claims(it.ref.Kind, it.spec) {
		has, err := e.scopeHolds(ctx, tx, it, c.Group)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		for _, scope := range past[c.Group] {
			if scope != it.ref.Scope {
				out[it.ref] = append(out[it.ref], takes.TakesOver(c.Group, scopeName(ctx, tx, scope)))
			}
		}
	}
	return nil
}

// scopeHolds reports whether the item's scope holds a Group already, by the
// item as it stands now, or another resource of its kind.
func (e *Engine) scopeHolds(ctx context.Context, tx *store.Tx, it *item, group string) (bool, error) {
	holds := func(spec proto.Message) bool {
		return slices.ContainsFunc(e.kinds.Claims(it.ref.Kind, spec), func(c resource.Claim) bool { return c.Group == group })
	}
	if holds(it.current) {
		return true, nil
	}
	stored, err := tx.ListResources(ctx, it.ref.Scope, string(it.ref.Kind))
	if err != nil {
		return false, err
	}
	for _, r := range stored {
		if r.Name == it.ref.Name {
			continue
		}
		spec, err := e.kinds.Decode(it.ref.Kind, r.Spec)
		if err != nil {
			return false, err
		}
		if holds(spec) {
			return true, nil
		}
	}
	return false, nil
}
