// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource/container"
)

func ports(bindings ...string) []*resourcev1.PortMapping {
	var out []*resourcev1.PortMapping
	for i, b := range bindings {
		ip, proto, _ := strings.Cut(b, "/")
		p := &resourcev1.PortMapping{HostPort: 80, ContainerPort: uint32(80 + i), HostIp: ip}
		if proto == "udp" {
			p.Protocol = resourcev1.Protocol_PROTOCOL_UDP
		}
		out = append(out, p)
	}
	return out
}

// A port Docker can't bind on two addresses together is refused: every
// address overlaps any other, and 0.0.0.0 and :: every address of their
// own family. Otherwise the new container would fail to start only once
// the old one was gone.
func TestPortsDockerCantBindTogetherAreRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		bindings []string
		want     string
	}{
		"every and one":    {[]string{"", "127.0.0.1"}, "server port 80 is published on every address and on 127.0.0.1, which Docker can't bind together"},
		"one and every":    {[]string{"::1", ""}, "server port 80 is published on ::1 and on every address"},
		"IPv4's every":     {[]string{"0.0.0.0", "10.0.0.1"}, "server port 80 is published on 0.0.0.0 (every IPv4 address) and on 10.0.0.1"},
		"IPv6's every":     {[]string{"::1", "::"}, "server port 80 is published on ::1 and on :: (every IPv6 address)"},
		"every and IPv4's": {[]string{"", "0.0.0.0"}, "server port 80 is published on every address and on 0.0.0.0 (every IPv4 address)"},
		"twice":            {[]string{"127.0.0.1", "127.0.0.1"}, "server port 80 is published twice on 127.0.0.1"},
		"twice everywhere": {[]string{"", ""}, "server port 80 is published twice on every address"},
		"written two ways": {[]string{"::1", "0:0:0:0:0:0:0:1"}, "server port 80 is published twice on ::1"},
		"udp":              {[]string{"/udp", "127.0.0.1/udp"}, "server port 80/udp is published on every address and on 127.0.0.1"},
	} {
		t.Run(name, func(t *testing.T) {
			err := container.Validate(&resourcev1.Container{Image: "nginx:1.27", Ports: ports(tc.bindings...)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
	for name, bindings := range map[string][]string{
		"each family's every": {"0.0.0.0", "::"},
		"two addresses":       {"127.0.0.1", "127.0.0.2"},
		"two families":        {"127.0.0.1", "::1"},
		"two protocols":       {"", "/udp"},
		"IPv4's every and v6": {"0.0.0.0", "::1"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := container.Validate(&resourcev1.Container{Image: "nginx:1.27", Ports: ports(bindings...)}); err != nil {
				t.Errorf("Validate = %v, want ports Docker binds together taken", err)
			}
		})
	}
}

// The fake engine binds ports as Docker does: a container whose ports
// overlap, its own or another's, doesn't start.
func TestTheFakeBindsPortsAsDockerDoes(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	c := e.Client()
	if err := c.PullImage(t.Context(), "nginx:1.27", nil); err != nil {
		t.Fatal(err)
	}
	run := func(name string, bindings map[string][]docker.PortBinding) error {
		id, err := c.CreateContainer(t.Context(), name, &docker.CreateRequest{
			Config: docker.Config{Image: "nginx:1.27"}, HostConfig: docker.HostConfig{PortBindings: bindings},
		})
		if err != nil {
			return err
		}
		return c.StartContainer(t.Context(), id)
	}
	if err := run("both", map[string][]docker.PortBinding{"80/tcp": {{HostPort: "80"}}, "81/tcp": {{HostIP: "127.0.0.1", HostPort: "80"}}}); err == nil ||
		!strings.Contains(err.Error(), "port is already allocated") {
		t.Errorf("starting a container with every address and 127.0.0.1 on one port = %v", err)
	}
	if err := run("v4", map[string][]docker.PortBinding{"80/tcp": {{HostIP: "0.0.0.0", HostPort: "8080"}}}); err != nil {
		t.Fatal(err)
	}
	if err := run("v6", map[string][]docker.PortBinding{"80/tcp": {{HostIP: "::", HostPort: "8080"}}}); err != nil {
		t.Errorf("starting beside 0.0.0.0 on ::, which Docker binds together = %v", err)
	}
	if err := run("one", map[string][]docker.PortBinding{"80/tcp": {{HostIP: "10.0.0.1", HostPort: "8080"}}}); err == nil {
		t.Error("started on 10.0.0.1 beside 0.0.0.0 on one port")
	}
}
