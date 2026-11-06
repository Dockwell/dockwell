// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/secrets"
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

// MoveGrace is how long a server whose agent is moving to a new stream, as it does
// each time its certificate is renewed, goes on reading as connected, and how long a
// read waits for it, before it reads as offline. An agent is back within a second
// unless something is wrong, and this leaves room for its first retries, which back
// off from a second.
const MoveGrace = 10 * time.Second

// ErrMoving is a call the agent refused without running it, or a read it stopped,
// because it is moving to a new stream. What it was doing can go on over the next.
var ErrMoving = errors.New("the agent is moving to a new stream")

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
	// naming says what names values on a server, for agents that don't write them in.
	naming Naming

	mu        sync.Mutex
	sessions  map[string]*Session // the newest stream of each server
	uploads   map[uint64]*upload  // logs calls under way, by their streams
	onConnect []func(server string)
	// opened is closed, and made again, whenever a session opens, for the
	// calls waiting for a server's next stream.
	opened chan struct{}
	// moved is when a server's last stream ended, for an agent that said it was
	// moving to a new one and hasn't opened it yet.
	moved map[string]time.Time
	// domains is what each server last said of its routes' domains. It
	// outlives the server's stream, so an offline server shows what it last
	// said, and it is lost with the control plane, as the agent says it again
	// when its stream opens.
	domains map[string][]*dockwellv1.DomainStatus
	// ingress is what each server last said of its ingress.
	ingress map[string]*dockwellv1.IngressStatus
	// health is what each server last said of its services, and health hears
	// each report. It outlives the stream as domains does.
	health       map[string]health.Report
	healthHearer func(server string, instances []*dockwellv1.InstanceHealth)
}

// NewAgents returns an empty set of agents. kinds reads the specs agents
// send back.
func NewAgents(kinds *resource.Registry, logger *slog.Logger) *Agents {
	return &Agents{
		kinds: kinds, logger: logger, sessions: map[string]*Session{}, uploads: map[uint64]*upload{}, opened: make(chan struct{}), moved: map[string]time.Time{},
		domains: map[string][]*dockwellv1.DomainStatus{}, ingress: map[string]*dockwellv1.IngressStatus{}, health: map[string]health.Report{},
	}
}

// OnHealth calls fn with each report of how a server's services are, in the
// order they come on one stream.
func (a *Agents) OnHealth(fn func(server string, instances []*dockwellv1.InstanceHealth)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.healthHearer = fn
}

// ReportHealth keeps what a server says of its services. An agent is trusted with
// its own server and nothing more, so what it says is made fit to show before it
// goes further, as an agent that has been taken over could say anything: it reaches
// a timeline and a terminal. With no time it read Docker, it has said nothing, which
// isn't that nothing runs.
func (a *Agents) ReportHealth(server string, h *agentv1.Health) {
	if h.GetScannedAt() == nil {
		// It hasn't read Docker yet, which says nothing of what runs, but it may say
		// that Docker hasn't answered it, as when it starts with Docker down.
		if h.GetDockerUnreadableSince() == nil {
			return
		}
		a.mu.Lock()
		a.health[server] = health.Report{
			DockerUnreadableSince: h.GetDockerUnreadableSince().AsTime(), ReportedAt: h.GetReportedAt().AsTime(), ReceivedAt: time.Now(),
		}
		a.mu.Unlock()
		return
	}
	instances := fitToShow(h.GetInstances())
	a.mu.Lock()
	report := health.Report{
		Said: true, Instances: instances, ScannedAt: h.GetScannedAt().AsTime(), ReportedAt: h.GetReportedAt().AsTime(), ReceivedAt: time.Now(),
	}
	if h.GetDockerUnreadableSince() != nil {
		report.DockerUnreadableSince = h.GetDockerUnreadableSince().AsTime()
	}
	a.health[server] = report
	hear := a.healthHearer
	a.mu.Unlock()
	if hear != nil {
		hear(server, instances)
	}
}

