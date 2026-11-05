// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// A resource that uses others in its scope by name, such as a service that
// uses its environment's variables and secrets (resource.Binder), pins
// each to the revision it resolves to whenever a change is planned. So:
//   - a change to a variable or secret also changes every service that
//     uses it, which the change takes in, and the plan shows each restart
//     with the new value, on each server: rotation is a plan;
//   - a server gets a spec that says exactly which revision of each value
//     it runs with, and the values are read at those revisions;
//   - a change applies only while what it pinned is still the latest.
// See docs/adr/0022-secrets-encrypted-at-rest.md.

// binder returns the item's kind as a Binder, if it is one.
func (e *Engine) binder(kind resource.Kind) (resource.Binder, bool) {
	t, err := e.kinds.Type(kind)
	if err != nil {
		return nil, false
	}
	b, ok := t.(resource.Binder)
	return b, ok
}

// valuer reports a kind whose resources others use.
func (e *Engine) valuer(kind resource.Kind) bool {
	t, err := e.kinds.Type(kind)
	if err != nil {
		return false
	}
	_, ok := t.(resource.Valuer)
	return ok
}

// usesOf lists what an item's spec uses, keyed in its scope.
func (e *Engine) usesOf(it *item, spec proto.Message) []resource.Use {
	b, ok := e.binder(it.ref.Kind)
	if !ok || spec == nil {
		return nil
	}
	return b.Uses(spec)
}

