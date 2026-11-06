// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

const env = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"

var (
	project = docker.Project(env)
	webRef  = resource.Ref{Kind: service.Kind, Scope: env, Name: "web"}
)

func registry(e *dockertest.Engine) {
	e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	e.Registry["nginx:1.28"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	e.Registry["postgres:17"] = dockertest.Image{Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}}}
	e.Registry["sick:1"] = dockertest.Image{Unhealthy: true, CheckSays: "curl: (22) The requested URL returned error: 503, token=hunter2hunter2", Config: docker.Config{
		Cmd: []string{"sick"}, Env: []string{"TOKEN=hunter2hunter2"}, Healthcheck: &docker.Healthcheck{Test: []string{"CMD", "curl", "-f", "http://localhost"}},
	}}
	e.Registry["crash:1"] = dockertest.Image{Exits: true, Config: docker.Config{Cmd: []string{"false"}}}
}

// recorder is an ingress that says what a rollout did, in order.
type recorder struct {
	mu  sync.Mutex
	log []string
	// hasRoutes makes it an ingress the service has routes on.
	hasRoutes bool
	// unreachable is what Reachable answers, and addErr what AddUpstream does.
	unreachable, addErr error
	// edited names routes edited by hand, as Routing says.
	edited []string
	// heldPorts is what the last Hold said of the service's routes.
	heldPorts map[string]uint32
}

func (r *recorder) note(what string, i ingress.Instance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, fmt.Sprintf("%s %d", what, i.Number))
}

func (r *recorder) took() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.log)
}

func (r *recorder) Endpoint(_ context.Context, i ingress.Instance) (string, docker.EndpointSettings, bool, error) {
	if !r.hasRoutes {
		return "", docker.EndpointSettings{}, false, nil
	}
	return docker.IngressNetwork, docker.EndpointSettings{IPAMConfig: &docker.EndpointIPAM{IPv4Address: fmt.Sprintf("172.30.0.%d", 1+i.Number)}}, true, nil
}

func (r *recorder) AddUpstream(_ context.Context, i ingress.Instance) error {
	if r.addErr != nil {
		return r.addErr
	}
	r.note("add", i)
	return nil
}

func (r *recorder) Move(_ context.Context, to ingress.Instance, from []ingress.Instance) error {
	if r.addErr != nil {
		return r.addErr
	}
	r.note("move to", to)
	for _, f := range from {
		r.note("move from", f)
	}
	return nil
}

func (r *recorder) Drain(_ context.Context, i ingress.Instance) error {
	r.note("drain", i)
	return nil
}

func (r *recorder) RemoveUpstream(_ context.Context, i ingress.Instance) error {
	r.note("forget", i)
	return nil
}

func (r *recorder) Reachable(context.Context, ingress.Instance) error { return r.unreachable }

func (r *recorder) Routing(context.Context, ingress.Service) (ingress.Routing, error) {
	return ingress.Routing{Routed: r.hasRoutes, Edited: r.edited}, nil
}

func (r *recorder) RoutedAfter(context.Context, ingress.Instance, []resource.Desired) (bool, error) {
	return r.hasRoutes, nil
}

func (r *recorder) Hold(_ ingress.Service, ports map[string]uint32) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log, r.heldPorts = append(r.log, "hold"), ports
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.log = append(r.log, "release")
	}
}

func (r *recorder) PublicPort(context.Context, uint32, bool) (bool, error) { return false, nil }

type stack struct {
	engine *dockertest.Engine
	rt     *container.Docker
	ing    *recorder
	local  resource.Local
}

func newStack(t *testing.T) *stack {
	t.Helper()
	e := dockertest.New(t)
	registry(e)
	e.AddNetwork(docker.Network{Name: docker.IngressNetwork, Driver: "bridge", Internal: true, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "172.30.0.0/16"}}}})
	ing := &recorder{hasRoutes: true}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: 200 * time.Millisecond, ListenWait: 100 * time.Millisecond, Ingress: ing}
	c := container.Driver{Runtime: rt}
	return &stack{engine: e, rt: rt, ing: ing, local: resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: c})}}
}

func app(image string) *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: image, Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
	}}}
}

// plan plans a service against the server, with what a change would send.
func (s *stack) plan(t *testing.T, ref resource.Ref, spec *resourcev1.Service, restart string) resource.Step {
	t.Helper()
	steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec, Restart: restart}})
	if err != nil {
		t.Fatalf("planning %s: %v", ref, err)
	}
	return steps[0]
}

// deploy plans a service, and applies what it says.
func (s *stack) deploy(t *testing.T, ref resource.Ref, spec *resourcev1.Service) resource.Step {
	t.Helper()
	step := s.plan(t, ref, spec, "")
	if step.Action != resource.ActionNone {
		if err := s.local.Apply(t.Context(), step, spec); err != nil {
			t.Fatalf("applying %s %s: %v", step.Action, ref, err)
		}
	}
	return step
}

func (s *stack) instances() []string { return s.engine.Instances(project, "web") }

func names(numbers ...int) []string {
	out := make([]string, len(numbers))
	for i, n := range numbers {
		out[i] = docker.InstanceName(project, "web", n)
	}
	return out
}

// A rolling replace keeps the old one serving until the new one is healthy, and
// goes through every step in the order that drops nothing.
func TestARollingReplaceGoesInOrder(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	s.ing.log = nil
	var stopped []string
	s.engine.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			stopped = append(stopped, "stop")
			s.ing.mu.Lock()
			s.ing.log = append(s.ing.log, "stop")
			s.ing.mu.Unlock()
		}
		return nil
	})
	step := s.deploy(t, webRef, app("nginx:1.28"))
	if step.Action != resource.ActionReplace || step.Effect != resource.EffectRoll || step.Downtime != 0 || step.Rollout == nil || !step.Rollout.Rolling {
		t.Errorf("the step = %+v, want a rolling replace with no downtime", step)
	}
	if step.Rollout.Drain != container.DefaultDrain {
		t.Errorf("it drains for %s", step.Rollout.Drain)
	}
	want := []string{"hold", "move to 2", "move from 1", "drain 1", "stop", "forget 1", "release"}
	if got := s.ing.took(); !slices.Equal(got, want) {
		t.Errorf("the rollout did %v, want %v", got, want)
	}
	if got := s.instances(); !slices.Equal(got, names(2)) {
		t.Errorf("Docker has %v, want %v", got, names(2))
	}
	if c := s.engine.Container(names(2)[0]); !c.State.Running || c.Config.Image != "nginx:1.28" {
		t.Errorf("the new one is %+v", c.State)
	}
	if step := s.plan(t, webRef, app("nginx:1.28"), ""); step.Action != resource.ActionNone {
		t.Errorf("right after, the plan is %s", step.Action)
	}
}

// Numbers go up, as Compose numbers a new one, so what names an old container
// can't come to mean a new one, and Compose finds each by its labels.
func TestNumbersGoUpOverManyDeploys(t *testing.T) {
	s := newStack(t)
	for i := 1; i <= 6; i++ {
		spec := app("nginx:1.27")
		spec.GetApp().Env = map[string]string{"DEPLOY": strconv.Itoa(i)}
		s.deploy(t, webRef, spec)
		name := names(i)[0]
		if got := s.instances(); !slices.Equal(got, []string{name}) {
			t.Fatalf("after deploy %d, Docker has %v, want %s", i, got, name)
		}
		c := s.engine.Container(name)
		if c.Config.Labels[docker.LabelNumber] != strconv.Itoa(i) {
			t.Errorf("%s has the number %q", name, c.Config.Labels[docker.LabelNumber])
		}
		list, err := s.engine.Client().ListContainers(t.Context(), dockertest.ComposeFilters(project, "web"))
		if err != nil || len(list) != 1 || list[0].ID != c.ID {
			t.Errorf("docker compose ps finds %+v, %v, want %s alone", list, err, name)
		}
	}
}

// A name in the way that isn't ours is left alone, and the next number used.
func TestANumberSomeoneElseHasIsSkipped(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	s.engine.AddContainer(docker.Container{Name: "/" + names(2)[0], Config: docker.Config{Image: "by-hand"}})
	s.deploy(t, webRef, app("nginx:1.28"))
	if got := s.instances(); !slices.Equal(got, names(3)) {
		t.Errorf("the service has %v, want %v", got, names(3))
	}
	if c := s.engine.Container(names(2)[0]); c == nil || c.State.Status != "running" {
		t.Errorf("the container someone else made is %+v", c)
	}
}

