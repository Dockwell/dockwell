// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// coolifyServer is a server where Coolify runs: its own containers, each started
// always, as its installer runs them, and an app it deployed.
func coolifyServer(t *testing.T) *dockertest.Engine {
	t.Helper()
	e := dockertest.New(t)
	for _, name := range []string{"coolify", "coolify-db", "coolify-redis", "coolify-realtime", "coolify-proxy"} {
		e.AddContainer(docker.Container{
			Name: "/" + name, Config: docker.Config{Image: "ghcr.io/coollabsio/" + name + ":4.7.5"},
			HostConfig: docker.HostConfig{RestartPolicy: docker.RestartPolicy{Name: "always"}},
		})
	}
	// A sentinel that's stopped and stays so has nothing to stop.
	e.AddContainer(docker.Container{
		Name: "/coolify-sentinel", Config: docker.Config{Image: "ghcr.io/coollabsio/sentinel:0.0.16"},
		State: docker.State{Status: "exited"}, HostConfig: docker.HostConfig{RestartPolicy: docker.RestartPolicy{Name: "no"}},
	})
	e.AddContainer(docker.Container{
		Name: "/ghost-k4s9", Config: docker.Config{Image: "ghost:5"},
		HostConfig: docker.HostConfig{RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"}},
	})
	return e
}

func names(parts []resource.PanelPart) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Name)
	}
	return out
}

// Stopping Coolify stops its own containers, the panel first, each to stay stopped
// with Docker and the server; leaves its proxy and what it ran; and its undo starts
// each again as it stood, its state last stopped, first started.
func TestStoppingCoolifyAndStartingItAgain(t *testing.T) {
	e := coolifyServer(t)
	s := Stopper{Docker: e.Client()}
	var said []string
	say := func(line string) { said = append(said, line) }

	plan, left, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelCoolify})
	if err != nil || !left {
		t.Fatalf("planning the stop = %v, %v", left, err)
	}
	if got := names(plan.Parts); !slices.Equal(got, []string{"coolify", "coolify-realtime", "coolify-redis", "coolify-db"}) || plan.Stays != "coolify-proxy" {
		t.Fatalf("the stop stops %v and leaves %q", got, plan.Stays)
	}
	if p := plan.Parts[0]; !p.WasRunning || p.WasRestart != "always" || p.Swarm {
		t.Errorf("coolify stood as %+v", p)
	}
	if err := s.ApplyPanel(t.Context(), *plan, say); err != nil {
		t.Fatal(err)
	}
	for _, name := range names(plan.Parts) {
		if c := e.Container(name); c.State.Running || c.HostConfig.RestartPolicy.Name != "no" {
			t.Errorf("%s is left %v, restart %q", name, c.State.Running, c.HostConfig.RestartPolicy.Name)
		}
	}
	for _, name := range []string{"coolify-proxy", "ghost-k4s9"} {
		if c := e.Container(name); !c.State.Running {
			t.Errorf("%s was stopped", name)
		}
	}
	if len(said) != 4 || said[0] != "Stopping coolify" {
		t.Errorf("the step said %q", said)
	}
	// Planned again, as a resume or a second stop does, there's nothing left.
	if _, left, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelCoolify}); err != nil || left {
		t.Errorf("planning it again = %v, %v", left, err)
	}
	if _, left, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelCoolify, Parts: plan.Parts}); err != nil || left {
		t.Errorf("resuming it = %v, %v", left, err)
	}

	back, ok := plan.Undone()
	if !ok {
		t.Fatal("a stop has no undo")
	}
	start, left, err := s.PlanPanel(t.Context(), back)
	if err != nil || !left || !start.Start {
		t.Fatalf("planning the start = %+v, %v, %v", start, left, err)
	}
	if got := names(start.Parts); !slices.Equal(got, []string{"coolify-db", "coolify-redis", "coolify-realtime", "coolify"}) {
		t.Errorf("the start goes in the order %v", got)
	}
	if err := s.ApplyPanel(t.Context(), *start, say); err != nil {
		t.Fatal(err)
	}
	for _, name := range names(plan.Parts) {
		if c := e.Container(name); !c.State.Running || c.HostConfig.RestartPolicy.Name != "always" {
			t.Errorf("started again, %s is %v, restart %q", name, c.State.Running, c.HostConfig.RestartPolicy.Name)
		}
	}
	if _, left, err := s.PlanPanel(t.Context(), back); err != nil || left {
		t.Errorf("planning the start again = %v, %v", left, err)
	}
}

