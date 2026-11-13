// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
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
		edits = append(edits, Edit{Ref: r.Ref, Spec: &resourcev1.ServerHost{}, Unit: &resource.UnitAsked{Name: r.Unit.Name, Action: back}})
	}
	return e.propose(ctx, Proposal{Summary: "Undo: " + c.Summary, Actor: by.Actor, Via: by.Via, Edits: edits}, c.ID, true)
}