// fitToShow holds what an agent reports to what its own monitor would send: as many
// containers as it watches, each in one short printable line, and a state the API
// has a name for.
func fitToShow(instances []*dockwellv1.InstanceHealth) []*dockwellv1.InstanceHealth {
	instances = instances[:min(len(instances), health.MaxInstances)]
	for _, i := range instances {
		if i == nil {
			continue
		}
		if _, known := dockwellv1.HealthState_name[int32(i.GetState())]; !known {
			i.State = dockwellv1.HealthState_HEALTH_STATE_UNKNOWN
		}
		i.Container = health.Clean(i.GetContainer(), nil)
		if i.GetLast() != nil {
			i.Last.Output = health.Clean(i.GetLast().GetOutput(), nil)
		}
	}
	return slices.DeleteFunc(instances, func(i *dockwellv1.InstanceHealth) bool { return i == nil })
}

// Health is what a server last said of its services, and whether its agent is
// connected to say more. One moving to a new stream is, for MoveGrace.
func (a *Agents) Health(server string) (report health.Report, connected bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.health[server], a.connectedLocked(server)
}

// connectedLocked reports a server's agent connected, or moving to a new stream: what
// it last said stands through a move, as it says it again as the next stream opens,
// which it does within a second unless something is wrong. One that hasn't by
// MoveGrace is gone, and one that went without saying it was moving is gone at once.
func (a *Agents) connectedLocked(server string) bool {
	if _, ok := a.sessions[server]; ok {
		return true
	}
	moved, ok := a.moved[server]
	return ok && time.Since(moved) < MoveGrace
}

// Report keeps what a server says of its routes' domains.
func (a *Agents) Report(server string, domains []*dockwellv1.DomainStatus, ingress *dockwellv1.IngressStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.domains[server] = domains
	a.ingress[server] = ingress
}

// Ingress is what a server last said of its ingress, or nil.
func (a *Agents) Ingress(server string) *dockwellv1.IngressStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ingress[server]
}

// Domain is what a server last said of a domain, and whether its agent is
// connected to say more, as Health has it. The status is nil until it has said.
func (a *Agents) Domain(server, domain string) (status *dockwellv1.DomainStatus, connected bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	connected = a.connectedLocked(server)
	for _, d := range a.domains[server] {
		if d.GetDomain() == domain {
			return d, connected
		}
	}
	return nil, connected
}

// SendValues has the agents send each step the values of the variables and
// secrets its spec uses, from values, sealed to the stream's delivery key.
// Without it, a step that uses any fails.
func (a *Agents) SendValues(values resource.Values) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.values = values
}

// Naming says which resources name values in what runs on a server, the keeper's
// secrets.Keeper.NamingValues.
type Naming interface {
	NamingValues(ctx context.Context, server string, refs []resource.Ref) ([]resource.Ref, error)
}

// CheckNaming has each stream ask naming, of an agent that doesn't say it writes a
// container's references in, which of what it reads, plans and applies names values,
// and refuse those (Session.checkReads). Without it, such an agent is refused all of
// them.
func (a *Agents) CheckNaming(naming Naming) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.naming = naming
}

// OnConnect calls fn, in its own goroutine, each time a server's agent
// connects, each time it can run a kind it couldn't, as when Docker starts
// after it, and each time what held calls up there ended, such as to resume
// what waits for it.
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

// Connected returns the servers with an agent connected now.
func (a *Agents) Connected() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Collect(maps.Keys(a.sessions))
}

// Session returns a server's stream, the one calls go to now, and whether it
// is connected.
func (a *Agents) Session(server string) (*Session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[server]
	return s, ok
}

