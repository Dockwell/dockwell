// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/ingress"
)

// fakeIngress says a service has an address, and holds the public ports.
type fakeIngress struct {
	address string
	held    map[uint32]bool
	asked   []ingress.Instance
}

func (f *fakeIngress) Endpoint(_ context.Context, i ingress.Instance) (string, docker.EndpointSettings, bool, error) {
	f.asked = append(f.asked, i)
	if f.address == "" {
		return "", docker.EndpointSettings{}, false, nil
	}
	return docker.IngressNetwork, docker.EndpointSettings{IPAMConfig: &docker.EndpointIPAM{IPv4Address: f.address}}, true, nil
}

func (f *fakeIngress) PublicPort(_ context.Context, port uint32, _ bool) (bool, error) {
	return f.held[port], nil
}

func withIngress(t *testing.T, ing *fakeIngress) (*Docker, *dockertest.Engine) {
	t.Helper()
	e := dockertest.New(t)
	e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	e.AddNetwork(docker.Network{Name: docker.IngressNetwork, Driver: "bridge", Internal: true, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "172.30.0.0/16"}}}})
	return &Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Ingress: ing}, e
}

var svc = Target{Name: "env_x-web-1", Project: "env_x", Service: "web", Scoped: true}

// A replaced container serves its routes as the old one did: it takes the
// instance's address, which stays the same.
func TestAReplacedServiceKeepsItsIngressAddress(t *testing.T) {
	ing := &fakeIngress{address: "172.30.0.2"}
	d, e := withIngress(t, ing)
	spec := &resourcev1.Container{Image: "nginx:1.27", Env: map[string]string{"A": "1"}}
	if err := d.Run(t.Context(), svc, spec, false); err != nil {
		t.Fatal(err)
	}
	first := e.Container(svc.Name)
	spec.Env["A"] = "2"
	if err := d.Run(t.Context(), svc, spec, false); err != nil {
		t.Fatal(err)
	}
	second := e.Container(svc.Name)
	if first.ID == second.ID {
		t.Fatal("the container wasn't replaced")
	}
	ep := second.NetworkSettings.Networks[docker.IngressNetwork]
	if ep == nil || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address != "172.30.0.2" {
		t.Errorf("the new container is on the network as %+v", ep)
	}
	if !second.State.Running {
		t.Errorf("the new container isn't running: %+v", second.State)
	}
	if got := ing.asked[0]; got != (ingress.Instance{Project: "env_x", Service: "web", Number: 1}) {
		t.Errorf("asked for %+v", got)
	}
}

func TestAServiceWithNoRoutesDoesntJoin(t *testing.T) {
	d, e := withIngress(t, &fakeIngress{})
	if err := d.Run(t.Context(), svc, &resourcev1.Container{Image: "nginx:1.27"}, false); err != nil {
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
	if err := d.Run(t.Context(), own, &resourcev1.Container{Image: "nginx:1.27"}, false); err != nil {
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
	if err := d.Run(t.Context(), svc, refused, false); err == nil {
		t.Error("Run took a port the ingress holds")
	}
	if err := d.CheckHost(t.Context(), svc, &resourcev1.Container{Image: "nginx:1.27", Ports: []*resourcev1.PortMapping{{HostPort: 8080, ContainerPort: 80}}}); err != nil {
		t.Errorf("CheckHost of another port = %v", err)
	}
}
