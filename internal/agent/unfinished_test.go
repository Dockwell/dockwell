// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

const rolloutEnv = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"

// dockerWrites is every write to Docker, in order, and holds each stop as Docker
// does: until the process exits, whoever asked and whether they're still there.
type dockerWrites struct {
	mu    sync.Mutex
	list  []string
	stops atomic.Int32
	exit  chan struct{} // closed once what's stopping has stopped
	ended sync.Once
}

func (w *dockerWrites) intercept(r *http.Request) error {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil
	}
	w.mu.Lock()
	w.list = append(w.list, r.Method+" "+r.URL.Path)
	w.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/stop") {
		w.stops.Add(1)
		<-w.exit
		// Every stop waiting here waits for the same exit, so once it comes, one whose
		// caller has gone has nothing left to do. Letting it reach the engine would
		// stop the container again, after the cleanup had started it.
		if err := r.Context().Err(); err != nil {
			return err
		}
	}
	return nil
}

func (w *dockerWrites) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.list, "\n")
}

func (w *dockerWrites) finishStopping() { w.ended.Do(func() { close(w.exit) }) }

// rollout is a service with a volume on a Docker of its own, so a deploy of it stops
// the old container before it starts the new one.
type rollout struct {
	engine *dockertest.Engine
	local  resource.Local
	cfg    Config
	ref    resource.Ref
	was    *resourcev1.Service
	now    *resourcev1.Service
	writes *dockerWrites
	log    *syncWriter
	logged *strings.Builder
	// at is where a test holds the step, if anywhere.
	at atomic.Pointer[func(string)]
}

func serviceSpec(t *testing.T, db string) *resourcev1.Service {
	t.Helper()
	s := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17", Env: map[string]string{"POSTGRES_DB": db}, StopGracePeriod: "2m"}}}
	if err := container.Validate(s.GetApp()); err != nil {
		t.Fatal(err)
	}
	return s
}

func newRollout(t *testing.T, grace time.Duration) *rollout {
	t.Helper()
	e := dockertest.New(t)
	e.Registry["postgres:17"] = dockertest.Image{Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{
		Cmd: []string{"postgres"}, Entrypoint: []string{"docker-entrypoint.sh"}, Env: []string{"PGDATA=/var/lib/postgresql/data"}, User: "postgres", WorkingDir: "/",
	}}
	b := &rollout{engine: e, writes: &dockerWrites{exit: make(chan struct{})}, logged: &strings.Builder{}}
	kept := &volume.Kept{}
	rt := &container.Docker{
		Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second, Deleted: kept,
		Points: func(name string) {
			if at := b.at.Load(); at != nil {
				(*at)(name)
			}
		},
	}
	c := container.Driver{Runtime: rt}
	reg := resource.NewRegistry(&c, &service.Driver{Containers: c}, &volume.Driver{Runtime: &volume.Docker{Client: rt.Client, Kept: kept}})
	b.local = resource.Local{Drivers: reg}
	b.ref = resource.Ref{Kind: service.Kind, Scope: rolloutEnv, Name: "db"}
	b.was, b.now = serviceSpec(t, "app"), serviceSpec(t, "shop")
	b.log = &syncWriter{w: b.logged}
	b.cfg = Config{Logger: slog.New(slog.NewTextHandler(b.log, nil)), Drivers: reg, CallGrace: grace, unfinished: newUnfinished()}
	b.apply(t, b.ref, b.was)
	e.Intercept(b.writes.intercept)
	t.Cleanup(func() { e.Intercept(nil); b.writes.finishStopping() })
	return b
}

// apply puts spec on the server, leaving no call or stream behind it.
func (b *rollout) apply(t *testing.T, ref resource.Ref, spec proto.Message) {
	t.Helper()
	steps, err := b.local.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.local.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatal(err)
	}
}

// deployCall is the step that takes the service to now, from a plan.
func (b *rollout) deployCall(t *testing.T, id uint64, spec proto.Message, rollback bool) *agentv1.Call {
	t.Helper()
	steps, err := b.local.Plan(t.Context(), []resource.Desired{{Ref: b.ref, Spec: spec, Rollback: rollback}})
	if err != nil {
		t.Fatal(err)
	}
	return stepCall(t, id, resourcepb.Step(steps[0], ""), b.ref, spec)
}

func stepCall(t *testing.T, id uint64, step *dockwellv1.PlanStep, ref resource.Ref, spec proto.Message) *agentv1.Call {
	t.Helper()
	encoded, err := resource.Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	return &agentv1.Call{Id: id, Timeout: durationpb.New(10 * time.Minute), Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{
		Step: step, Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(ref), Spec: encoded},
	}}}
}

