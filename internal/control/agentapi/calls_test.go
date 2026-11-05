// SPDX-License-Identifier: AGPL-3.0-only

package agentapi_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/container/containertest"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// gate is a firewall whose rule "slow" takes until it is released, or its
// call is cancelled, to put in place.
type gate struct {
	*firewall.Fake
	started chan struct{}
	release chan struct{}

	mu     sync.Mutex
	gotErr error // why a slow put stopped, if it was stopped
}

func newGate() *gate {
	return &gate{Fake: firewall.NewFake(), started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (g *gate) Put(ctx context.Context, name string, rule *resourcev1.FirewallRule) error {
	if name == "slow" {
		g.started <- struct{}{}
		select {
		case <-g.release:
		case <-ctx.Done():
			g.mu.Lock()
			g.gotErr = ctx.Err()
			g.mu.Unlock()
			return ctx.Err()
		}
	}
	return g.Fake.Put(ctx, name, rule)
}

// forget lets go of why a slow put stopped, for the next one to say.
func (g *gate) forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gotErr = nil
}

func (g *gate) stopped() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gotErr
}

// runAgent runs an agent with drivers until the returned stop is called,
// and waits until the control plane can reach it.
func runAgent(t *testing.T, s *stack, id *enroll.Identity, drivers *resource.Registry) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = agent.Run(ctx, agent.Config{
			Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(),
			Collector: fakeCollector(), Drivers: drivers,
		})
		close(done)
	}()
	waitForAgent(t, s.agents, id.ServerID, true)
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func waitForAgent(t *testing.T, agents *agentapi.Agents, server string, connected bool) resource.Agent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a, ok := agents.Agent(server)
		if ok == connected {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s's agent to be connected: %t", server, connected)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The engine's calls reach the agent's own drivers over its stream: what
// it plans, applies and observes is what the drivers on the server do.
func TestCallsReachTheAgentsDrivers(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	rt, fw := containertest.NewFake(), firewall.NewFake()
	runAgent(t, s, id, resource.NewRegistry(&container.Driver{Runtime: rt, RecreateDowntime: 2 * time.Second}, &firewall.Driver{Backend: fw}))
	a := waitForAgent(t, s.agents, id.ServerID, true)
	// The agent said in its hello which kinds it has drivers for, and its
	// session refuses any other.
	_, err := a.Plan(t.Context(), []resource.Desired{{Ref: resource.Ref{Kind: service.Kind, Scope: id.ServerID, Name: "web"}}})
	if !errors.Is(err, resource.ErrNoDriver) || !strings.Contains(err.Error(), "(it has drivers for container, firewall.rule)") {
		t.Fatalf("planning a service = %v, want it refused, naming the container and firewall.rule drivers the agent has", err)
	}

	web := resource.Ref{Kind: container.Kind, Scope: id.ServerID, Name: "web"}
	spec := &resourcev1.Container{Image: "nginx:1.27", Env: map[string]string{"API_KEY": "s3cret-value"}}
	http := resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "http"}
	rule := &resourcev1.FirewallRule{Port: 80}
	// The engine sends specs as their kinds check and fill them in.
	if err := (&container.Driver{}).Validate(spec); err != nil {
		t.Fatal(err)
	}
	if err := (&firewall.Driver{}).Validate(rule); err != nil {
		t.Fatal(err)
	}
	steps, err := a.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}, {Ref: http, Spec: rule}})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].Ref != web || steps[0].Action != resource.ActionCreate || steps[1].Action != resource.ActionCreate {
		t.Fatalf("Plan = %+v, want two creates", steps)
	}
	for _, c := range steps[0].Changes {
		if c.Path == "env[API_KEY]" && (!c.Sensitive || c.To != "") {
			t.Errorf("the plan carries a secret: %+v", c)
		}
	}
	if rt.Runs != 0 || fw.Len() != 0 {
		t.Fatal("planning changed the server")
	}
	if err := a.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(t.Context(), steps[1], rule); err != nil {
		t.Fatal(err)
	}
	observed, err := a.Observe(t.Context(), []resource.Ref{web, http, {Kind: firewall.Kind, Scope: id.ServerID, Name: "nothing"}})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(observed[0], spec) || !proto.Equal(observed[1], rule) || observed[2] != nil {
		t.Errorf("Observe = %v, want what was applied, and nothing for a rule that isn't there", observed)
	}
	// Planning again finds nothing to do.
	if steps, err = a.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}}); err != nil || steps[0].Action != resource.ActionNone {
		t.Errorf("Plan after applying = %+v, %v, want nothing to do", steps, err)
	}
	// A step the driver can't carry out fails with the driver's reason.
	rt.Broken["nginx:broken"] = true
	broken := &resourcev1.Container{Image: "nginx:broken"}
	if err := (&container.Driver{}).Validate(broken); err != nil {
		t.Fatal(err)
	}
	steps, err = a.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: broken}})
	if err != nil {
		t.Fatal(err)
	}
	// The driver's reason comes back naming the server it failed on.
	err = a.Apply(t.Context(), steps[0], broken)
	var failed *resource.ServerError
	if !errors.As(err, &failed) || failed.Server != "hz-1" || !strings.HasPrefix(err.Error(), "hz-1: ") || !strings.Contains(err.Error(), "isn't healthy") {
		t.Errorf("applying a broken image = %v, want the health check's failure on hz-1", err)
	}
}