// WaitSession waits for a server's agent to be connected, until ctx is done.
func (a *Agents) WaitSession(ctx context.Context, server string) (*Session, error) {
	for {
		a.mu.Lock()
		s, opened := a.sessions[server], a.opened
		a.mu.Unlock()
		if s != nil && !s.moving() {
			return s, nil
		}
		select {
		case <-opened:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// SessionPastMove returns a server's stream, as Session does, and what it does for
// an agent moving to a new stream is wait for that one, for no longer than within:
// a server whose stream has been gone that long reads as not connected at once. An
// agent that went without saying it was moving isn't waited for. A long move, such
// as one waiting for a step under way, is out of time here too, as its agent takes
// no reads until it has moved.
func (a *Agents) SessionPastMove(ctx context.Context, server string, within time.Duration) (*Session, bool) {
	deadline := time.Now().Add(within)
	for {
		a.mu.Lock()
		s, opened, moved := a.sessions[server], a.opened, a.moved[server]
		a.mu.Unlock()
		switch {
		case s != nil && !s.moving():
			return s, true
		case s == nil && moved.IsZero():
			return nil, false
		case s == nil && moved.Add(within).Before(deadline):
			deadline = moved.Add(within)
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, false
		}
		wait := time.NewTimer(left)
		select {
		case <-opened:
		case <-wait.C: // once more, as a stream that opened as the time ran out will do
		case <-ctx.Done():
			wait.Stop()
			return nil, false
		}
		wait.Stop()
	}
}

// open starts a session for a stream that has said hello. send writes to
// the stream; Session.close ends the session when the stream does.
func (a *Agents) open(server, name string, hello *agentv1.Hello, send func(*agentv1.ConnectResponse) error) *Session {
	s := &Session{
		agents: a, server: server, name: name, send: send, deliveryKey: slices.Clone(hello.GetDeliveryKey()), writesReferences: hello.GetWritesReferences(),
		pending: map[uint64]chan *agentv1.CallResult{}, progress: map[uint64]func(string){}, outcomes: map[uint64]func(resource.Outcome){}, done: make(chan struct{}),
	}
	s.setKinds(hello.GetKinds(), hello.GetUnavailable())
	a.mu.Lock()
	a.sessions[server] = s
	delete(a.moved, server)
	close(a.opened)
	a.opened = make(chan struct{})
	a.mu.Unlock()
	a.connected(server)
	return s
}

// connected runs the OnConnect hooks for server, each in its own
// goroutine: when its agent connects, when it can run more kinds than it
// could, as when Docker starts after the agent, and when what held calls
// up there ended, so what waited for them, such as a rollback, goes on.
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
	// writesReferences says the agent writes a container's references in.
	writesReferences bool

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
	// outcomes hears what each pending call found out.
	outcomes map[uint64]func(resource.Outcome)
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
	fn, told := s.progress[p.GetId()], s.outcomes[p.GetId()]
	s.mu.Unlock()
	if o := p.GetOutcome(); o != nil {
		if told != nil {
			told(resource.Outcome{Digest: o.GetDigest(), Previous: o.GetPrevious(), Replacing: o.GetReplacing(), Note: o.GetNote(), Untouched: o.GetUntouched(), Did: resourcepb.DidFrom(o.GetDid())})
		}
		return
	}
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
	moving := s.draining
	s.sendMu.Unlock()
	s.agents.mu.Lock()
	if s.agents.sessions[s.server] == s {
		delete(s.agents.sessions, s.server)
		if moving {
			s.agents.moved[s.server] = time.Now()
		}
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
	delete(s.outcomes, r.GetId())
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
		return ErrMoving
	case s.ended:
		return ErrLost
	}
	return s.send(msg)
}

// leftMoving reports a session that ended as its agent moved to a new stream.
func (s *Session) leftMoving() bool {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.ended && s.draining
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
// is sent again on the new one, within the same time. prepare, when set,
// readies c for each stream it is sent on, as values sealed to one
// stream's delivery key open on no other.
func (s *Session) call(ctx context.Context, c *agentv1.Call, fallback time.Duration, prepare func(*Session) error) (*agentv1.CallResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fallback)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()
	timeout := time.Until(deadline)
	for {
		if prepare != nil {
			if err := prepare(s); err != nil {
				return nil, err
			}
		}
		r, err := s.callOnce(ctx, c)
		if !errors.Is(err, ErrMoving) {
			return r, s.timedOut(err, timeout)
		}
		next, err := s.agents.next(ctx, s)
		if err != nil {
			return nil, s.timedOut(err, timeout)
		}
		s = next
	}
}

// timedOut says, of a call that ran out of time, how long it had. Its time
// may run out on the agent first, as the agent's clock started later or ran
// faster, so what the agent said counts as well as the call's own deadline.
func (s *Session) timedOut(err error, timeout time.Duration) error {
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return fault.New(err, "%s's agent didn't answer within %s, so Dockwell stopped waiting. What it was asked may still be running, "+
			"or stuck on something on the server, such as Docker or a network mount. Check those on %s, then try again",
			s.name, human.Duration(timeout.Round(time.Second)), s.name)
	}
	return err
}

// outOfTime is what an agent said of a call whose time ran out there.
type outOfTime struct{ message string }

func (e outOfTime) Error() string { return e.message }

func (outOfTime) Unwrap() error { return context.DeadlineExceeded }

// unfinished is what an agent said of a call it didn't run, as the last step on a
// resource hasn't ended there.
type unfinished struct{ message string }

func (e unfinished) Error() string { return e.message }

func (unfinished) Unwrap() error { return resource.ErrUnfinished }

// callOnce sends c on this stream and waits for its result.
func (s *Session) callOnce(ctx context.Context, c *agentv1.Call) (*agentv1.CallResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		if s.moving() {
			return nil, ErrMoving // the agent is on, or coming to, its next stream
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
	if fn := resource.OutcomeFunc(ctx); fn != nil {
		s.outcomes[c.Id] = fn
	}
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		delete(s.pending, c.GetId())
		delete(s.progress, c.GetId())
		delete(s.outcomes, c.GetId())
		s.mu.Unlock()
	}
	deadline, _ := ctx.Deadline()
	c.Timeout = durationpb.New(time.Until(deadline))
	if err := s.writeCall(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Call{Call: c}}); err != nil {
		forget()
		if errors.Is(err, ErrMoving) {
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
		return nil, ErrMoving
	default:
		reason := errors.New(e.GetMessage())
		switch {
		case e.GetOutOfTime():
			reason = outOfTime{e.GetMessage()}
		case e.GetUnfinished():
			reason = unfinished{e.GetMessage()}
		}
		if plain := e.GetPlain(); plain != "" {
			reason = resource.PlainlyBecause(e.GetReason(), plain, reason)
		}
		return nil, &resource.ServerError{Server: s.name, Err: reason}
	}
}

// lost is a call whose stream ended before its result came. A step may
// have run; a plan or a read changed nothing.
func (s *Session) lost(c *agentv1.Call) error {
	if c.GetApply() != nil {
		return resource.Plainly(fmt.Sprintf("%s went offline during this step, so Dockwell can't tell whether it ran. Check that %s is up.", s.name, s.name),
			fault.New(ErrLost, "%s's agent disconnected during this step", s.name))
	}
	if c.GetSaveFile() != nil {
		return fault.New(ErrLost, "%s's agent disconnected before it answered, so the file may or may not have saved. "+
			"Open it again once the agent reconnects, to see what's there", s.name)
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
	if err := s.checkAllWrites(desired); err != nil {
		return nil, err
	}
	refs := make([]resource.Ref, len(desired))
	for i, d := range desired {
		refs[i] = d.Ref
	}
	if err := s.checkReads(ctx, refs); err != nil {
		return nil, err
	}
	call := &agentv1.PlanCall{}
	for _, d := range desired {
		if err := s.supports(d.Ref); err != nil {
			return nil, err
		}
		dr, err := desiredProto(d.Ref, d.Spec)
		if err != nil {
			return nil, err
		}
		dr.Pull, dr.Run = d.Pull, d.Run
		dr.Restart = d.Restart
		dr.Rollback = d.Rollback
		call.Desired = append(call.Desired, dr)
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Plan{Plan: call}}, PlanTimeout, nil)
	if err != nil {
		return nil, err
	}
	got := r.GetPlan().GetSteps()
	if len(got) != len(desired) {
		return nil, fmt.Errorf("%s's agent judged %d steps for %d resources", s.name, len(got), len(desired))
	}
	steps := make([]resource.Step, len(got))
	for i, p := range got {
		want := desired[i].Ref //nolint:gosec // got has as many as desired, as checked above
		if steps[i], err = resourcepb.FromStep(p); err != nil {
			// Read as anything else, the step would be skipped or
			// misstated, so the plan fails.
			return nil, fmt.Errorf("%s's agent planned a step for %s that this control plane can't read: %w. "+
				"The agent is newer than the control plane, so upgrade the control plane to the agent's version", s.name, want, err)
		}
		// The step is about the resource asked for, whatever the agent says.
		steps[i].Ref = want
		// A field the schema marks sensitive never shows its value, whatever
		// the agent says of it: plans are kept, and an agent that got it
		// wrong mustn't put a secret in one.
		if t, err := s.agents.kinds.Type(want.Kind); err == nil {
			md := t.New().ProtoReflect().Descriptor()
			for j := range steps[i].Changes {
				c := &steps[i].Changes[j]
				if !resource.IsStateChange(*c) && (c.Sensitive || resource.SensitivePath(md, c.Path)) {
					c.Sensitive, c.From, c.To = true, "", ""
				}
			}
		}
	}
	return steps, nil
}

// Apply asks the agent to carry out a step and verify the result. A step
// whose spec uses variables or secrets carries their values, sealed to the
// delivery key of the stream it goes out on, which only this agent, on
// that stream, can open. A step sent again on the agent's next stream, as
// when it moves to a renewed certificate, is sealed again for that one.
func (s *Session) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if err := s.supports(step.Ref); err != nil {
		return err
	}
	if err := s.checkWrites(step.Ref, desired); err != nil {
		return err
	}
	if err := s.checkReads(ctx, []resource.Ref{step.Ref}); err != nil {
		return err
	}
	dr, err := desiredProto(step.Ref, desired)
	if err != nil {
		return err
	}
	values, uses, err := s.values(ctx, step.Ref, desired)
	if err != nil {
		return err
	}
	defer clear(values)
	call := &agentv1.ApplyCall{Step: resourcepb.Step(step, ""), Desired: dr, Pulled: step.Pulled, Finishing: step.Finishing}
	if d := resource.DeployOf(ctx); d != (resource.Deploy{}) {
		call.Deploy = &agentv1.Deploy{Revision: int64(d.Revision), ChangeId: d.Change, Rollback: d.Rollback}
	}
	var seal func(*Session) error
	if uses {
		seal = func(on *Session) error {
			sealed, err := on.seal(step.Ref, values)
			call.Values = sealed
			return err
		}
	}
	// What a step waits on by design, such as a health check with a long start
	// period, comes on top of the time any step gets.
	_, err = s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Apply{Apply: call}}, ApplyTimeout+step.Budget(), seal)
	return err
}