// docker compose up keeps a container's number when it recreates it, and the
// next rollout finds it and goes on from there.
func TestARolloutFindsWhatComposeMade(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	s.deploy(t, webRef, app("nginx:1.28"))
	// As docker compose up recreates it: the same number and name, with its own
	// hash on it.
	old := s.engine.Container(names(2)[0])
	s.engine.Remove(old.Name[1:])
	labels := map[string]string{docker.LabelProject: project, docker.LabelService: "web", docker.LabelNumber: "2", docker.LabelOneoff: "False", docker.LabelConfigHash: "compose's own"}
	s.engine.AddContainer(docker.Container{
		Name: "/" + names(2)[0], Image: old.Image, Config: docker.Config{Image: "nginx:1.28", Cmd: []string{"nginx"}, Labels: labels},
		HostConfig: docker.HostConfig{RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"}},
	})
	if step := s.plan(t, webRef, app("nginx:1.28"), ""); step.Action != resource.ActionNone {
		t.Errorf("what compose made reads as %s: %v", step.Action, step.Changes)
	}
	s.deploy(t, webRef, app("nginx:1.27"))
	if got := s.instances(); !slices.Equal(got, names(3)) {
		t.Errorf("the next rollout left %v, want %v", got, names(3))
	}
}

func TestWhatCantRollSaysSoAndWhy(t *testing.T) {
	volume := func(name, target string, ro bool) *resourcev1.Mount {
		return &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: name}, Target: target, ReadOnly: ro}
	}
	path := func(p, target string, ro bool) *resourcev1.Mount {
		return &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: target, ReadOnly: ro}
	}
	port := func(h uint32) *resourcev1.PortMapping { return &resourcev1.PortMapping{HostPort: h, ContainerPort: 80} }
	start := &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST}
	stop := &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST}
	for name, tc := range map[string]struct {
		old, next *resourcev1.Container
		image     string
		rolling   bool
		ports     []string
		volumes   []string
		paths     []string
		data      []string
		chosen    bool
	}{
		"nothing shared": {old: &resourcev1.Container{}, next: &resourcev1.Container{}, rolling: true},
		"a published port": {
			old: &resourcev1.Container{Ports: []*resourcev1.PortMapping{port(8080)}}, next: &resourcev1.Container{Ports: []*resourcev1.PortMapping{port(8080)}},
			ports: []string{"8080/tcp"},
		},
		"another port": {
			old: &resourcev1.Container{Ports: []*resourcev1.PortMapping{port(8080)}}, next: &resourcev1.Container{Ports: []*resourcev1.PortMapping{port(8081)}}, rolling: true,
		},
		"a port on other addresses": {
			old:     &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8080, ContainerPort: 80, HostIp: "10.0.0.1"}}},
			next:    &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8080, ContainerPort: 80, HostIp: "10.0.0.2"}}},
			rolling: true,
		},
		"a port on every address and one": {
			old:   &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8080, ContainerPort: 80, HostIp: "10.0.0.1"}}},
			next:  &resourcev1.Container{Ports: []*resourcev1.PortMapping{port(8080)}},
			ports: []string{"8080/tcp"},
		},
		"a volume both write": {
			old: &resourcev1.Container{Mounts: []*resourcev1.Mount{volume("data", "/data", false)}}, next: &resourcev1.Container{Mounts: []*resourcev1.Mount{volume("data", "/data", false)}},
			volumes: []string{"data"},
		},
		"a volume one writes": {
			old: &resourcev1.Container{Mounts: []*resourcev1.Mount{volume("data", "/data", false)}}, next: &resourcev1.Container{Mounts: []*resourcev1.Mount{volume("data", "/data", true)}},
			volumes: []string{"data"},
		},
		"a volume both read": {
			old: &resourcev1.Container{Mounts: []*resourcev1.Mount{volume("data", "/data", true)}}, next: &resourcev1.Container{Mounts: []*resourcev1.Mount{volume("data", "/data", true)}},
			rolling: true,
		},
		"a server path both write": {
			old: &resourcev1.Container{Mounts: []*resourcev1.Mount{path("/srv/app", "/app", false)}}, next: &resourcev1.Container{Mounts: []*resourcev1.Mount{path("/srv/app", "/app", false)}},
			paths: []string{"/srv/app"},
		},
		"a server path under another": {
			old: &resourcev1.Container{Mounts: []*resourcev1.Mount{path("/srv/app", "/app", false)}}, next: &resourcev1.Container{Mounts: []*resourcev1.Mount{path("/srv/app/data", "/data", false)}},
			paths: []string{"/srv/app/data"},
		},
		"a server path both read": {
			old: &resourcev1.Container{Mounts: []*resourcev1.Mount{path("/srv/app", "/app", true)}}, next: &resourcev1.Container{Mounts: []*resourcev1.Mount{path("/srv/app", "/app", true)}},
			rolling: true,
		},
		"another volume": {
			old: &resourcev1.Container{Mounts: []*resourcev1.Mount{volume("a", "/data", false)}}, next: &resourcev1.Container{Mounts: []*resourcev1.Mount{volume("b", "/data", false)}},
			rolling: true,
		},
		"what the image keeps": {
			old: &resourcev1.Container{}, next: &resourcev1.Container{}, image: "postgres:17",
			data: []string{"/var/lib/postgresql/data"},
		},
		"asked to roll over a volume": {
			old: &resourcev1.Container{Mounts: []*resourcev1.Mount{volume("data", "/data", false)}}, next: &resourcev1.Container{Rollout: start, Mounts: []*resourcev1.Mount{volume("data", "/data", false)}},
			rolling: true, chosen: true, volumes: []string{"data"},
		},
		"asked to stop first": {old: &resourcev1.Container{}, next: &resourcev1.Container{Rollout: stop}, chosen: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			image := tc.image
			if image == "" {
				image = "nginx:1.27"
			}
			old, next := proto.Clone(tc.old).(*resourcev1.Container), proto.Clone(tc.next).(*resourcev1.Container)
			old.Image, next.Image = image, image
			next.Env = map[string]string{"NEW": "yes"}
			for _, c := range []*resourcev1.Container{old, next} {
				for _, m := range c.GetMounts() {
					if v := m.GetVolume(); v != "" {
						s.engine.AddVolume(docker.Volume{Name: docker.ScopedName(project, v)})
					}
				}
			}
			oldSpec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: old}}
			nextSpec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: next}}
			s.deploy(t, webRef, oldSpec)
			step := s.plan(t, webRef, nextSpec, "")
			r := step.Rollout
			if r == nil {
				t.Fatalf("the step says nothing of how it replaces what runs: %+v", step)
			}
			if r.Rolling != tc.rolling {
				t.Errorf("rolling = %t, want %t: %+v", r.Rolling, tc.rolling, r)
			}
			if !slices.Equal(r.Ports, tc.ports) || !slices.Equal(r.Volumes, tc.volumes) || !slices.Equal(r.Paths, tc.paths) || !slices.Equal(r.ImageData, tc.data) {
				t.Errorf("it says %+v, want ports %v, volumes %v, paths %v, image data %v", r, tc.ports, tc.volumes, tc.paths, tc.data)
			}
			if r.Chosen != tc.chosen {
				t.Errorf("chosen = %t, want %t", r.Chosen, tc.chosen)
			}
			// The plan tells the truth: no downtime rolling, and otherwise the most.
			switch {
			case tc.rolling && (step.Downtime != 0 || step.Effect != resource.EffectRoll):
				t.Errorf("a rolling step has downtime %s and effect %v", step.Downtime, step.Effect)
			case !tc.rolling && (step.Downtime != container.DefaultRecreateDowntime || step.Effect != resource.EffectRecreate):
				t.Errorf("a step that stops first has downtime %s and effect %v", step.Downtime, step.Effect)
			}
			// What it says is what it does: two containers at once, or never.
			s.ing.log = nil
			var most int
			s.engine.Intercept(func(*http.Request) error {
				running := 0
				for _, n := range s.instances() {
					if c := s.engine.Container(n); c != nil && c.State.Running {
						running++
					}
				}
				most = max(most, running)
				return nil
			})
			if err := s.local.Apply(t.Context(), step, nextSpec); err != nil {
				t.Fatalf("applying it: %v", err)
			}
			if got := s.instances(); len(got) != 1 {
				t.Errorf("it left %v", got)
			}
			if !tc.rolling && most > 1 {
				t.Errorf("a step that stops first had %d containers running at once", most)
			}
			if tc.rolling && most < 2 {
				t.Errorf("a rolling step never had two running")
			}
		})
	}
}

// A database's files are never written by two containers, so a volume makes a
// service stop first, whatever else it could do, unless its spec says otherwise,
// and a service that publishes a port can't be told otherwise.
func TestAVolumesOwnerIsAloneUnlessItSaysOtherwise(t *testing.T) {
	c := &resourcev1.Container{
		Image: "nginx:1.27", Rollout: &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST},
		Ports: []*resourcev1.PortMapping{{HostPort: 8080, ContainerPort: 80}},
	}
	if err := container.Validate(c); err == nil || !strings.Contains(err.Error(), "can't start a new container before the old one stops") {
		t.Errorf("start first on a published port = %v", err)
	}
	onHost := &resourcev1.Container{
		Image: "nginx:1.27", HostNetwork: true, Rollout: &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST},
	}
	if err := container.Validate(onHost); err == nil || !strings.Contains(err.Error(), "both would listen on its ports") {
		t.Errorf("start first on the server's own network = %v", err)
	}
	if err := (&container.Driver{}).Validate(&resourcev1.Container{Image: "nginx:1.27", Rollout: &resourcev1.Rollout{Drain: "1m"}}); err == nil || !strings.Contains(err.Error(), "as a service") {
		t.Errorf("a rollout on a container on its own = %v", err)
	}
	for _, bad := range []string{"soon", "-1s", "2h"} {
		if err := container.Validate(&resourcev1.Container{Image: "nginx:1.27", Rollout: &resourcev1.Rollout{Drain: bad}}); err == nil {
			t.Errorf("a drain of %q was taken", bad)
		}
		if err := container.Validate(&resourcev1.Container{Image: "nginx:1.27", StopGracePeriod: bad}); err == nil {
			t.Errorf("a stop grace of %q was taken", bad)
		}
	}
	g := &resourcev1.Container{Image: "nginx:1.27", StopGracePeriod: "1500ms"}
	if err := container.Validate(g); err != nil || g.GetStopGracePeriod() != "2s" {
		t.Errorf("a stop grace of 1500ms = %q, %v, want it in whole seconds, as Docker keeps it", g.GetStopGracePeriod(), err)
	}
}

