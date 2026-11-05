// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"context"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// What a rollout leans on in Docker itself, on a real engine: that a rename holds,
// that its names, numbers and labels read back, that a container's address takes a
// connection from the server, and that Docker's own check says what it printed. The
// same test runs on the fake engine, so it is known to say what it means before CI
// has run it, and so the fake is held to what Docker does whenever CI does.

type dockerScene struct {
	client  *docker.Client
	local   resource.Local
	ref     resource.Ref
	project string
	image   string
	// real is an engine that runs what it is told, for what the fake can't.
	real bool
}

func (s *dockerScene) spec(deploy, path string) *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: s.image, Env: map[string]string{"DEPLOY": deploy}, Rollout: &resourcev1.Rollout{Drain: "1s"},
		Healthcheck: &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 80, Path: path}, Interval: "1s", Timeout: "2s", Retries: 3},
	}}}
}

func (s *dockerScene) plan(t *testing.T, spec *resourcev1.Service, restart string) resource.Step {
	t.Helper()
	steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: s.ref, Spec: spec, Restart: restart}})
	if err != nil {
		t.Fatal(err)
	}
	return steps[0]
}

func (s *dockerScene) apply(t *testing.T, step resource.Step, spec *resourcev1.Service) {
	t.Helper()
	if err := s.local.Apply(t.Context(), step, spec); err != nil {
		t.Fatal(err)
	}
}

// one checks that all there is of the service is its one container of a number,
// by the name, the number label and the state Docker reads back, and returns it.
func (s *dockerScene) one(t *testing.T, number int) *docker.Container {
	t.Helper()
	list, err := s.client.ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject + "=" + s.project, docker.LabelService + "=web"}})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range list {
		names = append(names, strings.TrimPrefix(c.Names[0], "/"))
	}
	want := docker.InstanceName(s.project, "web", number)
	if !slices.Equal(names, []string{want}) {
		t.Fatalf("Docker has %v, want only %s", names, want)
	}
	c, err := s.client.ContainerNamed(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	if !c.State.Running {
		t.Errorf("%s is %s, want running", want, c.State.Status)
	}
	if got := c.Config.Labels[docker.LabelNumber]; got != strconv.Itoa(number) {
		t.Errorf("%s's number label is %q", want, got)
	}
	return c
}

func (s *dockerScene) rollingDeploy(t *testing.T) {
	// The first: Docker's own check is off, and the agent's is on its label.
	good := s.spec("1", "/")
	step := s.plan(t, good, "")
	if step.Action != resource.ActionCreate {
		t.Fatalf("the first plan = %s", step.Action)
	}
	s.apply(t, step, good)
	first := s.one(t, 1)
	if hc := first.Config.Healthcheck; hc == nil || !slices.Equal(hc.Test, []string{"NONE"}) {
		t.Errorf("Docker's own check should be off for the agent's, and is %+v", hc)
	}
	if first.Config.Labels[health.Label] == "" {
		t.Error("the agent's check isn't on the container's label")
	}
	if first.State.Health != nil && first.State.Health.Status != "" && first.State.Health.Status != "none" {
		t.Errorf("Docker checks it too: %+v", first.State.Health)
	}
	if again := s.plan(t, good, ""); again.Action != resource.ActionNone {
		t.Fatalf("right after it was made, the plan is %s: %+v", again.Action, again.Changes)
	}

	// A rolling deploy takes the next number, and leaves nothing of the old.
	good = s.spec("2", "/")
	step = s.plan(t, good, "")
	if step.Action != resource.ActionReplace || step.Downtime != 0 || step.Rollout == nil || !step.Rollout.Rolling {
		t.Fatalf("a deploy with nothing shared = %+v", step)
	}
	s.apply(t, step, good)
	second := s.one(t, 2)
	if second.ID == first.ID {
		t.Error("the deploy kept the container")
	}
	if again := s.plan(t, good, ""); again.Action != resource.ActionNone {
		t.Errorf("after the deploy, the plan is %s: %+v", again.Action, again.Changes)
	}

	// One that never passes: what ran goes on, and the plan back removes it alone.
	bad := s.spec("3", "/nope")
	err := s.local.Apply(t.Context(), s.plan(t, bad, ""), bad)
	if err == nil || !strings.Contains(err.Error(), "fails its health check") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a deploy whose check answers 404 = %v", err)
	}
	if c, err := s.client.ContainerNamed(t.Context(), second.Name[1:]); err != nil || !c.State.Running || c.ID != second.ID {
		t.Fatalf("after a failed deploy, what ran is %+v, %v", c, err)
	}
	back := s.plan(t, good, "")
	if back.Action == resource.ActionNone || back.Rollout == nil || len(back.Rollout.Others) != 1 {
		t.Fatalf("the plan back should remove what failed, and is %+v", back)
	}
	s.apply(t, back, good)
	if c := s.one(t, 2); c.ID != second.ID {
		t.Errorf("going back replaced what ran, %s, with %s", second.ID, c.ID)
	}

	// A restart is the same, once.
	step = s.plan(t, good, "chg_1")
	if step.Action != resource.ActionReplace || step.Restart != "chg_1" || len(step.Changes) != 0 {
		t.Fatalf("planning a restart = %+v", step)
	}
	s.apply(t, step, good)
	if got := s.one(t, 3).Config.Labels[container.LabelRestart]; got != "chg_1" {
		t.Errorf("the restart's container says %q", got)
	}
	if again := s.plan(t, good, "chg_1"); again.Action != resource.ActionNone {
		t.Errorf("planning the same restart again = %s", again.Action)
	}

	// Two can't publish one port, so it stops first, and keeps its number, through
	// the rename that takes.
	port := uint32(freeDockerPort(t))
	published := s.spec("4", "/")
	published.GetApp().Ports = []*resourcev1.PortMapping{{HostPort: port, ContainerPort: 80, HostIp: "127.0.0.1"}}
	s.apply(t, s.plan(t, published, ""), published)
	before := s.one(t, 4)
	published.GetApp().Env["DEPLOY"] = "5"
	step = s.plan(t, published, "")
	if step.Rollout == nil || step.Rollout.Rolling || len(step.Rollout.Ports) != 1 || step.Downtime == 0 {
		t.Fatalf("a deploy that publishes one port = %+v", step)
	}
	s.apply(t, step, published)
	if c := s.one(t, 4); c.ID == before.ID {
		t.Error("the deploy kept the container")
	}
	if again := s.plan(t, published, ""); again.Action != resource.ActionNone {
		t.Errorf("after a stop-first deploy, the plan is %s: %+v", again.Action, again.Changes)
	}
	if !s.real {
		return
	}

	// Docker's own check: it says what the command printed, and it fails the deploy
	// the same. Only an engine that runs things can show that.
	own := s.spec("6", "/")
	own.GetApp().Ports = nil
	own.GetApp().Healthcheck = &resourcev1.Healthcheck{Test: []string{"CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1/nope"}, Interval: "1s", Timeout: "2s", Retries: 2}
	err = s.local.Apply(t.Context(), s.plan(t, own, ""), own)
	if err == nil || !strings.Contains(err.Error(), "fails its health check") || !strings.Contains(err.Error(), "404") {
		t.Errorf("a deploy whose command check prints a 404 = %v", err)
	}
	if c, err := s.client.ContainerNamed(t.Context(), docker.InstanceName(s.project, "web", 4)); err != nil || !c.State.Running {
		t.Errorf("after a failed deploy, what ran is %+v, %v", c, err)
	}
}

