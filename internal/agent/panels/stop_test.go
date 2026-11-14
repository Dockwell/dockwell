// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"slices"
	"strings"
	"testing"

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

// Handing over the domains stops the panel's proxy alone, so Dockwell's ingress
// takes ports 80 and 443, and its undo starts it again as it stood.
func TestStoppingThePanelsProxyAlone(t *testing.T) {
	e := coolifyServer(t)
	s := Stopper{Docker: e.Client()}
	plan, left, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelCoolify, Proxy: true})
	if err != nil || !left || !plan.Proxy || plan.Stays != "" || !slices.Equal(names(plan.Parts), []string{"coolify-proxy"}) {
		t.Fatalf("the proxy's stop = %+v, %v, %v", plan, left, err)
	}
	if err := s.ApplyPanel(t.Context(), *plan, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if c := e.Container("coolify-proxy"); c.State.Running || c.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the proxy is left %v, restart %q", c.State.Running, c.HostConfig.RestartPolicy.Name)
	}
	if c := e.Container("coolify"); !c.State.Running {
		t.Error("the panel itself was stopped")
	}
	back, _ := plan.Undone()
	if !back.Proxy {
		t.Fatalf("the undo asks %+v", back)
	}
	start, _, err := s.PlanPanel(t.Context(), back)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPanel(t.Context(), *start, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if c := e.Container("coolify-proxy"); !c.State.Running || c.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("started again, the proxy is %v, restart %q", c.State.Running, c.HostConfig.RestartPolicy.Name)
	}
	// A proxy's stop acts on nothing else of the panel's.
	if _, _, err := s.PlanPanel(t.Context(), resource.PanelAsked{Kind: resource.PanelCoolify, Proxy: true, Start: true, Parts: []resource.PanelPart{{Name: "coolify", WasRunning: true}}}); err == nil {
		t.Error("a proxy's start acted on coolify")
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
