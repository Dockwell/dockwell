// SPDX-License-Identifier: AGPL-3.0-only

// Package resourcepb converts between the resource model and the schema's
// messages, for the API and for the agent stream, so both say a step the
// same way.
package resourcepb

import (
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Ref converts a resource's name.
func Ref(r resource.Ref) *dockwellv1.ResourceRef {
	return &dockwellv1.ResourceRef{Kind: string(r.Kind), Scope: r.Scope, Name: r.Name}
}

// FromRef reads a resource's name.
func FromRef(r *dockwellv1.ResourceRef) resource.Ref {
	return resource.Ref{Kind: resource.Kind(r.GetKind()), Scope: r.GetScope(), Name: r.GetName()}
}

var actions = map[resource.Action]dockwellv1.PlanAction{
	resource.ActionNone:    dockwellv1.PlanAction_PLAN_ACTION_NONE,
	resource.ActionCreate:  dockwellv1.PlanAction_PLAN_ACTION_CREATE,
	resource.ActionUpdate:  dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
	resource.ActionReplace: dockwellv1.PlanAction_PLAN_ACTION_REPLACE,
	resource.ActionDelete:  dockwellv1.PlanAction_PLAN_ACTION_DELETE,
}

// Action converts what a step does.
func Action(a resource.Action) dockwellv1.PlanAction { return actions[a] }

// FromAction reads what a step does; an unknown action is none.
func FromAction(a dockwellv1.PlanAction) resource.Action {
	for k, v := range actions {
		if v == a {
			return k
		}
	}
	return resource.ActionNone
}

var effects = map[resource.Effect]dockwellv1.StepEffect{
	resource.EffectNone:     dockwellv1.StepEffect_STEP_EFFECT_NONE,
	resource.EffectRestart:  dockwellv1.StepEffect_STEP_EFFECT_RESTART,
	resource.EffectRecreate: dockwellv1.StepEffect_STEP_EFFECT_RECREATE,
}

var warnings = map[resource.Warning]dockwellv1.PlanWarning{
	resource.WarningMayLockOut: dockwellv1.PlanWarning_PLAN_WARNING_MAY_LOCK_OUT,
}

var ops = map[resource.Op]dockwellv1.FieldOp{
	resource.OpAdded:    dockwellv1.FieldOp_FIELD_OP_ADDED,
	resource.OpRemoved:  dockwellv1.FieldOp_FIELD_OP_REMOVED,
	resource.OpModified: dockwellv1.FieldOp_FIELD_OP_MODIFIED,
}

// Changes converts field changes. A sensitive field's values are never
// carried, whatever the change holds.
func Changes(cs []resource.Change) []*dockwellv1.FieldChange {
	out := make([]*dockwellv1.FieldChange, len(cs))
	for i, c := range cs {
		fc := &dockwellv1.FieldChange{Path: c.Path, Op: ops[c.Op], From: c.From, To: c.To, Sensitive: c.Sensitive}
		if c.Sensitive {
			fc.From, fc.To = "", ""
		}
		out[i] = fc
	}
	return out
}

// FromChanges reads field changes.
func FromChanges(fcs []*dockwellv1.FieldChange) []resource.Change {
	out := make([]resource.Change, len(fcs))
	for i, fc := range fcs {
		c := resource.Change{Path: fc.GetPath(), From: fc.GetFrom(), To: fc.GetTo(), Sensitive: fc.GetSensitive()}
		for k, v := range ops {
			if v == fc.GetOp() {
				c.Op = k
			}
		}
		if c.Sensitive {
			c.From, c.To = "", ""
		}
		out[i] = c
	}
	return out
}

// Step converts a step. resourceID names the resource, when known.
func Step(s resource.Step, resourceID string) *dockwellv1.PlanStep {
	p := &dockwellv1.PlanStep{
		ResourceId: resourceID, Resource: Ref(s.Ref), Action: Action(s.Action), Effect: effects[s.Effect],
		DestroysData: s.Destroys, Changes: Changes(s.Changes),
	}
	if s.Downtime > 0 {
		p.Downtime = durationpb.New(s.Downtime)
	}
	for _, w := range s.Warnings {
		p.Warnings = append(p.Warnings, warnings[w])
	}
	return p
}

// FromStep reads a step.
func FromStep(p *dockwellv1.PlanStep) resource.Step {
	s := resource.Step{
		Ref: FromRef(p.GetResource()), Changes: FromChanges(p.GetChanges()),
		Decision: resource.Decision{Action: FromAction(p.GetAction()), Destroys: p.GetDestroysData()},
	}
	if d := p.GetDowntime(); d != nil {
		s.Downtime = max(d.AsDuration(), 0)
	}
	for k, v := range effects {
		if v == p.GetEffect() {
			s.Effect = k
		}
	}
	for _, w := range p.GetWarnings() {
		for k, v := range warnings {
			if v == w {
				s.Warnings = append(s.Warnings, k)
			}
		}
	}
	return s
}

// Duration converts a duration, unset when zero.
func Duration(d time.Duration) *durationpb.Duration {
	if d <= 0 {
		return nil
	}
	return durationpb.New(d)
}
