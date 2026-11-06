// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// A crash test kills a real process with SIGKILL at each step of a rollout, as
// the agent or the whole server dying would: no defer runs, nothing flushes, and
// what is left is what Docker, the table of addresses and the proxy have. The
// process is this test binary, run again as an agent on the same fake engine and
// directory. Then a new agent comes up over what it left.

const (
	envCrashAt     = "DOCKWELL_TEST_CRASH_AT"
	envCrashDir    = "DOCKWELL_TEST_CRASH_DIR"
	envCrashSocket = "DOCKWELL_TEST_CRASH_SOCKET"
	envCrashSpec   = "DOCKWELL_TEST_CRASH_SPEC"
)

func TestMain(m *testing.M) {
	if at := os.Getenv(envCrashAt); at != "" {
		agentThatDiesAt(at)
		os.Exit(3) // it never got to the point it was to die at
	}
	os.Exit(m.Run())
}

// agentThatDiesAt runs one rollout, and kills itself at a step.
func agentThatDiesAt(at string) {
	var spec resourcev1.Service
	b, err := os.ReadFile(os.Getenv(envCrashSpec))
	if err == nil {
		err = proto.Unmarshal(b, &spec)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	s := agentOn(docker.New(os.Getenv(envCrashSocket)), os.Getenv(envCrashDir))
	s.rt.Points = func(name string) {
		if name == at {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
	}
	steps, err := s.local.Plan(context.Background(), []resource.Desired{{Ref: webRef, Spec: &spec}})
	if err == nil {
		err = s.local.Apply(context.Background(), steps[0], &spec)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
}

// fileProxy is a proxy that keeps its routes in a file, so they outlive the
// agent as Caddy's config does.
type fileProxy struct{ path string }

func (p fileProxy) read() []ingress.Route {
	var out []ingress.Route
	b, err := os.ReadFile(p.path)
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (p fileProxy) write(routes []ingress.Route) error {
	b, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

func (fileProxy) Name() string { return "the proxy" }

func (fileProxy) Status(context.Context) (ingress.Status, error) {
	return ingress.Status{Exists: true, Running: true}, nil
}
func (fileProxy) Prepare(context.Context, *ingress.PanelSite) error     { return nil }
func (fileProxy) Start(context.Context, *ingress.PanelSite, bool) error { return nil }
func (fileProxy) Stop(context.Context) error                            { return nil }
func (p fileProxy) Routes(context.Context) ([]ingress.Route, error)     { return p.read(), nil }
func (p fileProxy) PutRoute(_ context.Context, r ingress.Route) error {
	routes := slices.DeleteFunc(p.read(), func(o ingress.Route) bool { return o.ID == r.ID })
	return p.write(append(routes, r))
}

func (p fileProxy) PutRoutes(ctx context.Context, list []ingress.Route) error {
	for i := range list {
		if err := p.PutRoute(ctx, list[i]); err != nil {
			return err
		}
	}
	return nil
}

func (p fileProxy) DeleteRoute(_ context.Context, id string) error {
	return p.write(slices.DeleteFunc(p.read(), func(o ingress.Route) bool { return o.ID == id }))
}
func (fileProxy) Recover(context.Context) (bool, error)              { return false, nil }
func (fileProxy) SetClosed(context.Context, bool) error              { return nil }
func (fileProxy) SetPanel(context.Context, *ingress.PanelSite) error { return nil }
func (fileProxy) InFlight(context.Context, string) (int, error)      { return 0, nil }
func (fileProxy) Certificates(context.Context, []string) (map[string]ingress.CertState, error) {
	return nil, nil
}
func (fileProxy) Retry(context.Context) error { return nil }

// agent is what comes up on a server: nothing in memory but what it read.
type agent struct {
	rt    *container.Docker
	mgr   *ingress.Manager
	local resource.Local
}

func agentOn(client *docker.Client, dir string) *agent {
	m := &ingress.Manager{
		Client: client, Proxy: fileProxy{path: filepath.Join(dir, "routes.json")}, Dir: dir,
		Holders: &ingress.Holders{Client: client}, DrainPoll: time.Millisecond,
	}
	rt := &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: 2 * time.Second, ListenWait: 2 * time.Second, Ingress: listening{m}}
	c := container.Driver{Runtime: rt}
	return &agent{rt: rt, mgr: m, local: resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: c})}}
}

// listening is the manager with something answering on each container's port, as
// nothing runs in the fake engine's containers to.
type listening struct{ *ingress.Manager }

func (listening) Reachable(context.Context, ingress.Instance) error { return nil }

var checked = &resourcev1.Healthcheck{Test: []string{"CMD", "true"}}

// web is a service with a check, so "healthy" needs no real listener.
func web(image string, env ...string) *resourcev1.Service {
	a := app(image)
	a.GetApp().Healthcheck = proto.Clone(checked).(*resourcev1.Healthcheck)
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if a.GetApp().Env == nil {
			a.GetApp().Env = map[string]string{}
		}
		a.GetApp().Env[k] = v
	}
	return a
}