// A new container that fails its check takes no traffic, and what ran goes on
// as it was. The rollback that follows has nothing to stop.
func TestAFailedCheckLeavesWhatRanServing(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	s.ing.log = nil
	bad := app("sick:1")
	step := s.plan(t, webRef, bad, "")
	err := s.local.Apply(t.Context(), step, bad)
	if err == nil || !strings.Contains(err.Error(), "fails its health check") {
		t.Fatalf("deploying what fails its check = %v", err)
	}
	if !strings.Contains(err.Error(), "The requested URL returned error: 503") {
		t.Errorf("the failure doesn't say what the check said: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the failure holds what the check printed from its environment: %v", err)
	}
	for _, did := range s.ing.took() {
		if strings.HasPrefix(did, "add") || strings.HasPrefix(did, "drain") {
			t.Errorf("a container that failed its check got as far as %q", did)
		}
	}
	if c := s.engine.Container(names(1)[0]); c == nil || !c.State.Running {
		t.Errorf("what ran doesn't run any more: %+v", c)
	}
	if got := s.instances(); !slices.Equal(got, names(1, 2)) {
		t.Errorf("Docker has %v", got)
	}

	// The rollback: the old one meets the spec already, so nothing stops for it.
	back := s.plan(t, webRef, app("nginx:1.27"), "")
	if back.Action != resource.ActionReplace || back.Downtime != 0 || !back.Rollout.Rolling || !slices.Equal(back.Rollout.Others, names(2)) {
		t.Errorf("the rollback's step = %+v, %+v", back, back.Rollout)
	}
	first := s.engine.Container(names(1)[0]).ID
	if err := s.local.Apply(t.Context(), back, app("nginx:1.27")); err != nil {
		t.Fatalf("rolling back: %v", err)
	}
	if got := s.instances(); !slices.Equal(got, names(1)) {
		t.Errorf("after rolling back, Docker has %v", got)
	}
	if s.engine.Container(names(1)[0]).ID != first {
		t.Error("rolling back made what ran anew")
	}
}

// A container that exits, or keeps restarting, fails at once with why.
func TestAContainerThatExitsFailsAtOnce(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	bad := app("crash:1")
	err := s.local.Apply(t.Context(), s.plan(t, webRef, bad, ""), bad)
	if err == nil || !strings.Contains(err.Error(), "exit code 1") || !strings.Contains(err.Error(), names(2)[0]) {
		t.Errorf("deploying what exits = %v, want its exit code and which container it was", err)
	}
	if c := s.engine.Container(names(1)[0]); c == nil || !c.State.Running {
		t.Error("what ran stopped")
	}
}

// A service with a route and no check must take connections on its port before
// it gets traffic: "it runs" isn't enough, and a first request would find an app
// still starting.
func TestARouteWaitsForItsPort(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	s.ing.log = nil
	s.ing.unreachable = errors.New("nothing takes a connection on port 8080, which a route for web sends to")
	next := app("nginx:1.28")
	err := s.local.Apply(t.Context(), s.plan(t, webRef, next, ""), next)
	if err == nil || !strings.Contains(err.Error(), "isn't ready for its traffic") || !strings.Contains(err.Error(), "port 8080") {
		t.Fatalf("a new one that never listens = %v", err)
	}
	if slices.ContainsFunc(s.ing.took(), func(d string) bool { return strings.HasPrefix(d, "add") }) {
		t.Error("it got traffic")
	}
	if c := s.engine.Container(names(1)[0]); c == nil || !c.State.Running {
		t.Error("what ran stopped")
	}
}

// The agent's checks work on a rollout as they do afterwards.
func TestARolloutMakesTheAgentsCheck(t *testing.T) {
	s := newStack(t)
	s.rt.HealthWait = 0
	s.rt.Poll = time.Millisecond
	var said []string
	var passing bool
	s.rt.Probe = func(_ context.Context, c *health.Check, address string) (health.Result, error) {
		said = append(said, address)
		if passing {
			return health.Result{Passed: true, Output: "GET /up answered 200"}, nil
		}
		return health.Result{Output: "GET /up on port 8080 answered 503 Service Unavailable"}, nil
	}
	check := &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 8080, Path: "/up"}, Interval: "1s", Retries: 2}
	spec := func(image string) *resourcev1.Service {
		a := app(image)
		a.GetApp().Healthcheck = proto.Clone(check).(*resourcev1.Healthcheck)
		return a
	}
	passing = true
	s.deploy(t, webRef, spec("nginx:1.27"))
	c := s.engine.Container(names(1)[0])
	if c.Config.Healthcheck == nil || c.Config.Healthcheck.Test[0] != "NONE" {
		t.Errorf("Docker has the check %+v, want none of its own, not the image's", c.Config.Healthcheck)
	}
	if c.Config.Labels[health.Label] == "" {
		t.Error("the container doesn't carry the check")
	}
	if step := s.plan(t, webRef, spec("nginx:1.27"), ""); step.Action != resource.ActionNone {
		t.Errorf("a check the agent makes reads back as %s: %v", step.Action, step.Changes)
	}
	passing = false
	bad := spec("nginx:1.28")
	err := s.local.Apply(t.Context(), s.plan(t, webRef, bad, ""), bad)
	if err == nil || !strings.Contains(err.Error(), "GET /up on port 8080 answered 503") {
		t.Fatalf("a new one that fails the agent's check = %v", err)
	}
	if len(said) == 0 || said[len(said)-1] != "172.30.0.3" {
		t.Errorf("it checked %v, want the new one's own address", said)
	}
}

