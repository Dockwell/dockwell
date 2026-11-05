// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"
	"errors"
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
	// Observe returns each resource as the server has it, normalized like
	// a desired spec, or nil if it doesn't exist.
	Observe(ctx context.Context, refs []Ref) ([]proto.Message, error)
}

// What an Agent's call returns when it didn't run to an end on the server.
// The engine tells them apart: a call that didn't run can wait for the
// agent, and one that may have run is checked again.
var (
	// ErrLost means the agent's stream ended while a call waited for its
	// result. The call may or may not have run.
	ErrLost = errors.New("the server's agent disconnected")
	// ErrNotConnected means the server's agent isn't connected, so the
	// call never reached it and didn't run.
	ErrNotConnected = errors.New("the server's agent isn't connected")
)

// ServerError is a call that a server's agent ran and failed, with the
// agent's own reason. It names the server, so what wraps it needn't.
type ServerError struct {
	Server string // the server's name
	Err    error
}

func (e *ServerError) Error() string { return e.Server + ": " + e.Err.Error() }

func (e *ServerError) Unwrap() error { return e.Err }

// Values finds what a desired spec's uses resolve to on one server: each
// value by the name it is seen as. The control plane's keeper implements
// it, and gives values only for a resource placed on that server. See
// docs/adr/0022-secrets-encrypted-at-rest.md.
type Values interface {
	Values(ctx context.Context, server string, ref Ref, desired proto.Message) (map[string]string, error)
}

type valuesKey struct{}

// WithValues gives a driver's Apply the values of what its desired spec
// uses, for that one call. They live in memory only.
func WithValues(ctx context.Context, values map[string]string) context.Context {
	return context.WithValue(ctx, valuesKey{}, values)
}

// ValuesFrom returns the values WithValues gave, or nil.
func ValuesFrom(ctx context.Context) map[string]string {
	v, _ := ctx.Value(valuesKey{}).(map[string]string)
	return v
}

// Uses reports what a desired spec of d's kind uses, if anything.
func Uses(t Type, spec proto.Message) []Use {
	b, ok := t.(Binder)
	if !ok || spec == nil {
		return nil
	}
	return b.Uses(spec)
}

// Local is an Agent backed by drivers in this process.
type Local struct {
	Drivers *Registry
	// Server and Values give an agent in this process the values of what a
	// desired spec uses, as the agent stream gives a remote agent. Without
	// them, the values come with the context, as the agent's stream puts
	// them there.
	Server string
	Values Values
}

var _ Agent = Local{}

// Checker is a driver that checks a desired spec against its server,
// beyond what Validate can know without it, such as where a server path
// leads through symlinks. The agent checks each desired spec as it plans
// it; the driver checks again as it applies.
type Checker interface {
	Check(ctx context.Context, ref Ref, desired proto.Message) error
}

// PlanChecker is a driver that checks a resource about to start against what
// else is on the server, and the rest of what the plan wants there, as a port
// only one thing can hold. all holds every resource the plan wants here,
// normalized, want among them.
type PlanChecker interface {
	CheckPlan(ctx context.Context, want Desired, all []Desired) error
}

// Judge is a driver whose judgement of a step needs its server, beyond the
// specs, such as a container whose image keeps data that deleting it takes
// out of use. The agent asks it in place of Plan; the control plane, which
// plans an offline server from its last known state, asks Plan.
type Judge interface {
	Judge(ctx context.Context, ref Ref, actual, desired proto.Message, changes []Change) (Decision, error)
}

// desiredKey is where a plan's context holds all it touches on the server.
type desiredKey struct{}

// DesiredIn is every resource the plan under way touches on this server, for
// a driver whose judgement of one step depends on the others, as a server's
// last route stops its ingress however many a plan removes. It is nil outside
// a plan.
func DesiredIn(ctx context.Context) []Desired {
	d, _ := ctx.Value(desiredKey{}).([]Desired)
	return d
}

// WithDesired says what a plan under way touches on this server, as Plan does.
func WithDesired(ctx context.Context, desired []Desired) context.Context {
	return context.WithValue(ctx, desiredKey{}, desired)
}