// values are the values of what a desired spec uses, from the keeper, and
// whether it uses any.
func (s *Session) values(ctx context.Context, ref resource.Ref, desired proto.Message) (map[string]string, bool, error) {
	t, err := s.agents.kinds.Type(ref.Kind)
	if err != nil || len(resource.Uses(t, desired)) == 0 {
		return nil, false, err
	}
	s.agents.mu.Lock()
	provider := s.agents.values
	s.agents.mu.Unlock()
	if provider == nil {
		return nil, false, fmt.Errorf("can't send the variables and secrets %s uses to %s: this control plane has no keeper for them", ref, s.name)
	}
	if len(s.deliveryKey) == 0 {
		return nil, false, s.noDeliveryKey(ref)
	}
	values, err := provider.Values(ctx, s.server, ref, desired)
	if err != nil {
		return nil, false, err
	}
	return values, true, nil
}

// seal seals values for a resource to this stream's delivery key.
func (s *Session) seal(ref resource.Ref, values map[string]string) ([]byte, error) {
	if len(s.deliveryKey) == 0 {
		return nil, s.noDeliveryKey(ref)
	}
	sealed, err := secrets.SealValues(s.deliveryKey, s.server, ref, values)
	if err != nil {
		return nil, fmt.Errorf("can't seal the values %s uses for %s: %w", ref.Name, s.name, err)
	}
	return sealed, nil
}