func TestACheckIsOneOfThree(t *testing.T) {
	for name, hc := range map[string]*resourcev1.Healthcheck{
		"test and http":   {Test: []string{"CMD", "true"}, Http: &resourcev1.HttpCheck{Port: 80}},
		"test and tcp":    {Test: []string{"CMD", "true"}, Tcp: &resourcev1.TcpCheck{Port: 80}},
		"http and tcp":    {Http: &resourcev1.HttpCheck{Port: 80}, Tcp: &resourcev1.TcpCheck{Port: 80}},
		"no port":         {Http: &resourcev1.HttpCheck{}},
		"a port too high": {Tcp: &resourcev1.TcpCheck{Port: 70000}},
		"a relative path": {Http: &resourcev1.HttpCheck{Port: 80, Path: "healthz"}},
		// A // starts a URL's host in the path's place, and could name another.
		"a path that names a host": {Http: &resourcev1.HttpCheck{Port: 80, Path: "//169.254.169.254/latest"}},
		"a line break":             {Http: &resourcev1.HttpCheck{Port: 80, Path: "/up\r\nHost: evil"}},
		"a space":                  {Http: &resourcev1.HttpCheck{Port: 80, Path: "/up now"}},
		"a host with a path":       {Http: &resourcev1.HttpCheck{Port: 80, Host: "a.example/evil"}},
		"too often":                {Tcp: &resourcev1.TcpCheck{Port: 80}, Interval: "100ms"},
		"too patient":              {Tcp: &resourcev1.TcpCheck{Port: 80}, Timeout: "5m"},
		"too forgiving":            {Tcp: &resourcev1.TcpCheck{Port: 80}, Retries: 1000},
	} {
		if err := container.Validate(&resourcev1.Container{Image: "nginx:1.27", Healthcheck: hc}); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	good := &resourcev1.Container{Image: "nginx:1.27", Healthcheck: &resourcev1.Healthcheck{
		Http: &resourcev1.HttpCheck{Port: 8080, Path: "/up?deep=1", Host: "app.example.com:8080"}, Interval: "2000ms", Timeout: "1s", StartPeriod: "1m", Retries: 5,
	}}
	if err := container.Validate(good); err != nil {
		t.Errorf("a good check = %v", err)
	}
	if good.GetHealthcheck().GetInterval() != "2s" {
		t.Errorf("its interval reads %q", good.GetHealthcheck().GetInterval())
	}
}

// A restart replaces a service with its spec as it is, marked with the change
// that asked, so it happens once, however many times it is planned.
func TestARestartHappensOnce(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	s.ing.log = nil
	step := s.plan(t, webRef, app("nginx:1.27"), "chg_1")
	if step.Action != resource.ActionReplace || step.Restart != "chg_1" || step.Downtime != 0 || len(step.Changes) != 0 || !step.Rollout.Rolling {
		t.Errorf("planning a restart = %+v", step)
	}
	if err := s.local.Apply(t.Context(), step, app("nginx:1.27")); err != nil {
		t.Fatal(err)
	}
	if got := s.instances(); !slices.Equal(got, names(2)) {
		t.Errorf("Docker has %v", got)
	}
	if l := s.engine.Container(names(2)[0]).Config.Labels[container.LabelRestart]; l != "chg_1" {
		t.Errorf("the new one carries %q", l)
	}
	if again := s.plan(t, webRef, app("nginx:1.27"), "chg_1"); again.Action != resource.ActionNone {
		t.Errorf("planning the same restart again = %s, want nothing to do", again.Action)
	}
	if next := s.plan(t, webRef, app("nginx:1.27"), "chg_2"); next.Action != resource.ActionReplace {
		t.Errorf("planning another restart = %s", next.Action)
	}
	if step := s.plan(t, webRef, app("nginx:1.27"), ""); step.Action != resource.ActionNone {
		t.Errorf("with no restart asked for, the plan is %s", step.Action)
	}
}

// fixedValues is what a server's agent has of what a service uses.
type fixedValues map[string]string

func (v fixedValues) Values(context.Context, string, resource.Ref, proto.Message) (map[string]string, error) {
	return v, nil
}

// A plan has none of a service's values, as only a step's call brings them, and it
// still reads the service as it is, by the label that names what it uses. A step has
// them, so a new one carries them, and a rollout keeps what matches, values and all.
func TestAServiceThatUsesASecretRollsAndSettles(t *testing.T) {
	s := newStack(t)
	values := fixedValues{"DB": "hunter2hunter2"}
	s.local.Values, s.local.Server = values, "srv_1"
	spec := func(revision int64) *resourcev1.Service {
		a := app("nginx:1.27")
		a.Secrets, a.Revisions = map[string]string{"DB": "DB"}, map[string]int64{"DB": revision}
		return a
	}
	env := func(number int) string {
		t.Helper()
		for _, kv := range s.engine.Container(names(number)[0]).Config.Env {
			if v, ok := strings.CutPrefix(kv, "DB="); ok {
				return v
			}
		}
		return ""
	}
	s.deploy(t, webRef, spec(1))
	if got := env(1); got != "hunter2hunter2" {
		t.Errorf("the container has DB=%q", got)
	}
	if again := s.plan(t, webRef, spec(1), ""); again.Action != resource.ActionNone {
		t.Fatalf("with nothing changed, the plan is %s: %+v, %+v", again.Action, again.Changes, again.Rollout)
	}

	// A new revision of the secret rolls, and takes its new value with it.
	values["DB"] = "correcthorsebattery"
	step := s.deploy(t, webRef, spec(2))
	if step.Action != resource.ActionReplace || step.Rollout == nil || !step.Rollout.Rolling {
		t.Fatalf("a new revision of what it uses = %+v", step)
	}
	if got := s.instances(); !slices.Equal(got, names(2)) || env(2) != "correcthorsebattery" {
		t.Errorf("Docker has %v, with DB=%q", got, env(2))
	}
	if again := s.plan(t, webRef, spec(2), ""); again.Action != resource.ActionNone {
		t.Errorf("after the rollout, the plan is %s: %+v", again.Action, again.Rollout)
	}

	// A rollout that stopped partway is found, and goes on from what matches: the
	// plan names the one that goes, and the step keeps the one that's there.
	s.engine.AddContainer(oldInstance(s, 1, "nginx:1.27"))
	left := s.plan(t, webRef, spec(2), "")
	if left.Action != resource.ActionReplace || left.Rollout == nil || !slices.Equal(left.Rollout.Others, names(1)) {
		t.Fatalf("with one left over, the plan is %s: %+v", left.Action, left.Rollout)
	}
	kept := s.engine.Container(names(2)[0]).ID
	if err := s.local.Apply(t.Context(), left, spec(2)); err != nil {
		t.Fatal(err)
	}
	if got := s.instances(); !slices.Equal(got, names(2)) || s.engine.Container(names(2)[0]).ID != kept {
		t.Errorf("going on made %v, and replaced what matched", got)
	}

	// A restart, once, with the values it needs.
	restart := s.plan(t, webRef, spec(2), "chg_1")
	if restart.Action != resource.ActionReplace {
		t.Fatalf("planning a restart = %s", restart.Action)
	}
	if err := s.local.Apply(t.Context(), restart, spec(2)); err != nil {
		t.Fatal(err)
	}
	if got := s.instances(); !slices.Equal(got, names(3)) || env(3) != "correcthorsebattery" {
		t.Errorf("after a restart, Docker has %v, with DB=%q", got, env(3))
	}
	if again := s.plan(t, webRef, spec(2), "chg_1"); again.Action != resource.ActionNone {
		t.Errorf("planning the same restart again = %s", again.Action)
	}
}

// oldInstance is a container of web that an earlier deploy left, older than what
// runs.
func oldInstance(s *stack, number int, image string) docker.Container {
	return docker.Container{
		Name: "/" + names(number)[0], Config: docker.Config{Image: image, Cmd: []string{"nginx"}, Labels: map[string]string{
			docker.LabelProject: project, docker.LabelService: "web", docker.LabelNumber: strconv.Itoa(number), docker.LabelOneoff: "False",
		}},
		HostConfig: s.engine.Container(names(2)[0]).HostConfig,
	}
}

// A restart has no revision to roll back to, so what it made goes when it fails,
// and what ran goes on.
func TestARestartThatFailsLeavesWhatRan(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	s.engine.SetExits("nginx:1.27", true)
	step := s.plan(t, webRef, app("nginx:1.27"), "chg_1")
	err := s.local.Apply(t.Context(), step, app("nginx:1.27"))
	if err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("a restart whose new one exits = %v", err)
	}
	if got := s.instances(); !slices.Equal(got, names(1)) {
		t.Errorf("Docker has %v, want what ran alone", got)
	}
	if c := s.engine.Container(names(1)[0]); !c.State.Running {
		t.Error("what ran stopped")
	}
}

// A service that can't have two stops first for a restart too, and the plan says
// how long for.
func TestARestartStopsFirstWhereItMust(t *testing.T) {
	s := newStack(t)
	spec := app("postgres:17")
	s.deploy(t, webRef, spec)
	data := s.engine.Container(names(1)[0]).Mounts[0].Name
	step := s.plan(t, webRef, spec, "chg_1")
	if step.Downtime != container.DefaultRecreateDowntime || step.Rollout.Rolling || !slices.Equal(step.Rollout.ImageData, []string{"/var/lib/postgresql/data"}) {
		t.Errorf("a restart of a database = %+v, %+v", step, step.Rollout)
	}
	if err := s.local.Apply(t.Context(), step, spec); err != nil {
		t.Fatal(err)
	}
	c := s.engine.Container(names(1)[0])
	if c == nil || c.Mounts[0].Name != data || !c.State.Running || c.Config.Labels[container.LabelRestart] != "chg_1" {
		t.Errorf("after the restart it is %+v", c)
	}
}

// A stop grace period is how long the old one gets, and so how long the plan says
// it is down, and Docker keeps it for docker compose too.
func TestAStopGraceComesThrough(t *testing.T) {
	s := newStack(t)
	spec := app("postgres:17")
	spec.GetApp().StopGracePeriod = "1m0s"
	s.deploy(t, webRef, spec)
	c := s.engine.Container(names(1)[0])
	if c.Config.StopTimeout == nil || *c.Config.StopTimeout != 60 {
		t.Errorf("Docker has the stop timeout %v", c.Config.StopTimeout)
	}
	if step := s.plan(t, webRef, spec, ""); step.Action != resource.ActionNone {
		t.Errorf("a stop grace reads back as %s: %v", step.Action, step.Changes)
	}
	next := proto.Clone(spec).(*resourcev1.Service)
	next.GetApp().Env = map[string]string{"A": "1"}
	if step := s.plan(t, webRef, next, ""); step.Downtime != time.Minute+container.LogCatchUp+2*time.Second {
		t.Errorf("a step on it is down for %s, want the minute it gets to stop, a second for its last lines, and 2 s to start", step.Downtime)
	}
	if step := s.plan(t, webRef, next, ""); step.Budget() < time.Minute {
		t.Errorf("the step's budget is %s, less than it can take", step.Budget())
	}
}

func TestDeletingAServiceRemovesEachContainer(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	bad := app("sick:1")
	_ = s.local.Apply(t.Context(), s.plan(t, webRef, bad, ""), bad)
	if got := s.instances(); len(got) != 2 {
		t.Fatalf("Docker has %v, want a rollout left partway", got)
	}
	steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.local.Apply(t.Context(), steps[0], nil); err != nil {
		t.Fatal(err)
	}
	if got := s.instances(); len(got) != 0 {
		t.Errorf("deleting the service left %v", got)
	}
}

// A one-off, as docker compose run makes, isn't an instance: a rollout leaves it.
func TestAOneOffIsLeftAlone(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	run := project + "-web-run-1234"
	s.engine.AddContainer(docker.Container{Name: "/" + run, Config: docker.Config{Labels: map[string]string{
		docker.LabelProject: project, docker.LabelService: "web", docker.LabelOneoff: "True", docker.LabelNumber: "99",
	}}})
	s.deploy(t, webRef, app("nginx:1.28"))
	if s.engine.Container(run) == nil {
		t.Error("a one-off went with the rollout")
	}
	if got := s.engine.Container(names(2)[0]); got == nil {
		t.Errorf("the new one isn't number 2: %v", s.instances())
	}
}

// Containers scaled by hand are the service's: a deploy leaves one, of the new
// spec, and its plan names the others, as they go with it.
func TestContainersScaledByHandGoWithADeploy(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	c := s.engine.Container(names(1)[0])
	labels := map[string]string{docker.LabelProject: project, docker.LabelService: "web", docker.LabelNumber: "2", docker.LabelOneoff: "False", docker.LabelConfigHash: "x"}
	s.engine.AddContainer(docker.Container{Name: "/" + names(2)[0], Image: c.Image, Config: docker.Config{Image: "nginx:1.27", Labels: labels}})
	next := app("nginx:1.28")
	step := s.plan(t, webRef, next, "")
	if !slices.Equal(step.Rollout.Others, names(2, 1)) {
		t.Errorf("the plan names %v", step.Rollout.Others)
	}
	if err := s.local.Apply(t.Context(), step, next); err != nil {
		t.Fatal(err)
	}
	if got := s.instances(); !slices.Equal(got, names(3)) {
		t.Errorf("Docker has %v", got)
	}
}

