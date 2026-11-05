// SPDX-License-Identifier: AGPL-3.0-only

package health_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/health"
)

const project = "env_01kat4zr6e3v0y5xbkzb5d9q6m"

func labels(service string, number int, extra ...string) map[string]string {
	l := map[string]string{
		docker.LabelProject: project, docker.LabelService: service, docker.LabelNumber: strconv.Itoa(number), docker.LabelOneoff: "False",
	}
	for i := 0; i+1 < len(extra); i += 2 {
		l[extra[i]] = extra[i+1]
	}
	return l
}

func add(e *dockertest.Engine, service string, extra ...string) *docker.Container {
	return e.AddContainer(docker.Container{
		Name:   "/" + docker.InstanceName(project, service, 1),
		Config: docker.Config{Labels: labels(service, 1, extra...)},
		State:  docker.State{Status: "running", Running: true, StartedAt: time.Now().Add(-time.Hour)},
		NetworkSettings: struct {
			Networks map[string]*docker.EndpointSettings `json:"Networks"`
		}{Networks: map[string]*docker.EndpointSettings{"bridge": {IPAddress: "172.17.0.2"}}},
	})
}

// start runs a monitor over e, with probe making its checks.
func start(t *testing.T, e *dockertest.Engine, probe func(context.Context, *health.Check, string) (health.Result, error)) *health.Monitor {
	t.Helper()
	m := &health.Monitor{Client: e.Client(), Scan: 5 * time.Millisecond, Refresh: 10 * time.Millisecond, Probe: probe}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return m
}