var wwwRef = resource.Ref{Kind: "route", Scope: env, Name: "www"}

// scene is a server with a service on a route, as a rollout finds it.
type scene struct {
	engine *dockertest.Engine
	dir    string
	first  *agent
}

func newScene(t *testing.T, spec *resourcev1.Service) *scene {
	t.Helper()
	e := dockertest.New(t)
	registry(e)
	dir, err := os.MkdirTemp("", "dwc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// The ports are the proxy's, as the first route on a server says, which is
	// what makes an agent reconcile its ingress as it starts.
	if err := os.WriteFile(filepath.Join(dir, "active"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &scene{engine: e, dir: dir, first: agentOn(e.Client(), dir)}
	s.first.mustDeploy(t, spec)
	if err := s.first.mgr.Apply(t.Context(), wwwRef, &resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 8080}, nil); err != nil {
		t.Fatalf("adding the route: %v", err)
	}
	return s
}

func (a *agent) mustDeploy(t *testing.T, spec *resourcev1.Service) {
	t.Helper()
	steps, err := a.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: spec}})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if steps[0].Action != resource.ActionNone {
		if err := a.local.Apply(t.Context(), steps[0], spec); err != nil {
			t.Fatalf("applying %s: %v", steps[0].Action, err)
		}
	}
}

// killAt runs a rollout in a process of its own, and sees it die at a step.
func (s *scene) killAt(t *testing.T, at string, spec *resourcev1.Service) {
	t.Helper()
	b, err := proto.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	specFile := filepath.Join(t.TempDir(), "spec")
	if err := os.WriteFile(specFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), envCrashAt+"="+at, envCrashDir+"="+s.dir, envCrashSocket+"="+s.engine.Socket, envCrashSpec+"="+specFile)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the agent lived past %s: %v\n%s", at, err, out)
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("the agent ended %v before it got to %s:\n%s", exit, at, out)
	}
}

func (s *scene) instances() []string { return s.engine.Instances(project, "web") }

func (s *scene) running() int {
	n := 0
	for _, name := range s.instances() {
		if c := s.engine.Container(name); c != nil && c.State.Running {
			n++
		}
	}
	return n
}

// routeAt reads the route as the proxy has it.
func (s *scene) route(t *testing.T) ingress.Route {
	t.Helper()
	routes := fileProxy{path: filepath.Join(s.dir, "routes.json")}.read()
	if len(routes) != 1 {
		t.Fatalf("the proxy has %+v", routes)
	}
	return routes[0]
}

