// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Docker takes a name as a full ID, then an exact name, then a prefix of
// one ID. A container named by hex digits, such as db, must never read
// back as, stop, remove or change a container whose ID starts with them,
// whether one ID or several do.
func TestNamesNeverReachAnotherContainer(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	l := resource.Local{Drivers: drivers(e)}
	ctx := t.Context()
	someone := e.AddContainer(docker.Container{
		ID: "db", Name: "/someones-app",
		HostConfig: docker.HostConfig{RestartPolicy: docker.RestartPolicy{Name: "always"}},
	})
	e.AddContainer(docker.Container{ID: "a1", Name: "/first"})
	e.AddContainer(docker.Container{ID: "a1", Name: "/second"})
	untouched := func() {
		t.Helper()
		c := e.Container("someones-app")
		switch {
		case c == nil:
			t.Fatal("Dockwell removed someone's container, whose ID starts with db")
		case !c.State.Running:
			t.Fatal("Dockwell stopped someone's container, whose ID starts with db")
		case c.HostConfig.RestartPolicy.Name != "always":
			t.Fatalf("Dockwell changed someone's container's restart policy to %q", c.HostConfig.RestartPolicy.Name)
		}
	}

	for _, name := range []string{"db", "a1"} {
		ref := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: name}
		observed, err := l.Observe(ctx, []resource.Ref{ref})
		if err != nil || observed[0] != nil {
			t.Errorf("observing %s, which isn't there = %v, %v; want nothing", name, observed, err)
		}
	}

	db := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "db"}
	if err := l.Apply(ctx, resource.Step{Ref: db, Decision: resource.Decision{Action: resource.ActionDelete}}, nil); err != nil {
		t.Errorf("deleting db, which isn't there = %v", err)
	}
	untouched()
	spec := &resourcev1.Container{Image: "nginx:1.27", Restart: resourcev1.RestartPolicy_RESTART_POLICY_NO}
	if err := container.Validate(spec); err != nil {
		t.Fatal(err)
	}
	err := l.Apply(ctx, resource.Step{Ref: db, Decision: resource.Decision{Action: resource.ActionUpdate}}, spec)
	if err == nil || !strings.Contains(err.Error(), "there's no container db on this server to change") {
		t.Errorf("changing db in place, which isn't there = %v", err)
	}
	untouched()

	if step := apply(t, l, db, spec); step.Action != resource.ActionCreate {
		t.Errorf("planning db = %s, want a create", step.Action)
	}
	untouched()
	if c := e.Container("db"); c == nil || c.ID == someone.ID {
		t.Fatalf("db is %+v, want a container of its own", c)
	}
	unchanged(t, l, db, spec)
	apply(t, l, db, nil)
	untouched()
	if e.Container("db") != nil {
		t.Error("deleting db left it")
	}
	a1 := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "a1"}
	if step := apply(t, l, a1, spec); step.Action != resource.ActionCreate {
		t.Errorf("planning a1, a prefix two IDs share = %s, want a create", step.Action)
	}
	unchanged(t, l, a1, spec)
}

// An image named without a tag, by hex digits alone, is pulled as
// name:latest, never taken for an image whose ID starts with them.
func TestUntaggedImagesNeverReachAnotherImage(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	c := e.Client()
	if err := c.PullImage(t.Context(), "nginx:1.27", nil); err != nil {
		t.Fatal(err)
	}
	nginx, err := c.InspectImage(t.Context(), "nginx:1.27")
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(nginx.ID, "sha256:")[:8]
	e.Registry[name+":latest"] = dockertest.Image{Config: docker.Config{Cmd: []string{"serve"}}}
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "app"}
	spec := &resourcev1.Container{Image: name}
	apply(t, l, ref, spec)
	got := e.Container("app")
	if got.Image == nginx.ID || e.Pulls != 2 {
		t.Errorf("the image %s ran as nginx, whose ID starts with it, after %d pulls; want %s:latest pulled", name, e.Pulls, name)
	}
	unchanged(t, l, ref, spec)
}

