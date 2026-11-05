// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// drainLimit bounds how long a stream that is moving to a renewed
// certificate waits for its calls to finish.
const drainLimit = 15 * time.Minute

// calls runs the control plane's calls on one stream, each in its own
// goroutine, and sends each result back. They end with the stream: a call
// still running when it drops is cancelled, and the control plane plans
// again from what the server has when the agent reconnects.
//
// The stream ends in order: close refuses the calls that arrive from then
// on, the stream's receiver stops, and stop cancels what still runs and
// waits for it. So no call starts while stop waits, and moving to a renewed
// certificate never cuts a step off that it took on.
type calls struct {
	ctx   context.Context // the calls', apart from the stream's
	end   context.CancelFunc
	cfg   Config
	local resource.Local
	// delivery opens the values of variables and secrets sent for a step,
	// sealed for server.
	delivery *delivery.Key
	server   string
	send     func(*agentv1.ConnectRequest) error
	logger   *slog.Logger

	mu      sync.Mutex
	running map[uint64]context.CancelFunc
	closing bool // refuse calls from now on
	wg      sync.WaitGroup
	idle    chan struct{} // signalled when the last running call ends
	// drained is closed once the control plane answers Draining: it sends
	// no more calls on this stream.
	drained     chan struct{}
	drainedOnce sync.Once
}

func newCalls(ctx context.Context, cfg Config, send func(*agentv1.ConnectRequest) error, key *delivery.Key) *calls {
	ctx, end := context.WithCancel(ctx)
	var server string
	if cfg.Identity != nil {
		server = cfg.Identity.ServerID
	}
	return &calls{
		ctx: ctx, end: end, cfg: cfg, local: resource.Local{Drivers: cfg.Drivers}, delivery: key, server: server, send: send, logger: cfg.Logger,
		running: map[uint64]context.CancelFunc{}, idle: make(chan struct{}, 1), drained: make(chan struct{}),
	}
}

// acknowledged notes the control plane's answer to Draining.
func (c *calls) acknowledged() { c.drainedOnce.Do(func() { close(c.drained) }) }

// start runs a call, or refuses it once the stream is closing: nothing
// runs, and the control plane sends it again on the next stream.
func (c *calls) start(call *agentv1.Call) {
	timeout := call.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = time.Minute
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		c.reply(call.GetId(), &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{
			Message:  "this agent is moving to a new stream, so it didn't run the call; send it again once the agent reconnects",
			Draining: true,
		}}})
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	c.running[call.GetId()] = cancel
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer c.done(call.GetId())
		result := c.run(ctx, call)
		// A step cut short because this stream is ending can only say it was
		// cancelled. Sending nothing lets the control plane say what's true:
		// the agent disconnected, and the step may or may not have run.
		if c.ctx.Err() == nil || result.GetError() == nil {
			c.reply(call.GetId(), result)
		}
	}()
}

// reply sends a call's result.
func (c *calls) reply(id uint64, result *agentv1.CallResult) {
	result.Id = id
	if err := c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Result{Result: result}}); err != nil && c.ctx.Err() == nil && !c.isClosing() {
		c.logger.Warn("can't send a call's result; the control plane checks again when the stream is back",
			slog.Uint64("call", id), slog.String("error", errText(err)))
	}
}

// close refuses the calls that arrive from now on. Those already running
// carry on.
func (c *calls) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closing = true
}

func (c *calls) isClosing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closing
}

func (c *calls) done(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cancel, ok := c.running[id]; ok {
		cancel()
		delete(c.running, id)
	}
	if len(c.running) == 0 {
		select {
		case c.idle <- struct{}{}:
		default:
		}
	}
}

// cancel stops a call the control plane no longer waits for.
func (c *calls) cancel(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cancel, ok := c.running[id]; ok {
		cancel()
	}
}

func (c *calls) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.running) > 0
}

// stop cancels every call still running and waits for them. It closes
// first, so no call starts while it waits; stop the stream's receiver
// between the two.
func (c *calls) stop() {
	c.close()
	c.end()
	c.wg.Wait()
}

// drain readies a stream that moves to a renewed certificate to end, while
// it keeps reporting stats. It refuses calls from now on, and tells the
// control plane, which sends no more and says so; the calls that arrive
// before its answer are refused, and the control plane sends them again on
// the next stream. drain then waits for the calls under way to finish, so
// moving doesn't cut a step off halfway. It gives up after drainLimit.
func (c *calls) drain(ctx context.Context, recvErr <-chan error, tick <-chan time.Time, sendStats func() error) error {
	c.close()
	if err := c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Draining{Draining: &agentv1.Draining{}}}); err != nil {
		return err
	}
	limit := time.NewTimer(drainLimit)
	defer limit.Stop()
	drained, answered := c.drained, false
	for c.busy() || !answered {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-recvErr:
			return err
		case <-tick:
			if err := sendStats(); err != nil {
				return err
			}
		case <-c.idle:
		case <-drained:
			drained, answered = nil, true
		case <-limit.C:
			return nil
		}
	}
	return nil
}