// oneHealthy is what every rollout must end in, however it got there: one
// container, of the spec asked for, running, on the route alone, with nothing
// else on the table, and nothing left to plan.
func (s *scene) oneHealthy(t *testing.T, a *agent, spec *resourcev1.Service) *docker.Container {
	t.Helper()
	got := s.instances()
	if len(got) != 1 {
		t.Fatalf("Docker has %v, want one container", got)
	}
	if strings.Contains(got[0], "_") && !strings.HasPrefix(got[0], project) {
		t.Errorf("the container still has its swap name: %s", got[0])
	}
	c := s.engine.Container(got[0])
	if !c.State.Running {
		t.Errorf("%s isn't running: %+v", got[0], c.State)
	}
	if c.State.Health != nil && c.State.Health.Status != "healthy" {
		t.Errorf("%s is %s", got[0], c.State.Health.Status)
	}
	if want := spec.GetApp().GetImage(); c.Config.Image != want {
		t.Errorf("%s runs %s, want %s", got[0], c.Config.Image, want)
	}
	want := c.NetworkSettings.Networks[docker.IngressNetwork]
	if want == nil || want.IPAddress == "" {
		t.Fatalf("%s isn't on the ingress network: %+v", got[0], c.NetworkSettings.Networks)
	}
	if ups := s.route(t).Upstreams; !slices.Equal(ups, []string{want.IPAddress + ":8080"}) {
		t.Errorf("the route sends to %v, want %s alone", ups, want.IPAddress)
	}
	steps, err := a.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != resource.ActionNone {
		t.Errorf("once it settled, the plan is still %s: %v, %+v", steps[0].Action, steps[0].Changes, steps[0].Rollout)
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "addresses.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Addresses map[string]string `json:"addresses"`
	}
	if err := json.Unmarshal(b, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Addresses) != 1 {
		t.Errorf("the table of addresses has %v, want one", table.Addresses)
	}
	return c
}

// comeBack is an agent starting over what a dead one left, and a change going on:
// the agent reconciles, as it does on every start, and Resume plans the step
// against the server.
func (s *scene) comeBack(t *testing.T, spec *resourcev1.Service) *agent {
	t.Helper()
	a := agentOn(s.engine.Client(), s.dir)
	if err := a.mgr.Reconcile(t.Context()); err != nil {
		t.Errorf("the agent's reconcile: %v", err)
	}
	a.mustDeploy(t, spec)
	if err := a.mgr.Reconcile(t.Context()); err != nil {
		t.Errorf("the agent's next reconcile: %v", err)
	}
	return a
}

// Kill the agent at each step of a rolling replace. Whichever way the change
// goes after, forward as Resume takes it when the control plane stopped, or back
// as a rollback does when the agent's stream dropped, one healthy instance is
// left with no orphan, and traffic had somewhere to go throughout.
func TestKillingTheAgentAtEachStepOfARollout(t *testing.T) {
	old, next := web("nginx:1.27"), web("nginx:1.28")
	for _, at := range container.Rolling {
		for _, way := range []struct {
			name string
			to   *resourcev1.Service
		}{{"going on", next}, {"rolling back", old}} {
			for _, reboot := range []bool{false, true} {
				name := at + "/" + way.name
				if reboot {
					name += "/after a reboot"
				}
				t.Run(name, func(t *testing.T) {
					s := newScene(t, old)
					s.killAt(t, at, next)
					// With the agent dead, a route goes on serving: it has an
					// upstream, and at least one is a container that runs.
					if !s.routeHasARunningUpstream(t) {
						t.Errorf("at %s, with the agent dead, the route sends only to what doesn't run: %v, with %v", at, s.route(t).Upstreams, s.instances())
					}
					if reboot {
						s.engine.Reboot()
						if !s.routeHasARunningUpstream(t) {
							t.Errorf("at %s, after a reboot with no agent, the route sends only to what doesn't run", at)
						}
					}
					a := s.comeBack(t, way.to)
					s.oneHealthy(t, a, way.to)
				})
			}
		}
	}
}

func (s *scene) routeHasARunningUpstream(t *testing.T) bool {
	t.Helper()
	for _, up := range s.route(t).Upstreams {
		host, _, _ := strings.Cut(up, ":")
		for _, name := range s.instances() {
			c := s.engine.Container(name)
			if ep := c.NetworkSettings.Networks[docker.IngressNetwork]; ep != nil && ep.IPAddress == host && c.State.Running {
				return true
			}
		}
	}
	return false
}