// Dokploy's own Swarm services scale to none, and back to what each ran; its
// Traefik, a container, stays.
func TestStoppingDokploysServices(t *testing.T) {
	e := dockertest.New(t)
	for i, name := range []string{"dokploy", "dokploy-postgres", "dokploy-redis", "forum-q1_web"} {
		svc := docker.Service{ID: "s" + string(rune('1'+i))}
		n := uint64(1)
		svc.Spec.Name = name
		svc.Spec.Mode.Replicated = &struct {
			Replicas *uint64 `json:"Replicas,omitempty"`
		}{Replicas: &n}
		e.Services = append(e.Services, svc)
	}
	e.AddContainer(docker.Container{Name: "/dokploy-traefik", Config: docker.Config{Image: "traefik:v3.1"}})
	s := Stopper{Docker: e.Client()}
	plan, left, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelDokploy})
	if err != nil || !left || plan.Stays != "dokploy-traefik" || !slices.Equal(names(plan.Parts), []string{"dokploy", "dokploy-redis", "dokploy-postgres"}) {
		t.Fatalf("the stop = %+v, %v, %v", plan, left, err)
	}
	if err := s.ApplyPanel(t.Context(), *plan, func(string) {}); err != nil {
		t.Fatal(err)
	}
	replicasOf := func(name string) uint64 {
		svc, err := e.Client().InspectService(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := svc.Spec.Mode.Replicas()
		return n
	}
	for _, name := range names(plan.Parts) {
		if n := replicasOf(name); n != 0 {
			t.Errorf("%s runs %d", name, n)
		}
	}
	if replicasOf("forum-q1_web") != 1 {
		t.Error("an app Dokploy ran was scaled")
	}
	back, _ := plan.Undone()
	start, _, err := s.PlanPanel(t.Context(), back)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPanel(t.Context(), *start, func(string) {}); err != nil {
		t.Fatal(err)
	}
	for _, name := range names(plan.Parts) {
		if n := replicasOf(name); n != 1 {
			t.Errorf("started again, %s runs %d", name, n)
		}
	}
}

// A plan's parts come from the control plane, so the agent acts on none but the
// panel's own, never its proxy, and puts back only what a stop reads.
func TestAStopActsOnThePanelsOwnAlone(t *testing.T) {
	e := coolifyServer(t)
	s := Stopper{Docker: e.Client()}
	for name, ask := range map[string]resource.PanelAsked{
		"an app":       {Kind: resource.PanelCoolify, Start: true, Parts: []resource.PanelPart{{Name: "ghost-k4s9", WasRunning: true}}},
		"its proxy":    {Kind: resource.PanelCoolify, Parts: []resource.PanelPart{{Name: "coolify-proxy", WasRunning: true}}},
		"another's":    {Kind: resource.PanelDokploy, Start: true, Parts: []resource.PanelPart{{Name: "coolify", WasRunning: true}}},
		"a policy":     {Kind: resource.PanelCoolify, Start: true, Parts: []resource.PanelPart{{Name: "coolify", WasRestart: "always --privileged"}}},
		"many":         {Kind: resource.PanelDokploy, Start: true, Parts: []resource.PanelPart{{Name: "dokploy", Swarm: true, WasReplicas: 1000}}},
		"no such kind": {Kind: "portainer"},
	} {
		if _, _, err := s.PlanPanel(t.Context(), ask); err == nil {
			t.Errorf("%s: planned", name)
		}
	}
	if _, _, err := (Stopper{Docker: dockertest.New(t).Client()}).PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelCoolify}); err == nil ||
		!strings.Contains(err.Error(), "Coolify doesn't run here") {
		t.Errorf("a server with no Coolify = %v", err)
	}
}

