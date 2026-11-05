// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"slices"

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
// stands. What the revision uses, such as a variable or a secret, it uses at
// its latest value, as every change does (uses.go).
//
// It refuses only what would do nothing: what the servers report matching, not
// just the record. A revision that matches on the record, when a rollback
// failed or a container was removed by hand, plans what puts it back, which is
// how someone recovers. It reverts nothing, so the change that got here keeps
// its undo. say gives the plan its summary and notes, from the latest resource
// and the spec it goes back to. Apply the plan to carry it out.
func (e *Engine) RollBackTo(ctx context.Context, resourceID string, number int, say func(store.Resource, proto.Message) (string, []Note), by By) (*Plan, error) {
	var ed Edit
	var reverts, words string
	var notes []Note
	var nothing error
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
		if ed.Spec, err = e.decode(ctx, tx, store.InRevision(resourceID, rev.Kind, number), rev.Spec); err != nil {
			return err
		}
		if ed.Servers, err = inFleet(ctx, tx, ref, resourceID, number, latest.ChangeID); err != nil {
			return err
		}
		matches, err := e.matcher(ctx, tx, latest)
		if err != nil {
			return err
		}
		deployments, err := tx.Deployments(ctx, resourceID, number)
		if err != nil {
			return err
		}
		same, err := matches(ed.Spec, deployments)
		if err != nil {
			return err
		}
		revs, err := tx.RevisionsOf(ctx, latest.ChangeID)
		if err != nil {
			return err
		}
		if !same && len(revs) == 1 {
			reverts = latest.ChangeID
		}
		nothing = fault.New(ErrCantUndo, "%s already runs what revision %d had, so going back to it would change nothing. Choose a revision that differs", latest.Name, number)
		words, notes = say(latest, ed.Spec)
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, fault.New(ErrNotFound, "there's no %s. It may have been removed; list what exists with ListResources", resourceID)
	}
	if err != nil {
		return nil, err
	}
	return e.propose(ctx, Proposal{Summary: words, Actor: by.Actor, Via: by.Via, Edits: []Edit{ed}, Notes: notes, RollsBackTo: number, IfNothing: nothing}, reverts, false)
}

// ResourceRevision is one revision of a resource, with the change that made
// it, what it changed, and where it was placed.
type ResourceRevision struct {
	store.Revision
	// Decoded is its spec, read: nil for a revision that deleted the resource.
	Decoded proto.Message
	// Record is the change that made it, with its plan in brief.
	Record *Record
	// Changes are the fields it changed, as its plan says.
	Changes []resource.Change
	// Deployments are where it was placed, and how that went.
	Deployments []store.Deployment
	// MatchesLatest means going back to it would do nothing.
	MatchesLatest bool
	// RolledBackTo is the revision it put back, for one a rollback made.
	RolledBackTo int
}

// History reads at most limit of a resource's revisions, newest first, from
// before revision before, or from its latest when before is 0. It reads them
// in a few queries, however many there are, as a screen reads this again each
// time a change moves.
func (e *Engine) History(ctx context.Context, resourceID string, before, limit int) ([]ResourceRevision, error) {
	var out []ResourceRevision
	err := e.db.View(ctx, func(tx *store.Tx) error {
		latest, err := tx.GetResourceByID(ctx, resourceID)
		if err != nil {
			return err
		}
		revs, err := tx.PageRevisions(ctx, resourceID, before, limit)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(revs))
		numbers := make([]int, len(revs))
		for i := range revs {
			numbers[i] = revs[i].Revision
			if !slices.Contains(ids, revs[i].ChangeID) {
				ids = append(ids, revs[i].ChangeID)
			}
		}
		read, err := tx.GetChanges(ctx, ids)
		if err != nil {
			return err
		}
		changes := make([]store.Change, len(ids))
		full := make(map[string]*Plan, len(ids))
		at := make(map[string]int, len(ids))
		for i, id := range ids {
			c, ok := read[id]
			if !ok {
				return store.ErrNotFound
			}
			changes[i], at[id] = c, i
			if full[id], err = DecodePlan(c.Plan); err != nil {
				return err
			}
		}
		records, err := e.Records(ctx, tx, changes)
		if err != nil {
			return err
		}
		deployments, err := tx.DeploymentsOf(ctx, resourceID, numbers)
		if err != nil {
			return err
		}
		matches, err := e.matcher(ctx, tx, latest)
		if err != nil {
			return err
		}
		specs := make([]store.StoredSpec, 0, len(revs))
		for i := range revs {
			if revs[i].Spec != nil {
				specs = append(specs, store.StoredSpec{At: store.InRevision(resourceID, revs[i].Kind, revs[i].Revision), Spec: revs[i].Spec})
			}
		}
		decoded, err := e.decodeAll(ctx, tx, specs)
		if err != nil {
			return err
		}
		next := 0
		for i := range revs {
			rev := revs[i]
			r := ResourceRevision{Revision: rev, Record: records[at[rev.ChangeID]], Deployments: deployments[rev.Revision]}
			if rev.Spec != nil {
				r.Decoded = decoded[next]
				next++
			}
			if p := full[rev.ChangeID]; p != nil {
				r.RolledBackTo = p.RollsBackTo
				for j := range p.Resources {
					if p.Resources[j].ID == resourceID {
						r.Changes = p.Resources[j].Changes
					}
				}
			}
			if r.MatchesLatest, err = matches(r.Decoded, r.Deployments); err != nil {
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

// matcher reports whether a revision's spec, with its deployments, matches the
// latest on the record. It doesn't compare what each uses was pinned to, as a
// change pins it again to the latest. It reads the latest once, and each server
// once.
func (e *Engine) matcher(ctx context.Context, tx *store.Tx, latest store.Resource) (func(proto.Message, []store.Deployment) (bool, error), error) {
	if latest.Spec == nil {
		return func(proto.Message, []store.Deployment) (bool, error) { return false, nil }, nil
	}
	kind := resource.Kind(latest.Kind)
	current, err := e.decode(ctx, tx, store.InRevision(latest.ID, latest.Kind, latest.Revision), latest.Spec)
	if err != nil {
		return nil, err
	}
	current = e.unpinned(kind, current)
	gone := map[string]bool{}
	kept := func(list []string) ([]string, error) {
		out := make([]string, 0, len(list))
		for _, s := range list {
			g, seen := gone[s]
			if !seen {
				var err error
				if g, err = serverGone(ctx, tx, s); err != nil {
					return nil, err
				}
				gone[s] = g
			}
			if !g {
				out = append(out, s)
			}
		}
		slices.Sort(out)
		return out, nil
	}
	placed, err := tx.Placement(ctx, latest.ID, latest.Revision)
	if err != nil {
		return nil, err
	}
	running, err := kept(placed)
	if err != nil {
		return nil, err
	}
	return func(spec proto.Message, deployments []store.Deployment) (bool, error) {
		if spec == nil {
			return false, nil
		}
		if !resource.Equal(current, e.unpinned(kind, spec)) {
			return false, nil
		}
		servers := make([]string, len(deployments))
		for i := range deployments {
			servers[i] = deployments[i].Server
		}
		servers, err = kept(servers)
		if err != nil {
			return false, err
		}
		return slices.Equal(servers, running), nil
	}, nil
}

// unpinned is a spec without the revisions of what it uses.
func (e *Engine) unpinned(kind resource.Kind, spec proto.Message) proto.Message {
	b, ok := e.binder(kind)
	if !ok {
		return spec
	}
	out := proto.Clone(spec)
	b.Pin(out, nil)
	return out
}