// The same for a service that can't have two: at no step are two running, even
// after a reboot, and the data it keeps stays with it.
func TestKillingTheAgentAtEachStepOfAStopFirstReplace(t *testing.T) {
	spec := func(env string) *resourcev1.Service {
		a := app("postgres:17")
		a.GetApp().Healthcheck = proto.Clone(checked).(*resourcev1.Healthcheck)
		a.GetApp().Env = map[string]string{"VERSION": env}
		return a
	}
	old, next := spec("old"), spec("new")
	for _, at := range container.StopFirst {
		for _, way := range []struct {
			name string
			to   *resourcev1.Service
		}{{"going on", next}, {"rolling back", old}} {
			for _, reboot := range []bool{false, true} {
				name := at + "/" + way.name
				if reboot {
					name += "/after a reboot"
				}
				t.Run(name, func(t *testing.T) {
					s := newScene(t, old)
					data := s.engine.Newest(project, "web").Mounts[0].Name
					s.killAt(t, at, next)
					if n := s.running(); n > 1 {
						t.Errorf("at %s, %d containers run at once", at, n)
					}
					if reboot {
						s.engine.Reboot()
						if n := s.running(); n > 1 {
							t.Errorf("at %s, after a reboot, %d containers run at once", at, n)
						}
					}
					a := s.comeBack(t, way.to)
					c := s.oneHealthy(t, a, way.to)
					if got := mountAt(c, "/var/lib/postgresql/data"); got != data {
						t.Errorf("the data is at %q, not %q", got, data)
					}
				})
			}
		}
	}
}

func mountAt(c *docker.Container, target string) string {
	for _, m := range c.Mounts {
		if m.Destination == target {
			return m.Name
		}
	}
	return ""
}

// A restart killed at each step happens, once, and leaves one healthy instance.
func TestKillingTheAgentAtEachStepOfARestart(t *testing.T) {
	spec := web("nginx:1.27")
	for _, at := range container.Rolling {
		t.Run(at, func(t *testing.T) {
			s := newScene(t, spec)
			before := s.engine.Newest(project, "web").ID
			s.first.rt.Points = func(name string) {
				if name == at {
					panic(killed{})
				}
			}
			steps, err := s.first.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: spec, Restart: "chg_1"}})
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						if _, ok := r.(killed); !ok {
							panic(r)
						}
					}
				}()
				_ = s.first.local.Apply(t.Context(), steps[0], spec)
				t.Errorf("the agent lived past %s", at)
			}()
			a := agentOn(s.engine.Client(), s.dir)
			if err := a.mgr.Reconcile(t.Context()); err != nil {
				t.Error(err)
			}
			again, err := a.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: spec, Restart: "chg_1"}})
			if err != nil {
				t.Fatal(err)
			}
			if again[0].Action != resource.ActionNone {
				if err := a.local.Apply(t.Context(), again[0], spec); err != nil {
					t.Fatalf("resuming the restart: %v", err)
				}
			}
			if err := a.mgr.Reconcile(t.Context()); err != nil {
				t.Error(err)
			}
			c := s.oneHealthy(t, a, spec)
			if c.ID == before {
				t.Error("the restart never happened")
			}
			if c.Config.Labels[container.LabelRestart] != "chg_1" {
				t.Errorf("what it left carries %q", c.Config.Labels[container.LabelRestart])
			}
			if done, err := a.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: spec, Restart: "chg_1"}}); err != nil || done[0].Action != resource.ActionNone {
				t.Errorf("planning the restart once more = %+v, %v, want nothing to do", done, err)
			}
		})
	}
}

type killed struct{}

// Docker going away at each step fails the step, and the same rollout planned
// once it is back ends the same way.
// replacement is one way a step replaces a service, with what makes it go that way.
type replacement struct {
	name      string
	points    []string
	old, next *resourcev1.Service
}

// eachStep goes through every point of both ways a service is replaced.
func eachStep() iter.Seq2[replacement, string] {
	return func(yield func(replacement, string) bool) {
		for _, way := range []replacement{
			{"rolling", container.Rolling, web("nginx:1.27"), web("nginx:1.28")},
			{"stopping first", container.StopFirst, web("postgres:17", "V=old"), web("postgres:17", "V=new")},
		} {
			for _, at := range way.points {
				if !yield(way, at) {
					return
				}
			}
		}
	}
}