func eventually(t *testing.T, what string, m *health.Monitor, ok func(map[string]*dockwellv1.InstanceHealth) bool) map[string]*dockwellv1.InstanceHealth {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		byName := map[string]*dockwellv1.InstanceHealth{}
		for _, i := range m.Report() {
			byName[i.GetContainer()] = i
		}
		if ok(byName) {
			return byName
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s; it reports %v", what, byName)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func stateOf(name string, want dockwellv1.HealthState) func(map[string]*dockwellv1.InstanceHealth) bool {
	return func(m map[string]*dockwellv1.InstanceHealth) bool {
		i, ok := m[name]
		return ok && i.GetState() == want
	}
}

// With no check, all anyone knows is that it runs: never that it is healthy.
func TestWhatNobodyChecksIsntHealthy(t *testing.T) {
	e := dockertest.New(t)
	add(e, "web")
	name := docker.InstanceName(project, "web", 1)
	m := start(t, e, nil)
	got := eventually(t, "it reads as having no check", m, stateOf(name, dockwellv1.HealthState_HEALTH_STATE_NO_CHECK))
	if i := got[name]; i.GetCheck() != dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_NONE || i.GetLast() != nil {
		t.Errorf("a container nothing checks reports %v", i)
	}
	e.Stop(name)
	got = eventually(t, "it reads as not running", m, stateOf(name, dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING))
	if !strings.Contains(got[name].GetLast().GetOutput(), "isn't running") {
		t.Errorf("it says %v", got[name].GetLast())
	}
}

func TestItReadsWhatDockersCheckSays(t *testing.T) {
	e := dockertest.New(t)
	c := add(e, "db")
	name := docker.InstanceName(project, "db", 1)
	e.Edit(c.ID, func(c *docker.Container) {
		c.Config.Healthcheck = &docker.Healthcheck{Test: []string{"CMD", "pg_isready"}}
		c.Config.Env = []string{"PGPASSWORD=hunter2hunter2"}
		c.State.Health = &docker.Health{Status: "starting"}
	})
	m := start(t, e, nil)
	eventually(t, "it reads as starting", m, stateOf(name, dockwellv1.HealthState_HEALTH_STATE_STARTING))
	e.SetHealth(name, "healthy", "accepting connections\n")
	got := eventually(t, "it reads as healthy", m, stateOf(name, dockwellv1.HealthState_HEALTH_STATE_HEALTHY))
	if got[name].GetCheck() != dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_COMMAND || got[name].GetLast().GetOutput() != "accepting connections" || !got[name].GetLast().GetPassed() {
		t.Errorf("a container that passed reports %v", got[name])
	}
	e.SetHealth(name, "unhealthy", "no response, password hunter2hunter2")
	got = eventually(t, "it reads as unhealthy", m, stateOf(name, dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY))
	out := got[name].GetLast().GetOutput()
	if !strings.Contains(out, "no response") || strings.Contains(out, "hunter2") {
		t.Errorf("a container that failed says %q, want its output with the password masked", out)
	}
	if got[name].GetLast().GetPassed() || got[name].GetFailing() != 3 {
		t.Errorf("a container that failed reports %v", got[name])
	}
	e.SetRestarting(name, 137)
	got = eventually(t, "it reads as unhealthy for restarting", m, func(g map[string]*dockwellv1.InstanceHealth) bool {
		return strings.Contains(g[name].GetLast().GetOutput(), "keeps restarting")
	})
	if !strings.Contains(got[name].GetLast().GetOutput(), "137") {
		t.Errorf("it says %q", got[name].GetLast().GetOutput())
	}
}

// A check the agent makes passes once, and fails only as many times in a row as
// it allows, and goes on after a deploy.
func TestItMakesTheChecksTheAgentMakes(t *testing.T) {
	e := dockertest.New(t)
	check := &health.Check{HTTP: &health.HTTP{Port: 8080, Path: "/up"}, Interval: time.Millisecond, Retries: 3}
	add(e, "web", health.Label, check.Encode())
	name := docker.InstanceName(project, "web", 1)
	var passing atomic.Bool
	var calls atomic.Int64
	var address atomic.Value
	m := start(t, e, func(_ context.Context, c *health.Check, a string) (health.Result, error) {
		calls.Add(1)
		address.Store(a)
		if passing.Load() {
			return health.Result{Passed: true, Output: "GET /up on port 8080 answered 200 OK", At: time.Now(), Took: time.Millisecond}, nil
		}
		return health.Result{Output: "GET /up on port 8080 answered 503 Service Unavailable", At: time.Now(), Took: time.Millisecond}, nil
	})
	got := eventually(t, "it reads as unhealthy", m, stateOf(name, dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY))
	if got[name].GetCheck() != dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_HTTP || got[name].GetFailing() < 3 || !strings.Contains(got[name].GetLast().GetOutput(), "503") {
		t.Errorf("a failing check reports %v", got[name])
	}
	if address.Load() != "172.17.0.2" {
		t.Errorf("it checked %v, want the container's own address", address.Load())
	}
	passing.Store(true)
	got = eventually(t, "it reads as healthy", m, stateOf(name, dockwellv1.HealthState_HEALTH_STATE_HEALTHY))
	if got[name].GetFailing() != 0 || got[name].GetLast().GetTook() == nil {
		t.Errorf("a passing check reports %v", got[name])
	}
	passing.Store(false)
	eventually(t, "it reads as unhealthy again, after a deploy is long over", m, stateOf(name, dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY))
}

// Failures in a container's start period don't count, as Docker has it.
func TestAStartPeriodForgivesFailures(t *testing.T) {
	e := dockertest.New(t)
	check := &health.Check{TCP: &health.TCP{Port: 5432}, Interval: time.Millisecond, Retries: 1, StartPeriod: time.Hour}
	c := add(e, "db", health.Label, check.Encode())
	e.Edit(c.ID, func(c *docker.Container) { c.State.StartedAt = time.Now() })
	name := docker.InstanceName(project, "db", 1)
	var calls atomic.Int64
	m := start(t, e, func(context.Context, *health.Check, string) (health.Result, error) {
		calls.Add(1)
		return health.Result{Output: "refused", At: time.Now()}, nil
	})
	got := eventually(t, "it has failed a few checks", m, func(map[string]*dockwellv1.InstanceHealth) bool { return calls.Load() > 5 })
	if got[name].GetState() != dockwellv1.HealthState_HEALTH_STATE_STARTING {
		t.Errorf("in its start period, it reads as %v", got[name].GetState())
	}
}

// Only what lives in an environment is a service's: another Compose project on
// the same server, and a one-off, are their own.
func TestItWatchesOnlyServices(t *testing.T) {
	e := dockertest.New(t)
	add(e, "web")
	e.AddContainer(docker.Container{Name: "/other-web-1", Config: docker.Config{Labels: map[string]string{docker.LabelProject: "other", docker.LabelService: "web"}}})
	e.AddContainer(docker.Container{Name: "/" + project + "-web-run-1", Config: docker.Config{Labels: labels("web", 1, docker.LabelOneoff, "True")}})
	e.AddContainer(docker.Container{Name: "/by-hand"})
	m := start(t, e, nil)
	got := eventually(t, "it reports the service", m, func(g map[string]*dockwellv1.InstanceHealth) bool {
		return len(g) > 0 && g[docker.InstanceName(project, "web", 1)] != nil
	})
	if len(got) != 1 {
		t.Errorf("it reports %v", got)
	}
}

func TestItForgetsWhatHasGone(t *testing.T) {
	e := dockertest.New(t)
	add(e, "web")
	name := docker.InstanceName(project, "web", 1)
	m := start(t, e, nil)
	eventually(t, "it reports it", m, func(g map[string]*dockwellv1.InstanceHealth) bool { return g[name] != nil })
	e.Remove(name)
	eventually(t, "it stops reporting it", m, func(g map[string]*dockwellv1.InstanceHealth) bool { return g[name] == nil })
}

func TestItSaysWhenSomethingChanged(t *testing.T) {
	e := dockertest.New(t)
	add(e, "web")
	m := start(t, e, nil)
	select {
	case <-m.Changed():
	case <-time.After(5 * time.Second):
		t.Fatal("no word of a new container")
	}
}

// A server of many services, each with a check that hangs: checks run late, and
// the agent's goroutines and heap stay where they are. This is what keeps a
// service's checks from growing the agent without limit (ADR 0018).
func TestManyServicesStayWithinWhatAMonitorHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("this measures memory")
	}
	e := dockertest.New(t)
	const services = 3000
	check := (&health.Check{HTTP: &health.HTTP{Port: 8080, Path: "/up"}, Interval: time.Second, Timeout: 50 * time.Millisecond}).Encode()
	for i := range services {
		add(e, fmt.Sprintf("web%d", i), health.Label, check)
	}
	var running, most atomic.Int64
	probe := func(ctx context.Context, _ *health.Check, _ string) (health.Result, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
		return health.Result{Output: strings.Repeat("x", health.MaxOutput), At: time.Now()}, nil
	}
	before := runtime.NumGoroutine()
	m := &health.Monitor{Client: e.Client(), Scan: 50 * time.Millisecond, Refresh: 100 * time.Millisecond, Probe: probe}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, "it has seen them all", m, func(g map[string]*dockwellv1.InstanceHealth) bool { return len(g) == services })

	heap := func() uint64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	first := heap()
	peakGoroutines := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		peakGoroutines = max(peakGoroutines, runtime.NumGoroutine())
		_ = m.Report()
		time.Sleep(5 * time.Millisecond)
	}
	second := heap()
	if most.Load() > health.DefaultWorkers {
		t.Errorf("%d checks ran at once, and a monitor makes %d", most.Load(), health.DefaultWorkers)
	}
	// Its own work, its workers, and the fake engine and client alongside it.
	if extra := peakGoroutines - before; extra > health.DefaultWorkers+64 {
		t.Errorf("the monitor took %d goroutines for %d services", extra, services)
	}
	if second > first+8<<20 {
		t.Errorf("the heap went from %d MiB to %d MiB in 3 s, with nothing new to hold", first>>20, second>>20)
	}
	t.Logf("%d services: %d checks at once, %d goroutines beyond the start, heap %.1f MiB, %.0f bytes a service",
		services, most.Load(), peakGoroutines-before, float64(second)/(1<<20), float64(second)/services)
}