// Plan observes each resource and judges the step from there to desired.
// Each desired spec is checked and normalized again first, as the control
// plane did: a spec that skipped that, or a default this agent fills in
// that the control plane didn't, can't make a plan that applying then
// fails to match.
func (l Local) Plan(ctx context.Context, desired []Desired) ([]Step, error) {
	ctx = WithDesired(ctx, desired)
	steps := make([]Step, 0, len(desired))
	normal := make([]Desired, len(desired))
	drivers := make([]Driver, len(desired))
	for i, want := range desired {
		d, err := l.Drivers.Driver(want.Ref.Kind)
		if err != nil {
			return nil, err
		}
		spec, err := normalize(d, want.Spec)
		if err != nil {
			return nil, fmt.Errorf("can't plan %s: %w", want.Ref, err)
		}
		normal[i], drivers[i] = Desired{Ref: want.Ref, Spec: spec}, d
	}
	for i, want := range normal {
		d, spec := drivers[i], want.Spec
		if c, ok := d.(Checker); ok && spec != nil {
			if err := c.Check(ctx, want.Ref, spec); err != nil {
				return nil, fmt.Errorf("can't plan %s: %w", want.Ref, err)
			}
		}
		actual, err := Observe(ctx, d, want.Ref)
		if err != nil {
			return nil, fmt.Errorf("can't read %s: %w", want.Ref, err)
		}
		var step Step
		if j, ok := d.(Judge); ok {
			step, err = judgeStep(ctx, j, want.Ref, actual, spec)
		} else {
			step, err = PlanStep(d, want.Ref, actual, spec)
		}
		if err != nil {
			return nil, err
		}
		// Only what is about to start can fail to: a resource left as it is
		// isn't refused for what is around it.
		if c, ok := d.(PlanChecker); ok && spec != nil && (step.Action == ActionCreate || step.Action == ActionReplace) {
			if err := c.CheckPlan(ctx, want, normal); err != nil {
				return nil, fmt.Errorf("can't plan %s: %w", want.Ref, err)
			}
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// normalize checks a desired spec with its driver, and returns the
// normalized copy; nil stays nil. The spec itself is left as it is, as the
// caller may hold it.
func normalize(d Driver, spec proto.Message) (proto.Message, error) {
	if spec == nil {
		return nil, nil
	}
	spec = proto.Clone(spec)
	if err := d.Validate(spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// judgeStep is PlanStep with a Judge, which knows the server.
func judgeStep(ctx context.Context, j Judge, ref Ref, actual, desired proto.Message) (Step, error) {
	if Equal(actual, desired) {
		return Step{Ref: ref}, nil
	}
	changes := Diff(actual, desired)
	dec, err := j.Judge(ctx, ref, actual, desired, changes)
	if err != nil {
		return Step{}, fmt.Errorf("can't plan %s: %w", ref, err)
	}
	return Step{Ref: ref, Decision: dec, Changes: changes}, nil
}

// Apply carries out step, then checks the server has what was asked for
// and that the driver finds it healthy. The desired spec is normalized
// again first, as Plan does.
func (l Local) Apply(ctx context.Context, step Step, desired proto.Message) error {
	d, err := l.Drivers.Driver(step.Ref.Kind)
	if err != nil {
		return err
	}
	if desired, err = normalize(d, desired); err != nil {
		return fmt.Errorf("can't %s %s: %w", step.Action, step.Ref, err)
	}
	if l.Values != nil && len(Uses(d, desired)) > 0 {
		values, err := l.Values.Values(ctx, l.Server, step.Ref, desired)
		if err != nil {
			return fmt.Errorf("can't get the values %s uses: %w", step.Ref, err)
		}
		ctx = WithValues(ctx, values)
	}
	if err := d.Apply(ctx, step, desired); err != nil {
		return fmt.Errorf("can't %s %s: %w", step.Action, step.Ref, err)
	}
	actual, err := Observe(ctx, d, step.Ref)
	if err != nil {
		return fmt.Errorf("applied %s, but can't read it back: %w", step.Ref, err)
	}
	if !Equal(actual, desired) {
		what := "something else"
		if changes := Diff(desired, actual); len(changes) > 0 {
			what = "a different " + changes[0].Path
		}
		return fmt.Errorf("applied %s, but the server has %s than asked for", step.Ref, what)
	}
	if desired == nil {
		return nil
	}
	if err := d.Verify(ctx, step.Ref, desired); err != nil {
		return fmt.Errorf("applied %s, but it isn't healthy: %w", step.Ref, err)
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
		if out[i], err = Observe(ctx, d, ref); err != nil {
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
	if Equal(actual, desired) {
		return Step{Ref: ref}, nil
	}
	changes := Diff(actual, desired)
	dec, err := d.Plan(actual, desired, changes)
	if err != nil {
		return Step{}, fmt.Errorf("can't plan %s: %w", ref, err)
	}
	return Step{Ref: ref, Decision: dec, Changes: changes}, nil
}

// Observe reads a resource through its driver and normalizes it like a
// desired spec, so what a server reports in its own order or with its own
// defaults compares equal to the spec that made it. What Validate refuses,
// as a newer rule may refuse what is stored, is put in order as far as its
// kind's Normalizer can, so only what truly differs shows as a difference.
func Observe(ctx context.Context, d Driver, ref Ref) (proto.Message, error) {
	actual, err := d.Observe(ctx, ref)
	if err != nil || actual == nil {
		return nil, err
	}
	normal := proto.Clone(actual)
	if d.Validate(normal) == nil {
		return normal, nil
	}
	if n, ok := d.(Normalizer); ok {
		n.Normalize(actual)
	}
	return actual, nil
}