// blockingLogs follows until its reader leaves, as logs do: it never ends by itself.
type blockingLogs struct {
	calls atomic.Int32
	sent  chan struct{}
	after chan *agentv1.LogCursor
}

func (*blockingLogs) Sources(context.Context, *dockwellv1.ResourceRef, int64) ([]*dockwellv1.LogSource, error) {
	return nil, nil
}

func (*blockingLogs) Forgotten([]*agentv1.ForgottenService) {}

func (l *blockingLogs) Stream(ctx context.Context, call *agentv1.LogsCall, send func(*agentv1.SendLogsRequest) error) error {
	n := l.calls.Add(1)
	if n > 1 {
		for _, c := range call.GetAfter() {
			l.after <- c
		}
	}
	err := send(&agentv1.SendLogsRequest{Stream: call.GetStream(), Chunks: []*dockwellv1.LogChunk{{
		SourceId: "a", Lines: []*dockwellv1.LogLine{{Time: timestamppb.Now(), Text: fmt.Sprintf("line from call %d", n)}},
	}}})
	if err != nil {
		return err
	}
	l.sent <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

// A follow has no end, so it can't hold up moving to a renewed certificate, as a
// step under way does: it stops, and carries on over the agent's next stream.
func TestAFollowDoesntHoldUpMovingToANewStream(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	reader := &blockingLogs{sent: make(chan struct{}, 4), after: make(chan *agentv1.LogCursor, 4)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = agent.Run(ctx, agent.Config{
			Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(),
			Collector: fakeCollector(), Logs: reader,
		})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitForAgent(t, s.agents, id.ServerID, true)
	session, ok := s.agents.Session(id.ServerID)
	if !ok {
		t.Fatal("no session")
	}
	result := make(chan error, 1)
	got := make(chan string, 4)
	go func() {
		result <- session.Logs(t.Context(), &agentv1.LogsCall{Follow: true}, func(m *agentv1.SendLogsRequest) error {
			for _, c := range m.GetChunks() {
				for _, l := range c.GetLines() {
					got <- l.GetText()
				}
			}
			return nil
		})
	}()
	<-reader.sent
	if line := <-got; line != "line from call 1" {
		t.Fatalf("got %q", line)
	}

	// The certificate is renewed: the follow ends at once, as one that can go on.
	moved := time.Now()
	id.Replace(id.Key(), id.Cert())
	select {
	case err := <-result:
		if !errors.Is(err, resource.ErrNotConnected) {
			t.Fatalf("a follow as its agent moved = %v, want it to say it can carry on", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a follow held up moving to a new stream")
	}
	t.Logf("the follow ended %s after the certificate was renewed", time.Since(moved))
	waitUntil(t, "the agent's next stream", func() bool {
		next, ok := s.agents.Session(id.ServerID)
		return ok && next != session
	})
	next, err := s.agents.WaitSession(t.Context(), id.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, stop := context.WithCancel(t.Context())
	defer stop()
	second := make(chan error, 1)
	go func() {
		second <- next.Logs(ctx2, &agentv1.LogsCall{Follow: true}, func(m *agentv1.SendLogsRequest) error {
			for _, c := range m.GetChunks() {
				for _, l := range c.GetLines() {
					got <- l.GetText()
				}
			}
			return nil
		})
	}()
	if line := <-got; line != "line from call 2" {
		t.Fatalf("got %q on the next stream", line)
	}
	stop()
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Errorf("a follow whose reader left = %v", err)
	}
}

// A logs call's lines come only to the server it was asked of: another server's
// agent can't send lines into it, by guessing its stream or otherwise.
func TestLogLinesComeOnlyFromTheServerAsked(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	other := s.enrollLocal(t)
	client, release, err := other.Client()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stream := client.SendLogs(t.Context())
	if err := stream.Send(&agentv1.SendLogsRequest{Stream: 12345}); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	_, err = stream.CloseAndReceive()
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("lines nobody asked for = %v, want them refused", err)
	}
	_ = id
}

// An agent moving to a renewed certificate lets the calls under way finish,
// and refuses the ones that arrive meanwhile. The control plane sends a
// refused call again on the agent's next stream, so it succeeds rather than
// failing.
func TestACallRefusedWhileTheAgentMovesIsSentAgain(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	fw := newGate()
	runAgent(t, s, id, resource.NewRegistry(&firewall.Driver{Backend: fw}))
	a := waitForAgent(t, s.agents, id.ServerID, true)
	slow := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "slow"}, Decision: resource.Decision{Action: resource.ActionCreate}}
	rule := &resourcev1.FirewallRule{Port: 8080}
	if err := (&firewall.Driver{}).Validate(rule); err != nil {
		t.Fatal(err)
	}
	applied := make(chan error, 1)
	go func() { applied <- a.Apply(t.Context(), slow, rule) }()
	<-fw.started

	id.Replace(id.Key(), id.Cert()) // renewed: the agent drains its stream
	time.Sleep(50 * time.Millisecond)
	type planned struct {
		steps []resource.Step
		err   error
	}
	plans := make(chan planned, 1)
	go func() {
		steps, err := a.Plan(t.Context(), []resource.Desired{{Ref: resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "http"}, Spec: rule}})
		plans <- planned{steps, err}
	}()
	select {
	case p := <-plans:
		t.Fatalf("a plan sent while the agent drains ended with %+v, %v before the call under way did; want it refused, and sent again", p.steps, p.err)
	case <-time.After(300 * time.Millisecond):
	}

	close(fw.release)
	if err := <-applied; err != nil {
		t.Errorf("the call under way as the agent moved = %v, want it to finish", err)
	}
	select {
	case p := <-plans:
		if p.err != nil || len(p.steps) != 1 || p.steps[0].Action != resource.ActionCreate {
			t.Errorf("the plan sent again = %+v, %v, want one create", p.steps, p.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refused plan wasn't sent again on the agent's next stream")
	}
	waitUntil(t, "the agent is on its next stream", func() bool {
		next, ok := s.agents.Agent(id.ServerID)
		return ok && next != a
	})
	// A call that reaches the stream the agent left goes to its new one.
	if _, err := a.Plan(t.Context(), nil); err != nil {
		t.Errorf("a call on the stream the agent moved from = %v, want it sent on the new one", err)
	}
}

// fixedValues is a keeper that gives every step the same values.
type fixedValues map[string]string

func (v fixedValues) Values(context.Context, string, resource.Ref, proto.Message) (map[string]string, error) {
	return maps.Clone(v), nil
}

// Values are sealed to one stream's delivery key, which the agent drops
// when it moves to a new stream. A step that uses a secret, refused while
// the agent moves to a renewed certificate and sent again on its next
// stream, carries its values sealed again for that stream, so it runs
// rather than failing and rolling its change back.
func TestValuesAreSealedAgainForTheStreamACallMovesTo(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	fw, engine := newGate(), containertest.NewFake()
	s.agents.SendValues(fixedValues{"TOKEN": "s3cret-on-the-next-stream"})
	runAgent(t, s, id, resource.NewRegistry(&firewall.Driver{Backend: fw}, &service.Driver{Containers: container.Driver{Runtime: engine}}))
	a := waitForAgent(t, s.agents, id.ServerID, true)
	slow := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "slow"}, Decision: resource.Decision{Action: resource.ActionCreate}}
	rule := &resourcev1.FirewallRule{Port: 8080}
	if err := (&firewall.Driver{}).Validate(rule); err != nil {
		t.Fatal(err)
	}
	ruled := make(chan error, 1)
	go func() { ruled <- a.Apply(t.Context(), slow, rule) }()
	<-fw.started

	id.Replace(id.Key(), id.Cert()) // renewed: the agent drains its stream
	time.Sleep(50 * time.Millisecond)
	web := resource.Ref{Kind: service.Kind, Scope: "env_01JD6Z3R8M9QF2K7T4XW5BNC0V", Name: "web"}
	svc := &resourcev1.Service{
		Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/web:1"}},
		Secrets:   map[string]string{"TOKEN": "API_TOKEN"},
		Revisions: map[string]int64{"TOKEN": 1},
	}
	if err := (&service.Driver{}).Validate(svc); err != nil {
		t.Fatal(err)
	}
	applied := make(chan error, 1)
	go func() {
		applied <- a.Apply(t.Context(), resource.Step{Ref: web, Decision: resource.Decision{Action: resource.ActionCreate}}, svc)
	}()
	select {
	case err := <-applied:
		t.Fatalf("a step sent while the agent drains ended with %v before the call under way did; want it refused, and sent again", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(fw.release)
	if err := <-ruled; err != nil {
		t.Errorf("the call under way as the agent moved = %v, want it to finish", err)
	}
	select {
	case err := <-applied:
		if err != nil {
			t.Fatalf("the step sent again on the agent's next stream = %v, want it to run with its values", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refused step wasn't sent again on the agent's next stream")
	}
	got, err := engine.Inspect(t.Context(), service.Target(web))
	if err != nil || got.GetEnv()["TOKEN"] != "s3cret-on-the-next-stream" {
		t.Errorf("the service's container runs with %v, %v; want the secret it uses", got.GetEnv(), err)
	}
}

// An agent reports the kinds it has drivers for, and a plan for any other
// kind fails at planning, saying so, without reaching the agent.
func TestAKindTheAgentCantRunFailsAtPlanning(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	runAgent(t, s, id, nil)
	a := waitForAgent(t, s.agents, id.ServerID, true)
	web := resource.Ref{Kind: container.Kind, Scope: id.ServerID, Name: "web"}
	_, err := a.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: &resourcev1.Container{Image: "nginx:1.27"}}})
	if !errors.Is(err, resource.ErrNoDriver) || !strings.Contains(err.Error(), "the agent on hz-1 has no driver for container resources") {
		t.Errorf("planning a container on an agent with no drivers = %v, want it refused for want of a driver", err)
	}
	if err := a.Apply(t.Context(), resource.Step{Ref: web, Decision: resource.Decision{Action: resource.ActionCreate}}, nil); !errors.Is(err, resource.ErrNoDriver) {
		t.Errorf("applying = %v, want it refused for want of a driver", err)
	}
}

// A caller that gives up cancels its call on the agent, and a call that
// runs out of time ends there too.
func TestCallsAreCancelledOnTheAgent(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	fw := newGate()
	runAgent(t, s, id, resource.NewRegistry(&firewall.Driver{Backend: fw}))
	a := waitForAgent(t, s.agents, id.ServerID, true)
	slow := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "slow"}, Decision: resource.Decision{Action: resource.ActionCreate}}
	rule := &resourcev1.FirewallRule{Port: 8080}

	// Cancelled by the caller, long before the call's own time runs out.
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- a.Apply(ctx, slow, rule) }()
	<-fw.started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled call = %v, want context.Canceled", err)
	}
	waitUntil(t, "the agent stops the cancelled call", func() bool { return errors.Is(fw.stopped(), context.Canceled) })

	// Out of time: the agent gets the time that is left, and stops there.
	fw.forget()
	ctx, cancel = context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	go func() { errc <- a.Apply(ctx, slow, rule) }()
	<-fw.started
	// The agent has the same time, so either side can say so first.
	if err := <-errc; err == nil || !strings.Contains(err.Error(), "didn't answer within") && !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("a call out of time = %v, want it to say it ran out of time", err)
	}
	waitUntil(t, "the agent stops the call that ran out of time", func() bool { return fw.stopped() != nil })
	if fw.Len() != 0 {
		t.Error("a stopped call still put its rule in place")
	}
}

