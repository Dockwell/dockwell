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

// Dockwell relabels a server path for SELinux, as Docker's z and Z do,
// but not a volume.
func TestOnlyServerPathsAreRelabelled(t *testing.T) {
	err := container.Validate(&resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data", Relabel: resourcev1.Relabel_RELABEL_SHARED},
	}})
	if err == nil || !strings.Contains(err.Error(), "the volume data mounted at /data is relabelled for SELinux, which Dockwell can do for a server path alone") {
		t.Errorf("Validate = %v", err)
	}
}

// An image reference Docker won't take is refused when a change is
// proposed, saying what is wrong, rather than failing as the server pulls
// it and rolling back.
func TestImageReferencesAreChecked(t *testing.T) {
	for _, ok := range []string{
		"nginx", "nginx:1.27", "library/nginx:1.27-alpine", "ghcr.io/acme/web:1.4.2", "registry.acme.dev:5000/team/app_x:v2",
		"localhost/app", "localhost:5000/app:dev", "nginx@sha256:" + strings.Repeat("a", 64), "nginx:1.27@sha256:" + strings.Repeat("0", 64),
		"[::1]:5000/app:1", "My.Registry.io/app:1", "a__b/c-d/e.f:G_1",
	} {
		if err := container.CheckImage(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for ref, want := range map[string]string{
		"":                                  "needs an image",
		"nginx :1.27":                       "has a space in it",
		"nginx:":                            "has the tag \"\"",
		"nginx:.1":                          "has the tag \".1\"",
		"nginx:" + strings.Repeat("a", 129): "A tag is up to 128",
		"nginx@sha256:abc":                  "has the digest \"sha256:abc\"",
		"Nginx:1.27":                        "Write it in lowercase, as nginx:1.27",
		"ghcr.io/Acme/Web:1":                "Write it in lowercase, as ghcr.io/acme/web:1",
		"acme//web":                         "an empty part between slashes",
		"acme/web-:1":                       `has "web-" in its name`,
		":1.27":                             "has no name before its tag",
		"bad_host.io:x/app":                 `names the registry "bad_host.io:x"`,
		strings.Repeat("a", 256) + ":1":     "a name of 256 characters",
	} {
		err := container.CheckImage(ref)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q = %v, want it refused, saying %q", ref, err, want)
		}
		if ref == "" {
			continue
		}
		if err := container.Validate(&resourcev1.Container{Image: ref}); err == nil {
			t.Errorf("Validate takes the image %q", ref)
		}
	}
}

// A container on its own that names Docker's bridge network alone joins
// it as one that names none does, and its spec says so; a service's
// bridge is a network of its environment's, and stays.
func TestBridgeAloneIsTheDefault(t *testing.T) {
	c := &resourcev1.Container{Image: "nginx:1.27", Networks: []string{"bridge"}}
	if err := (&container.Driver{}).Validate(c); err != nil || len(c.GetNetworks()) != 0 {
		t.Errorf("networks [bridge] = %v, %v; want none", c.GetNetworks(), err)
	}
	c = &resourcev1.Container{Image: "nginx:1.27", Networks: []string{"bridge", "back"}}
	if err := (&container.Driver{}).Validate(c); err != nil || len(c.GetNetworks()) != 2 {
		t.Errorf("networks [bridge, back] = %v, %v; want both", c.GetNetworks(), err)
	}
	app := &resourcev1.Container{Image: "nginx:1.27", Networks: []string{"bridge"}}
	if err := container.Validate(app); err != nil || len(app.GetNetworks()) != 1 {
		t.Errorf("a service's networks [bridge] = %v, %v; want the environment's bridge", app.GetNetworks(), err)
	}
}
