// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// RollBackTo plans putting one resource back as an earlier revision had it,
// on the servers that revision ran on that are still in the fleet. It takes
// any earlier revision, and touches this resource alone, where Undo reverses
// a change whole, and only its latest. A rollback of a revision a change made
// by itself reverts that change, as undoing it would; one of a revision made
// beside others says so only in its summary, as the rest of that change
// stands. Apply the plan to carry it out.
func (e *Engine) RollBackTo(ctx context.Context, resourceID string, number int, summary func(store.Resource) string, by By) (*Plan, error) {
	var ed Edit
	var reverts, words string
	err := e.db.View(ctx, func(tx *store.Tx) error {
		latest, err := tx.GetResourceByID(ctx, resourceID)
		if err != nil {
			return err
		}
		if latest.Spec == nil {
			return fault.New(ErrCantUndo, "%s was deleted, so it can't roll back. Create it again instead", latest.Name)
		}
		if number == latest.Revision {
			return fault.New(ErrCantUndo, "%s is on revision %d already, so there's nothing to go back to. Choose an earlier revision", latest.Name, number)
		}
		rev, err := tx.GetRevision(ctx, resourceID, number)
		if errors.Is(err, store.ErrNotFound) || number > latest.Revision {
			return fault.New(ErrNotFound, "%s has no revision %d. Its latest is %d", latest.Name, number, latest.Revision)
		}
		if err != nil {
			return err
		}
		if rev.Spec == nil {
			return fault.New(ErrCantUndo, "revision %d of %s deleted it, so there's nothing in it to go back to. Choose another revision", number, latest.Name)
		}
		ref := resource.Ref{Kind: resource.Kind(rev.Kind), Scope: rev.Scope, Name: rev.Name}
		ed.Ref = ref
		if ed.Spec, err = e.kinds.Decode(ref.Kind, rev.Spec); err != nil {
			return err
		}
		ed.Spec = proto.Clone(ed.Spec)
		if ed.Servers, err = inFleet(ctx, tx, ref, resourceID, number, latest.ChangeID); err != nil {
			return err
		}
		revs, err := tx.RevisionsOf(ctx, latest.ChangeID)
		if err != nil {
			return err
		}
		if len(revs) == 1 {
			reverts = latest.ChangeID
		}
		words = summary(latest)
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, fault.New(ErrNotFound, "there's no %s. It may have been removed; list what exists with ListResources", resourceID)
	}
	if err != nil {
		return nil, err
	}
	return e.propose(ctx, Proposal{Summary: words, Actor: by.Actor, Via: by.Via, Edits: []Edit{ed}}, reverts)
}

// RevisionRecord is one revision of a resource, with the change that made
// it, what it changed, and where it was placed.
type RevisionRecord struct {
	store.Revision
	// Record is the change that made it, with its plan in brief.
	Record *Record
	// Changes are the fields it changed, as its plan says.
	Changes []resource.Change
	// Deployments are where it was placed, and how that went.
	Deployments []store.Deployment
}

// History reads at most limit of a resource's revisions, newest first, from
// before revision before, or from its latest when before is 0.
func (e *Engine) History(ctx context.Context, resourceID string, before, limit int) ([]RevisionRecord, error) {
	var out []RevisionRecord
	err := e.db.View(ctx, func(tx *store.Tx) error {
		if _, err := tx.GetResourceByID(ctx, resourceID); err != nil {
			return err
		}
		revs, err := tx.PageRevisions(ctx, resourceID, before, limit)
		if err != nil {
			return err
		}
		// A change made together with others may have made several, on one
		// page; and each is read once.
		seen := map[string]int{}
		var changes []store.Change
		full := map[string]*Plan{}
		for i := range revs {
			id := revs[i].ChangeID
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = len(changes)
			c, err := tx.GetChange(ctx, id)
			if err != nil {
				return err
			}
			changes = append(changes, c)
			if full[id], err = DecodePlan(c.Plan); err != nil {
				return err
			}
		}
		records, err := e.Records(ctx, tx, changes)
		if err != nil {
			return err
		}
		for i := range revs {
			rev := revs[i]
			r := RevisionRecord{Revision: rev, Record: records[seen[rev.ChangeID]]}
			if p := full[rev.ChangeID]; p != nil {
				for j := range p.Resources {
					if p.Resources[j].ID == resourceID {
						r.Changes = p.Resources[j].Changes
					}
				}
			}
			if r.Deployments, err = tx.Deployments(ctx, resourceID, rev.Revision); err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, fault.New(ErrNotFound, "there's no %s. It may have been removed; list what exists with ListResources", resourceID)
	}
	return out, err
}