// A stream that drops fails the calls waiting on it at once, saying the
// step may or may not have run; once the agent reconnects, calls reach it
// again.
// A call whose time runs out on the agent, whose answer may beat the control
// plane's own deadline, is still a call that ran out of time, with its words.
func TestTimeRunningOutOnTheAgentCountsAsTime(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	runAgent(t, s, id, resource.NewRegistry(&firewall.Driver{Backend: agentOutOfTime{Fake: firewall.NewFake()}}))
	a := waitForAgent(t, s.agents, id.ServerID, true)
	step := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "http"}, Decision: resource.Decision{Action: resource.ActionCreate}}
	err := a.Apply(t.Context(), step, &resourcev1.FirewallRule{Port: 80})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "didn't answer within") {
		t.Errorf("err = %v, want it to say the agent didn't answer in time", err)
	}
	if plain, _ := resource.Explain(err); !strings.HasPrefix(plain, "The step's time ran out") {
		t.Errorf("it reads %q", plain)
	}
}

// agentOutOfTime is a firewall whose call runs out of time on the agent, with
// the control plane's own deadline still some way off.
type agentOutOfTime struct{ *firewall.Fake }

func (agentOutOfTime) Put(context.Context, string, *resourcev1.FirewallRule) error {
	return context.DeadlineExceeded
}