func TestDockerGoingDownAtEachStep(t *testing.T) {
	for way, at := range eachStep() {
		old, next := way.old, way.next
		t.Run(way.name+"/"+at, func(t *testing.T) {
			s := newScene(t, old)
			var down atomic.Bool
			s.engine.Intercept(func(*http.Request) error {
				if down.Load() {
					return errors.New("Cannot connect to the Docker daemon")
				}
				return nil
			})
			s.first.rt.Points = func(name string) {
				if name == at {
					down.Store(true)
				}
			}
			steps, err := s.first.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: next}})
			if err != nil {
				t.Fatal(err)
			}
			err = s.first.local.Apply(t.Context(), steps[0], next)
			if at != container.PointForgotten && err == nil {
				t.Errorf("with Docker gone at %s, the step passed", at)
			}
			down.Store(false)
			s.engine.Reboot()
			s.first.rt.Points = nil
			a := s.comeBack(t, next)
			s.oneHealthy(t, a, next)
		})
	}
}

// A rollout cancelled at each step, as the agent's stream ending cancels what
// runs, ends the same way when it is planned again.
func TestACancelledRolloutEndsTheSameWay(t *testing.T) {
	for way, at := range eachStep() {
		old, next := way.old, way.next
		t.Run(way.name+"/"+at, func(t *testing.T) {
			s := newScene(t, old)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.first.rt.Points = func(name string) {
				if name == at {
					cancel()
				}
			}
			steps, err := s.first.local.Plan(ctx, []resource.Desired{{Ref: webRef, Spec: next}})
			if err != nil {
				t.Fatal(err)
			}
			_ = s.first.local.Apply(ctx, steps[0], next)
			if !s.routeHasARunningUpstream(t) {
				t.Errorf("cancelled at %s, the route sends only to what doesn't run", at)
			}
			a := s.comeBack(t, next)
			s.oneHealthy(t, a, next)
		})
	}
}

// docker network prune can take the ingress network from a stopped service. A
// rollout makes it again on the subnet the table has, as its new container needs
// it, and with another network on that subnet, it is refused by name before
// anything runs, as Caddy dials those addresses whichever network has them.
func TestARolloutReadsWhatBecameOfTheIngressNetwork(t *testing.T) {
	s := newScene(t, web("nginx:1.27", "DEPLOY=1"))
	subnet := s.engine.Network(docker.IngressNetwork).IPAM.Config[0].Subnet
	s.engine.Stop(names(1)[0])
	if err := s.engine.Client().RemoveNetwork(t.Context(), docker.IngressNetwork); err != nil {
		t.Fatal(err)
	}
	s.engine.AddNetwork(docker.Network{Name: "someones-app", Driver: "bridge", IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: subnet}}}})
	next := web("nginx:1.27", "DEPLOY=2")
	steps, err := s.first.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: next}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.first.local.Apply(t.Context(), steps[0], next)
	if err == nil || !strings.Contains(err.Error(), "someones-app") || !strings.Contains(err.Error(), "docker network rm someones-app") {
		t.Fatalf("a rollout with another network on the subnet = %v, want it to name that network and what to do", err)
	}
	if got := s.engine.Instances(project, "web"); !slices.Equal(got, names(1)) {
		t.Errorf("a refused rollout left %v", got)
	}
	if s.engine.Network(docker.IngressNetwork) != nil {
		t.Error("the network was made on a subnet that was taken")
	}
	if err := s.engine.Client().RemoveNetwork(t.Context(), "someones-app"); err != nil {
		t.Fatal(err)
	}
	s.first.mustDeploy(t, next)
	if got := s.engine.Instances(project, "web"); !slices.Equal(got, names(2)) {
		t.Errorf("with the subnet free, Docker has %v", got)
	}
	n := s.engine.Network(docker.IngressNetwork)
	if n == nil || n.IPAM.Config[0].Subnet != subnet {
		t.Errorf("the network came back as %+v, want it on %s", n, subnet)
	}
	if ep := s.engine.Container(names(2)[0]).NetworkSettings.Networks[docker.IngressNetwork]; ep == nil || ep.IPAMConfig == nil {
		t.Errorf("the new container isn't on it: %+v", ep)
	}
}