// withUsers adds to a change every resource that uses a value it changes
// or deletes and isn't in it yet, with the spec it has now, so the change
// pins it again: a change to a secret restarts every service that uses it,
// in the same plan.
func (e *Engine) withUsers(ctx context.Context, tx *store.Tx, items []item) ([]item, error) {
	refs, err := e.users(ctx, tx, items)
	if err != nil {
		return nil, err
	}
	switched := e.switched(items)
	for _, ref := range refs {
		it, err := e.load(ctx, tx, ref)
		if err != nil {
			return nil, err
		}
		it.spec = proto.Clone(it.current)
		// A value the change replaces with one of another kind, by the
		// same name, is used as the new one.
		if rb, ok := e.rebinder(ref.Kind); ok {
			for _, u := range e.usesOf(&it, it.current) {
				if to, ok := switched[resource.Ref{Kind: u.Kind, Scope: ref.Scope, Name: u.Name}]; ok {
					rb.Rebind(it.spec, u.Name, u.Kind, to)
				}
			}
		}
		if it.encoded, err = resource.Encode(it.spec); err != nil {
			return nil, err
		}
		if it.servers, err = e.place(ctx, tx, it, nil); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

// switched finds the values a change replaces with a value of another
// kind by the same name, as when it makes a variable a secret: for each
// one it deletes, the kind that takes its place.
func (e *Engine) switched(items []item) map[resource.Ref]resource.Kind {
	made := map[resource.Ref]resource.Kind{} // by the name of what is made, and its scope
	for i := range items {
		if it := &items[i]; e.valuer(it.ref.Kind) && it.spec != nil && it.current == nil {
			made[resource.Ref{Scope: it.ref.Scope, Name: it.ref.Name}] = it.ref.Kind
		}
	}
	out := map[resource.Ref]resource.Kind{}
	for i := range items {
		it := &items[i]
		if !e.valuer(it.ref.Kind) || it.spec != nil || it.current == nil {
			continue
		}
		if to, ok := made[resource.Ref{Scope: it.ref.Scope, Name: it.ref.Name}]; ok && to != it.ref.Kind {
			out[it.ref] = to
		}
	}
	return out
}

// switchSides lists both sides of each switch between a kind that keeps
// its values sealed and one that doesn't: what the change deletes, and what
// takes its place under the same name.
func (e *Engine) switchSides(items []item) map[resource.Ref]bool {
	out := map[resource.Ref]bool{}
	for from, to := range e.switched(items) {
		if e.sealedKind(from.Kind) != e.sealedKind(to) {
			out[from] = true
			out[resource.Ref{Kind: to, Scope: from.Scope, Name: from.Name}] = true
		}
	}
	return out
}

// rebinder returns a kind as a Rebinder, if it is one.
func (e *Engine) rebinder(kind resource.Kind) (resource.Rebinder, bool) {
	t, err := e.kinds.Type(kind)
	if err != nil {
		return nil, false
	}
	rb, ok := t.(resource.Rebinder)
	return rb, ok
}

// users lists the resources that use a value the change changes or
// deletes, at their latest revisions, and aren't in it, by kind and name.
// It reads what each uses from the store's index of uses, which opens no
// spec.
func (e *Engine) users(ctx context.Context, tx *store.Tx, items []item) ([]resource.Ref, error) {
	in := map[resource.Ref]bool{}
	changed := map[string]map[resource.Kind][]string{} // by scope, then kind
	for i := range items {
		it := &items[i]
		in[it.ref] = true
		if e.valuer(it.ref.Kind) && it.changes() {
			if changed[it.ref.Scope] == nil {
				changed[it.ref.Scope] = map[resource.Kind][]string{}
			}
			changed[it.ref.Scope][it.ref.Kind] = append(changed[it.ref.Scope][it.ref.Kind], it.ref.Name)
		}
	}
	var out []resource.Ref
	for _, scopeID := range slices.Sorted(maps.Keys(changed)) {
		for _, kind := range slices.Sorted(maps.Keys(changed[scopeID])) {
			for names := range slices.Chunk(changed[scopeID][kind], 500) {
				users, err := tx.UsersOf(ctx, scopeID, string(kind), names...)
				if err != nil {
					return nil, err
				}
				for i := range users {
					u := &users[i]
					ref := resource.Ref{Kind: resource.Kind(u.Kind), Scope: u.Scope, Name: u.Name}
					if !in[ref] {
						in[ref] = true
						out = append(out, ref)
					}
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b resource.Ref) int { return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name)) })
	return out, nil
}

// storeUses is what a spec uses, as the store's index keeps it.
func storeUses(uses []resource.Use) []store.Use {
	out := make([]store.Use, len(uses))
	for i, u := range uses {
		out[i] = store.Use{As: u.As, Kind: string(u.Kind), Name: u.Name, Revision: u.Revision}
	}
	return out
}

// revisions says, for each resource a spec uses, the revision it resolves
// to once the change applies: what the change makes of it, or its latest
// revision. It refuses one that won't be there.
func (e *Engine) revisions(ctx context.Context, tx *store.Tx, items []item, it *item, spec proto.Message) (map[string]int, error) {
	byRef := map[resource.Ref]*item{}
	for i := range items {
		byRef[items[i].ref] = &items[i]
	}
	revs := map[string]int{}
	for _, u := range e.usesOf(it, spec) {
		ref := resource.Ref{Kind: u.Kind, Scope: it.ref.Scope, Name: u.Name}
		if other, ok := byRef[ref]; ok {
			if other.spec == nil {
				return nil, fault.New(ErrInvalid, "%s %s uses %s %s as %s, which this change deletes. Stop using it in %s first, or in the same change",
					it.ref.Kind, it.ref.Name, u.Kind, u.Name, u.As, it.ref.Name)
			}
			revs[u.As] = other.base
			if other.changes() {
				revs[u.As] = other.base + 1
			}
			continue
		}
		r, err := tx.GetResource(ctx, string(u.Kind), it.ref.Scope, u.Name)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			return nil, fault.New(ErrInvalid, "%s %s uses %s %s as %s, which isn't in its environment. Make it first, or in the same change",
				it.ref.Kind, it.ref.Name, u.Kind, u.Name, u.As)
		}
		if err != nil {
			return nil, err
		}
		revs[u.As] = r.Revision
	}
	return revs, nil
}

// pinAll pins what each item uses to the revision it resolves to.
func (e *Engine) pinAll(ctx context.Context, tx *store.Tx, items []item) error {
	for i := range items {
		it := &items[i]
		b, ok := e.binder(it.ref.Kind)
		if !ok || it.spec == nil {
			continue
		}
		revs, err := e.revisions(ctx, tx, items, it, it.spec)
		if err != nil {
			return err
		}
		b.Pin(it.spec, revs)
		if it.encoded, err = resource.Encode(it.spec); err != nil {
			return err
		}
	}
	return nil
}

// stillPinned checks, as a change applies, that what it pinned is still
// the latest, and that it still takes in every resource that uses a value
// it changes: one that started using it since would otherwise keep the
// old value.
func (e *Engine) stillPinned(ctx context.Context, tx *store.Tx, c store.Change, items []item) error {
	for i := range items {
		it := &items[i]
		if it.spec == nil {
			continue
		}
		want, err := e.revisions(ctx, tx, items, it, it.spec)
		if err != nil {
			return fmt.Errorf("%w. %s can't apply as it was planned, so propose the change again", err, c.ID)
		}
		for _, u := range e.usesOf(it, it.spec) {
			if want[u.As] != u.Revision {
				return fault.New(ErrStale, "%s %s changed after %s was planned, so %s would start with the wrong value. Propose the change again",
					u.Kind, u.Name, c.ID, it.ref.Name)
			}
		}
	}
	late, err := e.users(ctx, tx, items)
	if err != nil {
		return err
	}
	if len(late) > 0 {
		return fault.New(ErrStale, "%s %s started using a value %s changes after it was planned. Propose the change again, so it restarts with the new value",
			late[0].Kind, late[0].Name, c.ID)
	}
	return nil
}

// checkNames refuses a value whose name another kind of value in its
// scope already has: an environment's variables and secrets share names,
// as the services that use them see one environment.
func (e *Engine) checkNames(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	if it.spec == nil || it.current != nil || !e.valuer(it.ref.Kind) {
		return nil
	}
	for _, kind := range e.kinds.All() {
		if kind == it.ref.Kind || !e.valuer(kind) {
			continue
		}
		live, err := liveIn(ctx, tx, items, it.ref.Scope, kind)
		if err != nil {
			return err
		}
		if slices.Contains(live, it.ref.Name) {
			return fault.New(ErrInvalid, "there's a %s named %s in that environment already. Give the %s another name, or delete the %s in the same change",
				kind, it.ref.Name, it.ref.Kind, kind)
		}
	}
	return nil
}
