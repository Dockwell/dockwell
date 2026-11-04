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
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
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
// for its result. The call may or may not have run.
var ErrLost = errors.New("the server's agent disconnected")

// Agents reaches each connected server's agent over its stream, with the
// calls the engine makes: plan, apply and observe. It is the engine's
// revision.Agents. Each call carries an ID, so results can come back in
// any order, and a timeout; a caller that gives up cancels it on the
// agent. See docs/adr/0017-the-change-api-and-the-agent-stream.md.
type Agents struct {
	kinds  *resource.Registry
	logger *slog.Logger

	mu        sync.Mutex
	sessions  map[string]*Session // the newest stream of each server
	onConnect []func(server string)
}

// NewAgents returns an empty set of agents. kinds reads the specs agents
// send back.
func NewAgents(kinds *resource.Registry, logger *slog.Logger) *Agents {
	return &Agents{kinds: kinds, logger: logger, sessions: map[string]*Session{}}
}

// OnConnect calls fn, in its own goroutine, each time a server's agent
// connects, such as to resume what waits for it.
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

// Kinds returns what a connected server's agent has drivers for.
func (a *Agents) Kinds(server string) ([]resource.Kind, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[server]
	if !ok {
		return nil, false
	}
	return slices.Clone(s.kinds), true
}

// open starts a session for a stream that has said hello. send writes to
// the stream; Session.close ends the session when the stream does.
func (a *Agents) open(server, name string, kinds []string, send func(*agentv1.ConnectResponse) error) *Session {
	s := &Session{
		agents: a, server: server, name: name, send: send,
		pending: map[uint64]chan *agentv1.CallResult{}, done: make(chan struct{}),
	}
	for _, k := range kinds {
		s.kinds = append(s.kinds, resource.Kind(k))
	}
	slices.Sort(s.kinds)
	a.mu.Lock()
	a.sessions[server] = s
	hooks := slices.Clone(a.onConnect)
	a.mu.Unlock()
	for _, fn := range hooks {
		go fn(server)
	}
	return s
}

// Session is one stream to a server's agent, and the calls waiting on it.
type Session struct {
	agents *Agents
	server string
	name   string
	kinds  []resource.Kind

	sendMu sync.Mutex
	send   func(*agentv1.ConnectResponse) error
	// ended stops sends once the stream's handler is returning, which
	// must not write to it any more.
	ended bool

	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan *agentv1.CallResult
	done    chan struct{}
	closed  bool
}

var _ resource.Agent = (*Session)(nil)

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

// call sends c and waits for its result, until ctx is done or the stream
// ends. c's timeout is what remains of ctx's deadline, or fallback.
func (s *Session) call(ctx context.Context, c *agentv1.Call, fallback time.Duration) (*agentv1.CallResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fallback)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()
	timeout := time.Until(deadline)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, s.lost()
	}
	s.next++
	c.Id = s.next
	ch := make(chan *agentv1.CallResult, 1)
	s.pending[c.Id] = ch
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		delete(s.pending, c.GetId())
		s.mu.Unlock()
	}
	c.Timeout = durationpb.New(timeout)
	if err := s.write(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Call{Call: c}}); err != nil {
		forget()
		return nil, s.lost()
	}
	select {
	case r := <-ch:
		if e := r.GetError(); e != nil {
			return nil, errors.New(e.GetMessage())
		}
		return r, nil
	case <-s.done:
		forget()
		return nil, s.lost()
	case <-ctx.Done():
		forget()
		// Best effort: the agent stops the call when it can.
		_ = s.write(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Cancel{Cancel: &agentv1.CancelCall{Id: c.GetId()}}})
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s's agent didn't answer within %s: %w", s.name, human.Duration(timeout.Round(time.Second)), ctx.Err())
		}
		return nil, ctx.Err()
	}
}

func (s *Session) lost() error {
	return fmt.Errorf("%w: %s's agent disconnected before it answered, so the step may or may not have run. Dockwell checks it again when the agent reconnects", ErrLost, s.name)
}

// supports refuses a kind the agent has no driver for.
func (s *Session) supports(ref resource.Ref) error {
	if slices.Contains(s.kinds, ref.Kind) {
		return nil
	}
	have := "none"
	if len(s.kinds) > 0 {
		names := make([]string, len(s.kinds))
		for i, k := range s.kinds {
			names[i] = string(k)
		}
		have = strings.Join(names, ", ")
	}
	return fmt.Errorf("%w: the agent on %s has no driver for %s resources, so it can't run %s %s (it has drivers for %s). "+
		"Run it on a server whose agent can, or update the agent on %s", resource.ErrNoDriver, s.name, ref.Kind, ref.Kind, ref.Name, have, s.name)
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
		steps[i] = resourcepb.FromStep(p)
		// The step is about the resource asked for, whatever the agent says.
		steps[i].Ref = desired[i].Ref
	}
	return steps, nil
}

// Apply asks the agent to carry out a step and verify the result.
func (s *Session) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if err := s.supports(step.Ref); err != nil {
		return err
	}
	dr, err := desiredProto(step.Ref, desired)
	if err != nil {
		return err
	}
	_, err = s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{Step: resourcepb.Step(step, ""), Desired: dr}}}, ApplyTimeout)
	return err
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
