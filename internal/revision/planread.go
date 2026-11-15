// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"fmt"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// planRead is a change's stored plan, decoded once as the engine reads the change,
// with where each resource's plans are in it. What each item asks beyond its spec,
// a restart, a restore or a reboot, is read from it, so a change of n items decodes
// its plan once, not several times for each item, which made applying grow with
// the square of a change's size (#686).
type planRead struct {
	// id is the change's.
	id string
	// plan is nil where the change has none, or one that can't be read.
	plan *Plan
	// err says why the plan can't be read.
	err error
	// at is where each resource's plans are in plan.Resources, in order.
	at map[resource.Ref][]int
}

// readPlan decodes change c's plan.
func readPlan(c store.Change) *planRead {
	r := &planRead{id: c.ID}
	if len(c.Plan) == 0 {
		r.err = fmt.Errorf("can't read a stored plan: %s has none", c.ID)
		return r
	}
	if r.plan, r.err = DecodePlan(c.Plan); r.err != nil {
		r.plan = nil
		return r
	}
	r.at = make(map[resource.Ref][]int, len(r.plan.Resources))
	for i := range r.plan.Resources {
		ref := r.plan.Resources[i].Ref
		r.at[ref] = append(r.at[ref], i)
	}
	return r
}

// first is the first of the plan's resources for ref that match says is the one,
// or nil. A nil match takes the first for ref.
func (r *planRead) first(ref resource.Ref, match func(*ResourcePlan) bool) *ResourcePlan {
	if r == nil || r.plan == nil {
		return nil
	}
	for _, i := range r.at[ref] {
		if p := &r.plan.Resources[i]; match == nil || match(p) {
			return p
		}
	}
	return nil
}

// whole is the plan, or why it can't be read: a change with none has none to read.
func (r *planRead) whole() (*Plan, error) {
	switch {
	case r == nil:
		return nil, errors.New("can't read a stored plan: none was read")
	case r.plan == nil:
		return nil, r.err
	}
	return r.plan, nil
}

// any reports whether match says any of the plan's resources is one.
func (r *planRead) any(match func(*ResourcePlan) bool) bool {
	if r == nil || r.plan == nil {
		return false
	}
	for i := range r.plan.Resources {
		if match(&r.plan.Resources[i]) {
			return true
		}
	}
	return false
}

// noUndo is why the change can't be undone, as the first of its resources that
// acts beside the servers says; "" where none does (ADR 0098).
func noUndo(r *planRead) string {
	if r == nil || r.plan == nil {
		return ""
	}
	for i := range r.plan.Resources {
		if why := r.plan.Resources[i].NoUndo; why != "" {
			return why
		}
	}
	return ""
}