// An app's Swarm service scales to none as Dockwell's copy starts, and back to what
// it ran; the agent acts on none but a service Dokploy labelled as an app's.
func TestScalingADokployAppsService(t *testing.T) {
	e := dockertest.New(t)
	service := func(name string, labels map[string]string, n uint64) docker.Service {
		svc := docker.Service{ID: name}
		svc.Spec.Name, svc.Spec.Labels = name, labels
		svc.Spec.Mode.Replicated = &struct {
			Replicas *uint64 `json:"Replicas,omitempty"`
		}{Replicas: &n}
		return svc
	}
	e.Services = []docker.Service{
		service("dokploy", nil, 1),
		service("shop-k2m4", map[string]string{"dokploy.application": "shop", "dokploy.project": "store"}, 2),
		service("someone-elses", nil, 1),
	}
	s := Stopper{Docker: e.Client()}
	ask := resource.PanelAsked{Kind: resource.PanelDokploy, Apps: true, Parts: []resource.PanelPart{{Name: "shop-k2m4"}}}
	plan, left, err := s.PlanPanel(t.Context(), ask)
	if err != nil || !left || !plan.Apps || plan.Parts[0].WasReplicas != 2 {
		t.Fatalf("the scale = %+v, %v, %v", plan, left, err)
	}
	if err := s.ApplyPanel(t.Context(), *plan, func(string) {}); err != nil {
		t.Fatal(err)
	}
	replicas := func() uint64 {
		svc, _ := e.Client().InspectService(t.Context(), "shop-k2m4")
		n, _ := svc.Spec.Mode.Replicas()
		return n
	}
	if replicas() != 0 {
		t.Error("the app's service still runs")
	}
	back, _ := plan.Undone()
	start, _, err := s.PlanPanel(t.Context(), back)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPanel(t.Context(), *start, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if replicas() != 2 {
		t.Errorf("scaled back, it runs %d", replicas())
	}
	for _, name := range []string{"dokploy", "someone-elses", "dokploy-traefik"} {
		if _, _, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelDokploy, Apps: true, Parts: []resource.PanelPart{{Name: name}}}); err == nil {
			t.Errorf("%s was scaled", name)
		}
	}
	if _, _, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelCoolify, Apps: true, Parts: []resource.PanelPart{{Name: "shop-k2m4"}}}); err == nil {
		t.Error("Coolify's apps were scaled as Swarm services")
	}
}

// Started from Dockwell with no stop's plan, each of the panel's own that's stopped
// takes back the restart policy its installer gives it: Coolify's sentinel
// restarts unless stopped, and the rest always.
func TestAStartGivesEachItsOwnPolicy(t *testing.T) {
	e := coolifyServer(t)
	s := Stopper{Docker: e.Client()}
	plan, _, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelCoolify})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPanel(t.Context(), *plan, func(string) {}); err != nil {
		t.Fatal(err)
	}
	start, left, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelCoolify, Start: true})
	if err != nil || !left {
		t.Fatalf("the start = %v, %v", left, err)
	}
	if err := s.ApplyPanel(t.Context(), *start, func(string) {}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"coolify": "always", "coolify-db": "always", "coolify-sentinel": "unless-stopped"} {
		if c := e.Container(name); !c.State.Running || c.HostConfig.RestartPolicy.Name != want {
			t.Errorf("%s is %v, restart %q, want %q", name, c.State.Running, c.HostConfig.RestartPolicy.Name, want)
		}
	}
}

// A scale to none waits for the service's task to stop, as Swarm stops it a while
// after the spec changes: what takes its place, such as a copy of a database on
// the same data, never starts beside it. A task that doesn't stop fails the step.
func TestAScaleWaitsForItsTaskToStop(t *testing.T) {
	e := dockertest.New(t)
	n := uint64(1)
	svc := docker.Service{ID: "s1"}
	svc.Spec.Name, svc.Spec.Labels = "db-k2m4", map[string]string{"dokploy.application": "db", "dokploy.project": "store"}
	svc.Spec.Mode.Replicated = &struct {
		Replicas *uint64 `json:"Replicas,omitempty"`
	}{Replicas: &n}
	e.Services = []docker.Service{svc}
	e.AddContainer(docker.Container{
		Name: "/db-k2m4.1.x7k2", Config: docker.Config{Image: "postgres:17", Labels: map[string]string{"com.docker.swarm.service.name": "db-k2m4"}},
		State: docker.State{Status: "running", Running: true},
	})
	e.TaskShutdown = 400 * time.Millisecond
	s := Stopper{Docker: e.Client()}
	plan, _, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelDokploy, Apps: true, Parts: []resource.PanelPart{{Name: "db-k2m4"}}})
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	var said []string
	if err := s.ApplyPanel(t.Context(), *plan, func(s string) { said = append(said, s) }); err != nil {
		t.Fatal(err)
	}
	if c := e.Container("db-k2m4.1.x7k2"); c.State.Running || time.Since(began) < 400*time.Millisecond {
		t.Errorf("the scale ended after %s with its task running %v", time.Since(began), c.State.Running)
	}
	if !slices.Contains(said, "Waiting for the task of db-k2m4 to stop") {
		t.Errorf("the step said %v", said)
	}

	// Scaled back, the task runs again; one that won't stop fails the step.
	back, _ := plan.Undone()
	start, _, err := s.PlanPanel(t.Context(), back)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPanel(t.Context(), *start, func(string) {}); err != nil {
		t.Fatal(err)
	}
	e.TaskShutdown = time.Hour
	s.TaskWait = 300 * time.Millisecond
	if err := s.ApplyPanel(t.Context(), *plan, func(string) {}); err == nil || !strings.Contains(err.Error(), "still has a task that isn't over") {
		t.Errorf("a task that doesn't stop = %v", err)
	}
}