// run carries out one call with the agent's drivers.
func (c *calls) run(ctx context.Context, call *agentv1.Call) *agentv1.CallResult {
	fail := func(err error) *agentv1.CallResult {
		return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: err.Error()}}}
	}
	switch {
	case call.GetPlan() != nil:
		desired := make([]resource.Desired, 0, len(call.GetPlan().GetDesired()))
		for _, d := range call.GetPlan().GetDesired() {
			ref, spec, err := c.decode(d)
			if err != nil {
				return fail(err)
			}
			desired = append(desired, resource.Desired{Ref: ref, Spec: spec, Pull: d.GetPull(), Run: d.GetRun()})
		}
		steps, err := c.local.Plan(ctx, desired)
		if err != nil {
			return fail(err)
		}
		out := &agentv1.PlanResult{}
		for i := range steps {
			out.Steps = append(out.Steps, resourcepb.Step(steps[i], ""))
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Plan{Plan: out}}
	case call.GetApply() != nil:
		step, err := resourcepb.FromStep(call.GetApply().GetStep())
		if err != nil {
			return fail(fmt.Errorf("this agent can't read the step it was sent: %w. "+
				"The control plane is newer than the agent, so update the agent to the control plane's version", err))
		}
		ref, spec, err := c.decode(call.GetApply().GetDesired())
		if err != nil {
			return fail(err)
		}
		step.Ref, step.Pulled = ref, call.GetApply().GetPulled()
		progress := c.progress(call.GetId())
		defer progress.stop()
		ctx = resource.WithProgress(ctx, progress.say)
		ctx = resource.WithOutcome(ctx, func(o resource.Outcome) {
			_ = c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Progress{Progress: &agentv1.CallProgress{
				Id: call.GetId(), Outcome: &agentv1.StepOutcome{Digest: o.Digest, Previous: o.Previous, Replacing: o.Replacing, Note: o.Note, Untouched: o.Untouched, Did: resourcepb.Did(o.Did)},
			}}})
		})
		// The values of what the spec uses, opened in memory for this
		// step alone; they never reach a log, the disk or Desired, which
		// hears of the step with the call's own context.
		applyCtx := ctx
		if sealed := call.GetApply().GetValues(); len(sealed) > 0 {
			if c.delivery == nil {
				return fail(errors.New("this agent has no delivery key to open the values sent for this step"))
			}
			values, err := c.delivery.Open(c.server, ref, sealed)
			if err != nil {
				return fail(err)
			}
			defer clear(values)
			applyCtx = resource.WithValues(ctx, values)
		}
		if err := c.local.Apply(applyCtx, step, spec); err != nil {
			return fail(err)
		}
		if c.cfg.Desired != nil {
			if err := c.cfg.Desired.Applied(ctx, ref, spec); err != nil {
				c.logger.Warn("applied a step, but can't keep its desired state", slog.String("resource", ref.String()), slog.String("error", err.Error()))
			}
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Apply{Apply: &agentv1.ApplyResult{}}}
	case call.GetObserve() != nil:
		refs := make([]resource.Ref, 0, len(call.GetObserve().GetResources()))
		for _, r := range call.GetObserve().GetResources() {
			refs = append(refs, resourcepb.FromRef(r))
		}
		observed, err := c.local.Observe(ctx, refs)
		if err != nil {
			return fail(err)
		}
		out := &agentv1.ObserveResult{}
		for _, o := range observed {
			res := &agentv1.ObservedResource{}
			if o != nil {
				b, err := resource.Encode(o)
				if err != nil {
					return fail(err)
				}
				res.Spec = b
			}
			out.Resources = append(out.Resources, res)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Observe{Observe: out}}
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{
		Message: "this agent doesn't know that call; update it to the control plane's version",
	}}}
}

// progressEvery is how often a call says how it is going, at most.
const progressEvery = time.Second

// progress returns what a call's drivers tell how it is going, which goes
// to the control plane at most once every progressEvery. Stop it once
// the call ends.
func (c *calls) progress(id uint64) *throttle {
	return newThrottle(progressEvery, func(message string) {
		_ = c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Progress{Progress: &agentv1.CallProgress{Id: id, Message: message}}})
	})
}

// throttle passes messages on at most once every interval. One that comes
// sooner isn't lost: the latest waits, and goes when the interval ends, so
// a change of phase, such as from pulling to starting, always shows.
type throttle struct {
	every time.Duration
	send  func(string)

	mu      sync.Mutex
	last    time.Time   // when the last message went
	waiting string      // the latest message to go next
	timer   *time.Timer // set while one waits
	stopped bool
}

func newThrottle(every time.Duration, send func(string)) *throttle {
	return &throttle{every: every, send: send}
}

// say passes a message on now, or once the interval ends.
func (t *throttle) say(message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch wait := t.every - time.Since(t.last); {
	case t.stopped:
	case t.timer != nil:
		t.waiting = message
	case wait > 0:
		t.waiting = message
		t.timer = time.AfterFunc(wait, t.flush)
	default:
		t.last = time.Now()
		t.send(message)
	}
}

func (t *throttle) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timer = nil
	if t.stopped {
		return
	}
	t.last = time.Now()
	t.send(t.waiting)
}

// stop drops what waits: the call's result says the last word, and must
// not be followed by its progress.
func (t *throttle) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// decode reads a desired resource with the agent's drivers.
func (c *calls) decode(d *agentv1.DesiredResource) (resource.Ref, proto.Message, error) {
	ref := resourcepb.FromRef(d.GetResource())
	if d.Spec == nil {
		return ref, nil, nil
	}
	spec, err := c.cfg.Drivers.Decode(ref.Kind, d.GetSpec())
	return ref, spec, err
}
