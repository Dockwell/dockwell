// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// none is no network at all, so a container on it reaches nothing a person must
// approve, and it can't be beside another, as Docker has it. Nor can host. The last
// check of #92 found none needing a person, in words that called it a network
// others could reach.
func TestNoneIsNoNetwork(t *testing.T) {
	d := &container.Driver{}
	spec := &resourcev1.Container{Image: "nginx:1.27", Networks: []string{"none"}}
	if err := d.Validate(spec); err != nil {
		t.Fatal(err)
	}
	if got := d.HostAccess(spec); len(got) != 0 {
		t.Errorf("a container on none reaches %+v, want nothing", got)
	}
	for _, alone := range []string{"none", "host"} {
		spec := &resourcev1.Container{Image: "nginx:1.27", Networks: []string{alone, "front"}}
		if err := d.Validate(spec); err == nil || !strings.Contains(err.Error(), "List "+alone+" alone") {
			t.Errorf("%s beside another network = %v, want it refused", alone, err)
		}
	}
}

// A container on its own names Docker's networks, which must be there by that
// name: a plan says so, rather than the apply, and a name Docker would take as the
// start of another network's ID is refused as that. The last check of #92 found
// both planning fine and refused only at apply.
func TestAContainersNetworksAreCheckedAsItPlans(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	e.AddNetwork(docker.Network{ID: "cafe", Name: "front", Driver: "bridge"})
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}
	for network, want := range map[string]string{
		"gone": "the network gone isn't on this server, so web can't join it. Make it with docker network create gone",
		"cafe": "the network cafe isn't on this server, so web can't join it. Docker takes cafe for the start of another network's ID",
	} {
		spec := &resourcev1.Container{Image: "nginx:1.27", Networks: []string{network}}
		if err := l.Drivers.Validate(ref.Kind, spec); err != nil {
			t.Fatal(err)
		}
		_, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("planning to join %s = %v, want it refused with %q", network, err, want)
		}
		if strings.Contains(err.Error(), "front") {
			t.Errorf("the refusal names the network whose ID it starts: %v", err)
		}
	}
	spec := &resourcev1.Container{Image: "nginx:1.27", Networks: []string{"front"}}
	apply(t, l, ref, spec)
	unchanged(t, l, ref, spec)
}

// What each refusal of a name taken on a shared network says to do depends on what
// holds the name, and which name it is: the last check of #92 found the advice was
// to change a container_name when the clash was the service's own name, and to stop
// a container when a stopped one counts. A container on its own is held to the names
// on its networks too, as a service is, and a name can't move between two services
// in one change, as the server still runs the one it moves from.
func TestATakenNameSaysWhatFreesIt(t *testing.T) {
	e := dockertest.New(t)
	e.AddNetwork(docker.Network{Name: "edge", Driver: "bridge"})
	envA := "env_01kat4zr6e3v0y5xbkzb5d9q6m"
	onEdge := func(c docker.Container, aliases ...string) {
		c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"edge": {Aliases: aliases}}
		e.AddContainer(c)
	}
	onEdge(docker.Container{
		ID: "a1", Name: "/" + envA + "-db-1",
		Config: docker.Config{Labels: map[string]string{docker.LabelProject: envA, docker.LabelService: "db"}},
	}, "db")
	onEdge(docker.Container{ID: "b2", Name: "/cache"}, "redis")
	d := &container.Docker{Client: e.Client()}
	alone := func(name string) container.Target {
		return container.Target{Name: name, Project: "srv_01kat4zr6e3v0y5xbkzb5d9q6n", Service: name}
	}
	svc := func(env, name string) container.Target {
		return container.Target{Name: env + "-" + name + "-1", Project: env, Service: name, Scoped: true}
	}
	envB := "env_01kat4zr6e3v0y5xbkzb5d9q6p"
	for name, c := range map[string]struct {
		target container.Target
		spec   *resourcev1.Container
		want   []string
	}{
		"a container on its own of a service's name": {
			target: alone("db"), spec: &resourcev1.Container{Networks: []string{"edge"}},
			want: []string{"db would answer to db on the network edge", "It is db, which Dockwell runs elsewhere on this server. Rename one of the two, or take one of them off the network edge"},
		},
		"a container on its own of an alias": {
			target: alone("redis"), spec: &resourcev1.Container{Networks: []string{"edge"}},
			want: []string{"Rename redis, or remove that container with docker rm cache", "Stopping it isn't enough"},
		},
		"a service's own name": {
			target: svc(envB, "cache"), spec: &resourcev1.Container{ExternalNetworks: []string{"edge"}},
			want: []string{"Rename cache, or remove that container with docker rm cache", "Stopping it isn't enough"},
		},
		"a service's other name": {
			target: svc(envB, "web"), spec: &resourcev1.Container{ExternalNetworks: []string{"edge"}, Aliases: []string{"redis"}},
			want: []string{"Give web another name in place of redis, in its aliases or its Compose file's container_name, or remove that container with docker rm cache"},
		},
		"another environment's service": {
			target: svc(envB, "db"), spec: &resourcev1.Container{ExternalNetworks: []string{"edge"}},
			want: []string{"It is db, which Dockwell runs elsewhere on this server"},
		},
		"a name moving between two services": {
			target: svc(envA, "api"), spec: &resourcev1.Container{ExternalNetworks: []string{"edge"}, Aliases: []string{"db"}},
			want: []string{"It is db, of this environment", "a name can't move to api in the same change", "Take the name from db, or delete it, in a change of its own, apply that, then plan this again"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := d.CheckHost(t.Context(), c.target, c.spec)
			for _, want := range c.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("it says %v, want %q", err, want)
				}
			}
			if err != nil && strings.Contains(err.Error(), "stop that container") {
				t.Errorf("it says to stop the container, which doesn't free the name: %v", err)
			}
		})
	}
	// A stopped container keeps its names on its networks.
	e.Stop("cache")
	if err := d.CheckHost(t.Context(), alone("redis"), &resourcev1.Container{Networks: []string{"edge"}}); err == nil {
		t.Error("a stopped container's name is free, want it taken")
	}
	for _, target := range []container.Target{alone("web"), svc(envB, "web")} {
		spec := &resourcev1.Container{Networks: []string{"edge"}}
		if target.Scoped {
			spec = &resourcev1.Container{ExternalNetworks: []string{"edge"}}
		}
		if err := d.CheckHost(t.Context(), target, spec); err != nil {
			t.Errorf("%s, with a name nobody has on the network: %v", target.Name, err)
		}
	}
}

