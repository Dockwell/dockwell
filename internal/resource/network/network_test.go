// SPDX-License-Identifier: AGPL-3.0-only

package network_test

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
)

const env = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"

// A network is named and labelled as Compose names and labels a project's,
// reads back as its spec whatever options Docker adds, can't change, and
// can't go while a container is on it.
func TestNetworks(t *testing.T) {
	e := dockertest.New(t)
	l := resource.Local{Drivers: resource.NewRegistry(&network.Driver{Client: e.Client()})}
	back := resource.Ref{Kind: network.Kind, Scope: env, Name: "back"}
	spec := &resourcev1.Network{Internal: true, Options: map[string]string{"com.docker.network.bridge.name": "dw-back"}}
	if err := l.Drivers.Validate(network.Kind, spec); err != nil {
		t.Fatal(err)
	}
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: back, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatal(err)
	}
	name := "env_01kat4zr6e3v0y5xbkzb5d9q6m_back"
	n := e.Network(name)
	if n == nil || n.Labels[docker.LabelProject] != "env_01kat4zr6e3v0y5xbkzb5d9q6m" || n.Labels[docker.LabelNetwork] != "back" || !n.Internal {
		t.Fatalf("Docker has %+v, want %s with Compose's labels", n, name)
	}

	// Options Docker adds of its own aren't drift; one added by hand is.
	e.AddNetwork(func() docker.Network {
		c := *n
		c.Options = map[string]string{"com.docker.network.bridge.name": "dw-back", "com.docker.network.enable_ipv4": "true"}
		return c
	}())
	if steps, _ := l.Plan(t.Context(), []resource.Desired{{Ref: back, Spec: spec}}); steps[0].Action != resource.ActionNone {
		t.Errorf("an option Docker added reads as drift: %v", steps[0].Changes)
	}
	changed := &resourcev1.Network{Internal: false}
	_ = l.Drivers.Validate(network.Kind, changed)
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: back, Spec: changed}}); err == nil || !strings.Contains(err.Error(), "docker can't change a network once it is made") {
		t.Errorf("planning a change to a network = %v, want it refused", err)
	}

	e.Registry["busybox:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"sh"}}}
	c := e.Client()
	if err := c.PullImage(t.Context(), "busybox:1", nil); err != nil {
		t.Fatal(err)
	}
	id, err := c.CreateContainer(t.Context(), "app", &docker.CreateRequest{Config: docker.Config{Image: "busybox:1"}, HostConfig: docker.HostConfig{NetworkMode: name}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	del := resource.Step{Ref: back, Decision: resource.Decision{Action: resource.ActionDelete}}
	if err := l.Apply(t.Context(), del, nil); err == nil || !strings.Contains(err.Error(), "containers are still on the network "+name+" (app)") {
		t.Errorf("deleting a network in use = %v", err)
	}
	if err := c.RemoveContainer(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(t.Context(), del, nil); err != nil {
		t.Fatal(err)
	}
	if e.Network(name) != nil {
		t.Error("the deleted network is still there")
	}
}