func TestAFailureComesOverTheStreamInPlainWords(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	runAgent(t, s, id, resource.NewRegistry(&firewall.Driver{Backend: plainFailing{Fake: firewall.NewFake()}}))
	a := waitForAgent(t, s.agents, id.ServerID, true)
	step := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "http"}, Decision: resource.Decision{Action: resource.ActionCreate}}
	err := a.Apply(t.Context(), step, &resourcev1.FirewallRule{Port: 80})
	plain, detail := resource.Explain(err)
	if plain != "Port 80 can't open. Check the firewall, then try again." {
		t.Errorf("plain = %q", plain)
	}
	if !strings.Contains(detail, "iptables said no") {
		t.Errorf("detail = %q, want what the agent said", detail)
	}
	if got := resource.ReasonOf(err); got != resource.ReasonPort {
		t.Errorf("it comes down to %q, want what the agent said it does", got)
	}
}

// plainFailing is a firewall that fails each rule, in plain words.
type plainFailing struct{ *firewall.Fake }

func (plainFailing) Put(context.Context, string, *resourcev1.FirewallRule) error {
	return resource.PlainlyBecause(resource.ReasonPort, "Port 80 can't open. Check the firewall, then try again.", errors.New("iptables said no"))
}

func TestAStreamThatDropsFailsItsCalls(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	fw := newGate()
	stop := runAgent(t, s, id, resource.NewRegistry(&firewall.Driver{Backend: fw}))
	a := waitForAgent(t, s.agents, id.ServerID, true)
	slow := resource.Step{Ref: resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "slow"}, Decision: resource.Decision{Action: resource.ActionCreate}}
	errc := make(chan error, 1)
	go func() { errc <- a.Apply(context.Background(), slow, &resourcev1.FirewallRule{Port: 8080}) }()
	<-fw.started
	stop()
	select {
	case err := <-errc:
		if !errors.Is(err, agentapi.ErrLost) {
			t.Errorf("a call on a dropped stream = %v, want ErrLost", err)
		}
		// What can't be known, in words that end, and nothing of what comes next: the engine says that.
		plain, _ := resource.Explain(err)
		if want := "hz-1 went offline during this step, so Dockwell can't tell whether it ran. Check that hz-1 is up."; plain != want {
			t.Errorf("a call on a dropped stream reads %q, want %q", plain, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call on a dropped stream is still waiting")
	}
	waitForAgent(t, s.agents, id.ServerID, false)
	// The dropped session takes no more calls, and says they never reached
	// the agent.
	if _, err := a.Plan(t.Context(), nil); !errors.Is(err, resource.ErrNotConnected) || !strings.Contains(err.Error(), "didn't run") {
		t.Errorf("a call on the old session = %v, want ErrNotConnected", err)
	}

	// The agent comes back, with its drivers, on a new stream.
	runAgent(t, s, id, resource.NewRegistry(&firewall.Driver{Backend: fw}))
	a = waitForAgent(t, s.agents, id.ServerID, true)
	steps, err := a.Plan(t.Context(), []resource.Desired{{Ref: resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "http"}, Spec: &resourcev1.FirewallRule{Port: 80}}})
	if err != nil || len(steps) != 1 || steps[0].Action != resource.ActionCreate {
		t.Errorf("Plan after reconnecting = %+v, %v", steps, err)
	}
}

