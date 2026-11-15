// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
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
		before := map[resource.Ref]proto.Message{}
		stored, err := tx.ListKind(ctx, string(kind))
		if err != nil {
			return err
		}
		for _, r := range stored {
			if r.Spec == nil {
				continue
			}
			spec, err := e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			before[resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}] = spec
		}
		// As the change leaves them: what it edits as edited, and what it deletes gone.
		after := make(map[resource.Ref]proto.Message, len(before))
		for ref, spec := range before {
			after[ref] = spec
		}
		inChange := map[resource.Ref]bool{}
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