// stopCutShort is Docker taking a stop and carrying on with it, as moby does
// whoever asked ("Cancelling the request should not cancel the stop"), as the
// agent's stream drops.
func stopCutShort(s *scene, cancel context.CancelFunc) func(*http.Request) error {
	var once atomic.Bool
	return func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop") && once.CompareAndSwap(false, true) {
			for _, name := range s.instances() {
				s.engine.Stop(name)
			}
			cancel()
			time.Sleep(50 * time.Millisecond)
			return errors.New("the client has gone")
		}
		return nil
	}
}

// A stop-first deploy whose call ends while Docker stops the old container. Whichever
// way the change then goes, one healthy instance is left: a stop the rollout made is
// never read as one a person made.
func TestAStopFirstCutShortWhileStoppingEndsRunning(t *testing.T) {
	spec := func(v string) *resourcev1.Service {
		a := web("postgres:17", "VERSION="+v)
		return a
	}
	old, next := spec("old"), spec("new")
	for _, way := range []struct {
		name string
		to   *resourcev1.Service
	}{{"going on, as after a control plane stopped", next}, {"rolling back, as after an agent's stream dropped", old}} {
		t.Run(way.name, func(t *testing.T) {
			s := newScene(t, old)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.engine.Intercept(stopCutShort(s, cancel))
			steps, err := s.first.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: next}})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.first.local.Apply(ctx, steps[0], next); err == nil {
				t.Fatal("the step passed")
			}
			s.engine.Intercept(nil)
			if n := s.running(); n != 1 {
				t.Errorf("the agent is still up, and left %d running: %v", n, s.instances())
			}
			a := s.comeBack(t, way.to)
			s.oneHealthy(t, a, way.to)
		})
	}
}

// Docker stops the old one, then can't remove it.
func TestAStopFirstWhoseRemoveFailsEndsRunning(t *testing.T) {
	spec := func(v string) *resourcev1.Service { return web("postgres:17", "VERSION="+v) }
	old, next := spec("old"), spec("new")
	s := newScene(t, old)
	first := s.engine.Newest(project, "web").ID
	var failed atomic.Bool
	s.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, first) && failed.CompareAndSwap(false, true) {
			return errors.New("removal of container is already in progress")
		}
		return nil
	})
	steps, err := s.first.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: next}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.first.local.Apply(t.Context(), steps[0], next)
	if err == nil || !strings.Contains(err.Error(), "is running again") {
		t.Fatalf("the step said %v", err)
	}
	s.engine.Intercept(nil)
	if n := s.running(); n != 1 || len(s.instances()) != 1 {
		t.Errorf("after the step failed, %d run of %v, and what it made should have gone", n, s.instances())
	}
	a := s.comeBack(t, old)
	s.oneHealthy(t, a, old)
}

// What can't be put back says so, and says what to do, and the next step to the
// service finishes it, as the mark is still on it.
func TestAStopFirstThatCantPutWhatRanBackSaysSo(t *testing.T) {
	spec := func(v string) *resourcev1.Service { return web("postgres:17", "VERSION="+v) }
	old, next := spec("old"), spec("new")
	s := newScene(t, old)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cut := stopCutShort(s, cancel)
	var starts atomic.Int32
	s.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start") {
			starts.Add(1)
			return errors.New("Docker can't start it for now")
		}
		return cut(r)
	})
	steps, err := s.first.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: next}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.first.local.Apply(ctx, steps[0], next)
	if err == nil || !strings.Contains(err.Error(), "nothing of it may be running") || !strings.Contains(err.Error(), "docker start") {
		t.Errorf("the step said %v", err)
	}
	s.engine.Intercept(nil)
	if starts.Load() == 0 {
		t.Error("it didn't try")
	}
	// Nothing reads this as stopped by hand: planning finds what is left.
	a := agentOn(s.engine.Client(), s.dir)
	planned, err := a.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: old}})
	if err != nil {
		t.Fatal(err)
	}
	if planned[0].Action == resource.ActionNone && s.running() == 0 {
		t.Fatal("what a rollout stopped reads as what a person stopped")
	}
	a = s.comeBack(t, old)
	s.oneHealthy(t, a, old)
}

