// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// How long a call may take when its caller sets no deadline. Planning and
// observing read the server; applying may pull an image or wait for a
// health check.
const (
	PlanTimeout    = time.Minute
	ObserveTimeout = time.Minute
	ApplyTimeout   = 10 * time.Minute
)

// ErrLost means a server's stream ended while a call on it was waiting
// for its result. The call may or may not have run. It is resource.ErrLost,
// which the engine knows.
var ErrLost = resource.ErrLost

// reconnectGrace is how long a call that an agent refused, because it is
// moving to a new stream, waits for the new one once the old one ends.
const reconnectGrace = 30 * time.Second

// errDraining is a call the agent refused without running it, because it
// is moving to a new stream.
var errDraining = errors.New("the agent is moving to a new stream")

// Agents reaches each connected server's agent over its stream, with the
// calls the engine makes: plan, apply and observe. It is the engine's
// revision.Agents. Each call carries an ID, so results can come back in
// any order, and a timeout; a caller that gives up cancels it on the
// agent. See docs/adr/0017-the-change-api-and-the-agent-stream.md.
type Agents struct {
	kinds  *resource.Registry
	logger *slog.Logger
	// values finds the values of the variables and secrets a service uses,
	// for the server it is about to run on.
	values resource.Values

	mu        sync.Mutex
	sessions  map[string]*Session // the newest stream of each server
	onConnect []func(server string)
	// opened is closed, and made again, whenever a session opens, for the
	// calls waiting for a server's next stream.
	opened chan struct{}
}

// NewAgents returns an empty set of agents. kinds reads the specs agents
// send back.
func NewAgents(kinds *resource.Registry, logger *slog.Logger) *Agents {
	return &Agents{kinds: kinds, logger: logger, sessions: map[string]*Session{}, opened: make(chan struct{})}
}

// SendValues has the agents send each step the values of the variables and
// secrets its spec uses, from values, sealed to the stream's delivery key.
// Without it, a step that uses any fails.
func (a *Agents) SendValues(values resource.Values) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.values = values
}