// A plan that said nothing stops can't start a second writer because the server
// changed since it was made.
func TestAStaleRollingPlanStopsRatherThanShare(t *testing.T) {
	s := newStack(t)
	s.engine.AddVolume(docker.Volume{Name: docker.ScopedName(project, "data")})
	spec := app("nginx:1.27")
	s.deploy(t, webRef, spec)
	next := app("nginx:1.28")
	next.GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}}
	step := s.plan(t, webRef, next, "")
	if !step.Rollout.Rolling {
		t.Fatalf("nothing shares the volume yet: %+v", step.Rollout)
	}
	// Someone puts the volume on the old one meanwhile.
	s.engine.Edit(names(1)[0], func(c *docker.Container) {
		c.HostConfig.Mounts = []docker.Mount{{Type: "volume", Source: docker.ScopedName(project, "data"), Target: "/data"}}
	})
	err := s.local.Apply(t.Context(), step, next)
	if err == nil || !strings.Contains(err.Error(), "Plan the change again") {
		t.Fatalf("a stale rolling plan = %v", err)
	}
	if got := s.instances(); !slices.Equal(got, names(1)) {
		t.Errorf("Docker has %v", got)
	}
}

// A named volume over a path its image declares, dropped: it rolls, with nothing
// shared, and what the service sees at that path is no longer what it had. Nothing
// but the plan can say so, as the image is what puts an empty volume there.
func TestDroppingAVolumeOverAnImagesOwnSaysSo(t *testing.T) {
	s := newStack(t)
	s.engine.Registry["redis:7"] = dockertest.Image{Volumes: []string{"/data"}, Config: docker.Config{Cmd: []string{"redis-server"}}}
	s.engine.AddVolume(docker.Volume{Name: docker.ScopedName(project, "cache-data")})
	old := app("redis:7")
	old.GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "cache-data"}, Target: "/data"}}
	s.deploy(t, webRef, old)
	step := s.plan(t, webRef, app("redis:7"), "")
	want := []resource.DroppedMount{{Source: "cache-data", Target: "/data", EmptyVolume: true}}
	if step.Rollout == nil || !slices.Equal(step.Rollout.Dropped, want) {
		t.Fatalf("the plan says %+v, want %+v", step.Rollout, want)
	}
	if !step.Rollout.Rolling {
		t.Errorf("nothing is shared, so it rolls, and it is %+v", step.Rollout)
	}
	// With an image that declares nothing there, it says only what goes.
	s = newStack(t)
	s.engine.AddVolume(docker.Volume{Name: docker.ScopedName(project, "cache-data")})
	old = app("nginx:1.27")
	old.GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "cache-data"}, Target: "/data"}}
	s.deploy(t, webRef, old)
	step = s.plan(t, webRef, app("nginx:1.27"), "")
	if want := []resource.DroppedMount{{Source: "cache-data", Target: "/data"}}; !slices.Equal(step.Rollout.Dropped, want) {
		t.Errorf("the plan says %+v, want %+v", step.Rollout.Dropped, want)
	}
}

// A check runs inside the container, or on a port of its own, so its passing says
// nothing of the port a route sends to. Moving traffic there and removing what ran
// would leave the site answering 502, with the API saying healthy.
func TestAPassingCheckStillAsksTheRoutesPort(t *testing.T) {
	for name, withCheck := range map[string]func(*resourcev1.Service){
		"Docker's command check": func(a *resourcev1.Service) {
			a.GetApp().Healthcheck = proto.Clone(checked).(*resourcev1.Healthcheck)
		},
		"an agent check on another port": func(a *resourcev1.Service) {
			a.GetApp().Healthcheck = &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 8081, Path: "/healthz"}, Interval: "1s"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			s.rt.Probe = func(context.Context, *health.Check, string) (health.Result, error) {
				return health.Result{Passed: true, Output: "ok"}, nil
			}
			old := app("nginx:1.27")
			withCheck(old)
			s.deploy(t, webRef, old)
			s.ing.log = nil
			s.ing.unreachable = errors.New("nothing takes a connection on port 8080, which a route for web sends to")
			next := app("nginx:1.28")
			withCheck(next)
			err := s.local.Apply(t.Context(), s.plan(t, webRef, next, ""), next)
			if err == nil || !strings.Contains(err.Error(), "passes its health check, but isn't ready for its traffic") || !strings.Contains(err.Error(), "port 8080") {
				t.Fatalf("a deploy whose route's port answers nothing = %v", err)
			}
			if took := s.ing.took(); slices.ContainsFunc(took, func(w string) bool { return strings.HasPrefix(w, "move") || strings.HasPrefix(w, "drain") }) {
				t.Errorf("it moved traffic: %v", took)
			}
			if c := s.engine.Container(names(1)[0]); c == nil || !c.State.Running {
				t.Errorf("what ran is %+v", c)
			}
		})
	}
}

// One directory under two names is one directory: decide compares where each
// lands, through a volume's binding and a path's links, and stops first.
func TestOneDirectoryUnderTwoNamesStopsFirst(t *testing.T) {
	bound := map[string]string{"type": "none", "o": "bind", "device": "/srv/db"}
	volume := func(name string) *resourcev1.Mount {
		return &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: name}, Target: "/var/lib/data"}
	}
	path := func(p string) *resourcev1.Mount {
		return &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/var/lib/data"}
	}
	for name, tc := range map[string]struct {
		old, next *resourcev1.Mount
		want      string
	}{
		"a volume, then the server path it binds":   {volume("db"), path("/srv/db"), "/srv/db"},
		"the server path, then a volume binding it": {path("/srv/db"), volume("db"), "/srv/db"},
		"two volumes binding one path":              {volume("db"), volume("db2"), "/srv/db"},
		"a volume, then a path under what it binds": {volume("db"), path("/srv/db/tables"), "/srv/db/tables"},
		"a volume, then a link to what it binds":    {volume("db"), path("/srv/link"), "/srv/db"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			s.rt.ResolvePath = func(p string) (string, error) {
				if p == "/srv/link" {
					return "/srv/db", nil
				}
				return p, nil
			}
			for _, v := range []string{"db", "db2"} {
				s.engine.AddVolume(docker.Volume{Name: docker.ScopedName(project, v), Driver: "local", Options: bound})
			}
			old, next := app("nginx:1.27"), app("nginx:1.27")
			old.GetApp().Mounts = []*resourcev1.Mount{tc.old}
			next.GetApp().Mounts = []*resourcev1.Mount{tc.next}
			s.deploy(t, webRef, old)
			step := s.plan(t, webRef, next, "")
			if step.Rollout.Rolling || !slices.Contains(step.Rollout.Paths, tc.want) {
				t.Errorf("two containers would write to %s at once: %+v", tc.want, *step.Rollout)
			}
		})
	}
	// Two different places roll.
	s := newStack(t)
	s.engine.AddVolume(docker.Volume{Name: docker.ScopedName(project, "db"), Driver: "local", Options: bound})
	old, next := app("nginx:1.27"), app("nginx:1.27")
	old.GetApp().Mounts = []*resourcev1.Mount{volume("db")}
	next.GetApp().Mounts = []*resourcev1.Mount{path("/srv/elsewhere")}
	s.deploy(t, webRef, old)
	if step := s.plan(t, webRef, next, ""); !step.Rollout.Rolling {
		t.Errorf("two different places should roll: %+v", *step.Rollout)
	}
}

// What the agent can't read, it can't call safe: a volume on another driver, or a
// path it can't follow. It stops first, and says so.
func TestWhatTheAgentCantReadStopsFirstAndSaysSo(t *testing.T) {
	nfs := func(s *stack) {
		for _, v := range []string{"a", "b"} {
			s.engine.AddVolume(docker.Volume{Name: docker.ScopedName(project, v), Driver: "local", Options: map[string]string{"type": "nfs", "o": "addr=10.0.0.1", "device": ":/export"}})
		}
	}
	volume := func(name string) *resourcev1.Mount {
		return &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: name}, Target: "/data"}
	}
	path := &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/secret"}, Target: "/data"}
	for name, tc := range map[string]struct {
		old, next *resourcev1.Mount
		noRead    bool
		want      []string
	}{
		"two volumes over a network": {old: volume("a"), next: volume("b"), want: []string{"a", "b"}},
		"a path it can't follow":     {old: volume("a"), next: path, noRead: true, want: []string{"/srv/secret", "a"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			nfs(s)
			if tc.noRead {
				s.rt.ResolvePath = func(string) (string, error) { return "", errors.New("permission denied") }
			}
			old, next := app("nginx:1.27"), app("nginx:1.27")
			old.GetApp().Mounts = []*resourcev1.Mount{tc.old}
			next.GetApp().Mounts = []*resourcev1.Mount{tc.next}
			s.deploy(t, webRef, old)
			step := s.plan(t, webRef, next, "")
			if step.Rollout.Rolling || !slices.Equal(step.Rollout.CantTell, tc.want) {
				t.Errorf("it should stop first, naming %v, and is %+v", tc.want, *step.Rollout)
			}
		})
	}
}

