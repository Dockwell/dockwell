// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// Validate twice gives what Validate once does, for every kind on an
// agent, as Local.Plan and Local.Apply run it again.
func TestValidateIsIdempotent(t *testing.T) {
	full := func() *resourcev1.Container {
		return &resourcev1.Container{
			Image: "ghcr.io/acme/web:1.4.2", Entrypoint: []string{""}, WorkingDir: "/app/../srv/",
			Env: map[string]string{"A": "1"}, Labels: map[string]string{"x": "y"}, Networks: []string{"z", "bridge", "a"},
			Ports: []*resourcev1.PortMapping{
				{HostPort: 90, ContainerPort: 80, HostIp: "::ffff:1.2.3.4"},
				{HostPort: 80, ContainerPort: 80, Protocol: resourcev1.Protocol_PROTOCOL_UDP, HostIp: "2001:0db8::1"},
				{HostPort: 70, ContainerPort: 80},
			},
			Mounts: []*resourcev1.Mount{
				{Source: &resourcev1.Mount_HostPath{HostPath: "/srv//a/"}, Target: "/z/", Relabel: resourcev1.Relabel_RELABEL_SHARED},
				{Source: &resourcev1.Mount_Volume{Volume: "v"}, Target: "/a/b/../c", ReadOnly: true},
			},
			Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "true"}, Interval: "90s", Timeout: "1m0s", StartPeriod: "1500ms", Retries: 2},
		}
	}
	bridgeOnly := full()
	bridgeOnly.Networks = []string{"bridge"}
	r := resource.NewRegistry(&container.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{}, &firewall.Driver{})
	for _, tc := range []struct {
		kind resource.Kind
		spec proto.Message
	}{
		{container.Kind, full()},
		{container.Kind, bridgeOnly},
		{service.Kind, &resourcev1.Service{Runs: &resourcev1.Service_App{App: full()}}},
		{volume.Kind, &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "ro,bind", "device": "/srv/media/"}, Labels: map[string]string{"a": "b"}}},
		{network.Kind, &resourcev1.Network{Options: map[string]string{"a": "b"}, Internal: true}},
		{firewall.Kind, &resourcev1.FirewallRule{Port: 80, Sources: []string{"10.0.0.0/8", "192.168.0.1/24"}}},
	} {
		if err := r.Validate(tc.kind, tc.spec); err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		once := proto.Clone(tc.spec)
		if err := r.Validate(tc.kind, tc.spec); err != nil {
			t.Errorf("%s: a second Validate refuses what the first took: %v", tc.kind, err)
			continue
		}
		if !resource.Equal(once, tc.spec) {
			t.Errorf("%s: a second Validate changed it: %v", tc.kind, resource.Diff(once, tc.spec))
		}
	}
}
