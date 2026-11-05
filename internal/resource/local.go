// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
)

// Desired is one resource as a change wants it; a nil Spec deletes it.
type Desired struct {
	Ref  Ref
	Spec proto.Message
	// Pull asks the server for the resource's image again as the step applies,
	// and to replace what runs only if that's newer. It says what a request asks
	// for, not what the spec is, so it isn't kept.
	Pull bool
	// Run says the change asks this resource to run, so one that's there and isn't
	// running is started: a person's own request, as a deploy or a rollback is. A
	// stop by hand isn't drift (ADR 0021), so nothing else starts it, but for a
	// change that replaces it anyway.
	Run bool
	// Restart names a restart the resource is to have, though its spec hasn't
	// changed: the change's ID. Its kind is a Settler.
	Restart string
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

// PathFollower is a driver that can say where the server paths a resource mounts
// lead through symlinks on this server, so its plan can say what they really reach.
type PathFollower interface {
	Leads(ctx context.Context, ref Ref, desired proto.Message) (map[string]string, error)
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

type pullingKey struct{}

// Pulling says a plan is judging a step that asks for its image again, so what
// reads as the spec by its tag may not be what the tag names by the time it runs.
func Pulling(ctx context.Context) bool {
	pulling, _ := ctx.Value(pullingKey{}).(bool)
	return pulling
}

// Runner is a driver whose resources run, and so can be there without running.
type Runner interface {
	// NotRunning says in a word how a resource that's there isn't running, such
	// as "exited". It's empty for one that runs, or isn't there.
	NotRunning(ctx context.Context, ref Ref) (string, error)
}

// StatePath names what differs, in a plan's changes, when a resource is there and
// isn't running.
const StatePath = "state"

// notRunningWords are the words Docker has for a container that isn't running.
var notRunningWords = []string{"created", "exited", "dead", "paused", "removing", "restarting", "stopped"}

// IsStateChange says c is a resource starting that's there and isn't running. It
// names no field of a spec, and its words are a known few, so it can't hold a
// value someone should not see.
func IsStateChange(c Change) bool {
	return c.Path == StatePath && c.To == "running" && slices.Contains(notRunningWords, c.From)
}

// Settler is a driver whose resource can read back as its spec and still have
// work left, as a service whose rollout stopped partway reads as its new
// container with the old one still there. Planning asks it what is left when
// the specs are equal, so what a crash left finds its way to an end. It also
// restarts a resource, which changes no spec.
type Settler interface {
	// Settle judges what is left of a resource that reads back as desired, and
	// reports false when nothing is. restart names a restart it is to have, if
	// it hasn't had that one yet.
	Settle(ctx context.Context, ref Ref, desired proto.Message, restart string) (Decision, bool, error)
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
		normal[i], drivers[i] = Desired{Ref: want.Ref, Spec: spec, Pull: want.Pull, Run: want.Run, Restart: want.Restart}, d
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
		// An in-place update doesn't restart anything, so a restart replaces it.
		settling := false
		if s, ok := d.(Settler); ok && spec != nil && (step.Action == ActionNone || want.Restart != "" && step.Action == ActionUpdate) {
			dec, left, err := s.Settle(ctx, want.Ref, spec, want.Restart)
			if err != nil {
				return nil, fmt.Errorf("can't plan %s: %w", want.Ref, err)
			}
			if left {
				step.Decision, settling = dec, true
			}
		}
		// A spec can't say it should be running, so nothing differs. What's there and
		// isn't running stays so, unless this change asks it to run, which a replace
		// or a restart does, or is replacing it anyway. The plan says which, and nothing
		// is up for that to take down. What a crash left is work to finish, not
		// something stopped, so it has no such word.
		if r, ok := d.(Runner); ok && spec != nil && actual != nil && (want.Run || want.Restart != "" || step.Action == ActionReplace && !settling) {
			state, err := r.NotRunning(ctx, want.Ref)
			if err != nil {
				return nil, fmt.Errorf("can't read %s: %w", want.Ref, err)
			}
			if state != "" {
				changes := append(slices.Clone(step.Changes), Change{Path: StatePath, Op: OpModified, From: state, To: "running"})
				dec := step.Decision
				if dec.Action != ActionReplace {
					if dec, err = decide(ctx, d, want.Ref, actual, spec, changes); err != nil {
						return nil, err
					}
				}
				dec.Downtime = 0
				step.Decision, step.Changes = dec, changes
			}
		}
		if want.Pull && spec != nil {
			// As far as the specs say, nothing differs or only its restart policy
			// does, but what its tag points at may. What that would take is what
			// another image takes, as far as anyone can tell before it pulls.
			if step.Action == ActionNone || step.Action == ActionUpdate {
				dec, err := decide(context.WithValue(ctx, pullingKey{}, true), d, want.Ref, actual, spec, append(slices.Clone(step.Changes), Change{Path: "image"}))
				if err != nil {
					return nil, err
				}
				step.Decision = dec
			}
			step.Pull = step.Action != ActionNone
		}
		if f, ok := d.(PathFollower); ok && spec != nil && step.Action != ActionNone {
			if step.Leads, err = f.Leads(ctx, want.Ref, spec); err != nil {
				return nil, fmt.Errorf("can't plan %s: %w", want.Ref, err)
			}
		}
		// Only what is about to start can fail to: a resource left as it is
		// isn't refused for what is around it.
		if c, ok := d.(PlanChecker); ok && spec != nil && (step.Action == ActionCreate || step.Action == ActionReplace) {
			if err := c.CheckPlan(ctx, want, normal); err != nil {
				return nil, fmt.Errorf("can't plan %s: %w", want.Ref, err)
			}
		}
		if step.Action == ActionCreate || step.Action == ActionReplace {
			step.Restart = want.Restart
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

// decide is what d says of changes, with what the server has if it can read it.
func decide(ctx context.Context, d Driver, ref Ref, actual, desired proto.Message, changes []Change) (Decision, error) {
	var dec Decision
	var err error
	if j, ok := d.(Judge); ok {
		dec, err = j.Judge(ctx, ref, actual, desired, changes)
	} else {
		dec, err = d.Plan(actual, desired, changes)
	}
	if err != nil {
		return Decision{}, fmt.Errorf("can't plan %s: %w", ref, err)
	}
	return dec, nil
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
		return cantApply(step, err)
	}
	if l.Values != nil && len(Uses(d, desired)) > 0 {
		values, err := l.Values.Values(ctx, l.Server, step.Ref, desired)
		if err != nil {
			return fmt.Errorf("can't get the values %s uses: %w", step.Ref, err)
		}
		ctx = WithValues(ctx, values)
	}
	if err := d.Apply(ctx, step, desired); errors.Is(err, ErrAlreadySo) {
		// Nothing changed, and a step that reads done still says it's running.
		if err := d.Verify(ctx, step.Ref, desired); err != nil {
			return fmt.Errorf("%s is as asked, but it isn't healthy: %w", step.Ref, err)
		}
		return nil
	} else if err != nil {
		return cantApply(step, err)
	}
	actual, err := Observe(ctx, d, step.Ref)
	if err != nil {
		return Plainly(fmt.Sprintf("%s changed, but Dockwell can't read it back: %s. Check it on this server, then try again.", step.Ref.Name, clause(err)),
			fmt.Errorf("applied %s, but can't read it back: %w", step.Ref, err))
	}
	if !Equal(actual, desired) {
		what := "something else"
		if changes := Diff(desired, actual); len(changes) > 0 {
			what = "a different " + changes[0].Path
		}
		return Plainly(fmt.Sprintf("%s changed, but this server has %s than asked for. Check what else manages it, then try again.", step.Ref.Name, what),
			fmt.Errorf("applied %s, but the server has %s than asked for", step.Ref, what))
	}
	if desired == nil {
		return nil
	}
	// A change in place leaves what someone stopped stopped, so nothing is running
	// to verify.
	if r, ok := d.(Runner); ok && step.Action == ActionUpdate {
		if state, err := r.NotRunning(ctx, step.Ref); err == nil && state != "" {
			return nil
		}
	}
	if err := d.Verify(ctx, step.Ref, desired); err != nil {
		if step.Restart != "" && step.Rollout != nil && !step.Rollout.Rolling {
			// The old one went before the new one started, and a restart has no
			// revision to go back to.
			return Plainly(fmt.Sprintf("%s restarted, and it isn't healthy, and the old container stopped first, so nothing of it runs now: %s. Fix what this says, then restart it again, or deploy it.", step.Ref.Name, clause(err)),
				fmt.Errorf("restarted %s, and it isn't healthy, and the old container stopped first, so nothing of it runs now: %w. Fix what this says, then restart it again, or deploy it", step.Ref, err))
		}
		return Plainly(fmt.Sprintf("%s isn't healthy: %s. Check it on this server, then try again.", step.Ref.Name, clause(err)),
			fmt.Errorf("applied %s, but it isn't healthy: %w", step.Ref, err))
	}
	return nil
}

// cantApply is a step its driver couldn't carry out. A driver that says why
// in plain words keeps them; otherwise it is the step and the driver's reason,
// without the scope in its ref, which is an ID.
func cantApply(step Step, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return Plainly(fmt.Sprintf("The step's time ran out before Dockwell could %s %s, so it stopped waiting. Check it on this server, then try again.", step.Action, step.Ref.Name),
			fmt.Errorf("can't %s %s: %w", step.Action, step.Ref, err))
	}
	return Plainly(fmt.Sprintf("Dockwell can't %s %s: %s. Check it on this server, then try again.", step.Action, step.Ref.Name, clause(err)),
		fmt.Errorf("can't %s %s: %w", step.Action, step.Ref, err))
}

// clause is an error's message to go on inside a sentence.
func clause(err error) string {
	return strings.TrimRight(strings.TrimSpace(err.Error()), ".")
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