// checkWrites refuses a spec whose fields name values for an agent that doesn't say
// it writes them in, which would run ${NAME} as the words it is: a password anyone who
// can read the plan knows.
func (s *Session) checkWrites(ref resource.Ref, spec proto.Message) error {
	if s.writesReferences || spec == nil || !binding.WritesReferences(spec) {
		return nil
	}
	return fmt.Errorf("the agent on %s can't write a value into a command, and %s names one there. Update the agent on %s", s.name, ref.Name, s.name)
}

// checkReads refuses, for an agent that doesn't say it writes references in, any of
// refs that names values on this server, now or in what the server was given. Such an
// agent reads a container's command as Docker has it, so what it reports of one,
// whatever the spec it is asked about, would carry a value into drift and plans. The
// whole call is refused, so drift reports the server as unchecked, and says why. With
// nothing to ask, as when CheckNaming was never called, everything counts as naming
// values, so a control plane that forgot it reads nothing of such an agent's rather
// than everything.
func (s *Session) checkReads(ctx context.Context, refs []resource.Ref) error {
	if s.writesReferences || len(refs) == 0 {
		return nil
	}
	s.agents.mu.Lock()
	naming := s.agents.naming
	s.agents.mu.Unlock()
	if naming == nil {
		return fmt.Errorf("%w: the agent on %s is from before a command could name a value, and this control plane can't tell what on %s names one, so it reads, plans and applies nothing there. "+
			"Update the agent on %s", resource.ErrAgentTooOld, s.name, s.name, s.name)
	}
	named, err := naming.NamingValues(ctx, s.server, refs)
	if err != nil {
		return fmt.Errorf("can't tell whether what %s runs names a value in its command: %w", s.name, err)
	}
	if len(named) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the agent on %s is from before a command could name a value, so it reads %s's command with its values written in. "+
		"Nothing on %s that names a value is read, planned or applied until you update the agent there", resource.ErrAgentTooOld, s.name, named[0].Name, s.name)
}