// inPlace is a spec that differs from spec only by what Docker changes on a container
// that exists.
func inPlace(spec *resourcev1.Service) *resourcev1.Service {
	out := proto.Clone(spec).(*resourcev1.Service)
	out.GetApp().Restart = resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS
	return out
}

// What can't be put back says the next change to it finishes this, and that holds of
// one that would otherwise go in place. Updating what's marked leaves it stopped, and
// a change in place has nothing running to verify, so it would read as applied.
func TestAChangeInPlaceFinishesWhatCouldntBePutBack(t *testing.T) {
	spec := func(v string) *resourcev1.Service { return web("postgres:17", "VERSION="+v) }
	old, next := spec("old"), spec("new")
	s := newScene(t, old)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cut := stopCutShort(s, cancel)
	s.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start") {
			return errors.New("Docker can't start it for now")
		}
		return cut(r)
	})
	steps, err := s.first.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: next}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.first.local.Apply(ctx, steps[0], next); err == nil || !strings.Contains(err.Error(), "next change to it finishes this") {
		t.Fatalf("the step said %v", err)
	}
	s.engine.Intercept(nil)
	if s.running() != 0 {
		t.Fatalf("it should have left nothing running, and %v run", s.instances())
	}
	want := inPlace(old)
	a := agentOn(s.engine.Client(), s.dir)
	planned, err := a.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: want}})
	if err != nil {
		t.Fatal(err)
	}
	if planned[0].Action != resource.ActionReplace {
		t.Fatalf("a plan calls finishing it %s: %+v", planned[0].Action, planned[0].Changes)
	}
	if err := a.local.Apply(t.Context(), planned[0], want); err != nil {
		t.Fatalf("the next change: %v", err)
	}
	s.oneHealthy(t, a, want)
}

// A kill alone leaves the same at most steps of either way of replacing: whatever
// reads as what's asked for but for its restart policy, in a state only a rollout
// leaves. Going in place from any of them ends where any other way forward does.
func TestAChangeInPlaceFinishesARolloutKilledAtEachStep(t *testing.T) {
	for name, way := range map[string]struct {
		points    []string
		old, next *resourcev1.Service
	}{
		"rolling":    {container.Rolling, web("nginx:1.27"), web("nginx:1.28")},
		"stop first": {container.StopFirst, web("postgres:17", "VERSION=old"), web("postgres:17", "VERSION=new")},
	} {
		for _, at := range way.points {
			t.Run(name+"/"+at, func(t *testing.T) {
				s := newScene(t, way.old)
				s.killAt(t, at, way.next)
				want := inPlace(way.next)
				a := s.comeBack(t, want)
				s.oneHealthy(t, a, want)
			})
		}
	}
}

// A restart that must stop first, cut short the same way. Nothing follows it, so
// the agent has to put back what ran itself.
func TestAStopFirstRestartCutShortWhileStoppingEndsRunning(t *testing.T) {
	spec := web("postgres:17")
	s := newScene(t, spec)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.engine.Intercept(stopCutShort(s, cancel))
	steps, err := s.first.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: spec, Restart: "chg_1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.first.local.Apply(ctx, steps[0], spec); err == nil {
		t.Fatal("the step passed")
	}
	s.engine.Intercept(nil)
	if n := s.running(); n != 1 {
		t.Errorf("a restart cut short should leave what ran: %d run: %v", n, s.instances())
	}
}

// Someone stopping it by hand stays a person's: it is no rollout's to start.
func TestAContainerStoppedByHandStaysStopped(t *testing.T) {
	spec := web("nginx:1.27")
	s := newScene(t, spec)
	s.engine.Stop(s.instances()[0])
	a := agentOn(s.engine.Client(), s.dir)
	steps, err := a.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != resource.ActionNone {
		t.Errorf("planning what a person stopped = %s: %+v", steps[0].Action, steps[0].Rollout)
	}
}