// Each connection is announced, so what waits for a server can resume.
func TestAgentsAnnounceConnections(t *testing.T) {
	s := startStack(t, 5*time.Second)
	connected := make(chan string, 4)
	s.agents.OnConnect(func(server string) { connected <- server })
	id := s.enrollLocal(t)
	stop := runAgent(t, s, id, nil)
	if got := <-connected; got != id.ServerID {
		t.Errorf("OnConnect got %s, want %s", got, id.ServerID)
	}
	stop()
	waitForAgent(t, s.agents, id.ServerID, false)
	runAgent(t, s, id, nil)
	select {
	case got := <-connected:
		if got != id.ServerID {
			t.Errorf("OnConnect got %s on reconnecting, want %s", got, id.ServerID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a reconnect wasn't announced")
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A step the control plane can't read, as from a newer agent, fails
// planning, naming the server and saying to upgrade the control plane. Read
// as anything else, it would be skipped or misstated.
func TestAStepANewerAgentPlansFailsPlanning(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	stream := openStream(t, id, string(firewall.Kind))
	a := waitForAgent(t, s.agents, id.ServerID, true)
	http := resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "http"}
	planned := make(chan error, 1)
	go func() {
		_, err := a.Plan(t.Context(), []resource.Desired{{Ref: http, Spec: &resourcev1.FirewallRule{Port: 80}}})
		planned <- err
	}()
	msg, err := stream.Receive()
	if err != nil || msg.GetCall().GetPlan() == nil {
		t.Fatalf("the control plane sent %v, %v, want a plan", msg, err)
	}
	newer := &dockwellv1.PlanStep{Resource: resourcepb.Ref(http), Action: 42, Effect: dockwellv1.StepEffect_STEP_EFFECT_NONE}
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Result{Result: &agentv1.CallResult{
		Id: msg.GetCall().GetId(), Result: &agentv1.CallResult_Plan{Plan: &agentv1.PlanResult{Steps: []*dockwellv1.PlanStep{newer}}},
	}}}); err != nil {
		t.Fatal(err)
	}
	err = <-planned
	var unknown *resourcepb.UnknownError
	if err == nil || !errors.As(err, &unknown) || !strings.HasPrefix(err.Error(), "hz-1's agent planned a step for firewall.rule http") ||
		!strings.Contains(err.Error(), "upgrade the control plane") {
		t.Errorf("a plan with action 42 = %v, want it to fail, naming hz-1 and saying to upgrade the control plane", err)
	}
}

// reporter is a server's routes' domains, which a test changes.
type reporter struct {
	mu      sync.Mutex
	list    []*dockwellv1.DomainStatus
	ingress *dockwellv1.IngressStatus
	changed chan struct{}
}

func (r *reporter) Ingress() *dockwellv1.IngressStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ingress
}