// checkAllWrites is checkWrites for all a plan asks of this agent.
func (s *Session) checkAllWrites(desired []resource.Desired) error {
	for _, d := range desired {
		if err := s.checkWrites(d.Ref, d.Spec); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) noDeliveryKey(ref resource.Ref) error {
	return fmt.Errorf("the agent on %s sent no delivery key, so it can't receive the variables and secrets %s uses. Update the agent on %s",
		s.name, ref.Name, s.name)
}

// Observe asks the agent for resources as the server has them.
func (s *Session) Observe(ctx context.Context, refs []resource.Ref) ([]proto.Message, error) {
	if err := s.checkReads(ctx, refs); err != nil {
		return nil, err
	}
	call := &agentv1.ObserveCall{}
	for _, ref := range refs {
		if err := s.supports(ref); err != nil {
			return nil, err
		}
		call.Resources = append(call.Resources, resourcepb.Ref(ref))
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Observe{Observe: call}}, ObserveTimeout, nil)
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
		ref := refs[i] //nolint:gosec // got has as many as refs, as checked above
		if out[i], err = s.agents.kinds.Decode(ref.Kind, o.GetSpec()); err != nil {
			return nil, fmt.Errorf("can't read %s from %s's agent: %w", ref, s.name, err)
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

// upload is one logs call's stream of lines, on its way from SendLogs to
// whoever waits for them.
type upload struct {
	server   string
	token    uint64
	messages chan *agentv1.SendLogsRequest
	done     chan struct{}
}

// openUpload notes a logs call about to be made. The token that names it is
// random, so one server's agent can't guess at another's calls, and each call
// checks the server's identity too.
func (a *Agents) openUpload(server string) *upload {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		var b [8]byte
		_, _ = rand.Read(b[:])
		token := binary.BigEndian.Uint64(b[:])
		if _, taken := a.uploads[token]; token == 0 || taken {
			continue
		}
		up := &upload{server: server, token: token, messages: make(chan *agentv1.SendLogsRequest), done: make(chan struct{})}
		a.uploads[token] = up
		return up
	}
}

func (a *Agents) closeUpload(up *upload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.uploads, up.token)
	close(up.done)
}

