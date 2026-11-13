// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// elsewhereWords is a change's summary for a reader who holds none of its edits.
const elsewhereWords = "A change in a project you can't see"

// Narrow keeps of a change what the caller in ctx holds, where it reaches a project
// they hold no role in through a link: what they hold in full, the rest as a count,
// by what it does, never its kinds, names, environments or servers; and for a reader
// who holds none of its edits, neither its summary nor who made it (ADR 0084,
// amending ADR 0049). An admin, the owner and Dockwell's own reading hold it all.
func Narrow(ctx context.Context, tx *store.Tx, c *dockwellv1.Change) error {
	caller, ok := place.CallerFrom(ctx)
	if !ok || store.RoleRank(place.Holds(caller.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin) {
		return nil
	}
	items, err := place.NewPlacer(tx).Items(ctx, c.GetId())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	type ref struct{ kind, scope, name string }
	held := map[ref]bool{}
	hidden, anEdit := 0, false
	for i := range items {
		it := &items[i]
		if place.Holds(caller.Grants, it.Place) == "" {
			hidden++
			continue
		}
		held[ref{it.Kind, it.Scope, it.Name}] = true
		anEdit = anEdit || !it.Follows
	}
	if hidden == 0 {
		return nil
	}
	sees := func(r *dockwellv1.ResourceRef) bool {
		return r == nil || held[ref{r.GetKind(), r.GetScope(), r.GetName()}]
	}
	out := &dockwellv1.ChangeElsewhere{EditsElsewhere: !anEdit}
	if plan := c.GetPlan(); plan != nil {
		kept := plan.Resources[:0]
		counts := &dockwellv1.PlanSummary{}
		ids := map[string]bool{}
		for _, r := range plan.GetResources() {
			if !sees(r.GetResource()) {
				if r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
					out.Items++
					if r.GetResource().GetKind() == "service" {
						out.Restarts++
					}
				}
				continue
			}
			kept = append(kept, r)
			ids[r.GetId()] = true
			switch r.GetAction() {
			case dockwellv1.PlanAction_PLAN_ACTION_CREATE:
				counts.Create++
			case dockwellv1.PlanAction_PLAN_ACTION_UPDATE:
				counts.Change++
			case dockwellv1.PlanAction_PLAN_ACTION_REPLACE:
				counts.Replace++
			case dockwellv1.PlanAction_PLAN_ACTION_DELETE:
				counts.Destroy++
			default:
			}
		}
		if len(plan.GetResources()) > 0 {
			plan.Resources, plan.Summary = kept, counts
		}
		servers := plan.Servers[:0]
		for _, sp := range plan.GetServers() {
			steps := sp.Steps[:0]
			for _, step := range sp.GetSteps() {
				if sees(step.GetResource()) {
					steps = append(steps, step)
				}
			}
			if sp.Steps = steps; len(steps) > 0 {
				servers = append(servers, sp)
			}
		}
		plan.Servers = servers
		notes := plan.Notes[:0]
		for _, n := range plan.GetNotes() {
			// A note about no one resource is the edits' author's, about them.
			if n.GetResource() != nil && sees(n.GetResource()) || n.GetResource() == nil && anEdit {
				notes = append(notes, n)
			}
		}
		plan.Notes = notes
		if id := plan.GetIntent().GetResourceId(); id != "" && !ids[id] {
			plan.Intent = nil
		}
		places, err := heldPlaces(ctx, tx, caller.Grants)
		if err != nil {
			return err
		}
		kept2 := plan.Places[:0]
		for _, p := range plan.GetPlaces() {
			if places[p.GetProject()] {
				kept2 = append(kept2, p)
			}
		}
		plan.Places = kept2
	}
	failedHidden, failedHere := false, false
	steps := c.Steps[:0]
	for _, s := range c.GetSteps() {
		failed := s.GetState() == dockwellv1.StepState_STEP_STATE_FAILED
		if !sees(s.GetResource()) {
			failedHidden = failedHidden || failed
			continue
		}
		failedHere = failedHere || failed
		steps = append(steps, s)
	}
	c.Steps = steps
	c.Confirmation = confirmationOf(c.Steps)
	switch {
	case failedHidden && failedHere:
		// What it says of the failure may be of what failed out of sight, whatever
		// failed here too: each step the reader holds says its own.
		c.Error = "It failed here, and on something in a project you can't see. Each step here that failed says why."
	case failedHidden:
		// What it says of the failure is of what failed, which the reader can't see.
		c.Error = "It failed on something in a project you can't see."
	}
	if !anEdit {
		c.Summary, c.ProposedBy, c.AppliedBy, c.Because = elsewhereWords, nil, nil, ""
		if c.RollbackStatus != nil {
			c.RollbackStatus.WaitingFor, c.RollbackStatus.Detail = nil, ""
		}
	}
	if out.Items == 0 {
		// A list's plan is in brief: what's hidden still counts.
		out.Items = int32(hidden)
	}
	c.Elsewhere = out
	return nil
}

// heldPlaces are the names of the projects grants hold a role in, as a plan names
// its places.
func heldPlaces(ctx context.Context, tx *store.Tx, grants []store.Grant) (map[string]bool, error) {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{"": true}
	for _, p := range tree.Projects {
		if place.Holds(grants, place.Place{Project: p.ID}) != "" {
			out[p.Name] = true
		}
	}
	return out, nil
}