// Past what it watches, a container reads as nobody having said, which is true:
// a server with absurdly many costs an agent no more.
func TestItWatchesUpToALimit(t *testing.T) {
	if testing.Short() {
		t.Skip("this makes thousands of containers")
	}
	e := dockertest.New(t)
	for i := range health.MaxInstances + 10 {
		add(e, fmt.Sprintf("web%d", i))
	}
	m := start(t, e, nil)
	got := eventually(t, "it has seen what it watches", m, func(g map[string]*dockwellv1.InstanceHealth) bool { return len(g) >= health.MaxInstances })
	time.Sleep(50 * time.Millisecond)
	if len(m.Report()) != health.MaxInstances {
		t.Errorf("it reports %d, want %d", len(got), health.MaxInstances)
	}
}

// Docker stops answering, and the agent stays connected to the control plane. What
// only Docker can say goes from what it last read to not known: nobody is looking.
func TestDockerGoneLeavesNoClaimStanding(t *testing.T) {
	for name, tc := range map[string]struct {
		want  dockwellv1.HealthState
		setup func(e *dockertest.Engine, c *docker.Container)
	}{
		"no check": {want: dockwellv1.HealthState_HEALTH_STATE_NO_CHECK},
		"Docker's check": {want: dockwellv1.HealthState_HEALTH_STATE_HEALTHY, setup: func(e *dockertest.Engine, c *docker.Container) {
			n := strings.TrimPrefix(c.Name, "/")
			e.Edit(n, func(c *docker.Container) { c.Config.Healthcheck = &docker.Healthcheck{Test: []string{"CMD", "true"}} })
			e.SetHealth(n, "healthy", "ok")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			e := dockertest.New(t)
			c := add(e, "web")
			if tc.setup != nil {
				tc.setup(e, c)
			}
			m := start(t, e, nil)
			n := docker.InstanceName(project, "web", 1)
			eventually(t, "it reads the state", m, stateOf(n, tc.want))
			scanned := m.ScannedAt()
			var down atomic.Bool
			down.Store(true)
			e.Intercept(func(*http.Request) error {
				if down.Load() {
					return errors.New("Cannot connect to the Docker daemon")
				}
				return nil
			})
			eventually(t, "it stops claiming that", m, stateOf(n, dockwellv1.HealthState_HEALTH_STATE_UNKNOWN))
			if got := m.ScannedAt(); got.After(scanned.Add(time.Second)) {
				t.Errorf("it says it read Docker at %s, with Docker gone since before", got)
			}
			down.Store(false)
			// It looks again at its slower pace, so this needs no more than that.
			m2 := start(t, e, nil)
			eventually(t, "a monitor with Docker back reads it", m2, stateOf(n, tc.want))
		})
	}
}