func (r *reporter) setIngress(st *dockwellv1.IngressStatus) {
	r.mu.Lock()
	r.ingress = st
	r.mu.Unlock()
	r.changed <- struct{}{}
}

func (r *reporter) Domains() []*dockwellv1.DomainStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.list
}

func (r *reporter) DomainsChanged() <-chan struct{} { return r.changed }

func (r *reporter) set(list ...*dockwellv1.DomainStatus) {
	r.mu.Lock()
	r.list = list
	r.mu.Unlock()
	r.changed <- struct{}{}
}

// Where a route's certificate stands reaches the control plane as the stream
// opens and as it changes, and stays once the agent has gone.
func TestAnAgentReportsItsDomains(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	issued := dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED
	r := &reporter{changed: make(chan struct{}, 1), list: []*dockwellv1.DomainStatus{{
		Domain: "app.example.com", Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_PENDING},
	}}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = agent.Run(ctx, agent.Config{
			Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(),
			Collector: fakeCollector(), Domains: r,
		})
		close(done)
	}()
	waitForAgent(t, s.agents, id.ServerID, true)
	state := func() (dockwellv1.CertificateState, bool) {
		st, connected := s.agents.Domain(id.ServerID, "app.example.com")
		return st.GetCertificate().GetState(), connected
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	eventually("the first report never came", func() bool {
		got, _ := state()
		return got == dockwellv1.CertificateState_CERTIFICATE_STATE_PENDING
	})
	r.set(&dockwellv1.DomainStatus{Domain: "app.example.com", Certificate: &dockwellv1.Certificate{State: issued}})
	eventually("the change never came", func() bool {
		got, _ := state()
		return got == issued
	})
	// So does where its ingress stands, as a certificate alone doesn't say whether
	// a domain answers.
	r.setIngress(&dockwellv1.IngressStatus{Proxy: "Caddy", Problem: dockwellv1.IngressProblem_INGRESS_PROBLEM_PORTS_TAKEN, Holders: []string{"openresty"}})
	eventually("the ingress's state never came", func() bool {
		return s.agents.Ingress(id.ServerID).GetProblem() == dockwellv1.IngressProblem_INGRESS_PROBLEM_PORTS_TAKEN
	})
	eventually("the server never said it", func() bool {
		got, err := s.hub.Get(id.ServerID)
		return err == nil && got.GetIngress().GetHolders() != nil && got.GetIngress().GetHolders()[0] == "openresty"
	})
	cancel()
	<-done
	eventually("the agent never read as gone", func() bool {
		_, connected := state()
		return !connected
	})
	if got, _ := state(); got != issued {
		t.Errorf("with the agent gone, it reads %s, want what it last said", got)
	}
	if st, _ := s.agents.Domain(id.ServerID, "other.example.com"); st != nil {
		t.Errorf("a domain it never said reads %v", st)
	}
}