func planCall(t *testing.T, id uint64, ref resource.Ref, spec proto.Message) *agentv1.Call {
	t.Helper()
	encoded, err := resource.Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	return &agentv1.Call{Id: id, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Plan{Plan: &agentv1.PlanCall{
		Desired: []*agentv1.DesiredResource{{Resource: resourcepb.Ref(ref), Spec: encoded}},
	}}}
}

// stream is a stream's calls, and what they sent.
func (b *rollout) stream() (*calls, *sent) {
	s := &sent{results: map[uint64]*agentv1.CallResult{}}
	return newCalls(context.Background(), b.cfg, s.send, nil), s
}

// waitFor waits for something that happens on another goroutine.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func answerTo(t *testing.T, s *sent, id uint64) *agentv1.CallResult {
	t.Helper()
	waitFor(t, "an answer", func() bool { return s.result(id) != nil })
	return s.result(id)
}

func (u *unfinished) stepsLeft() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.steps)
}

// A stop-first deploy whose stream drops while Docker stops the old container puts
// that container back, which takes as long as stopping it does: minutes, on a real
// server, where the agent's grace is 30 s. Its cleanup is healthy, so it isn't left
// behind, and the agent is back regardless. The rollback that follows is refused
// until the cleanup ends, as it would stop, start and rename the same container.
func TestASlowCleanupIsWaitedForAndOnlyItsResourceWaits(t *testing.T) {
	const grace = 30 * time.Millisecond
	b := newRollout(t, grace)
	first, _ := b.stream()
	first.start(b.deployCall(t, 1, b.now, false))
	waitFor(t, "Docker to be stopping the old one", func() bool { return b.writes.stops.Load() >= 1 })

	// The stream drops, and the cleanup outlasts the grace many times over.
	first.close()
	if !stops(first) {
		t.Fatal("a cleanup that takes its time kept the agent offline")
	}
	waitFor(t, "the old one to be stopped again, to bring it back", func() bool { return b.writes.stops.Load() >= 2 })
	time.Sleep(10 * grace)
	if n := b.cfg.unfinished.left.Load(); n != 0 || strings.Contains(b.logged.String(), "left it behind") {
		t.Fatalf("a healthy cleanup was treated as stuck: %d left behind, and the log says: %s", n, b.logged.String())
	}

	// The agent is back. Resume plans the rollback, and goes on to apply it.
	second, s := b.stream()
	defer second.stop()
	second.start(planCall(t, 2, b.ref, b.was))
	refused := answerTo(t, s, 2).GetError()
	if !refused.GetUnfinished() || !strings.Contains(refused.GetMessage(), "the last step on db hasn't ended") {
		t.Fatalf("a plan beside a step that hasn't ended: %v", s.result(2))
	}
	second.start(b.deployCall(t, 3, b.was, true))
	if got := answerTo(t, s, 3).GetError(); !got.GetUnfinished() {
		t.Fatalf("an apply beside a step that hasn't ended: %v", s.result(3))
	}
	if n := b.writes.stops.Load(); n != 2 {
		t.Fatalf("Docker heard %d stops, so something ran beside the cleanup:\n%s", n, b.writes.text())
	}

	// Anything else on the server carries on.
	web := resource.Ref{Kind: service.Kind, Scope: rolloutEnv, Name: "web"}
	spec := serviceSpec(t, "web")
	second.start(planCall(t, 4, web, spec))
	planned := answerTo(t, s, 4)
	if planned.GetError() != nil {
		t.Fatalf("another resource waits too: %v", planned)
	}
	second.start(stepCall(t, 5, planned.GetPlan().GetSteps()[0], web, spec))
	if got := answerTo(t, s, 5); got.GetError() != nil {
		t.Fatalf("another resource waits too: %v", got)
	}

	// Once Docker is done the old one runs again, and the rollback finds nothing to do.
	b.writes.finishStopping()
	waitFor(t, "the cleanup to end", func() bool { return b.cfg.unfinished.stepsLeft() == 0 })
	select {
	case <-b.cfg.unfinished.back:
	case <-time.After(3 * time.Second):
		t.Fatal("what was refused for it never hears that it ended")
	}
	second.start(planCall(t, 6, b.ref, b.was))
	again := answerTo(t, s, 6)
	if again.GetError() != nil {
		t.Fatalf("a plan once the step ended: %v", again)
	}
	if action := again.GetPlan().GetSteps()[0].GetAction(); action != dockwellv1.PlanAction_PLAN_ACTION_NONE {
		t.Errorf("the old one is back, and the plan is %s:\n%s", action, b.writes.text())
	}
	if n := b.engine.Newest(docker.Project(rolloutEnv), "db"); n == nil || !n.State.Running {
		t.Errorf("the service isn't running: %+v\n%s", n, b.writes.text())
	}
}

