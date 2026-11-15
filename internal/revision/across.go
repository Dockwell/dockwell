// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// checkAcross has each kind that checks an edit against every resource of its kind
// across the install (resource.AcrossChecker) check what a caller's change makes or
// edits, against what's stored as the change leaves it: what it edits as edited, and
// what it deletes gone, as a redirect's loop depends on which route answers where it
// lands (ADR 0070). Undo, rollback and adopting drift aren't asked, as for Keep.
func (e *Engine) checkAcross(ctx context.Context, tx *store.Tx, items []item) error {
	byKind := map[resource.Kind][]*item{}
	for i := range items {
		it := &items[i]
		if it.spec == nil || it.reverses || it.observed {
			continue
		}
		if t, err := e.kinds.Type(it.ref.Kind); err == nil {
			if _, ok := t.(resource.AcrossChecker); ok {
				byKind[it.ref.Kind] = append(byKind[it.ref.Kind], it)
			}
		}
	}
	for kind, checked := range byKind {
		t, err := e.kinds.Type(kind)
		if err != nil {
			return err
		}
		c := t.(resource.AcrossChecker)
		all := map[resource.Ref]proto.Message{}
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
			all[resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}] = spec
		}
		// As the change leaves them: what it edits as edited, and what it deletes gone.
		for i := range items {
			if items[i].ref.Kind != kind {
				continue
			}
			if items[i].spec == nil {
				delete(all, items[i].ref)
			} else {
				all[items[i].ref] = items[i].spec
			}
		}
		for _, it := range checked {
			if err := c.CheckAcross(it.ref, it.spec, all); err != nil {
				return fault.New(ErrInvalid, "%s: %w", it.ref, err)
			}
		}
	}
	return nil
}