// A deploy of a tag that moved, killed at each step, ends in one healthy instance of
// the new image. A run that finds a container of it with the old one still there, or
// under a name it has yet to give, isn't done, however that reads.
func TestKillingTheAgentAtEachStepOfAPull(t *testing.T) {
	for _, way := range []struct {
		name, image string
		volumes     []string
		points      []string
	}{
		{"rolling", "nginx:1.27", nil, container.Rolling},
		{"stopping first", "postgres:17", []string{"/var/lib/postgresql/data"}, container.StopFirst},
	} {
		for _, at := range way.points {
			t.Run(way.name+"/"+at, func(t *testing.T) {
				spec := app(way.image)
				spec.GetApp().Healthcheck = proto.Clone(checked).(*resourcev1.Healthcheck)
				s := newScene(t, spec)
				s.engine.Registry[way.image] = dockertest.Image{Digest: "sha256:" + strings.Repeat("c", 64), Volumes: way.volumes, Config: docker.Config{Cmd: []string{"app"}}}
				s.first.rt.Points = func(name string) {
					if name == at {
						panic(killed{})
					}
				}
				want := []resource.Desired{{Ref: webRef, Spec: spec, Pull: true, Run: true}}
				steps, err := s.first.local.Plan(t.Context(), want)
				if err != nil {
					t.Fatal(err)
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							if _, ok := r.(killed); !ok {
								panic(r)
							}
						}
					}()
					_ = s.first.local.Apply(t.Context(), steps[0], spec)
					t.Errorf("the agent lived past %s", at)
				}()
				a := agentOn(s.engine.Client(), s.dir)
				if err := a.mgr.Reconcile(t.Context()); err != nil {
					t.Error(err)
				}
				again, err := a.local.Plan(t.Context(), want)
				if err != nil {
					t.Fatal(err)
				}
				// As a resume has it: the run it goes on from had pulled here.
				again[0].Pulled = true
				if again[0].Action != resource.ActionNone {
					if err := a.local.Apply(t.Context(), again[0], spec); err != nil {
						t.Fatalf("resuming the pull: %v", err)
					}
				}
				if err := a.mgr.Reconcile(t.Context()); err != nil {
					t.Error(err)
				}
				c := s.oneHealthy(t, a, spec)
				img, err := s.engine.Client().InspectImage(t.Context(), docker.WithTag(way.image))
				if err != nil {
					t.Fatal(err)
				}
				if c.Image != img.ID {
					t.Errorf("it runs image %s, and the tag names %s", c.Image, img.ID)
				}
			})
		}
	}
}

// A marked container reads as running, as Docker may still be stopping it, however the
// step that marked it ended. So it isn't what serves, a plan doesn't say it never
// stopped, and going back to it finishes the stop before it starts it, or that stop
// would take down what came back.
func TestAMarkedContainerIsntWhatServes(t *testing.T) {
	old, next := web("nginx:1.27"), web("nginx:1.28")
	s := newScene(t, old)
	s.killAt(t, container.PointMarked, next)
	var marked *docker.Container
	for _, name := range s.instances() {
		if c := s.engine.Container(name); c != nil && strings.HasPrefix(c.Name, "/stopping_") {
			marked = c
		}
	}
	if marked == nil || !marked.State.Running {
		t.Fatalf("killed as it marked it, Docker has %v, and what's marked is %+v", s.instances(), marked)
	}
	a := agentOn(s.engine.Client(), s.dir)
	steps, err := a.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: old}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Rollout.AlreadyRuns() {
		t.Errorf("a plan reads what's marked as already running: %+v", steps[0].Rollout)
	}
	var mu sync.Mutex
	var calls []string
	s.engine.Intercept(func(r *http.Request) error {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if id := parts[len(parts)-2]; r.Method == http.MethodPost && id == marked.ID {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, parts[len(parts)-1])
		}
		return nil
	})
	if err := a.local.Apply(t.Context(), steps[0], old); err != nil {
		t.Fatal(err)
	}
	s.engine.Intercept(nil)
	stop, start := slices.Index(calls, "stop"), slices.Index(calls, "start")
	if stop < 0 || start < stop {
		t.Errorf("going back to it asked Docker for %v, and should finish stopping it before it starts it", calls)
	}
	s.oneHealthy(t, a, old)
}