// A stop that resumes, after the agent restarted mid-wait, finds the service asking
// for none with a task not over, here one starting on another node: that isn't
// done, so it plans the step again, and the step waits for it. A start waits for
// such a task too, as Swarm's scale up starts one at once.
func TestAStopThatResumesWaitsForWhatIsntOver(t *testing.T) {
	e := dockertest.New(t)
	n := uint64(0)
	svc := docker.Service{ID: "s1"}
	svc.Spec.Name, svc.Spec.Labels = "db-k2m4", map[string]string{"dokploy.application": "db", "dokploy.project": "store"}
	svc.Spec.Mode.Replicated = &struct {
		Replicas *uint64 `json:"Replicas,omitempty"`
	}{Replicas: &n}
	e.Services = []docker.Service{svc}
	var starting docker.Task
	starting.ID, starting.NodeID, starting.Status.State = "t2", "node-2", "starting"
	e.Tasks = map[string][]docker.Task{"db-k2m4": {starting}}
	s := Stopper{Docker: e.Client(), TaskWait: 300 * time.Millisecond}
	ask := resource.PanelAsked{Kind: resource.PanelDokploy, Apps: true, Parts: []resource.PanelPart{{Name: "db-k2m4", Swarm: true, WasRunning: true, WasReplicas: 1}}}
	plan, left, err := s.PlanPanel(t.Context(), ask)
	if err != nil || !left {
		t.Fatalf("a stop resumed with a task starting = %v, %v", left, err)
	}
	if err := s.ApplyPanel(t.Context(), *plan, func(string) {}); err == nil || !strings.Contains(err.Error(), "isn't over") {
		t.Errorf("the stop with a task starting = %v", err)
	}
	back, _ := plan.Undone()
	start, _, err := s.PlanPanel(t.Context(), back)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPanel(t.Context(), *start, func(string) {}); err == nil || !strings.Contains(err.Error(), "so Dockwell doesn't scale it back up beside it") {
		t.Errorf("a start with a task not over = %v", err)
	}
	// Once it's over, the stop is done, and the start scales up.
	e.Tasks = nil
	if _, left, err := s.PlanPanel(t.Context(), ask); err != nil || left {
		t.Errorf("with nothing left, the stop = %v, %v", left, err)
	}
	if err := s.ApplyPanel(t.Context(), *start, func(string) {}); err != nil {
		t.Error(err)
	}
}

// The wait is the service's own grace period and a minute more, not a fixed one.
func TestTheWaitFollowsTheGracePeriod(t *testing.T) {
	e := dockertest.New(t)
	n, grace := uint64(0), int64(2*time.Hour)
	svc := docker.Service{ID: "s1"}
	svc.Spec.Name = "db-k2m4"
	svc.Spec.Mode.Replicated = &struct {
		Replicas *uint64 `json:"Replicas,omitempty"`
	}{Replicas: &n}
	svc.Spec.TaskTemplate.ContainerSpec.StopGracePeriod = &grace
	e.Services = []docker.Service{svc}
	var running docker.Task
	running.ID, running.Status.State = "t1", "running"
	e.Tasks = map[string][]docker.Task{"db-k2m4": {running}}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := Stopper{Docker: e.Client()}.untilNoTask(ctx, "db-k2m4", func(string) {}, "after %s")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("with a grace period of two hours, the wait ended %v", err)
	}
}