// One stuck for good is left behind, and the agent goes on: its resource waits for
// it, as long as that takes, and nothing else does.
func TestAStepThatWontEndHoldsOnlyItsResource(t *testing.T) {
	fw := newDeafFirewall(t)
	cfg := Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&firewall.Driver{Backend: fw}), CallGrace: 10 * time.Millisecond, unfinished: newUnfinished(),
	}
	first := newCalls(context.Background(), cfg, (&sent{results: map[uint64]*agentv1.CallResult{}}).send, nil)
	first.start(applyCall(t, 1, "stuck"))
	<-fw.started
	if !stops(first) {
		t.Fatal("a step stuck for good kept the agent offline")
	}
	s := &sent{results: map[uint64]*agentv1.CallResult{}}
	second := newCalls(context.Background(), cfg, s.send, nil)
	defer second.stop()
	rule := &resourcev1.FirewallRule{Port: 8080}
	stuck := resource.Ref{Kind: firewall.Kind, Scope: "srv_A", Name: "stuck"}
	second.start(planCall(t, 2, stuck, rule))
	if got := answerTo(t, s, 2).GetError(); !got.GetUnfinished() {
		t.Fatalf("a plan beside a step stuck for good: %v", s.result(2))
	}
	second.start(planCall(t, 3, resource.Ref{Kind: firewall.Kind, Scope: "srv_A", Name: "other"}, rule))
	if got := answerTo(t, s, 3); got.GetError() != nil {
		t.Fatalf("another resource waits too: %v", got)
	}
	fw.answer()
	waitFor(t, "the step to come back", func() bool { return cfg.unfinished.left.Load() == 0 })
	second.start(planCall(t, 4, stuck, rule))
	if got := answerTo(t, s, 4); got.GetError() != nil {
		t.Fatalf("it came back, and a plan says %v", got)
	}
}

// A plan or an apply alongside a step that's running goes on, as the engine sees to
// what runs beside what: only a step that outlived its call holds its resource.
func TestAStepStillOnItsCallHoldsNothing(t *testing.T) {
	fw := newDeafFirewall(t)
	s := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&firewall.Driver{Backend: fw}), CallGrace: time.Minute,
	}, s.send, nil)
	c.start(applyCall(t, 1, "running"))
	<-fw.started
	c.start(planCall(t, 2, resource.Ref{Kind: firewall.Kind, Scope: "srv_A", Name: "running"}, &resourcev1.FirewallRule{Port: 8080}))
	if got := answerTo(t, s, 2); got.GetError() != nil {
		t.Fatalf("a plan beside a step on its call: %v", got)
	}
	fw.answer()
	c.stop()
	select {
	case <-c.unfinished.back:
		t.Error("the control plane hears of a step that held nothing up")
	default:
	}
}

// A stuck step can come back long after the server moved on. By then what it would
// clean up may be what serves, so from the moment it's left behind it writes nothing
// to Docker, wherever it was. The next step to the service finishes what's left.
func TestAStepLeftBehindWritesNothingToDocker(t *testing.T) {
	for name, at := range map[string]string{
		"it has made the new one, which it would discard":          container.PointSwapReady,
		"it stopped the old one, which it would take back":         container.PointSwapStopped,
		"it removed the old one, and would name and start the new": container.PointSwapGone,
	} {
		t.Run(name, func(t *testing.T) {
			b := newRollout(t, 10*time.Millisecond)
			b.writes.finishStopping()
			reached, disk := make(chan struct{}), make(chan struct{})
			hold := func(point string) {
				if point == at {
					close(reached)
					<-disk
				}
			}
			call := b.deployCall(t, 1, b.now, false)
			b.at.Store(&hold)
			c, _ := b.stream()
			c.start(call)
			<-reached
			c.close()
			if !stops(c) {
				t.Fatal("a step that's stuck kept the agent offline")
			}
			if n := b.cfg.unfinished.left.Load(); n != 1 {
				t.Fatalf("%d left behind", n)
			}
			left := b.writes.text()
			close(disk)
			waitFor(t, "the step to come back", func() bool { return b.cfg.unfinished.left.Load() == 0 })
			if now := b.writes.text(); now != left {
				t.Errorf("after it was left behind, it went on to write:\n%s", strings.TrimPrefix(now, left))
			}
		})
	}
}
