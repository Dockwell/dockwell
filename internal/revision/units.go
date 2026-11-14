// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"slices"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Acting on a server's system services (ADR 0082): an edit of the server's host
// that makes no revision, as a reboot is. Its plan keeps how the unit stood, so
// undoing it plans the action that puts that back.

// OnUnit has fn hear of each step that acted on a server's system service, passed
// or failed, by the server's ID, so whoever watches its services reads them again.
func (e *Engine) OnUnit(fn func(server string)) { e.onUnit = fn }

// unitIn is the action change c asks of ref's system service, as its plan says, or
// nil: what a resume plans again.
func unitIn(c store.Change, ref resource.Ref) *resource.UnitAsked {
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return nil
	}
	for i := range p.Resources {
		if u := p.Resources[i].Unit; p.Resources[i].Ref == ref && u != nil {
			return &resource.UnitAsked{Change: c.ID, Name: u.Name, Action: u.Action}
		}
	}
	return nil
}

// undoUnits plans putting back how each unit change c acted on stood: starting what
// it stopped, disabling what it enabled. A restart and a reload have nothing to put
// back.
func (e *Engine) undoUnits(ctx context.Context, c store.Change, took *Plan, by By) (*Plan, error) {
	if c.State != store.ChangeApplied {
		return nil, fault.New(ErrCantUndo, "%s is %s, and only applied changes can be undone", c.ID, c.State)
	}
	if err := e.withinUndoWindow(c); err != nil {
		return nil, err
	}
	var edits []Edit
	for i := range took.Resources {
		r := &took.Resources[i]
		if r.Unit == nil {
			continue
		}
		back, ok := r.Unit.Undone()
		if !ok {
			return nil, fault.New(ErrCantUndo, "%s can't be undone: a %s of %s has nothing to put back", c.ID, r.Unit.Action, r.Unit.Name)
		}
		edits = append(edits, Edit{Ref: r.Ref, Unit: &resource.UnitAsked{Name: r.Unit.Name, Action: back}})
	}
	return e.propose(ctx, Proposal{Summary: "Undo: " + c.Summary, Actor: by.Actor, Via: by.Via, Edits: edits}, c.ID, true)
}

// Stopping the panel an import came from (ADR 0089) is an edit of the server's
// host that makes no revision, as an action on a unit is. Its plan keeps how each
// of the panel's own containers stood, so undoing it plans starting each again.

// panelIn is what change c asks of the panel on ref's server, as its plan says, or
// nil: what a resume plans again, keeping how each part stood before the change
// began, so a stop that resumes doesn't read what it stopped as how it was.
func panelIn(c store.Change, ref resource.Ref) *resource.PanelAsked {
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return nil
	}
	for i := range p.Resources {
		if q := p.Resources[i].Panel; p.Resources[i].Ref == ref && q != nil {
			return &resource.PanelAsked{Change: c.ID, Kind: q.Kind, Start: q.Start, Parts: q.Parts}
		}
	}
	return nil
}

// undoPanels plans starting again what change c stopped of a panel, each part as
// it stood, and putting back the revisions it made beside, as the routes a
// handover of the panel's proxy made are deleted. A start has nothing of its own
// to put back.
func (e *Engine) undoPanels(ctx context.Context, c store.Change, took *Plan, by By) (*Plan, error) {
	// A failed one may have stopped a panel or its proxy before it failed, and its
	// undo starts that again where its rollback couldn't.
	if c.State != store.ChangeApplied && c.State != store.ChangeFailed {
		return nil, fault.New(ErrCantUndo, "%s is %s, and only applied or failed changes can be undone", c.ID, c.State)
	}
	if err := e.withinUndoWindow(c); err != nil {
		return nil, err
	}
	var edits []Edit
	if slices.ContainsFunc(took.Resources, func(r ResourcePlan) bool { return r.Panel == nil }) {
		back, err := e.reverse(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		edits = back
	}
	for i := range took.Resources {
		r := &took.Resources[i]
		if r.Panel == nil {
			continue
		}
		back, ok := r.Panel.Undone()
		if !ok {
			return nil, fault.New(ErrCantUndo, "%s can't be undone: it started %s again. Stop %s again to put that back", c.ID, r.Panel.Name(), r.Panel.Name())
		}
		edits = append(edits, Edit{Ref: r.Ref, Panel: &back})
	}
	return e.propose(ctx, Proposal{Summary: "Undo: " + c.Summary, Actor: by.Actor, Via: by.Via, Edits: edits}, c.ID, true)
}

// panelsBack are the edits that start again what change c stopped of a panel or
// its proxy, for each such step that began, as its progress says: what a rollback
// of a failed change puts back, as a stop makes no revision to roll back.
func panelsBack(c store.Change, progress *Progress) []Edit {
	plan, err := DecodePlan(c.Plan)
	if err != nil || progress == nil {
		return nil
	}
	var out []Edit
	for i := range plan.Resources {
		r := &plan.Resources[i]
		if r.Panel == nil || r.Panel.Start {
			continue
		}
		began := slices.ContainsFunc(progress.Steps, func(s StepProgress) bool {
			return s.Ref == r.Ref && (s.State == StepRunning || s.State == StepDone || s.State == StepFailed)
		})
		if back, ok := r.Panel.Undone(); began && ok {
			out = append(out, Edit{Ref: r.Ref, Panel: &back})
		}
	}
	return out
}