// A route edited by hand isn't Dockwell's to move, so a rolling replace would leave
// it dialing what it then removes. Stopping first keeps the instance's address, so
// what it dials goes on being the service, and the plan says why.
func TestAHandEditedRouteKeepsWhatItDials(t *testing.T) {
	s := newScene(t, web("nginx:1.27", "DEPLOY=1"))
	p := fileProxy{path: filepath.Join(s.dir, "routes.json")}
	routes := p.read()
	routes[0].Edited = true
	if err := p.write(routes); err != nil {
		t.Fatal(err)
	}
	next := web("nginx:1.27", "DEPLOY=2")
	steps, err := s.first.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: next}})
	if err != nil {
		t.Fatal(err)
	}
	r := steps[0].Rollout
	if r == nil || r.Rolling || !slices.Equal(r.EditedRoutes, []string{"www"}) || steps[0].Downtime == 0 {
		t.Fatalf("the plan should stop first, naming the route, and is %+v", r)
	}
	if err := s.first.local.Apply(t.Context(), steps[0], next); err != nil {
		t.Fatalf("the deploy failed: %v", err)
	}
	if !s.routeHasARunningUpstream(t) {
		t.Errorf("the deploy passed, and the route sends only to what has gone: %v, with Docker at %v", s.route(t).Upstreams, s.instances())
	}
}

// One pass says little of an app that comes up and falls over on its first real
// work. A new container has to stay healthy for a while before anything moves to
// it, and what ran goes on until then.
func TestAMomentOfHealthIsntEnoughToRemoveWhatRan(t *testing.T) {
	s := newStack(t)
	s.rt.HealthWait = 2 * time.Second
	var calls atomic.Int32
	s.rt.Probe = func(context.Context, *health.Check, string) (health.Result, error) {
		if calls.Add(1) <= soakFirst {
			return health.Result{Passed: true, Output: "ok"}, nil
		}
		return health.Result{Output: "GET /healthz on port 80 answered 503"}, nil
	}
	check := &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 80, Path: "/healthz"}, Interval: "1s", Retries: 1}
	old := app("nginx:1.27")
	old.GetApp().Healthcheck = check
	s.rt.Soak = -1
	s.deploy(t, webRef, old)
	s.rt.Soak = 300 * time.Millisecond
	calls.Store(0)
	s.ing.log = nil
	next := app("nginx:1.28")
	next.GetApp().Healthcheck = check
	step := s.plan(t, webRef, next, "")
	if step.Rollout.Soak != 300*time.Millisecond {
		t.Errorf("the plan says a soak of %s", step.Rollout.Soak)
	}
	err := s.local.Apply(t.Context(), step, next)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("an app that passes %d checks and fails from then on = %v", soakFirst, err)
	}
	if took := s.ing.took(); slices.ContainsFunc(took, func(w string) bool { return strings.HasPrefix(w, "move") }) {
		t.Errorf("it moved traffic: %v", took)
	}
	if c := s.engine.Container(names(1)[0]); c == nil || !c.State.Running {
		t.Errorf("what ran is %+v", c)
	}
	// One that goes on passing takes over.
	s.rt.Probe = func(context.Context, *health.Check, string) (health.Result, error) {
		return health.Result{Passed: true, Output: "ok"}, nil
	}
	again := app("nginx:1.28")
	again.GetApp().Healthcheck, again.GetApp().Env = check, map[string]string{"A": "1"}
	begun := time.Now()
	s.deploy(t, webRef, again)
	if took := time.Since(begun); took < 300*time.Millisecond {
		t.Errorf("a deploy took %s, with a soak of 300ms", took)
	}
}

// soakFirst is how many checks pass before it falls over: fewer than a soak asks.
const soakFirst = 2

// Docker has no way to add a name to a container that has joined a network, and
// joining again would cut what the new one has open there, so other services can
// reach it by name before it has passed its check. The plan says so.
func TestThePlanSaysOtherServicesCanReachANewContainerEarly(t *testing.T) {
	s := newStack(t)
	s.engine.AddNetwork(docker.Network{Name: docker.ScopedName(project, "back"), Driver: "bridge"})
	old, next := app("nginx:1.27"), app("nginx:1.28")
	old.GetApp().Networks = []string{"back"}
	next.GetApp().Networks = []string{"back"}
	s.deploy(t, webRef, old)
	step := s.plan(t, webRef, next, "")
	if !slices.Equal(step.Rollout.EarlyNetworks, []string{"back"}) {
		t.Errorf("the plan says %+v", *step.Rollout)
	}
	s.deploy(t, webRef, next)
	ep := s.engine.Container(names(2)[0]).NetworkSettings.Networks[docker.ScopedName(project, "back")]
	if !slices.Contains(ep.Aliases, "web") {
		t.Errorf("the new container should answer to its service's name once it has taken over: %v", ep.Aliases)
	}
	// Going back to what already runs starts nothing new, so there's nothing to say.
	if back := s.plan(t, webRef, next, "chg_1"); !slices.Equal(back.Rollout.EarlyNetworks, []string{"back"}) {
		t.Errorf("a restart makes a new one too, and says %+v", *back.Rollout)
	}
}

// A slow starter with no check fails on every deploy, as the wait for its port is
// short. What it reads has to say the way out.
func TestASlowStarterWithNoCheckIsToldOfAStartPeriod(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	s.ing.unreachable = errors.New("nothing takes a connection on port 8080, which a route for web sends to")
	next := app("nginx:1.28")
	err := s.local.Apply(t.Context(), s.plan(t, webRef, next, ""), next)
	if err == nil || !strings.Contains(err.Error(), "health check with a start period") {
		t.Fatalf("it says %v", err)
	}
}

// How long a plan says a new container gets is what it really gets: a service with
// no route and no check only has to stay up, and one whose image isn't here yet may
// bring a check of its own.
func TestAPlanSaysHowLongANewContainerReallyGets(t *testing.T) {
	s := newStack(t)
	s.rt.ListenWait, s.rt.Settle = 2*time.Minute, 2*time.Second
	s.ing.hasRoutes = false
	s.rt.HealthWait = 0
	s.deploy(t, webRef, app("nginx:1.27"))
	next := app("nginx:1.27")
	next.GetApp().Env = map[string]string{"A": "1"}
	if got := s.plan(t, webRef, next, "").Rollout; got.HealthyWithin != 2*time.Second || !got.NoCheck {
		t.Errorf("with no route and no check, it says %s, no check %t", got.HealthyWithin, got.NoCheck)
	}
	s.ing.hasRoutes = true
	if got := s.plan(t, webRef, next, "").Rollout; got.HealthyWithin != 2*time.Minute || got.NoCheck {
		t.Errorf("with a route and no check, it says %s, no check %t, when it waits for it to listen", got.HealthyWithin, got.NoCheck)
	}
	s.engine.Registry["not-pulled:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}}
	step := s.plan(t, webRef, app("not-pulled:1"), "")
	if !step.HealthUnknown || step.Rollout.HealthyWithin < container.DefaultHealthWait {
		t.Errorf("for an image it hasn't seen, it says %s, with unknown %t", step.Rollout.HealthyWithin, step.HealthUnknown)
	}
}

// A stop-first restart has nothing to go back to once the new one fails. It can't
// keep what ran, so what a person reads has to say so before, and after.
func TestAStopFirstRestartThatFailsSaysWhatItLeft(t *testing.T) {
	s := newStack(t)
	spec := app("postgres:17")
	s.deploy(t, webRef, spec)
	step := s.plan(t, webRef, spec, "chg_1")
	if step.Rollout == nil || step.Rollout.Rolling {
		t.Fatalf("a restart of what keeps data in its image should stop first: %+v", step.Rollout)
	}
	s.engine.SetExits("postgres:17", true)
	err := s.local.Apply(t.Context(), step, spec)
	if err == nil || !strings.Contains(err.Error(), "nothing of it runs now") || !strings.Contains(err.Error(), "restart it again") {
		t.Errorf("it says %v", err)
	}
}

// A container on the server's own network listens on the server's ports, so with
// another there it can't bind them, or a check of them reads the other. That holds
// either way round: one moving off it may still publish what the old one holds.
func TestTheServersOwnNetworkStopsFirst(t *testing.T) {
	for name, tc := range map[string]struct {
		from, to bool
		wantStop bool
	}{
		"staying on it":  {from: true, to: true, wantStop: true},
		"moving onto it": {to: true, wantStop: true},
		"moving off it":  {from: true, wantStop: true},
		"never on it":    {},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			s.ing.hasRoutes = false
			old, next := app("nginx:1.27"), app("nginx:1.28")
			old.GetApp().HostNetwork, next.GetApp().HostNetwork = tc.from, tc.to
			s.deploy(t, webRef, old)
			step := s.plan(t, webRef, next, "")
			if r := step.Rollout; r == nil || r.Rolling == tc.wantStop || r.HostNetwork != tc.wantStop || (step.Downtime > 0) != tc.wantStop {
				t.Fatalf("the plan is %+v, with %s of downtime, want stopping first %t", r, step.Downtime, tc.wantStop)
			}
			s.deploy(t, webRef, next)
			want := names(2)
			if tc.wantStop {
				want = names(1) // keeping its number, as a stop-first replace does
			}
			if got := s.instances(); !slices.Equal(got, want) {
				t.Errorf("Docker has %v, want %v", got, want)
			}
			if c := s.engine.Container(want[0]); c == nil || !c.State.Running || (c.HostConfig.NetworkMode == "host") != tc.to {
				t.Errorf("it is %+v", c)
			}
		})
	}
}