// A Docker that takes the connection and doesn't answer, as a stuck storage driver
// makes it, can't stop the checks the agent makes, which need nothing of it, and a
// list that hangs reads as nobody being able to say.
func TestAHungDockerStopsNoCheck(t *testing.T) {
	e := dockertest.New(t)
	add(e, "web", health.Label, `{"tcp":{"port":80},"interval":10000000}`)
	add(e, "plain")
	var probes atomic.Int32
	var failing atomic.Bool
	m := &health.Monitor{Client: e.Client(), Scan: 20 * time.Millisecond, CallTimeout: 100 * time.Millisecond, Refresh: time.Hour, Probe: func(context.Context, *health.Check, string) (health.Result, error) {
		probes.Add(1)
		return health.Result{Passed: !failing.Load(), Output: "ok", At: time.Now()}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	checked, plain := docker.InstanceName(project, "web", 1), docker.InstanceName(project, "plain", 1)
	eventually(t, "it is healthy", m, stateOf(checked, dockwellv1.HealthState_HEALTH_STATE_HEALTHY))
	eventually(t, "the other has no check", m, stateOf(plain, dockwellv1.HealthState_HEALTH_STATE_NO_CHECK))
	hung := make(chan struct{})
	defer close(hung)
	e.Intercept(func(r *http.Request) error {
		select {
		case <-hung:
		case <-r.Context().Done():
		}
		return nil
	})
	time.Sleep(100 * time.Millisecond) // the scan is inside a list by now
	before := probes.Load()
	failing.Store(true) // the app has died, and a check would say so
	eventually(t, "the agent's own check still says so", m, stateOf(checked, dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY))
	if probes.Load() == before {
		t.Error("a hung Docker stopped the checks")
	}
	eventually(t, "what only Docker can say reads as not known", m, stateOf(plain, dockwellv1.HealthState_HEALTH_STATE_UNKNOWN))
}

// What the agent's own check last said stops standing once it's too old, as a check
// that has stopped finishing says nothing of how the app is now.
func TestWhatACheckSaidLongAgoReadsAsNotKnown(t *testing.T) {
	e := dockertest.New(t)
	add(e, "web", health.Label, `{"tcp":{"port":80},"interval":10000000}`)
	m := start(t, e, func(context.Context, *health.Check, string) (health.Result, error) {
		return health.Result{Passed: true, Output: "ok", At: time.Now().Add(-time.Hour)}, nil
	})
	eventually(t, "it stops claiming that", m, stateOf(docker.InstanceName(project, "web", 1), dockwellv1.HealthState_HEALTH_STATE_UNKNOWN))
}

// A check an interval shorter than the scan runs at its interval, not the scan's.
func TestAnIntervalShorterThanTheScanIsKept(t *testing.T) {
	e := dockertest.New(t)
	add(e, "web", health.Label, `{"tcp":{"port":80},"interval":50000000}`) // 50 ms
	var probes atomic.Int32
	m := &health.Monitor{Client: e.Client(), Scan: 500 * time.Millisecond, Refresh: time.Hour, Probe: func(context.Context, *health.Check, string) (health.Result, error) {
		probes.Add(1)
		return health.Result{Passed: true, Output: "ok", At: time.Now()}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	time.Sleep(2 * time.Second)
	if got := probes.Load(); got < 20 {
		t.Errorf("a check at an interval of 50 ms ran %d times in 2 s, as a scan every 500 ms would have it", got)
	}
}

// A scan reads in full only so many containers, and gets round them all: what a
// command check last printed doesn't stand for ever past the first few.
func TestEveryContainerIsReadAgainInTurn(t *testing.T) {
	e := dockertest.New(t)
	const services = 30
	for i := range services {
		name := fmt.Sprintf("svc%02d", i)
		n := strings.TrimPrefix(add(e, name).Name, "/")
		e.Edit(n, func(c *docker.Container) { c.Config.Healthcheck = &docker.Healthcheck{Test: []string{"CMD", "true"}} })
		e.SetHealth(n, "healthy", "first")
	}
	m := start(t, e, nil)
	said := func(what string) func(map[string]*dockwellv1.InstanceHealth) bool {
		return func(by map[string]*dockwellv1.InstanceHealth) bool {
			if len(by) != services {
				return false
			}
			for _, i := range by {
				if i.GetLast().GetOutput() != what {
					return false
				}
			}
			return true
		}
	}
	eventually(t, "all read", m, said("first"))
	for i := range services {
		e.SetHealth(docker.InstanceName(project, fmt.Sprintf("svc%02d", i), 1), "healthy", "second")
	}
	eventually(t, "all read again", m, said("second"))
}

// A container restarted between two scans reads as running both times, and may have a
// new address, with another container at its old one. The list says where each is, so
// a check goes where it is now, without reading each in full.
func TestACheckGoesWhereTheListSaysEachIs(t *testing.T) {
	e := dockertest.New(t)
	const services = 30
	var names []string
	for i := range services {
		name := fmt.Sprintf("svc%02d", i)
		n := strings.TrimPrefix(add(e, name, health.Label, `{"tcp":{"port":80},"interval":5000000}`).Name, "/")
		e.Edit(n, func(c *docker.Container) {
			c.NetworkSettings.Networks["bridge"] = &docker.EndpointSettings{IPAddress: fmt.Sprintf("172.17.0.%d", 10+i)}
		})
		names = append(names, n)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	m := start(t, e, func(_ context.Context, _ *health.Check, address string) (health.Result, error) {
		mu.Lock()
		seen[address] = true
		mu.Unlock()
		return health.Result{Passed: true, Output: "ok", At: time.Now()}, nil
	})
	eventually(t, "all healthy", m, func(by map[string]*dockwellv1.InstanceHealth) bool {
		if len(by) != services {
			return false
		}
		for _, i := range by {
			if i.GetState() != dockwellv1.HealthState_HEALTH_STATE_HEALTHY {
				return false
			}
		}
		return true
	})
	for i, n := range names {
		e.Edit(n, func(c *docker.Container) {
			c.NetworkSettings.Networks["bridge"] = &docker.EndpointSettings{IPAddress: fmt.Sprintf("172.17.1.%d", 10+i)}
			c.State.StartedAt = time.Now()
		})
	}
	time.Sleep(700 * time.Millisecond) // a scan of this many, and what was under way ends
	mu.Lock()
	seen = map[string]bool{}
	mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for a := range seen {
		if strings.HasPrefix(a, "172.17.0.") {
			t.Errorf("a check still goes to %s, which that container no longer has", a)
		}
	}
	if len(seen) == 0 {
		t.Error("no check went anywhere")
	}
}

// A monitor that has read nothing has nothing to report, and sending an empty report
// would read as nothing running.
func TestAMonitorThatHasReadNothingHasNothingToSay(t *testing.T) {
	e := dockertest.New(t)
	add(e, "web")
	e.Intercept(func(*http.Request) error { return errors.New("Cannot connect to the Docker daemon") })
	m := start(t, e, nil)
	time.Sleep(100 * time.Millisecond)
	if report := m.Report(); report != nil || !m.ScannedAt().IsZero() {
		t.Errorf("having read nothing, it reports %d instances, read at %s", len(report), m.ScannedAt())
	}
}

// Checks that hang hold a worker for their timeout. They can't hold them all, so one
// that would have said a service has died gets its turn.
func TestHungChecksDontHoldUpAServiceThatHasDied(t *testing.T) {
	e := dockertest.New(t)
	const hung = 40
	for i := range hung {
		add(e, fmt.Sprintf("hung%02d", i), health.Label, `{"tcp":{"port":81},"interval":1000000,"timeout":100000000}`)
	}
	add(e, "victim", health.Label, `{"tcp":{"port":80},"interval":1000000,"retries":1}`)
	var dead atomic.Bool
	m := &health.Monitor{Client: e.Client(), Scan: 10 * time.Millisecond, Refresh: time.Hour, Workers: 2, Probe: func(ctx context.Context, c *health.Check, _ string) (health.Result, error) {
		if c.TCP.Port == 81 {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return health.Result{}, ctx.Err()
			}
			return health.Result{Output: "got no answer", At: time.Now()}, nil
		}
		return health.Result{Passed: !dead.Load(), Output: "port 80", At: time.Now()}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	victim := docker.InstanceName(project, "victim", 1)
	eventually(t, "the victim is healthy", m, stateOf(victim, dockwellv1.HealthState_HEALTH_STATE_HEALTHY))
	// Until each has hung once, nothing is known of it. That's the first pass.
	eventually(t, "each hung check has hung once", m, func(by map[string]*dockwellv1.InstanceHealth) bool {
		for i := range hung {
			if by[docker.InstanceName(project, fmt.Sprintf("hung%02d", i), 1)].GetLast() == nil {
				return false
			}
		}
		return true
	})
	dead.Store(true)
	begun := time.Now()
	eventually(t, "the victim reads unhealthy", m, stateOf(victim, dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY))
	if took := time.Since(begun); took > time.Second {
		t.Errorf("with %d hung checks on 2 workers, a service that died read healthy for %s; in turn with them all, it would be 2 s", hung, took)
	}
}

// An image's own environment isn't the spec's to keep secret, and masking it takes
// words such as "production" out of what a check says. What the spec set, the values
// of what the service uses among them, stays masked.
func TestOnlyWhatTheSpecSetIsMasked(t *testing.T) {
	env := []string{"APP_ENV=production", "DATABASE_PASSWORD=hunter2hunter2", "PATH=/usr/local/bin"}
	labels := map[string]string{docker.OwnPrefix + "set": `["env:DATABASE_PASSWORD"]`}
	said := "production can't reach the database with hunter2hunter2"
	if got := health.Clean(said, health.SpecEnv(env, labels)); got != "production can't reach the database with •••" {
		t.Errorf("with the label, it says %q", got)
	}
	// With no label, as a container someone made by hand, it can't tell.
	if got := health.Clean(said, health.SpecEnv(env, nil)); strings.Contains(got, "production") || strings.Contains(got, "hunter2") {
		t.Errorf("with no label, it says %q", got)
	}
}
