// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
)

// fakeIngress says a service has an address, and holds the public ports.
type fakeIngress struct {
	address string
	held    map[uint32]bool
	asked   []ingress.Instance
	routed  bool
	// What a rollout did, in order, and what Reachable answers.
	added, drained, removed []ingress.Instance
	unreachable             error
}

// Endpoint gives each instance an address of its own, counting up from address.
func (f *fakeIngress) Endpoint(_ context.Context, i ingress.Instance) (string, docker.EndpointSettings, bool, error) {
	f.asked = append(f.asked, i)
	if f.address == "" {
		return "", docker.EndpointSettings{}, false, nil
	}
	a := netip.MustParseAddr(f.address)
	for range i.Number - 1 {
		a = a.Next()
	}
	return docker.IngressNetwork, docker.EndpointSettings{IPAMConfig: &docker.EndpointIPAM{IPv4Address: a.String()}}, true, nil
}

func (f *fakeIngress) PublicPort(_ context.Context, port uint32, _ bool) (bool, error) {
	return f.held[port], nil
}

func (f *fakeIngress) RoutedAfter(context.Context, ingress.Instance, []resource.Desired) (bool, error) {
	return f.routed, nil
}

func (f *fakeIngress) AddUpstream(_ context.Context, i ingress.Instance) error {
	f.added = append(f.added, i)
	return nil
}

func (f *fakeIngress) Move(_ context.Context, to ingress.Instance, _ []ingress.Instance) error {
	f.added = append(f.added, to)
	return nil
}

func (f *fakeIngress) Drain(_ context.Context, i ingress.Instance) error {
	f.drained = append(f.drained, i)
	return nil
}

func (f *fakeIngress) RemoveUpstream(_ context.Context, i ingress.Instance) error {
	f.removed = append(f.removed, i)
	return nil
}

func (*fakeIngress) Routing(context.Context, ingress.Service) (ingress.Routing, error) {
	return ingress.Routing{Routed: true}, nil
}

func (f *fakeIngress) Reachable(context.Context, ingress.Instance) error { return f.unreachable }

func (f *fakeIngress) Hold(ingress.Service) func() { return func() {} }

func withIngress(t *testing.T, ing *fakeIngress) (*Docker, *dockertest.Engine) {
	t.Helper()
	e := dockertest.New(t)
	e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	e.AddNetwork(docker.Network{Name: docker.IngressNetwork, Driver: "bridge", Internal: true, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "172.30.0.0/16"}}}})
	return &Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Ingress: ing}, e
}

var svc = Target{Name: "env_x-web-1", Project: "env_x", Service: "web", Scoped: true}