// uploadFor finds a logs call of a server, or nil.
func (a *Agents) uploadFor(server string, token uint64) *upload {
	a.mu.Lock()
	defer a.mu.Unlock()
	if up := a.uploads[token]; up != nil && up.server == server {
		return up
	}
	return nil
}

// Logs asks the agent to read logs, and passes each message of what it reads
// to each until it has read it all, or each fails, or ctx ends, which stops the
// reading on the agent at once. each may block: the agent drops what it can't
// send meanwhile, and says how much.
//
// It returns ErrLost when the stream ends first, and ErrNotConnected when the
// call never reached the agent. When the agent stopped it to move to a new stream,
// or moved before it got there, that is ErrMoving too, as the agent is on its way
// back. A reader carries on with the next stream.
func (s *Session) Logs(ctx context.Context, call *agentv1.LogsCall, each func(*agentv1.SendLogsRequest) error) error {
	up := s.agents.openUpload(s.server)
	defer s.agents.closeUpload(up)
	call.Stream = up.token
	c := &agentv1.Call{Call: &agentv1.Call_Logs{Logs: call}}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		if s.moving() {
			return s.stoppedToMove()
		}
		return s.notConnected()
	}
	s.next++
	c.Id = s.next
	result := make(chan *agentv1.CallResult, 1)
	s.pending[c.Id] = result
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		delete(s.pending, c.GetId())
		s.mu.Unlock()
		// Best effort: closing the upload stops it too.
		_ = s.write(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Cancel{Cancel: &agentv1.CancelCall{Id: c.GetId()}}})
	}
	if err := s.writeCall(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Call{Call: c}}); err != nil {
		s.mu.Lock()
		delete(s.pending, c.GetId())
		s.mu.Unlock()
		if errors.Is(err, ErrMoving) {
			return s.stoppedToMove()
		}
		return s.notConnected()
	}
	// The result and the stream's end can both be ready when the select wakes, as
	// each may take its time, and Go picks one at random. A result that is there
	// wins: the agent answered, and a read that finished hasn't lost its agent.
	var r *agentv1.CallResult
wait:
	for {
		select {
		case m := <-up.messages:
			if err := each(m); err != nil {
				cancel()
				return err
			}
		case r = <-result:
			break wait
		case <-s.done:
			select {
			case r = <-result:
				break wait
			default:
			}
			// An agent moves as soon as it hears the control plane has stopped
			// sending, which can be before a read it stopped has answered.
			if s.moving() {
				return s.stoppedToMove()
			}
			return fault.New(ErrLost, "%s's agent disconnected while it read logs", s.name)
		case <-ctx.Done():
			cancel()
			return ctx.Err()
		}
	}
	switch e := r.GetError(); {
	case e == nil:
		return nil
	case e.GetDraining():
		// It can say so before it says it is moving, so this goes by the answer.
		return s.stoppedToMove()
	default:
		return &resource.ServerError{Server: s.name, Err: errors.New(e.GetMessage())}
	}
}

// stoppedToMove is a logs call that didn't finish as the agent moves to a new stream.
func (s *Session) stoppedToMove() error {
	return fmt.Errorf("%w: %w", ErrMoving, s.notConnected())
}

// LogSources asks the agent which containers a service has logs from.
func (s *Session) LogSources(ctx context.Context, call *agentv1.LogSourcesCall) ([]*dockwellv1.LogSource, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_LogSources{LogSources: call}}, ObserveTimeout, nil)
	if err != nil {
		return nil, err
	}
	return r.GetLogSources().GetSources(), nil
}

// Containers asks the agent what Docker lists: every container, or the one named.
func (s *Session) Containers(ctx context.Context, container string) (*agentv1.ContainersResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Containers{Containers: &agentv1.ContainersCall{Container: container}}}, ObserveTimeout, nil)
	if err != nil {
		return nil, err
	}
	return r.GetContainers(), nil
}

// Forgotten tells a server which services' kept logs are out of reach.
func (s *Session) Forgotten(ctx context.Context, services []*agentv1.ForgottenService) error {
	_, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Forgotten{Forgotten: &agentv1.ForgottenCall{Services: services}}}, ObserveTimeout, nil)
	return err
}
