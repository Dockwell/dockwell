// SPDX-License-Identifier: AGPL-3.0-only

package agentapi_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
		if !errors.Is(err, agentapi.ErrLost) || !strings.Contains(err.Error(), "may or may not have run") {
			t.Errorf("a call on a dropped stream = %v, want ErrLost", err)
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
	if len(heard) == 0 || heard[0] != "pulling nginx:1.27" {
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