// An agent offers the Docker kinds only while Docker answers. Until then,
// planning one says why, and how to fix it; once Docker starts, the agent
// tells the control plane, and its kinds run. What waited for them, such
// as a rollback, goes on then, as when the agent connects.
func TestDockerKindsWaitForDocker(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	sock := filepath.Join(t.TempDir(), "docker.sock")
	client := docker.New(sock)
	containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond}}
	drivers := resource.NewRegistry(&containers, &service.Driver{Containers: containers}, &firewall.Driver{Backend: firewall.NewFake()})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = agent.Run(ctx, agent.Config{
			Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(),
			Collector: fakeCollector(), Drivers: drivers, KindsEvery: 20 * time.Millisecond,
		})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	a := waitForAgent(t, s.agents, id.ServerID, true)
	connected := make(chan string, 16)
	s.agents.OnConnect(func(server string) { connected <- server })

	web := resource.Ref{Kind: service.Kind, Scope: "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M", Name: "web"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}}
	if err := (&service.Driver{}).Validate(spec); err != nil {
		t.Fatal(err)
	}
	_, err := a.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}})
	if !errors.Is(err, resource.ErrNoDriver) || !strings.Contains(err.Error(), "can't run service web now: Docker isn't installed, or its socket isn't at "+sock+". Install Docker Engine") {
		t.Fatalf("planning a service without Docker = %v, want it refused, saying Docker isn't installed and how to install it", err)
	}
	// The firewall's driver needs no Docker, so the agent offers it all along.
	rule := &resourcev1.FirewallRule{Port: 80}
	_ = (&firewall.Driver{}).Validate(rule)
	if _, err := a.Plan(t.Context(), []resource.Desired{{Ref: resource.Ref{Kind: firewall.Kind, Scope: id.ServerID, Name: "http"}, Spec: rule}}); err != nil {
		t.Errorf("planning a firewall rule without Docker = %v", err)
	}

	// Docker starts.
	e := dockertest.New(t)
	e.Registry["nginx:1.27"] = dockertest.Image{Size: 8 << 20, Config: docker.Config{Cmd: []string{"nginx"}}}
	if err := os.Symlink(e.Socket, sock); err != nil {
		t.Fatal(err)
	}
	var steps []resource.Step
	deadline := time.Now().Add(5 * time.Second)
	for {
		if steps, err = a.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}}); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("planning once Docker answers = %v", err)
	}
	select {
	case got := <-connected:
		if got != id.ServerID {
			t.Errorf("OnConnect got %s once Docker started, want %s", got, id.ServerID)
		}
	case <-time.After(5 * time.Second):
		t.Error("the agent can run the Docker kinds now, and nothing that waited for them heard so")
	}

	// Pulling the image says how far it is, over the stream, to whoever
	// follows the step.
	var mu sync.Mutex
	var heard []string
	follow := resource.WithProgress(t.Context(), func(m string) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, m)
	})
	if err := a.Apply(follow, steps[0], spec); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) == 0 || heard[0] != "Pulling nginx:1.27" {
		t.Errorf("heard %q while the agent applied, want what it pulled", heard)
	}
	if c := e.Container(service.ContainerName(web)); c == nil || !c.State.Running {
		t.Errorf("the service's container isn't running on Docker: %+v", c)
	}
}

// alternating is a firewall driver whose every other check hangs until it
// is given up on, as a Docker that takes the connection and says nothing:
// the check before each hello answers, and the next, in the stream's
// loop, hangs.
type alternating struct {
	firewall.Driver
	checks atomic.Int32
}

func (a *alternating) Ready(ctx context.Context) error {
	if a.checks.Add(1)%2 == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// A check of which kinds the agent can run that hangs never holds up the
// stream: the agent still hears it end, and reconnects within its backoff
// rather than once the check gives up, 5 s later.
func TestAHungCheckDoesntHoldTheStream(t *testing.T) {
	// The control plane closes a stream silent for 300 ms, and an agent
	// sends stats every 5 s, so each stream ends 300 ms in.
	s := startStack(t, 300*time.Millisecond)
	connected := make(chan string, 16)
	s.agents.OnConnect(func(server string) { connected <- server })
	id := s.enrollLocal(t)
	drivers := resource.NewRegistry(&alternating{Driver: firewall.Driver{Backend: firewall.NewFake()}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = agent.Run(ctx, agent.Config{
			Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(),
			Collector: fakeCollector(), Drivers: drivers, KindsEvery: 20 * time.Millisecond,
		})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	for i, within := range []time.Duration{5 * time.Second, 3 * time.Second} {
		select {
		case <-connected:
		case <-time.After(within):
			t.Fatalf("connection %d didn't come within %s: a check that hung held up the stream", i+1, within)
		}
	}
}

// A field the schema marks sensitive never shows its value in a plan,
// whatever the agent says of it: an agent that reports a container's
// environment unmasked, as a buggy or older one might, has it masked.
func TestAnAgentCantPutASecretInAPlan(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	stream := openStream(t, id, string(container.Kind))
	a := waitForAgent(t, s.agents, id.ServerID, true)
	web := resource.Ref{Kind: container.Kind, Scope: id.ServerID, Name: "web"}
	type planned struct {
		steps []resource.Step
		err   error
	}
	done := make(chan planned, 1)
	go func() {
		steps, err := a.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: &resourcev1.Container{Image: "nginx:1.28"}}})
		done <- planned{steps, err}
	}()
	msg, err := stream.Receive()
	if err != nil || msg.GetCall().GetPlan() == nil {
		t.Fatalf("the control plane sent %v, %v, want a plan", msg, err)
	}
	leaky := &dockwellv1.PlanStep{
		Resource: resourcepb.Ref(web), Action: dockwellv1.PlanAction_PLAN_ACTION_REPLACE, Effect: dockwellv1.StepEffect_STEP_EFFECT_RECREATE,
		Changes: []*dockwellv1.FieldChange{
			{Path: "env[API_KEY]", Op: dockwellv1.FieldOp_FIELD_OP_MODIFIED, From: "old-s3cret", To: "new-s3cret"},
			{Path: "image", Op: dockwellv1.FieldOp_FIELD_OP_MODIFIED, From: "nginx:1.27", To: "nginx:1.28"},
			{Path: "labels[traefik.enable]", Op: dockwellv1.FieldOp_FIELD_OP_MODIFIED, From: "false", To: "true"},
		},
	}
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Result{Result: &agentv1.CallResult{
		Id: msg.GetCall().GetId(), Result: &agentv1.CallResult_Plan{Plan: &agentv1.PlanResult{Steps: []*dockwellv1.PlanStep{leaky}}},
	}}}); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || len(got.steps) != 1 || len(got.steps[0].Changes) != 3 {
		t.Fatalf("Plan = %+v, %v", got.steps, got.err)
	}
	env, image, label := got.steps[0].Changes[0], got.steps[0].Changes[1], got.steps[0].Changes[2]
	if !env.Sensitive || env.From != "" || env.To != "" {
		t.Errorf("the environment's change = %+v, want it masked", env)
	}
	if image.Sensitive || image.To != "nginx:1.28" {
		t.Errorf("the image's change = %+v, want it shown", image)
	}
	// A label's key holds dots, and is still a label's: not sensitive.
	if label.Sensitive || label.To != "true" {
		t.Errorf("the label's change = %+v, want it shown", label)
	}
}