// OnConnect calls fn, in its own goroutine, each time a server's agent
// connects, and each time it can run a kind it couldn't, as when Docker
// starts after it, such as to resume what waits for it.
func (a *Agents) OnConnect(fn func(server string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onConnect = append(a.onConnect, fn)
}

// Agent returns a server's agent, and whether it is connected now. Calls
// go to the stream that is newest when Agent is called.
func (a *Agents) Agent(server string) (resource.Agent, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[server]
	if !ok {
		return nil, false
	}
	return s, true
}

// open starts a session for a stream that has said hello. send writes to
// the stream; Session.close ends the session when the stream does.
func (a *Agents) open(server, name string, hello *agentv1.Hello, send func(*agentv1.ConnectResponse) error) *Session {
	s := &Session{
		agents: a, server: server, name: name, send: send, deliveryKey: slices.Clone(hello.GetDeliveryKey()),
		pending: map[uint64]chan *agentv1.CallResult{}, progress: map[uint64]func(string){}, done: make(chan struct{}),
	}
	s.setKinds(hello.GetKinds(), hello.GetUnavailable())
	a.mu.Lock()
	a.sessions[server] = s
	close(a.opened)
	a.opened = make(chan struct{})
	a.mu.Unlock()
	a.connected(server)
	return s
}

// connected runs the OnConnect hooks for server, each in its own
// goroutine: when its agent connects, and when it can run more kinds than
// it could, as when Docker starts after the agent, so what waited for
// them, such as a rollback, goes on.
func (a *Agents) connected(server string) {
	a.mu.Lock()
	hooks := slices.Clone(a.onConnect)
	a.mu.Unlock()
	for _, fn := range hooks {
		go fn(server)
	}
}

// Session is one stream to a server's agent, and the calls waiting on it.
type Session struct {
	agents *Agents
	server string
	name   string

	kindsMu sync.Mutex
	kinds   []resource.Kind
	// unavailable says why the agent can't run a kind it has a driver
	// for, such as Docker not running.
	unavailable map[resource.Kind]string
	// deliveryKey is the stream's own public key, which the values of
	// variables and secrets are sealed to.
	deliveryKey []byte

	sendMu sync.Mutex
	send   func(*agentv1.ConnectResponse) error
	// ended stops sends once the stream's handler is returning, which
	// must not write to it any more.
	ended bool
	// draining stops calls once the agent said it is moving to a new
	// stream: they go to the next one.
	draining bool

	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan *agentv1.CallResult
	// progress hears how each pending call is going, where its caller
	// follows it.
	progress map[uint64]func(string)
	done     chan struct{}
	closed   bool
}

// setKinds records what the agent says it can run, in its hello or
// after.
// setKinds takes the kinds the agent can run now, and reports whether
// any is new: one it couldn't run before.
func (s *Session) setKinds(kinds []string, unavailable []*agentv1.UnavailableKind) (grew bool) {
	s.kindsMu.Lock()
	defer s.kindsMu.Unlock()
	before := slices.Clone(s.kinds)
	s.kinds = s.kinds[:0]
	for _, k := range kinds {
		s.kinds = append(s.kinds, resource.Kind(k))
		grew = grew || !slices.Contains(before, resource.Kind(k))
	}
	slices.Sort(s.kinds)
	s.unavailable = map[resource.Kind]string{}
	for _, u := range unavailable {
		s.unavailable[resource.Kind(u.GetKind())] = u.GetReason()
	}
	return grew
}

// progressed passes on how a call is going to its caller, if it follows.
func (s *Session) progressed(p *agentv1.CallProgress) {
	s.mu.Lock()
	fn := s.progress[p.GetId()]
	s.mu.Unlock()
	if fn != nil {
		fn(p.GetMessage())
	}
}

var _ resource.Agent = (*Session)(nil)

// next waits for a newer session of old's server than old, as when its
// agent moves to a new stream, until ctx is done, or old has ended and
// reconnectGrace has passed.
func (a *Agents) next(ctx context.Context, old *Session) (*Session, error) {
	ended := old.done
	var grace *time.Timer
	var graceC <-chan time.Time
	defer func() {
		if grace != nil {
			grace.Stop()
		}
	}()
	for {
		a.mu.Lock()
		current, opened := a.sessions[old.server], a.opened
		a.mu.Unlock()
		if current != nil && current != old {
			return current, nil
		}
		select {
		case <-opened:
		case <-ended:
			ended = nil
			grace = time.NewTimer(reconnectGrace)
			graceC = grace.C
		case <-graceC:
			// A stream that opened as the grace ran out still takes the call.
			a.mu.Lock()
			current = a.sessions[old.server]
			a.mu.Unlock()
			if current != nil && current != old {
				return current, nil
			}
			return nil, fault.New(resource.ErrNotConnected, "%s's agent was moving to a new stream and didn't reconnect within %s, so the call didn't run",
				old.name, human.Duration(reconnectGrace))
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// close ends the session: the calls still waiting fail with ErrLost, and
// the server is no longer connected unless a newer stream took over. It
// waits for a send under way, and stops any after it, so the stream's
// handler can return.
func (s *Session) close() {
	s.sendMu.Lock()
	s.ended = true
	s.sendMu.Unlock()
	s.agents.mu.Lock()
	if s.agents.sessions[s.server] == s {
		delete(s.agents.sessions, s.server)
	}
	s.agents.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
}

// deliver hands a result to the call waiting for it. A result nobody waits
// for, as when its call timed out, is dropped.
func (s *Session) deliver(r *agentv1.CallResult) {
	s.mu.Lock()
	ch, ok := s.pending[r.GetId()]
	delete(s.pending, r.GetId())
	delete(s.progress, r.GetId())
	s.mu.Unlock()
	if ok {
		ch <- r
	}
}

func (s *Session) write(msg *agentv1.ConnectResponse) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.ended {
		return ErrLost
	}
	return s.send(msg)
}

// writeCall sends a call, unless the agent is moving to a new stream.
// Checking under the send lock means a call goes out before Drained or
// not at all, so the agent answers every call it was sent.
func (s *Session) writeCall(msg *agentv1.ConnectResponse) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	switch {
	case s.draining:
		return errDraining
	case s.ended:
		return ErrLost
	}
	return s.send(msg)
}

// moving reports a stream the agent left for a new one.
func (s *Session) moving() bool {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.draining
}

// drain answers an agent that is moving to a new stream: no more calls go
// on this one, and the agent hears so after every call sent before.
func (s *Session) drain() {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.draining || s.ended {
		return
	}
	s.draining = true
	_ = s.send(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Drained{Drained: &agentv1.Drained{}}})
}

// call sends c and waits for its result, until ctx is done or the stream
// ends. c's timeout is what remains of ctx's deadline, or fallback. A call
// the agent refuses because it is moving to a new stream didn't run, so it
// is sent again on the new one, within the same time.
func (s *Session) call(ctx context.Context, c *agentv1.Call, fallback time.Duration) (*agentv1.CallResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fallback)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()
	timeout := time.Until(deadline)
	for {
		r, err := s.callOnce(ctx, c)
		if !errors.Is(err, errDraining) {
			return r, s.timedOut(ctx, err, timeout)
		}
		next, err := s.agents.next(ctx, s)
		if err != nil {
			return nil, s.timedOut(ctx, err, timeout)
		}
		s = next
	}
}