// Two changes applying at once, on the same server, can't both find a name free on
// a shared network and both take it: each checks again as it makes its container,
// one at a time. Here the first change's container is held as Docker makes it, and
// the second change is refused once it is made, rather than made beside it. The
// last check of #92 found both passing, by reading.
func TestTwoChangesCantTakeOneNameAtOnce(t *testing.T) {
	for name, held := range map[string]func(*http.Request) bool{
		// As it makes its container, which the lock stops the second passing.
		"making": func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/containers/create") },
		// Made and not started, with the lock released, as a stop-first deploy leaves
		// it for as long as the old one takes to stop. Docker has no ID for a network
		// on a container until it first starts, so only the network's name finds it.
		"not yet started": func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/start") },
	} {
		t.Run(name, func(t *testing.T) {
			testTwoChangesCantTakeOneName(t, held)
		})
	}
}

// testTwoChangesCantTakeOneName holds the first change at what held says, until the
// second makes a container or a second has gone by.
func testTwoChangesCantTakeOneName(t *testing.T, held func(*http.Request) bool) {
	e := dockertest.New(t)
	images(e)
	e.AddNetwork(docker.Network{Name: "edge", Driver: "bridge"})
	l := resource.Local{Drivers: drivers(e)}
	first := resource.Ref{Kind: service.Kind, Scope: "env_01kat4zr6e3v0y5xbkzb5d9q6m", Name: "api"}
	second := resource.Ref{Kind: service.Kind, Scope: "env_01kat4zr6e3v0y5xbkzb5d9q6p", Name: "web"}
	spec := func() *resourcev1.Service {
		return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: "nginx:1.27", ExternalNetworks: []string{"edge"}, Aliases: []string{"shared"},
		}}}
	}
	var once, hold sync.Once
	secondMakes := make(chan struct{})
	e.Intercept(func(r *http.Request) error {
		if r.Method != http.MethodPost {
			return nil
		}
		if strings.HasSuffix(r.URL.Path, "/containers/create") && strings.Contains(r.URL.Query().Get("name"), docker.Project(second.Scope)) {
			once.Do(func() { close(secondMakes) })
			return nil
		}
		if held(r) {
			// Held until the second is made too, as it would be without the check, or
			// for long enough that it would be by now.
			hold.Do(func() {
				select {
				case <-secondMakes:
				case <-time.After(time.Second):
				}
			})
		}
		return nil
	})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, ref := range []resource.Ref{first, second} {
		s := spec()
		if err := l.Drivers.Validate(ref.Kind, s); err != nil {
			t.Fatal(err)
		}
		steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: s}})
		if err != nil {
			t.Fatalf("plan %s: %v", ref, err)
		}
		wg.Go(func() {
			if i == 1 {
				time.Sleep(50 * time.Millisecond) // the first is making its container by now
			}
			errs[i] = l.Apply(t.Context(), steps[0], s)
		})
	}
	wg.Wait()
	if errs[0] != nil {
		t.Errorf("the first change: %v", errs[0])
	}
	if errs[1] == nil || !strings.Contains(errs[1].Error(), "web would answer to shared on the network edge") {
		t.Errorf("the second change = %v, want it refused for the name the first took", errs[1])
	}
	var on int
	for _, name := range e.Containers() {
		if c := e.Container(name); c != nil && c.NetworkSettings.Networks["edge"] != nil {
			on++
		}
	}
	if on != 1 {
		t.Errorf("%d containers are on edge, answering to shared; want 1", on)
	}
}
