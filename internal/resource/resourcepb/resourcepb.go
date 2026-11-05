// SPDX-License-Identifier: AGPL-3.0-only

// Package resourcepb converts between the resource model and the schema's
// messages, for the API and for the agent stream, so both say a step the
// same way.
package resourcepb

import (
	"fmt"
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

// from reads an enum value of the schema as the model's, and reports
// whether this build knows it.
func from[M comparable, P comparable](m map[M]P, v P) (M, bool) {
	for k, p := range m {
		if p == v {
			return k, true
		}
	}
	var zero M
	return zero, false
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
		c.Op, _ = from(ops, fc.GetOp())
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
	if s.UntilHealthy > 0 {
		p.UntilHealthy = durationpb.New(s.UntilHealthy)
	}
	for _, w := range s.Warnings {
		p.Warnings = append(p.Warnings, warnings[w])
	}
	p.KeptPaths, p.Restores, p.HostAccess, p.HealthUnknown = s.Kept, s.Restores, HostAccess(s.HostAccess), s.HealthUnknown
	return p
}

var accessKinds = map[resource.AccessKind]dockwellv1.HostAccessKind{
	resource.AccessPath:          dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH,
	resource.AccessDocker:        dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOCKER_SOCKET,
	resource.AccessNetwork:       dockwellv1.HostAccessKind_HOST_ACCESS_KIND_HOST_NETWORK,
	resource.AccessMount:         dockwellv1.HostAccessKind_HOST_ACCESS_KIND_VOLUME_MOUNT,
	resource.AccessNetworkDriver: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_NETWORK_DRIVER,
}

// HostAccess converts what a resource reaches on its server.
func HostAccess(list []resource.Access) []*dockwellv1.HostAccess {
	var out []*dockwellv1.HostAccess
	for _, a := range list {
		out = append(out, &dockwellv1.HostAccess{
			Kind: accessKinds[a.Kind], Path: a.Path, ReadOnly: a.ReadOnly, Type: a.Type,
			Address: a.Address, Relabel: a.Relabel, Driver: a.Driver, Detail: a.Detail,
		})
	}
	return out
}

// UnknownError is a step that says something this build doesn't know,
// such as an action that a newer build added. Read as anything else, the
// step would be skipped or misstated: an unknown action as none, or a
// warning dropped from a plan that needs one.
type UnknownError struct {
	What  string // "action", "effect" or "warning"
	Value int32  // as the schema numbers it
}

func (e *UnknownError) Error() string {
	return fmt.Sprintf("its %s, number %d, is one this build doesn't know", e.What, e.Value)
}

// FromStep reads a step. A step with an action, effect or warning this
// build doesn't know is an *UnknownError.
func FromStep(p *dockwellv1.PlanStep) (resource.Step, error) {
	s := resource.Step{
		Ref: FromRef(p.GetResource()), Changes: FromChanges(p.GetChanges()),
		Decision: resource.Decision{Destroys: p.GetDestroysData()},
	}
	var ok bool
	if s.Action, ok = from(actions, p.GetAction()); !ok {
		return resource.Step{}, &UnknownError{What: "action", Value: int32(p.GetAction())}
	}
	if s.Effect, ok = from(effects, p.GetEffect()); !ok {
		return resource.Step{}, &UnknownError{What: "effect", Value: int32(p.GetEffect())}
	}
	if d := p.GetDowntime(); d != nil {
		s.Downtime = max(d.AsDuration(), 0)
	}
	if d := p.GetUntilHealthy(); d != nil {
		s.UntilHealthy = max(d.AsDuration(), 0)
	}
	// An agent's word on host access counts for nothing: the control plane
	// decides it from the specs.
	s.Kept, s.Restores, s.HealthUnknown = p.GetKeptPaths(), p.GetRestores(), p.GetHealthUnknown()
	for _, w := range p.GetWarnings() {
		k, ok := from(warnings, w)
		if !ok {
			return resource.Step{}, &UnknownError{What: "warning", Value: int32(w)}
		}
		s.Warnings = append(s.Warnings, k)
	}
	return s, nil
}

// Duration converts a duration, unset when zero.
func Duration(d time.Duration) *durationpb.Duration {
	if d <= 0 {
		return nil
	}
	return durationpb.New(d)
}
