// SPDX-License-Identifier: AGPL-3.0-only

package agentapi_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
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
	docker, fw := container.NewFake(), firewall.NewFake()
	runAgent(t, s, id, resource.NewRegistry(&container.Driver{Runtime: docker, RecreateDowntime: 2 * time.Second}, &firewall.Driver{Backend: fw}))
	if kinds, _ := s.agents.Kinds(id.ServerID); len(kinds) != 2 || kinds[0] != container.Kind || kinds[1] != firewall.Kind {
		t.Fatalf("the agent said it has drivers for %v, want container and firewall.rule", kinds)
	}
	a := waitForAgent(t, s.agents, id.ServerID, true)

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
	if docker.Runs != 0 || fw.Len() != 0 {
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
	docker.Broken["nginx:broken"] = true
	broken := &resourcev1.Container{Image: "nginx:broken"}
	if err := (&container.Driver{}).Validate(broken); err != nil {
		t.Fatal(err)
	}
	steps, err = a.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: broken}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(t.Context(), steps[0], broken); err == nil || !strings.Contains(err.Error(), "isn't healthy") {
		t.Errorf("applying a broken image = %v, want the health check's failure", err)
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
	if err := <-errc; !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "didn't answer within") {
		t.Errorf("a call out of time = %v, want it to say the agent didn't answer in time", err)
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
	// The dropped session takes no more calls.
	if _, err := a.Plan(t.Context(), nil); !errors.Is(err, agentapi.ErrLost) {
		t.Errorf("a call on the old session = %v, want ErrLost", err)
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