// A service whose networks an old client named beside its task template keeps
// them in the plan too, and a start puts them back where Docker reads them.
func TestAServicesOldStyleNetworksComeBack(t *testing.T) {
	e := dockertest.New(t)
	n := uint64(1)
	svc := docker.Service{ID: "s1"}
	svc.Spec.Name, svc.Spec.Labels = "shop-k2m4", map[string]string{"dokploy.application": "shop", "dokploy.project": "store"}
	svc.Spec.Mode.Replicated = &struct {
		Replicas *uint64 `json:"Replicas,omitempty"`
	}{Replicas: &n}
	svc.Spec.Networks = json.RawMessage(`[{"Target":"k3net"}]`)
	e.Services = []docker.Service{svc}
	s := Stopper{Docker: e.Client()}
	plan, _, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelDokploy, Apps: true, Parts: []resource.PanelPart{{Name: "shop-k2m4"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Parts[0].WasNetworks; got != `[{"Target":"k3net"}]` {
		t.Fatalf("the plan keeps %q", got)
	}
	if err := s.ApplyPanel(t.Context(), *plan, func(string) {}); err != nil {
		t.Fatal(err)
	}
	back, _ := plan.Undone()
	start, _, err := s.PlanPanel(t.Context(), back)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPanel(t.Context(), *start, func(string) {}); err != nil {
		t.Fatal(err)
	}
	read, err := e.Client().InspectService(t.Context(), "shop-k2m4")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(read.Spec.TaskTemplate.Networks); got != `[{"Target":"k3net"}]` || len(read.Spec.Networks) != 0 {
		t.Errorf("started again, it joins %s, and beside it %s", got, read.Spec.Networks)
	}
}

// What may run beside a copy: a container running, restarting, paused or being
// removed. One whose start failed, which Swarm leaves behind, isn't, nor one that
// exited or died.
func TestWhichContainersOfATaskAreLive(t *testing.T) {
	for state, live := range map[string]bool{
		"running": true, "restarting": true, "paused": true, "removing": true,
		"created": false, "exited": false, "dead": false,
	} {
		if containerLive(state) != live {
			t.Errorf("a container %s reads live %v", state, !live)
		}
	}
}

// A task whose start failed leaves its container made and never started, which a
// scale to none doesn't take away: neither the stop nor the start that undoes it
// waits on that, so the app isn't left at none.
func TestALeftoverContainerFromAFailedStartBlocksNothing(t *testing.T) {
	e := dockertest.New(t)
	n := uint64(1)
	svc := docker.Service{ID: "s1"}
	svc.Spec.Name, svc.Spec.Labels = "shop-k2m4", map[string]string{"dokploy.application": "shop", "dokploy.project": "store"}
	svc.Spec.Mode.Replicated = &struct {
		Replicas *uint64 `json:"Replicas,omitempty"`
	}{Replicas: &n}
	e.Services = []docker.Service{svc}
	labels := map[string]string{"com.docker.swarm.service.name": "shop-k2m4"}
	e.AddContainer(docker.Container{Name: "/shop-k2m4.1.good", Config: docker.Config{Image: "nginx:1.27", Labels: labels}, State: docker.State{Status: "running", Running: true}})
	e.AddContainer(docker.Container{
		Name: "/shop-k2m4.1.broken", Config: docker.Config{Image: "nginx:1.27", Labels: labels},
		State: docker.State{Status: "created", Error: "exec: \"/no-such-binary\": stat /no-such-binary: no such file or directory"},
	})
	s := Stopper{Docker: e.Client(), TaskWait: 2 * time.Second}
	plan, _, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelDokploy, Apps: true, Parts: []resource.PanelPart{{Name: "shop-k2m4"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPanel(t.Context(), *plan, func(string) {}); err != nil {
		t.Fatalf("the stop with a leftover container = %v", err)
	}
	back, _ := plan.Undone()
	start, _, err := s.PlanPanel(t.Context(), back)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPanel(t.Context(), *start, func(string) {}); err != nil {
		t.Errorf("the start with a leftover container = %v", err)
	}
}