// A change an agent reports at a path that names a whole message, or a
// whole map, is masked when anything within it is sensitive: a service's
// app holds its container's environment. The Docker driver reports such
// steps.
func TestAnAgentCantPutASecretInAPlanAtAWholeMessage(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	stream := openStream(t, id, string(container.Kind), string(service.Kind))
	a := waitForAgent(t, s.agents, id.ServerID, true)
	for _, c := range []struct {
		ref           resource.Ref
		spec          proto.Message
		masked, shown string
	}{
		{resource.Ref{Kind: service.Kind, Scope: id.ServerID, Name: "api"}, &resourcev1.Service{}, "app", "secrets[DB]"},
		{resource.Ref{Kind: container.Kind, Scope: id.ServerID, Name: "web"}, &resourcev1.Container{Image: "nginx:1.28"}, "env", "ports"},
	} {
		done := make(chan []resource.Step, 1)
		go func() {
			steps, err := a.Plan(t.Context(), []resource.Desired{{Ref: c.ref, Spec: c.spec}})
			if err != nil {
				t.Error(err)
			}
			done <- steps
		}()
		msg, err := stream.Receive()
		if err != nil || msg.GetCall().GetPlan() == nil {
			t.Fatalf("the control plane sent %v, %v, want a plan", msg, err)
		}
		leaky := &dockwellv1.PlanStep{
			Resource: resourcepb.Ref(c.ref), Action: dockwellv1.PlanAction_PLAN_ACTION_REPLACE, Effect: dockwellv1.StepEffect_STEP_EFFECT_RECREATE,
			Changes: []*dockwellv1.FieldChange{
				{Path: c.masked, Op: dockwellv1.FieldOp_FIELD_OP_MODIFIED, From: "Container{env: API_KEY=old-s3cret}", To: "Container{env: API_KEY=new-s3cret}"},
				{Path: c.shown, Op: dockwellv1.FieldOp_FIELD_OP_ADDED, To: "DB"},
			},
		}
		if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Result{Result: &agentv1.CallResult{
			Id: msg.GetCall().GetId(), Result: &agentv1.CallResult_Plan{Plan: &agentv1.PlanResult{Steps: []*dockwellv1.PlanStep{leaky}}},
		}}}); err != nil {
			t.Fatal(err)
		}
		steps := <-done
		if len(steps) != 1 || len(steps[0].Changes) != 2 {
			t.Fatalf("%s: Plan = %+v", c.ref.Kind, steps)
		}
		if m := steps[0].Changes[0]; !m.Sensitive || m.From != "" || m.To != "" {
			t.Errorf("%s: the change at %s = %+v, want it masked", c.ref.Kind, c.masked, m)
		}
		if s := steps[0].Changes[1]; s.Sensitive || s.To != "DB" {
			t.Errorf("%s: the change at %s = %+v, want it shown", c.ref.Kind, c.shown, s)
		}
	}
}