// timedOut says, of a call that ran out of time, how long it had.
func (s *Session) timedOut(ctx context.Context, err error, timeout time.Duration) error {
	if err != nil && errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s's agent didn't answer within %s: %w", s.name, human.Duration(timeout.Round(time.Second)), err)
	}
	return err
}

// callOnce sends c on this stream and waits for its result.
func (s *Session) callOnce(ctx context.Context, c *agentv1.Call) (*agentv1.CallResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		if s.moving() {
			return nil, errDraining // the agent is on, or coming to, its next stream
		}
		return nil, s.notConnected()
	}
	s.next++
	c.Id = s.next
	ch := make(chan *agentv1.CallResult, 1)
	s.pending[c.Id] = ch
	if fn := resource.ProgressFunc(ctx); fn != nil {
		s.progress[c.Id] = fn
	}
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		delete(s.pending, c.GetId())
		delete(s.progress, c.GetId())
		s.mu.Unlock()
	}
	deadline, _ := ctx.Deadline()
	c.Timeout = durationpb.New(time.Until(deadline))
	if err := s.writeCall(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Call{Call: c}}); err != nil {
		forget()
		if errors.Is(err, errDraining) {
			return nil, err
		}
		return nil, s.notConnected()
	}
	// The result, the stream's end and the call's own end can all be ready
	// when the select wakes, and Go picks one of them at random. A result
	// that is there wins, whatever else ended: the agent answered.
	var r *agentv1.CallResult
	select {
	case r = <-ch:
	case <-s.done:
		select {
		case r = <-ch:
		default:
			forget()
			return nil, s.lost(c)
		}
	case <-ctx.Done():
		select {
		case r = <-ch:
		default:
			forget()
			// Best effort: the agent stops the call when it can.
			_ = s.write(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Cancel{Cancel: &agentv1.CancelCall{Id: c.GetId()}}})
			return nil, ctx.Err()
		}
	}
	switch e := r.GetError(); {
	case e == nil:
		// The call ran, even if its time ran out as the result came.
		return r, nil
	case ctx.Err() != nil:
		// It failed as its time ran out, or as its caller gave up: most
		// likely for that reason, as the agent stops a call at the
		// deadline the control plane gave it. Say so, as when the result
		// comes too late.
		return nil, ctx.Err()
	case e.GetDraining():
		return nil, errDraining
	default:
		return nil, &resource.ServerError{Server: s.name, Err: errors.New(e.GetMessage())}
	}
}

// lost is a call whose stream ended before its result came. A step may
// have run; a plan or a read changed nothing.
func (s *Session) lost(c *agentv1.Call) error {
	if c.GetApply() != nil {
		return fault.New(ErrLost, "%s's agent disconnected before it answered, so the step may or may not have run. "+
			"Dockwell checks it again when the agent reconnects", s.name)
	}
	return fault.New(ErrLost, "%s's agent disconnected before it answered. Try again once it reconnects", s.name)
}

// notConnected is a call that never reached the agent, as its stream had
// ended.
func (s *Session) notConnected() error {
	return fault.New(resource.ErrNotConnected, "%s's agent disconnected before the call reached it, so it didn't run", s.name)
}

// supports refuses a kind the agent has no driver for, or can't run now,
// with the agent's reason, such as Docker not running.
func (s *Session) supports(ref resource.Ref) error {
	s.kindsMu.Lock()
	defer s.kindsMu.Unlock()
	if slices.Contains(s.kinds, ref.Kind) {
		return nil
	}
	if why, ok := s.unavailable[ref.Kind]; ok {
		return fault.New(resource.ErrNoDriver, "%s can't run %s %s now: %s", s.name, ref.Kind, ref.Name, why)
	}
	have := "none"
	if len(s.kinds) > 0 {
		names := make([]string, len(s.kinds))
		for i, k := range s.kinds {
			names[i] = string(k)
		}
		have = strings.Join(names, ", ")
	}
	return fault.New(resource.ErrNoDriver, "the agent on %s has no driver for %s resources, so it can't run %s %s (it has drivers for %s). "+
		"Run it on a server whose agent can, or update the agent on %s", s.name, ref.Kind, ref.Kind, ref.Name, have, s.name)
}