// A new instance joins the ingress network at an address of its own, and takes
// over from the old one only once it is healthy.
func TestARollingReplaceMovesTrafficOnceHealthy(t *testing.T) {
	ing := &fakeIngress{address: "172.30.0.2"}
	d, e := withIngress(t, ing)
	spec := &resourcev1.Container{Image: "nginx:1.27", Env: map[string]string{"A": "1"}}
	if err := d.Run(t.Context(), svc, spec, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	first := e.Container("env_x-web-1")
	spec.Env["A"] = "2"
	if err := d.Run(t.Context(), svc, spec, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	second := e.Container("env_x-web-2")
	if second == nil || first.ID == second.ID {
		t.Fatalf("the service has %v, want a new container of number 2", e.Instances("env_x", "web"))
	}
	if e.Container("env_x-web-1") != nil {
		t.Errorf("the old container is still there: %v", e.Instances("env_x", "web"))
	}
	ep := second.NetworkSettings.Networks[docker.IngressNetwork]
	if ep == nil || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address != "172.30.0.3" {
		t.Errorf("the new container is on the network as %+v", ep)
	}
	if !second.State.Running {
		t.Errorf("the new container isn't running: %+v", second.State)
	}
	old, next := ingress.Instance{Project: "env_x", Service: "web", Number: 1}, ingress.Instance{Project: "env_x", Service: "web", Number: 2}
	if got := ing.asked; len(got) != 2 || got[0] != old || got[1] != next {
		t.Errorf("asked for %+v", got)
	}
	if !slices.Contains(ing.added, next) || !slices.Equal(ing.drained, []ingress.Instance{old}) || !slices.Equal(ing.removed, []ingress.Instance{old}) {
		t.Errorf("added %v, drained %v, removed %v", ing.added, ing.drained, ing.removed)
	}
}

func TestAServiceWithNoRoutesDoesntJoin(t *testing.T) {
	d, e := withIngress(t, &fakeIngress{})
	if err := d.Run(t.Context(), svc, &resourcev1.Container{Image: "nginx:1.27"}, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	if _, on := e.Container(svc.Name).NetworkSettings.Networks[docker.IngressNetwork]; on {
		t.Error("a service with no routes joined the ingress network")
	}
}

func TestAContainerOnItsOwnDoesntAskToJoin(t *testing.T) {
	ing := &fakeIngress{address: "172.30.0.2"}
	d, e := withIngress(t, ing)
	own := Target{Name: "web", Project: "srv_x", Service: "web"}
	if err := d.Run(t.Context(), own, &resourcev1.Container{Image: "nginx:1.27"}, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	if len(ing.asked) != 0 {
		t.Errorf("asked for %+v", ing.asked)
	}
	if _, on := e.Container("web").NetworkSettings.Networks[docker.IngressNetwork]; on {
		t.Error("a container on its own joined the ingress network")
	}
}

// The ingress's network is the ingress's to give and take, so a service on it
// doesn't read as drift, whichever networks it names.
func TestTheIngressNetworkIsntDrift(t *testing.T) {
	for _, tc := range []struct {
		name     string
		networks []string
		want     []string
	}{
		{"Docker's bridge and the ingress", []string{"bridge", docker.IngressNetwork}, nil},
		{"its own network and the ingress", []string{"env_x_back", docker.IngressNetwork}, []string{"back"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &docker.Container{Config: docker.Config{Image: "nginx:1.27"}}
			c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{}
			for _, n := range tc.networks {
				c.NetworkSettings.Networks[n] = &docker.EndpointSettings{}
			}
			got := Project(c, nil, svc).GetNetworks()
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Errorf("networks = %v, want %v", got, tc.want)
			}
		})
	}
}

// A container on it could take the address of a service that is stopped, and
// get its traffic.
func TestNoContainerCanNameTheIngressNetwork(t *testing.T) {
	d := &Driver{}
	err := d.Validate(&resourcev1.Container{Image: "nginx:1.27", Networks: []string{docker.IngressNetwork}})
	if err == nil || !strings.Contains(err.Error(), "the ingress's own") {
		t.Errorf("Validate = %v, want it refused", err)
	}
}

// Otherwise the new container fails to start only once the old one is gone.
func TestAServiceCantPublishWhatTheIngressHolds(t *testing.T) {
	d, _ := withIngress(t, &fakeIngress{held: map[uint32]bool{80: true, 443: true}})
	refused := &resourcev1.Container{Image: "nginx:1.27", Ports: []*resourcev1.PortMapping{{HostPort: 443, ContainerPort: 8443}}}
	err := d.CheckHost(t.Context(), svc, refused)
	if err == nil || !strings.Contains(err.Error(), "holds port 443") {
		t.Errorf("CheckHost = %v, want it refused, naming the port", err)
	}
	if err := d.Run(t.Context(), svc, refused, resource.Decision{}); err == nil {
		t.Error("Run took a port the ingress holds")
	}
	if err := d.CheckHost(t.Context(), svc, &resourcev1.Container{Image: "nginx:1.27", Ports: []*resourcev1.PortMapping{{HostPort: 8080, ContainerPort: 80}}}); err != nil {
		t.Errorf("CheckHost of another port = %v", err)
	}
}

// A route left on a service would stop reaching it on the server's own network, so
// the plan says so. Only a plan can tell, as it knows its other steps.
func TestAServiceWithARouteCantGoOntoTheServersNetwork(t *testing.T) {
	inPlan := resource.WithDesired(t.Context(), []resource.Desired{})
	on := &resourcev1.Container{Image: "nginx:1.27", HostNetwork: true}
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		routed  bool
		spec    *resourcev1.Container
		refused bool
	}{
		{name: "a route left on it", ctx: inPlan, routed: true, spec: on, refused: true},
		{name: "no route left on it", ctx: inPlan, spec: on},
		{name: "a route, on its own networks", ctx: inPlan, routed: true, spec: &resourcev1.Container{Image: "nginx:1.27"}},
		{name: "as a step applies", ctx: t.Context(), routed: true, spec: on},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := withIngress(t, &fakeIngress{routed: tc.routed})
			err := d.CheckHost(tc.ctx, svc, tc.spec)
			switch {
			case !tc.refused && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.refused && err == nil:
				t.Fatal("the plan went ahead")
			case tc.refused && !strings.Contains(err.Error(), "host_port"):
				t.Errorf("%q doesn't say what to do", err)
			}
		})
	}
}

// When a plan moves its route to a port on the server too, the service's step can
// come first, with the instance still holding an address. Docker would refuse that
// container on the ingress's network, and the old one would go on running.
func TestAServiceOnTheServersNetworkDoesntJoinTheIngress(t *testing.T) {
	ing := &fakeIngress{address: "172.30.0.2"}
	d, e := withIngress(t, ing)
	if err := d.Run(t.Context(), svc, &resourcev1.Container{Image: "nginx:1.27", HostNetwork: true}, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	c := e.Container(svc.Name)
	if _, on := c.NetworkSettings.Networks[docker.IngressNetwork]; on {
		t.Errorf("it's on the ingress's network: %+v", c.NetworkSettings.Networks)
	}
	if !c.State.Running {
		t.Errorf("it isn't running: %+v", c.State)
	}
	if len(ing.asked) != 0 {
		t.Errorf("it asked the ingress for an address: %+v", ing.asked)
	}
}
