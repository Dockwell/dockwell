// SPDX-License-Identifier: AGPL-3.0-only

package docker_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

func replicas(n uint64) docker.ServiceMode {
	var m docker.ServiceMode
	m.Replicated = &struct {
		Replicas *uint64 `json:"Replicas,omitempty"`
	}{Replicas: &n}
	return m
}

// Scaling a service changes its replicas and nothing else of it: Swarm takes the
// whole spec on an update, so what Dockwell doesn't type goes back as it was,
// numbers exactly, and an update names the version it read.
func TestScalingAServiceKeepsTheRestOfIt(t *testing.T) {
	e := dockertest.New(t)
	svc := docker.Service{ID: "s1", Version: docker.ServiceVersion{Index: 7}}
	svc.Spec.Name, svc.Spec.Mode = "dokploy", replicas(1)
	svc.Spec.TaskTemplate.ContainerSpec.Image = "dokploy/dokploy:v0.31.2"
	e.Services = []docker.Service{svc}
	e.ServiceExtras = map[string]map[string]json.RawMessage{"s1": {
		"EndpointSpec": json.RawMessage(`{"Ports":[{"TargetPort":3000,"PublishedPort":3000}]}`),
		"UpdateConfig": json.RawMessage(`{"Monitor":9007199254740993}`),
	}}
	c := e.Client()
	if err := c.ScaleService(t.Context(), "dokploy", 0); err != nil {
		t.Fatal(err)
	}
	read, err := c.InspectService(t.Context(), "dokploy")
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := read.Spec.Mode.Replicas(); !ok || n != 0 || read.Version.Index != 8 || read.Spec.TaskTemplate.ContainerSpec.Image != "dokploy/dokploy:v0.31.2" {
		t.Errorf("scaled to none, it reads %+v", read)
	}
	sent := e.ServiceUpdates[0]
	for _, kept := range []string{`"PublishedPort":3000`, `"Monitor":9007199254740993`, `"Replicas":0`} {
		if !strings.Contains(sent, kept) {
			t.Errorf("the update %s doesn't keep %s", sent, kept)
		}
	}
	if err := c.ScaleService(t.Context(), "dokploy", 1); err != nil {
		t.Fatal(err)
	}
	if read, _ := c.InspectService(t.Context(), "s1"); read == nil || read.Version.Index != 9 {
		t.Errorf("scaled back, it reads %+v", read)
	}

	// A service that runs one task on each node has no number to scale.
	global := docker.Service{ID: "g1"}
	global.Spec.Name, global.Spec.Mode.Global = "agent", &struct{}{}
	e.Services = append(e.Services, global)
	if err := c.ScaleService(t.Context(), "agent", 0); err == nil || !strings.Contains(err.Error(), "each node") {
		t.Errorf("scaling a global service = %v", err)
	}
}

// Scaling with ports sets the replicas and what's published alone, and keeps the
// rest of the spec as Docker has it, the endpoint's own settings too.
func TestScalingAServiceWithItsPorts(t *testing.T) {
	e := dockertest.New(t)
	svc := docker.Service{ID: "s1", Version: docker.ServiceVersion{Index: 3}}
	svc.Spec.Name, svc.Spec.Mode = "shop-k2m4", replicas(1)
	e.Services = []docker.Service{svc}
	e.ServiceExtras = map[string]map[string]json.RawMessage{"s1": {
		"UpdateConfig": json.RawMessage(`{"Monitor":9007199254740993}`),
	}}
	c := e.Client()
	if err := c.ScaleServicePorts(t.Context(), "shop-k2m4", 0, nil); err != nil {
		t.Fatal(err)
	}
	if sent := e.ServiceUpdates[0]; !strings.Contains(sent, `"Ports":[]`) || !strings.Contains(sent, `"Monitor":9007199254740993`) || !strings.Contains(sent, `"Replicas":0`) {
		t.Errorf("freeing its ports sent %s", sent)
	}
	back := []docker.ServicePort{{Protocol: "tcp", TargetPort: 80, PublishedPort: 8085, PublishMode: "ingress"}}
	if err := c.ScaleServicePorts(t.Context(), "shop-k2m4", 1, back); err != nil {
		t.Fatal(err)
	}
	read, err := c.InspectService(t.Context(), "shop-k2m4")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := read.Spec.Mode.Replicas(); n != 1 || len(read.Spec.EndpointSpec.Ports) != 1 || read.Spec.EndpointSpec.Ports[0] != back[0] {
		t.Errorf("scaled back with its ports, it reads %+v", read.Spec)
	}
}
