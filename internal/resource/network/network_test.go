// SPDX-License-Identifier: AGPL-3.0-only

package network_test

import (
	"slices"
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

// A link network's members are services of other environments, joined and left in
// place, answering to the app's name for what it needs, read back as such, and let
// go of before the network goes (ADR 0084). Its subnet is the one it was given.
func TestALinkNetworksMembersJoinInPlace(t *testing.T) {
	const shared = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q7N"
	e := dockertest.New(t)
	e.Registry["postgres:17"] = dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}}
	c := e.Client()
	if err := c.PullImage(t.Context(), "postgres:17", nil); err != nil {
		t.Fatal(err)
	}
	dbName := docker.ContainerName(docker.Project(shared), "db")
	id, err := c.CreateContainer(t.Context(), dbName, &docker.CreateRequest{Config: docker.Config{
		Image: "postgres:17", Labels: map[string]string{docker.LabelProject: docker.Project(shared), docker.LabelService: "db"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	l := resource.Local{Drivers: resource.NewRegistry(&network.Driver{Client: c})}
	ref := resource.Ref{Kind: network.Kind, Scope: env, Name: "db-link"}
	spec := func(members ...*resourcev1.NetworkMember) *resourcev1.Network {
		s := &resourcev1.Network{Internal: true, Subnet: "10.233.0.8/29", Members: members}
		if err := l.Drivers.Validate(network.Kind, s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	apply := func(s *resourcev1.Network, want resource.Action) {
		t.Helper()
		steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: s}})
		if err != nil {
			t.Fatal(err)
		}
		if steps[0].Action != want {
			t.Fatalf("planned %v (%v), want %v", steps[0].Action, steps[0].Changes, want)
		}
		if want != resource.ActionNone {
			if err := l.Apply(t.Context(), steps[0], s); err != nil {
				t.Fatal(err)
			}
		}
	}
	aliases := func() []string {
		t.Helper()
		ct := e.Container(dbName)
		if ct == nil {
			t.Fatal("the database's container is gone")
		}
		if ep := ct.NetworkSettings.Networks[network.TargetOf(ref).Name]; ep != nil {
			return ep.Aliases
		}
		return nil
	}
	db := &resourcev1.NetworkMember{Environment: shared, Service: "db", Alias: "db"}
	apply(spec(db), resource.ActionCreate)
	if n := e.Network(network.TargetOf(ref).Name); n == nil || len(n.IPAM.Config) == 0 || n.IPAM.Config[0].Subnet != "10.233.0.8/29" {
		t.Fatalf("the network is %+v, want its subnet", n)
	}
	if got := aliases(); !slices.Contains(got, "db") {
		t.Fatalf("the shared database answers to %v on the link network, want db", got)
	}
	apply(spec(db), resource.ActionNone)

	// Another name for it moves it; none takes it off, in place, as it runs.
	apply(spec(&resourcev1.NetworkMember{Environment: shared, Service: "db", Alias: "postgres"}), resource.ActionUpdate)
	if got := aliases(); !slices.Contains(got, "postgres") || slices.Contains(got, "db") {
		t.Errorf("after the update it answers to %v, want postgres", got)
	}
	apply(spec(), resource.ActionUpdate)
	if got := aliases(); got != nil {
		t.Errorf("taken off, it's still on the network as %v", got)
	}
	if e.Container(dbName).State.Status != "running" {
		t.Error("joining and leaving stopped the database")
	}

	// The network goes with its member on it, which leaves first.
	apply(spec(db), resource.ActionUpdate)
	if err := l.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionDelete}}, nil); err != nil {
		t.Fatalf("deleting a link network with its member on it = %v", err)
	}
	if e.Network(network.TargetOf(ref).Name) != nil || aliases() != nil {
		t.Error("the link network or its member's place on it is still there")
	}

	for want, bad := range map[string]*resourcev1.Network{
		"by its ID":  {Members: []*resourcev1.NetworkMember{{Environment: "shared", Service: "db", Alias: "db"}}},
		"answers to": {Members: []*resourcev1.NetworkMember{{Environment: shared, Service: "db"}}},
		"twice":      {Members: []*resourcev1.NetworkMember{db, db}},
		"IPv4 range": {Subnet: "10.233.0.9/29"},
	} {
		if err := l.Drivers.Validate(network.Kind, bad); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}
