// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
)

// Desired is one resource as a change wants it; a nil Spec deletes it.
type Desired struct {
	Ref  Ref
	Spec proto.Message
}

// Agent plans and applies on one server, with that server's drivers. In
// process it is a Local; across the network the agent stream carries the
// same calls.
type Agent interface {
	// Plan compares each desired resource with what the server has.
	Plan(ctx context.Context, desired []Desired) ([]Step, error)
	// Apply carries out a step and verifies the result.
	Apply(ctx context.Context, step Step, desired proto.Message) error
	// Observe returns each resource as the server has it, or nil if it
	// doesn't exist.
	Observe(ctx context.Context, refs []Ref) ([]proto.Message, error)
}

// Local is an Agent backed by drivers in this process.
type Local struct {
	Drivers *Registry
}

var _ Agent = Local{}

// Plan observes each resource and judges the step from there to desired.
func (l Local) Plan(ctx context.Context, desired []Desired) ([]Step, error) {
	steps := make([]Step, 0, len(desired))
	for _, want := range desired {
		d, err := l.Drivers.Driver(want.Ref.Kind)
		if err != nil {
			return nil, err
		}
		actual, err := d.Observe(ctx, want.Ref.Name)
		if err != nil {
			return nil, fmt.Errorf("can't read %s: %w", want.Ref, err)
		}
		step, err := PlanStep(d, want.Ref, actual, want.Spec)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// Apply carries out step, then verifies it.
func (l Local) Apply(ctx context.Context, step Step, desired proto.Message) error {
	d, err := l.Drivers.Driver(step.Ref.Kind)
	if err != nil {
		return err
	}
	if err := d.Apply(ctx, step, desired); err != nil {
		return fmt.Errorf("can't %s %s: %w", step.Action, step.Ref, err)
	}
	if err := d.Verify(ctx, step.Ref.Name, desired); err != nil {
		return fmt.Errorf("applied %s, but it isn't right yet: %w", step.Ref, err)
	}
	return nil
}

// Observe reads each resource from its driver.
func (l Local) Observe(ctx context.Context, refs []Ref) ([]proto.Message, error) {
	out := make([]proto.Message, len(refs))
	for i, ref := range refs {
		d, err := l.Drivers.Driver(ref.Kind)
		if err != nil {
			return nil, err
		}
		if out[i], err = d.Observe(ctx, ref.Name); err != nil {
			return nil, fmt.Errorf("can't read %s: %w", ref, err)
		}
	}
	return out, nil
}

// PlanStep judges one step with d: nothing to do when actual already is
// desired, otherwise the driver's decision on what differs.
func PlanStep(d Driver, ref Ref, actual, desired proto.Message) (Step, error) {
	// Equality decides, not the diff, so a field the diff can't show still
	// gets applied.
	if (actual == nil) == (desired == nil) && (actual == nil || proto.Equal(actual, desired)) {
		return Step{Ref: ref}, nil
	}
	changes := Diff(actual, desired)
	dec, err := d.Plan(actual, desired, changes)
	if err != nil {
		return Step{}, fmt.Errorf("can't plan %s: %w", ref, err)
	}
	return Step{Ref: ref, Decision: dec, Changes: changes}, nil
}
