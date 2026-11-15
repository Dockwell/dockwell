// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// checkAcross has each kind that checks an edit against every resource of its kind
// across the install (resource.AcrossChecker) check its resources against what's
// stored as the change leaves it: what it edits as edited, and what it deletes gone,
// as a redirect's loop depends on which route answers where it lands (ADR 0070).
// What the change makes or edits is checked first. Then, as editing or deleting one
// can change where another lands, each the change leaves as it is is checked too,
// and refused only where the change is what breaks it, so one that was broken before
// holds up nothing else. Undo, rollback and adopting drift aren't asked, as for Keep.
func (e *Engine) checkAcross(ctx context.Context, tx *store.Tx, items []item) error {
	byKind := map[resource.Kind][]*item{}
	for i := range items {
		it := &items[i]
		if it.reverses || it.observed {
			continue
		}
		if t, err := e.kinds.Type(it.ref.Kind); err == nil {
			if _, ok := t.(resource.AcrossChecker); ok {
				byKind[it.ref.Kind] = append(byKind[it.ref.Kind], it)
			}
		}
	}
	// The change's own scopes are the caller's, even one it makes.
	mine := map[string]bool{}
	for i := range items {
		mine[items[i].ref.Scope] = true
	}
	seen := func(ref resource.Ref) bool { return mine[ref.Scope] || inReach(ctx, tx, ref.Scope) }
	for kind, touched := range byKind {
		t, err := e.kinds.Type(kind)
		if err != nil {
			return err
		}
		c := t.(resource.AcrossChecker)
		before, after, inChange, err := e.across(ctx, tx, kind, items)
		if err != nil {
			return err
		}
		for _, it := range touched {
			if it.spec == nil {
				continue
			}
			if err := c.CheckAcross(it.ref, it.spec, after, seen); err != nil {
				return fault.New(ErrInvalid, "%s: %w", it.ref, err)
			}
		}
		// Those it leaves as they are, in a stable order, so the same change is
		// refused in the same words each time.
		others := make([]resource.Ref, 0, len(after))
		for ref := range after {
			if !inChange[ref] {
				others = append(others, ref)
			}
		}
		slices.SortFunc(others, func(a, b resource.Ref) int {
			return cmp.Or(strings.Compare(a.Scope, b.Scope), strings.Compare(a.Name, b.Name))
		})
		for _, ref := range others {
			if c.CheckAcross(ref, after[ref], after, seen) == nil || c.CheckAcross(ref, before[ref], before, seen) != nil {
				continue
			}
			if !seen(ref) {
				return fault.New(ErrInvalid, "a redirect in a project you can't see lands where this change leaves it sending visitors round in a circle. Keep what it lands on, or ask an admin")
			}
			return fault.New(ErrInvalid, "%s %s, which this change leaves as it is, would then send visitors round in a circle, as where it lands changes. Change it in this change too, or keep what it lands on", ref.Kind, ref.Name)
		}
	}
	return nil
}

// across is every resource of kind as stored, and as the change leaves them: what it
// edits as edited, and what it deletes gone, with what the change names.
func (e *Engine) across(ctx context.Context, tx *store.Tx, kind resource.Kind, items []item) (before, after map[resource.Ref]proto.Message, inChange map[resource.Ref]bool, err error) {
	before = map[resource.Ref]proto.Message{}
	stored, err := tx.ListKind(ctx, string(kind))
	if err != nil {
		return nil, nil, nil, err
	}
	for _, r := range stored {
		if r.Spec == nil {
			continue
		}
		spec, err := e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, nil, nil, err
		}
		before[resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}] = spec
	}
	after = make(map[resource.Ref]proto.Message, len(before))
	for ref, spec := range before {
		after[ref] = spec
	}
	inChange = map[resource.Ref]bool{}
	for i := range items {
		if items[i].ref.Kind != kind {
			continue
		}
		inChange[items[i].ref] = true
		if items[i].spec == nil {
			delete(after, items[i].ref)
		} else {
			after[items[i].ref] = items[i].spec
		}
	}
	return before, after, inChange, nil
}

// noteAcross says what a caller's change leaves behind it of each kind that notes
// across the install (resource.AcrossNoter), as a redirect that lands on nothing once
// the route it landed on goes or moves (ADR 0070). A note about what the caller can't
// see names none of it.
func (e *Engine) noteAcross(ctx context.Context, tx *store.Tx, items []item) ([]Note, error) {
	kinds := map[resource.Kind]resource.AcrossNoter{}
	mine := map[string]bool{}
	for i := range items {
		it := &items[i]
		mine[it.ref.Scope] = true
		if it.reverses || it.observed {
			continue
		}
		if t, err := e.kinds.Type(it.ref.Kind); err == nil {
			if n, ok := t.(resource.AcrossNoter); ok {
				kinds[it.ref.Kind] = n
			}
		}
	}
	var out []Note
	for _, kind := range slices.Sorted(maps.Keys(kinds)) {
		before, after, inChange, err := e.across(ctx, tx, kind, items)
		if err != nil {
			return nil, err
		}
		refs := slices.SortedFunc(maps.Keys(after), func(a, b resource.Ref) int {
			return cmp.Or(strings.Compare(a.Scope, b.Scope), strings.Compare(a.Name, b.Name))
		})
		for _, ref := range refs {
			if inChange[ref] {
				continue
			}
			reason, words := kinds[kind].NoteAcross(ref, before, after)
			if words == "" {
				continue
			}
			if !mine[ref.Scope] && !inReach(ctx, tx, ref.Scope) {
				out = append(out, Note{Reason: reason, Message: "A redirect in a project you can't see lands where this change leaves nothing to answer it."})
				continue
			}
			out = append(out, Note{Reason: reason, Message: words, Ref: ref})
		}
	}
	return out, nil
}
