// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
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

// usedKey names a used resource in its scope.
type usedKey struct {
	kind  resource.Kind
	scope string
	name  string
}

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

// binderKinds lists the kinds that use others, sorted.
func (e *Engine) binderKinds() []resource.Kind {
	var out []resource.Kind
	for _, k := range e.kinds.All() {
		if _, ok := e.binder(k); ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
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
	changed := map[usedKey]bool{}
	in := map[resource.Ref]bool{}
	for i := range items {
		it := &items[i]
		in[it.ref] = true
		if e.valuer(it.ref.Kind) && it.changes() {
			changed[usedKey{it.ref.Kind, it.ref.Scope, it.ref.Name}] = true
		}
	}
	if len(changed) == 0 {
		return items, nil
	}
	scopes := map[string]bool{}
	for k := range changed {
		scopes[k.scope] = true
	}
	for _, scopeID := range slices.Sorted(maps.Keys(scopes)) {
		for _, kind := range e.binderKinds() {
			users, err := tx.ListResources(ctx, scopeID, string(kind))
			if err != nil {
				return nil, err
			}
			for _, r := range users {
				ref := resource.Ref{Kind: kind, Scope: r.Scope, Name: r.Name}
				if in[ref] {
					continue
				}
				it, err := e.load(ctx, tx, ref)
				if err != nil {
					return nil, err
				}
				if !slices.ContainsFunc(e.usesOf(&it, it.current), func(u resource.Use) bool {
					return changed[usedKey{u.Kind, ref.Scope, u.Name}]
				}) {
					continue
				}
				it.spec = proto.Clone(it.current)
				if it.encoded, err = resource.Encode(it.spec); err != nil {
					return nil, err
				}
				if it.servers, err = e.place(ctx, tx, it, nil); err != nil {
					return nil, err
				}
				in[ref] = true
				items = append(items, it)
			}
		}
	}
	return items, nil
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
	all, err := e.withUsers(ctx, tx, slices.Clone(items))
	if err != nil {
		return err
	}
	if len(all) > len(items) {
		late := all[len(items)]
		return fault.New(ErrStale, "%s %s started using a value %s changes after it was planned. Propose the change again, so it restarts with the new value",
			late.ref.Kind, late.ref.Name, c.ID)
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
