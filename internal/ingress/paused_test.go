// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/docker"
)

func TestPauseUpstreamSingleAndMultiInstance(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "web", spec("app.example.com", "", "web", 8080))
	project := docker.Project(envID)
	first := Instance{Project: project, Service: "web", Number: 1}
	second := Instance{Project: project, Service: "web", Number: 2}

	// Single-container pause: keeps the reserved upstream address and marks the route Paused.
	if err := s.PauseUpstream(t.Context(), first); err != nil {
		t.Fatalf("PauseUpstream(first): %v", err)
	}
	routes, err := s.proxy.Routes(t.Context())
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(routes) != 1 || !routes[0].Paused || len(routes[0].Upstreams) != 1 {
		t.Fatalf("after single pause, routes = %+v, want 1 paused route keeping its upstream", routes)
	}
	// Pausing is runtime state, not route spec drift: Observe returns the same spec.
	got, err := s.Observe(t.Context(), routeRef("web"))
	if err != nil {
		t.Fatalf("Observe while paused: %v", err)
	}
	if !proto.Equal(got, spec("app.example.com", "", "web", 8080)) {
		t.Fatalf("Observe while paused = %+v, want unchanged route spec", got)
	}

	// Unpausing via AddUpstream clears Paused and restores normal proxying.
	if err := s.AddUpstream(t.Context(), first); err != nil {
		t.Fatalf("AddUpstream(first): %v", err)
	}
	routes, _ = s.proxy.Routes(t.Context())
	if len(routes) != 1 || routes[0].Paused {
		t.Fatalf("after unpause, routes = %+v, want Paused=false", routes)
	}

	// Multi-instance: pausing one while another is live drops only the paused upstream, leaving Paused=false.
	if _, _, ok, err := s.Endpoint(t.Context(), second); err != nil || !ok {
		t.Fatalf("Endpoint(second) = ok:%v err:%v", ok, err)
	}
	if err := s.AddUpstream(t.Context(), second); err != nil {
		t.Fatalf("AddUpstream(second): %v", err)
	}
	if err := s.PauseUpstream(t.Context(), first); err != nil {
		t.Fatalf("PauseUpstream(first) with second live: %v", err)
	}
	routes, _ = s.proxy.Routes(t.Context())
	if len(routes) != 1 || routes[0].Paused || len(routes[0].Upstreams) != 1 {
		t.Fatalf("after pausing one of two, routes = %+v, want unpaused route with 1 upstream", routes)
	}

	// Pausing the remaining instance transitions the route to Paused=true.
	if err := s.PauseUpstream(t.Context(), second); err != nil {
		t.Fatalf("PauseUpstream(second): %v", err)
	}
	routes, _ = s.proxy.Routes(t.Context())
	if len(routes) != 1 || !routes[0].Paused || len(routes[0].Upstreams) != 1 {
		t.Fatalf("after pausing last live instance, routes = %+v, want Paused=true with 1 upstream", routes)
	}
}

func TestPauseUpstreamRedeployWhilePausedAndNewRoute(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "web", spec("app.example.com", "", "web", 8080))
	project := docker.Project(envID)
	first := Instance{Project: project, Service: "web", Number: 1}
	second := Instance{Project: project, Service: "web", Number: 2}

	if err := s.PauseUpstream(t.Context(), first); err != nil {
		t.Fatalf("PauseUpstream(first): %v", err)
	}
	// Simulate redeploy-while-paused: second container gets a reserved address and replaces first while staying paused.
	if _, _, ok, err := s.Endpoint(t.Context(), second); err != nil || !ok {
		t.Fatalf("Endpoint(second) = ok:%v err:%v", ok, err)
	}
	if err := s.PauseUpstream(t.Context(), second); err != nil {
		t.Fatalf("PauseUpstream(second): %v", err)
	}
	routes, _ := s.proxy.Routes(t.Context())
	if len(routes) != 1 || !routes[0].Paused {
		t.Fatalf("after paused replacement, routes = %+v, want Paused=true", routes)
	}
	addr2 := s.table.Addresses[second.String()]
	if len(routes[0].Upstreams) != 1 || routes[0].Upstreams[0] != addr2+":8080" {
		t.Fatalf("after paused replacement, upstreams = %v, want [%s:8080]", routes[0].Upstreams, addr2)
	}

	// A new route added while the service's container is paused starts Paused=true too.
	s.engine.AddContainer(docker.Container{
		Name: "/" + project + "-web-2",
		Config: docker.Config{Labels: map[string]string{
			docker.LabelProject: project, docker.LabelService: "web", docker.LabelNumber: "2", docker.LabelOneoff: "False",
		}},
		State:           docker.State{Running: true, Paused: true, Status: "paused"},
		NetworkSettings: docker.ContainerNetworkSettings{Networks: map[string]*docker.EndpointSettings{"bridge": {}}},
	})
	_ = s.engine.Client().RemoveContainer(t.Context(), project+"-web-1", true)
	s.apply(t, "web2", spec("app2.example.com", "", "web", 8080))
	routes, _ = s.proxy.Routes(t.Context())
	var found bool
	for _, r := range routes {
		if r.Domain == "app2.example.com" {
			found = true
			if !r.Paused {
				t.Fatalf("new route on paused service has Paused=false, want true")
			}
		}
	}
	if !found {
		t.Fatalf("new route app2.example.com not found in %+v", routes)
	}
}