// Validate looks at a name alone, and Docker takes the start of a network's ID
// as readily as its name, so it is the agent's exact lookup that keeps a
// container on its own off the ingress's network, and an environment's. Each
// of these resolves to the network on Docker, as they do on the fake.
func TestNoContainerJoinsANetworkOfItsOwnByItsID(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	id := "8db0c28a6f5f" + strings.Repeat("0", 52)
	e.AddNetwork(docker.Network{ID: id, Name: docker.IngressNetwork, Driver: "bridge", Internal: true})
	envID := "cafe" + strings.Repeat("1", 60)
	e.AddNetwork(docker.Network{ID: envID, Name: "env_01kat4zr6e3v0y5xbkzb5d9q6m_back", Driver: "bridge"})
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}
	for _, n := range []string{id, "8db0c28a6f5f", "8d", "8", envID, "cafe"} {
		spec := &resourcev1.Container{Image: "nginx:1.27", Networks: []string{n}}
		if err := container.Validate(spec); err != nil {
			t.Fatal(err)
		}
		// The start of an ID can match others, which Docker calls ambiguous: a
		// refusal too. Either way, nothing is made.
		err := l.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec)
		refused := err != nil && (strings.Contains(err.Error(), "isn't on this server, so web can't join it") || strings.Contains(err.Error(), "is ambiguous"))
		if !refused {
			t.Errorf("joining %s = %v, want it refused", n, err)
		}
		if c := e.Container("web"); c != nil {
			t.Fatalf("web was made on %s, on %v", n, c.NetworkSettings.Networks)
		}
	}
}

// A container on its own joins Docker's networks by their exact names:
// one that isn't there is refused, never taken for a network whose ID
// starts with its name.
func TestNetworksAreJoinedByName(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	e.AddNetwork(docker.Network{ID: "cafe", Name: "someones-internal", Driver: "bridge", Internal: true})
	e.AddNetwork(docker.Network{Name: "front", Driver: "bridge"})
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}
	spec := &resourcev1.Container{Image: "nginx:1.27", Networks: []string{"cafe"}}
	if err := container.Validate(spec); err != nil {
		t.Fatal(err)
	}
	err := l.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec)
	if err == nil || !strings.Contains(err.Error(), "the network cafe isn't on this server, so web can't join it") {
		t.Errorf("joining cafe, which isn't there = %v", err)
	}
	if c := e.Container("web"); c != nil {
		t.Errorf("web was made, on %v", c.NetworkSettings.Networks)
	}
	spec.Networks = []string{"front"}
	apply(t, l, ref, spec)
	unchanged(t, l, ref, spec)
}

// A container on its own takes the mark a stop-first step gives what it stops, and a
// change that would go in place finishes it, as it does a service's: it takes its name
// back and runs, and the plan says what that costs.
func TestAChangeInPlaceFinishesAMarkedContainerOnItsOwn(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}
	spec := &resourcev1.Container{Image: "nginx:1.27", Restart: resourcev1.RestartPolicy_RESTART_POLICY_NO}
	apply(t, l, ref, spec)
	c := e.Container("web")
	marked := "stopping_" + c.ID[:12] + "_web"
	if err := e.Client().RenameContainer(t.Context(), c.ID, marked); err != nil {
		t.Fatal(err)
	}
	e.Stop(marked)
	want := &resourcev1.Container{Image: "nginx:1.27", Restart: resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS}
	if err := container.Validate(want); err != nil {
		t.Fatal(err)
	}
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: want}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != resource.ActionReplace || steps[0].Effect != resource.EffectRecreate {
		t.Fatalf("a plan calls finishing it %s, %v", steps[0].Action, steps[0].Effect)
	}
	if err := l.Apply(t.Context(), steps[0], want); err != nil {
		t.Fatal(err)
	}
	if got := e.Container("web"); got == nil || !got.State.Running {
		t.Errorf("it is %+v, want it running under its name", got)
	}
	if e.Container(marked) != nil {
		t.Error("what was marked is still there")
	}
	unchanged(t, l, ref, want)
}