// What was planned as rolling doesn't stop what runs, if an old container turns out
// to be on the server's own network by the time it applies.
func TestAStaleRollingPlanSaysTheServersOwnNetwork(t *testing.T) {
	s := newStack(t)
	s.ing.hasRoutes = false
	s.deploy(t, webRef, app("nginx:1.27"))
	next := app("nginx:1.28")
	step := s.plan(t, webRef, next, "")
	if !step.Rollout.Rolling {
		t.Fatalf("the plan is %+v", *step.Rollout)
	}
	s.engine.Edit(names(1)[0], func(c *docker.Container) { c.HostConfig.NetworkMode = "host" })
	err := s.local.Apply(t.Context(), step, next)
	if err == nil || !strings.Contains(err.Error(), "the server's own network") || !strings.Contains(err.Error(), "Plan the change again") {
		t.Fatalf("applying it = %v", err)
	}
	if got := s.instances(); !slices.Equal(got, names(1)) || !s.engine.Container(names(1)[0]).State.Running {
		t.Errorf("Docker has %v", got)
	}
}

// A container there has no address of its own, so the agent can't tell it from
// whatever else listens.
func TestTheServersOwnNetworkTakesNoAgentCheck(t *testing.T) {
	check := &resourcev1.Container{Image: "nginx:1.27", HostNetwork: true, Healthcheck: &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 80, Path: "/"}}}
	if err := container.Validate(check); err == nil || !strings.Contains(err.Error(), "Use a command check") {
		t.Errorf("an agent's check on it = %v", err)
	}
	command := &resourcev1.Container{Image: "nginx:1.27", HostNetwork: true, Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "true"}}}
	if err := container.Validate(command); err != nil {
		t.Errorf("a command check on it = %v", err)
	}
}

// On a network the server has, other projects' containers can reach a new one early
// too, by each name it answers to. It keeps those names on every network, however it
// joins: an older Docker takes one network as it makes a container, and the rest after.
func TestThePlanSaysWhereAndByWhatANewContainerCanBeReachedEarly(t *testing.T) {
	for name, api := range map[string]string{"joining as it is made": "1.47", "joining after": "1.43"} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			s.engine.APIVersion = api
			s.engine.AddNetwork(docker.Network{Name: docker.ScopedName(project, "back"), Driver: "bridge"})
			s.engine.AddNetwork(docker.Network{Name: "edge", Driver: "bridge"})
			spec := func(image string) *resourcev1.Service {
				a := app(image)
				a.GetApp().Networks, a.GetApp().ExternalNetworks, a.GetApp().Aliases = []string{"back"}, []string{"edge"}, []string{"db"}
				return a
			}
			s.deploy(t, webRef, spec("nginx:1.27"))
			step := s.plan(t, webRef, spec("nginx:1.28"), "")
			if r := step.Rollout; !r.Rolling || !slices.Equal(r.EarlyNetworks, []string{"back", "edge"}) || !slices.Equal(r.EarlyNames, []string{"db"}) {
				t.Fatalf("the plan says %+v", *r)
			}
			s.deploy(t, webRef, spec("nginx:1.28"))
			c := s.engine.Container(names(2)[0])
			for _, n := range []string{docker.ScopedName(project, "back"), "edge"} {
				if ep := c.NetworkSettings.Networks[n]; ep == nil || !slices.Equal(ep.Aliases, []string{"web", "db"}) {
					t.Errorf("on %s it answers to %+v", n, ep)
				}
			}
		})
	}
}

// Limits change in place, and a rollout has changed the name of what runs by then,
// so they have to find it by what it is, not by its first name.
func TestALimitReachesWhatARolloutMade(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	s.deploy(t, webRef, app("nginx:1.28"))
	limited := app("nginx:1.28")
	limited.GetApp().Resources = &resourcev1.Resources{Memory: "64M"}
	if step := s.deploy(t, webRef, limited); step.Action != resource.ActionUpdate {
		t.Fatalf("the step is %s, want it in place", step.Action)
	}
	c := s.engine.Container(names(2)[0])
	if c == nil || c.HostConfig.Memory != 64<<20 {
		t.Fatalf("what runs is %+v", c)
	}
	if got := s.instances(); !slices.Equal(got, names(2)) {
		t.Errorf("Docker has %v", got)
	}
}

// moved is a tag that names a new image at its registry, as after someone pushes one.
func moved(digit string, volumes ...string) dockertest.Image {
	return dockertest.Image{Digest: "sha256:" + strings.Repeat(digit, 64), Volumes: volumes, Config: docker.Config{Cmd: []string{"nginx"}}}
}

// A container of the old image still reads as the spec, which only says the tag. So
// a deploy of a tag that moved takes the next number, as any new image does, rather
// than keep what runs and read as done. With nothing newer after that, it stays.
func TestAPullOfANewerImageTakesTheNextNumber(t *testing.T) {
	s := newStack(t)
	spec := app("nginx:1.27")
	s.deploy(t, webRef, spec)
	old := s.engine.Container(names(1)[0])
	s.engine.Registry["nginx:1.27"] = moved("a")
	pull := func() resource.Step {
		t.Helper()
		steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: spec, Pull: true, Run: true}})
		if err != nil {
			t.Fatal(err)
		}
		return steps[0]
	}
	step := pull()
	if !step.Pull || step.Action != resource.ActionReplace || step.Rollout == nil || !step.Rollout.Rolling {
		t.Fatalf("the plan is %s, pull %t, %+v", step.Action, step.Pull, step.Rollout)
	}
	if err := s.local.Apply(t.Context(), step, spec); err != nil {
		t.Fatal(err)
	}
	if got := s.instances(); !slices.Equal(got, names(2)) {
		t.Fatalf("Docker has %v, want the next number", got)
	}
	now := s.engine.Container(names(2)[0])
	if now.Image == old.Image || !now.State.Running {
		t.Errorf("what runs is image %s, running %t, and the old was %s", now.Image, now.State.Running, old.Image)
	}
	if err := s.local.Apply(t.Context(), pull(), spec); err != nil {
		t.Fatal(err)
	}
	if got := s.instances(); !slices.Equal(got, names(2)) || s.engine.Container(names(2)[0]).ID != now.ID {
		t.Errorf("with nothing newer, Docker has %v", got)
	}
}

// What a plan says of a tag that may have moved is what the step does if it has. If
// it read what runs as what the spec wants, it would plan to roll, and a volume two
// containers mustn't share would then stop the step at the last moment.
func TestAPullOverSharedDataPlansToStopFirst(t *testing.T) {
	s := newStack(t)
	spec := app("postgres:17")
	s.deploy(t, webRef, spec)
	s.engine.Registry["postgres:17"] = moved("b", "/var/lib/postgresql/data")
	steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: spec, Pull: true, Run: true}})
	if err != nil {
		t.Fatal(err)
	}
	if r := steps[0].Rollout; r == nil || r.Rolling || steps[0].Downtime == 0 {
		t.Fatalf("the plan is %+v, with %s of downtime, want it to stop first", r, steps[0].Downtime)
	}
	if err := s.local.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatalf("the step the plan promised failed: %v", err)
	}
	if got := s.instances(); !slices.Equal(got, names(1)) || !s.engine.Container(names(1)[0]).State.Running {
		t.Errorf("Docker has %v", got)
	}
}

// keptLogs records what a step keeps, among what it asks of Docker.
type keptLogs struct {
	mu     sync.Mutex
	said   []string
	failed map[string]bool
}

func (k *keptLogs) note(what string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.said = append(k.said, what)
}

func (k *keptLogs) Begin(_ context.Context, c *docker.Container, failed bool) container.LogKeep {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.said = append(k.said, "begin "+c.ID+" running="+strconv.FormatBool(c.State.Running))
	if k.failed == nil {
		k.failed = map[string]bool{}
	}
	k.failed[c.ID] = failed
	return keptLog{k}
}

type keptLog struct{ k *keptLogs }

func (l keptLog) Finish(_ context.Context, c *docker.Container) {
	l.k.note("finish " + c.ID + " running=" + strconv.FormatBool(c.State.Running))
}

// watch adds what Docker is asked to stop and remove to what k records.
func (k *keptLogs) watch(e *dockertest.Engine) {
	e.Intercept(func(r *http.Request) error {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case r.Method == http.MethodPost && parts[len(parts)-1] == "stop":
			k.note("stop " + parts[len(parts)-2])
		case r.Method == http.MethodDelete && parts[len(parts)-2] == "containers":
			k.note("remove " + parts[len(parts)-1])
		}
		return nil
	})
}