func freeDockerPort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestARollingDeployOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	scope := dockertest.Scope(t)
	dockertest.Cleanup(t, client, docker.Project(scope))
	const image = "nginx:1.27-alpine"
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	c := container.Driver{Runtime: &container.Docker{Client: client}}
	s := &dockerScene{
		client: client, local: resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: c})},
		ref: resource.Ref{Kind: service.Kind, Scope: scope, Name: "web"}, project: docker.Project(scope), image: image, real: true,
	}
	s.rollingDeploy(t)
}

func TestARollingDeployOnTheFakeEngine(t *testing.T) {
	e := dockertest.New(t)
	registry(e)
	probe := func(_ context.Context, c *health.Check, _ string) (health.Result, error) {
		if c.HTTP.Path == "/" {
			return health.Result{Passed: true, Output: "GET / on port 80 answered 200 OK", At: time.Now()}, nil
		}
		return health.Result{Output: "GET " + c.HTTP.Path + " on port 80 answered 404 Not Found", At: time.Now()}, nil
	}
	c := container.Driver{Runtime: &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Probe: probe}}
	s := &dockerScene{
		client: e.Client(), local: resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: c})},
		ref: webRef, project: project, image: "nginx:1.27",
	}
	s.rollingDeploy(t)
}

// hostNetworkDeploy holds a service on the server's own network to stopping first,
// on whatever engine it has. Its command listens on nothing, so it takes no port
// from whatever else is on the machine.
func (s *dockerScene) hostNetworkDeploy(t *testing.T) {
	t.Helper()
	spec := func(deploy string) *resourcev1.Service {
		return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: s.image, HostNetwork: true, Command: []string{"sleep", "600"}, Env: map[string]string{"DEPLOY": deploy},
		}}}
	}
	first := spec("1")
	s.apply(t, s.plan(t, first, ""), first)
	next := spec("2")
	step := s.plan(t, next, "")
	if r := step.Rollout; r == nil || r.Rolling || !r.HostNetwork {
		t.Fatalf("the plan is %+v, want it to stop first, for the server's own network", r)
	}
	s.apply(t, step, next)
	name := docker.InstanceName(s.project, s.ref.Name, 1)
	c, err := s.client.InspectContainer(t.Context(), name)
	if err != nil {
		t.Fatalf("it should keep its number: %v", err)
	}
	if !c.State.Running || c.HostConfig.NetworkMode != "host" || !slices.Contains(c.Config.Env, "DEPLOY=2") {
		t.Errorf("it is %+v on %s, with %v", c.State, c.HostConfig.NetworkMode, c.Config.Env)
	}
}

func TestAServiceOnTheServersOwnNetworkOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	scope := dockertest.Scope(t)
	dockertest.Cleanup(t, client, docker.Project(scope))
	const image = "nginx:1.27-alpine"
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	c := container.Driver{Runtime: &container.Docker{Client: client}}
	s := &dockerScene{
		client: client, local: resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: c})},
		ref: resource.Ref{Kind: service.Kind, Scope: scope, Name: "web"}, project: docker.Project(scope), image: image, real: true,
	}
	s.hostNetworkDeploy(t)
}

func TestAServiceOnTheServersOwnNetworkOnTheFakeEngine(t *testing.T) {
	e := dockertest.New(t)
	registry(e)
	c := container.Driver{Runtime: &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}}
	s := &dockerScene{
		client: e.Client(), local: resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: c})},
		ref: webRef, project: project, image: "nginx:1.27",
	}
	s.hostNetworkDeploy(t)
}