// Plan asks the agent what moving each resource to its desired spec would
// take. It reads the server and changes nothing there.
func (s *Session) Plan(ctx context.Context, desired []resource.Desired) ([]resource.Step, error) {
	call := &agentv1.PlanCall{}
	for _, d := range desired {
		if err := s.supports(d.Ref); err != nil {
			return nil, err
		}
		dr, err := desiredProto(d.Ref, d.Spec)
		if err != nil {
			return nil, err
		}
		call.Desired = append(call.Desired, dr)
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Plan{Plan: call}}, PlanTimeout)
	if err != nil {
		return nil, err
	}
	got := r.GetPlan().GetSteps()
	if len(got) != len(desired) {
		return nil, fmt.Errorf("%s's agent judged %d steps for %d resources", s.name, len(got), len(desired))
	}
	steps := make([]resource.Step, len(got))
	for i, p := range got {
		if steps[i], err = resourcepb.FromStep(p); err != nil {
			// Read as anything else, the step would be skipped or
			// misstated, so the plan fails.
			return nil, fmt.Errorf("%s's agent planned a step for %s that this control plane can't read: %w. "+
				"The agent is newer than the control plane, so upgrade the control plane to the agent's version", s.name, desired[i].Ref, err)
		}
		// The step is about the resource asked for, whatever the agent says.
		steps[i].Ref = desired[i].Ref
		// A field the schema marks sensitive never shows its value, whatever
		// the agent says of it: plans are kept, and an agent that got it
		// wrong mustn't put a secret in one.
		if t, err := s.agents.kinds.Type(desired[i].Ref.Kind); err == nil {
			md := t.New().ProtoReflect().Descriptor()
			for j := range steps[i].Changes {
				c := &steps[i].Changes[j]
				if c.Sensitive || resource.SensitivePath(md, c.Path) {
					c.Sensitive, c.From, c.To = true, "", ""
				}
			}
		}
	}
	return steps, nil
}

// Apply asks the agent to carry out a step and verify the result. A step
// whose spec uses variables or secrets carries their values, sealed to
// this stream's delivery key, which only this agent, on this stream, can
// open.
func (s *Session) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if err := s.supports(step.Ref); err != nil {
		return err
	}
	dr, err := desiredProto(step.Ref, desired)
	if err != nil {
		return err
	}
	call := &agentv1.ApplyCall{Step: resourcepb.Step(step, ""), Desired: dr}
	if call.Values, err = s.sealedValues(ctx, step.Ref, desired); err != nil {
		return err
	}
	_, err = s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Apply{Apply: call}}, ApplyTimeout)
	return err
}

// sealedValues are the values of what a desired spec uses, sealed for this
// stream, or nil when it uses nothing.
func (s *Session) sealedValues(ctx context.Context, ref resource.Ref, desired proto.Message) ([]byte, error) {
	t, err := s.agents.kinds.Type(ref.Kind)
	if err != nil || len(resource.Uses(t, desired)) == 0 {
		return nil, err
	}
	s.agents.mu.Lock()
	provider := s.agents.values
	s.agents.mu.Unlock()
	if provider == nil {
		return nil, fmt.Errorf("can't send the variables and secrets %s uses to %s: this control plane has no keeper for them", ref, s.name)
	}
	if len(s.deliveryKey) == 0 {
		return nil, fmt.Errorf("the agent on %s sent no delivery key, so it can't receive the variables and secrets %s uses. Update the agent on %s",
			s.name, ref.Name, s.name)
	}
	values, err := provider.Values(ctx, s.server, ref, desired)
	if err != nil {
		return nil, err
	}
	defer clear(values)
	sealed, err := binding.Seal(s.deliveryKey, s.server, ref, values)
	if err != nil {
		return nil, fmt.Errorf("can't seal the values %s uses for %s: %w", ref.Name, s.name, err)
	}
	return sealed, nil
}

// Observe asks the agent for resources as the server has them.
func (s *Session) Observe(ctx context.Context, refs []resource.Ref) ([]proto.Message, error) {
	call := &agentv1.ObserveCall{}
	for _, ref := range refs {
		if err := s.supports(ref); err != nil {
			return nil, err
		}
		call.Resources = append(call.Resources, resourcepb.Ref(ref))
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Observe{Observe: call}}, ObserveTimeout)
	if err != nil {
		return nil, err
	}
	got := r.GetObserve().GetResources()
	if len(got) != len(refs) {
		return nil, fmt.Errorf("%s's agent read %d resources of %d", s.name, len(got), len(refs))
	}
	out := make([]proto.Message, len(refs))
	for i, o := range got {
		if o.Spec == nil {
			continue // the server doesn't have it
		}
		if out[i], err = s.agents.kinds.Decode(refs[i].Kind, o.GetSpec()); err != nil {
			return nil, fmt.Errorf("can't read %s from %s's agent: %w", refs[i], s.name, err)
		}
	}
	return out, nil
}

func desiredProto(ref resource.Ref, spec proto.Message) (*agentv1.DesiredResource, error) {
	d := &agentv1.DesiredResource{Resource: resourcepb.Ref(ref)}
	if spec != nil {
		b, err := resource.Encode(spec)
		if err != nil {
			return nil, err
		}
		d.Spec = b
	}
	return d, nil
}