// A step keeps what a container said before it goes, however it goes: most of it
// while it still runs, and the rest once it has stopped, before it's removed.
func TestEachWayAContainerGoesKeepsItsLog(t *testing.T) {
	want := func(id string) []string {
		return []string{"begin " + id + " running=true", "stop " + id, "finish " + id + " running=false", "remove " + id}
	}
	for name, spec := range map[string]func(image string) *resourcev1.Service{
		"rolling": app,
		"stopping first": func(image string) *resourcev1.Service {
			a := app(image)
			a.GetApp().Ports = []*resourcev1.PortMapping{{HostPort: 8080, ContainerPort: 80}}
			return a
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			keeper := &keptLogs{}
			s.rt.Logs = keeper
			s.deploy(t, webRef, spec("nginx:1.27"))
			old := s.engine.Newest(docker.Project(webRef.Scope), "web").ID
			if len(keeper.said) != 0 {
				t.Fatalf("it kept a log with nothing replaced: %v", keeper.said)
			}
			keeper.watch(s.engine)
			s.deploy(t, webRef, spec("nginx:1.28"))
			s.engine.Intercept(nil)
			if got := keeper.said; !slices.Equal(got, want(old)) {
				t.Errorf("it did %v, want %v", got, want(old))
			}
			if keeper.failed[old] {
				t.Error("an ordinary replace counted as failed")
			}
		})
	}
	t.Run("a failed restart", func(t *testing.T) {
		s := newStack(t)
		spec := app("nginx:1.27")
		s.deploy(t, webRef, spec)
		keeper := &keptLogs{}
		s.rt.Logs = keeper
		s.engine.SetExits("nginx:1.27", true)
		step := s.plan(t, webRef, spec, "chg_1")
		if err := s.local.Apply(t.Context(), step, spec); err == nil {
			t.Fatal("a restart that can't stay up went through")
		}
		// All there is to say why, as what it made goes, and nothing rolls back.
		var made string
		for id, failed := range keeper.failed {
			if !failed {
				t.Errorf("what a failed restart made, %s, didn't count as failed", id)
			}
			made = id
		}
		if made == "" || s.engine.Container(made) != nil {
			t.Errorf("it kept %v, and should have kept what it made, and removed it", keeper.said)
		}
	})
	t.Run("deleting it", func(t *testing.T) {
		s := newStack(t)
		s.deploy(t, webRef, app("nginx:1.27"))
		old := s.engine.Newest(docker.Project(webRef.Scope), "web").ID
		keeper := &keptLogs{}
		s.rt.Logs = keeper
		keeper.watch(s.engine)
		if err := s.rt.Remove(t.Context(), service.Target(webRef)); err != nil {
			t.Fatal(err)
		}
		s.engine.Intercept(nil)
		if got := keeper.said; !slices.Equal(got, want(old)) {
			t.Errorf("it did %v, want %v", got, want(old))
		}
	})
}

// A stop-first step's stated downtime has what it waits for a container's last lines.
func TestAStopFirstPlanCountsWaitingForLastLines(t *testing.T) {
	s := newStack(t)
	spec := app("postgres:17")
	s.deploy(t, webRef, spec)
	next := app("postgres:17")
	next.GetApp().Env = map[string]string{"A": "1"}
	step := s.plan(t, webRef, next, "")
	if step.Rollout == nil || step.Rollout.Rolling {
		t.Fatalf("it should stop first: %+v", step.Rollout)
	}
	if want := container.DefaultRecreateDowntime; step.Downtime != want {
		t.Errorf("its downtime is %s, want %s", step.Downtime, want)
	}
}

// What a rollout has a container under for now isn't what a person knows it by.
func TestPlainNameIsWhatAContainerGoesBy(t *testing.T) {
	for name, want := range map[string]string{
		"/env_a-web-3":                                "env_a-web-3",
		"env_a-web-3":                                 "env_a-web-3",
		"/stopping_0123456789ab_env_a-web-3":          "env_a-web-3",
		"/0123456789ab_env_a-web-1":                   "env_a-web-1",
		"/stopping_0123456789ab_ba9876543210_env-w-1": "env-w-1",
		"/stopping_soon-web-1":                        "stopping_soon-web-1",
	} {
		if got := container.PlainName(name); got != want {
			t.Errorf("PlainName(%q) = %q, want %q", name, got, want)
		}
	}
}

// A rolling step marks what it stops before it asks Docker to stop it, as Docker goes on
// stopping it however the agent fares, and the mark is what says so.
func TestARollingStepMarksWhatItStopsFirst(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	old := s.engine.Newest(docker.Project(webRef.Scope), "web").ID
	var named atomic.Value
	s.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/"+old+"/stop") {
			named.Store(s.engine.Container(old).Name)
		}
		return nil
	})
	defer s.engine.Intercept(nil)
	s.deploy(t, webRef, app("nginx:1.28"))
	if got, _ := named.Load().(string); !strings.HasPrefix(got, "/stopping_") {
		t.Errorf("as Docker was asked to stop it, it was named %q", got)
	}
}

// A soak of the agent's own check is at least three of them, however short the soak.
func TestASoakTakesThreeOfTheAgentsChecks(t *testing.T) {
	s := newStack(t)
	s.rt.HealthWait = 2 * time.Second
	var probes atomic.Int32
	s.rt.Probe = func(context.Context, *health.Check, string) (health.Result, error) {
		probes.Add(1)
		return health.Result{Passed: true, Output: "ok"}, nil
	}
	check := &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 80, Path: "/healthz"}, Interval: "1s", Timeout: "1s"}
	old, next := app("nginx:1.27"), app("nginx:1.28")
	old.GetApp().Healthcheck, next.GetApp().Healthcheck = check, check
	s.deploy(t, webRef, old)
	s.rt.Soak = time.Millisecond
	probes.Store(0)
	var atHealthy atomic.Int32
	s.rt.Points = func(name string) {
		if name == container.PointHealthy {
			atHealthy.Store(probes.Load())
		}
	}
	s.deploy(t, webRef, next)
	if n := atHealthy.Load(); n < 3 {
		t.Errorf("it called it healthy after %d of its checks", n)
	}
}

// A soak has to hold: a failure along the way starts it again, whichever check it is.
func TestAFailureStartsASoakAgain(t *testing.T) {
	const soak, partway = 400 * time.Millisecond, 100 * time.Millisecond
	t.Run("the agent's check", func(t *testing.T) {
		s := newStack(t)
		s.rt.HealthWait = 5 * time.Second
		var begun, failedAt atomic.Int64
		s.rt.Probe = func(context.Context, *health.Check, string) (health.Result, error) {
			if at := begun.Load(); at != 0 && time.Now().UnixNano() >= at+partway.Nanoseconds() && failedAt.CompareAndSwap(0, time.Now().UnixNano()) {
				return health.Result{Output: "GET /healthz on port 80 answered 503"}, nil
			}
			return health.Result{Passed: true, Output: "ok"}, nil
		}
		check := &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 80, Path: "/healthz"}, Interval: "1s", Timeout: "1s", Retries: 100}
		old, next := app("nginx:1.27"), app("nginx:1.28")
		old.GetApp().Healthcheck, next.GetApp().Healthcheck = check, check
		s.deploy(t, webRef, old)
		s.rt.Soak = soak
		begun.Store(time.Now().UnixNano())
		s.deploy(t, webRef, next)
		if at := failedAt.Load(); at == 0 || time.Since(time.Unix(0, at)) < soak {
			t.Errorf("it moved on %s after a check failed, within a soak of %s", time.Since(time.Unix(0, at)), soak)
		}
	})
	t.Run("Docker's check", func(t *testing.T) {
		s := newStack(t)
		s.rt.HealthWait = 5 * time.Second
		s.engine.Registry["checked:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"app"}, Healthcheck: &docker.Healthcheck{Test: []string{"CMD", "true"}}}}
		s.engine.Registry["checked:2"] = s.engine.Registry["checked:1"]
		s.deploy(t, webRef, app("checked:1"))
		s.rt.Soak = soak
		project := docker.Project(webRef.Scope)
		first := s.engine.Newest(project, "web").ID
		var back atomic.Int64
		timer := time.AfterFunc(partway, func() {
			name := strings.TrimPrefix(s.engine.Newest(project, "web").Name, "/")
			if s.engine.Newest(project, "web").ID == first {
				return
			}
			s.engine.SetHealth(name, "starting", "")
			time.Sleep(50 * time.Millisecond)
			s.engine.SetHealth(name, "healthy", "ok")
			back.Store(time.Now().UnixNano())
		})
		defer timer.Stop()
		s.deploy(t, webRef, app("checked:2"))
		if at := back.Load(); at == 0 || time.Since(time.Unix(0, at)) < soak-20*time.Millisecond {
			t.Errorf("it moved on %s after Docker's check came back, within a soak of %s", time.Since(time.Unix(0, at)), soak)
		}
	})
}

// A check that never made it says what it last said, whichever way it ran out of time.
func TestACheckThatTookTooLongSaysWhatItSaid(t *testing.T) {
	s := newStack(t)
	s.rt.HealthWait = 80 * time.Millisecond
	s.rt.Probe = func(context.Context, *health.Check, string) (health.Result, error) {
		return health.Result{Output: "GET /healthz on port 80 answered 503 Service Unavailable"}, nil
	}
	spec := app("nginx:1.28")
	spec.GetApp().Healthcheck = &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 80, Path: "/healthz"}, Interval: "1s", Timeout: "1s", Retries: 100}
	step := s.plan(t, webRef, spec, "")
	plain, _ := resource.Explain(s.local.Apply(t.Context(), step, spec))
	for _, want := range []string{"so Dockwell stopped waiting: GET /healthz on port 80 answered 503 Service Unavailable", "Fix what it answers, or the check, then try again", "start period"} {
		if !strings.Contains(plain, want) {
			t.Errorf("it says %q, without %q", plain, want)
		}
	}
}

// What isn't running is read by the number a rollout has reached, and not the first.
func TestWhatIsStoppedIsFoundAtItsNumber(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	s.deploy(t, webRef, app("nginx:1.28"))
	now := s.engine.Newest(docker.Project(webRef.Scope), "web")
	if strings.HasSuffix(now.Name, "-1") {
		t.Fatalf("it should have rolled, and is %s", now.Name)
	}
	s.engine.Stop(strings.TrimPrefix(now.Name, "/"))
	if got, err := s.rt.NotRunning(t.Context(), service.Target(webRef)); err != nil || got == "" {
		t.Errorf("NotRunning = %q, %v, with %s stopped", got, err, now.Name)
	}
}
