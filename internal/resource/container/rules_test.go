// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// A relabel makes a path usable on a server that enforces SELinux, and
// rewrites the labels of everything under it, so it counts as more.
func TestARelabelIsAccess(t *testing.T) {
	d := &container.Driver{}
	spec := func(r resourcev1.Relabel) proto.Message {
		c := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{
			{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/app"}, Target: "/app", ReadOnly: true, Relabel: r},
		}}
		if err := d.Validate(c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got := resource.NewAccess(d.HostAccess(spec(resourcev1.Relabel_RELABEL_UNSPECIFIED)), d.HostAccess(spec(resourcev1.Relabel_RELABEL_PRIVATE))); len(got) == 0 {
		t.Error("adding :Z to a server path counts as no new access, so a token applies it")
	}
}

// host_path and target both go into Docker's source:target:options, so a
// colon in target sets options the spec and Compose refuse.
func TestColonsInAMountAreRefused(t *testing.T) {
	c := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/app"}, Target: "/app:rshared"},
	}}
	if err := container.Validate(c); err == nil {
		t.Errorf("target %q passes Validate", c.Mounts[0].Target)
	}
	h := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_HostPath{HostPath: "/proc:/x"}, Target: "/app"},
	}}
	if err := container.Validate(h); err == nil {
		t.Errorf("host path %q passes Validate", h.Mounts[0].GetHostPath())
	}
}

// A replace makes the new container under a name of its own, and removes
// a try that failed before there, but never one Dockwell didn't make:
// that is refused, as a container at the name itself is.
func TestAReplaceLeavesAContainerAtItsSwapName(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: container.Kind, Scope: srv, Name: "web"}
	spec := &resourcev1.Container{Image: "postgres:17"}
	apply(t, l, ref, spec)
	id := e.Container("web").ID
	mine := id[:12] + "_web"
	e.AddContainer(docker.Container{Name: "/" + mine, Image: "postgres:16", Config: docker.Config{Image: "postgres:16", Labels: map[string]string{"mine": "yes"}}})
	next := &resourcev1.Container{Image: "postgres:17", Env: map[string]string{"A": "1"}}
	if err := l.Drivers.Validate(ref.Kind, next); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: next}}); err == nil || !strings.Contains(err.Error(), "a container named "+mine+" is on this server, but Dockwell didn't make it") {
		t.Errorf("planning a replace over %s = %v, want it refused", mine, err)
	}
	// Applying a plan made before someone started it is refused too.
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionReplace}}
	if err := l.Apply(t.Context(), step, next); err == nil || !strings.Contains(err.Error(), "Dockwell didn't make it") {
		t.Errorf("applying a replace over %s = %v, want it refused", mine, err)
	}
	if !slices.Contains(e.Containers(), mine) || e.Container("web").ID != id {
		t.Errorf("a replace removed %s, which Dockwell didn't make, or the old container: %v", mine, e.Containers())
	}
}

// Docker waits an interval after each check ends, and a check may take its
// timeout, 30 s by default, so three failures take 3 × (30 s + 30 s).
func TestHealthBoundCountsTheTimeout(t *testing.T) {
	got := container.HealthBound(&resourcev1.Healthcheck{Test: []string{"CMD", "true"}}, nil)
	if got != 3*(30*time.Second+30*time.Second) {
		t.Errorf("Docker's defaults = up to %s, but the slowest run of checks takes 3m", got)
	}
	got = container.HealthBound(&resourcev1.Healthcheck{Test: []string{"CMD", "true"}, Interval: "5s", Timeout: "30s", Retries: 3}, nil)
	if got != 3*(5*time.Second+30*time.Second) {
		t.Errorf("interval 5s, timeout 30s, 3 retries = up to %s, but the slowest run of checks takes 1m45s", got)
	}
}

// A service stored before the rules, with a mount now refused: what its
// plan, delete and drift do.
func TestAServiceStoredBeforeTheRules(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: "old"}
	name := service.ContainerName(ref)
	stored := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "postgres:17", Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
		Mounts: []*resourcev1.Mount{
			{Source: &resourcev1.Mount_HostPath{HostPath: "/var/lib/docker/volumes"}, Target: "/a"},
			{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/b"},
		},
	}}}
	e.AddVolume(docker.Volume{Name: "env_01kat4zr6e3v0y5xbkzb5d9q6m_data", Driver: "local"})
	e.AddContainer(docker.Container{
		Name: "/" + name, Image: "postgres:17",
		Config: docker.Config{Image: "postgres:17", Labels: map[string]string{docker.LabelProject: "env_01kat4zr6e3v0y5xbkzb5d9q6m", docker.LabelService: "old"}},
		HostConfig: docker.HostConfig{
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			Binds:         []string{"/var/lib/docker/volumes:/a"},
			Mounts:        []docker.Mount{{Type: "volume", Source: "env_01kat4zr6e3v0y5xbkzb5d9q6m_data", Target: "/b"}},
		},
	})
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: stored}}); err == nil || !strings.Contains(err.Error(), "Docker's data directory") {
		t.Errorf("planning it unchanged = %v, want the refusal, which says what to fix", err)
	}
	got, err := l.Observe(t.Context(), []resource.Ref{ref})
	if err != nil {
		t.Fatal(err)
	}
	if !resource.Equal(stored, got[0]) {
		t.Errorf("drift on a service nobody touched: %+v", resource.Diff(stored, got[0]))
	}
	// It can still be edited to fix it, or deleted.
	fixed := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17", Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/b"},
	}}}}
	if err := l.Drivers.Validate(ref.Kind, fixed); err != nil {
		t.Fatal(err)
	}
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: fixed}})
	if err != nil || steps[0].Action != resource.ActionReplace {
		t.Fatalf("editing it to fix it = %+v, %v; want a replace", steps, err)
	}
	if err := l.Apply(t.Context(), steps[0], fixed); err != nil {
		t.Errorf("applying the fix: %v", err)
	}
	steps, err = l.Plan(t.Context(), []resource.Desired{{Ref: ref}})
	if err != nil || steps[0].Action != resource.ActionDelete {
		t.Fatalf("planning its deletion = %+v, %v", steps, err)
	}
	if err := l.Apply(t.Context(), steps[0], nil); err != nil {
		t.Errorf("deleting it: %v", err)
	}
}

// A container on its own names volumes and networks as Docker does, so it
// mustn't name an environment's, or one Docker made for an image, or a
// volume the server keeps after a deletion.
func TestAContainerOnItsOwnKeepsToItsOwn(t *testing.T) {
	d := &container.Driver{}
	project := docker.Project(env)
	for name, c := range map[string]*resourcev1.Container{
		"an environment's volume":  {Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: project + "_nas"}, Target: "/x"}}},
		"one in another case":      {Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "ENV_01KAT_nas"}, Target: "/x"}}},
		"an environment's network": {Image: "nginx:1.27", Networks: []string{project + "_back"}},
		"an image's volume":        {Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: strings.Repeat("ab", 32)}, Target: "/x"}}},
	} {
		if err := d.Validate(c); err == nil {
			t.Errorf("%s passes", name)
		}
	}
	// A service's names are its environment's: env_ there is just a name.
	svc := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_Volume{Volume: "env_data"}, Target: "/x"},
	}}}}
	if err := (&service.Driver{}).Validate(svc); err != nil {
		t.Errorf("a service's volume env_data = %v", err)
	}

	e := dockertest.New(t)
	images(e)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reg, vols := keptDrivers(e, &now)
	l := resource.Local{Drivers: reg}
	e.AddVolume(docker.Volume{Name: "old-data", Driver: "local"})
	if err := vols.Kept.KeepImageVolumes("other", map[string]string{"/data": "old-data"}); err != nil {
		t.Fatal(err)
	}
	spec := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "old-data"}, Target: "/x"}}}
	if err := d.Validate(spec); err != nil {
		t.Fatal(err)
	}
	ref := resource.Ref{Kind: container.Kind, Scope: srv, Name: "pry"}
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(t.Context(), steps[0], spec); err == nil || !strings.Contains(err.Error(), "the volume old-data was deleted") {
		t.Errorf("mounting a kept volume = %v, want it refused", err)
	}
}

// Every spelling of a refused path, on the spec's own path, none a path a
// person could approve by accident.
func TestRefusedSpellings(t *testing.T) {
	for _, p := range []string{
		"/", "//", "///", "/.", "/./", "/..", "/../", "/../..", "/srv/..", "/srv/../", "/srv/./..", "//proc", "/proc/", "/./proc", "/proc/.", "/x/../proc", "/proc//1",
		"/sys/", "/dev//sda", "/boot/.", "/var", "/var/", "/var/lib", "/var/lib/", "//var//lib//docker//", "/var/lib/docker/../dockwell", "/var/./lib/dockwell/x",
		"relative", "./x", "../x", "~", "~/x", "", ".", "C:\\", "C:/", "\\\\host\\share", "%2F", "%2Fproc",
	} {
		c := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/x"}}}
		if err := container.Validate(c); err == nil {
			t.Errorf("host path %q passes as %q", p, c.Mounts[0].GetHostPath())
		}
	}
	// Literal on Linux, so these are other paths, for a person to approve.
	for _, p := range []string{"/PROC", "/Var/Lib/Docker", "/%70roc", "/proc\u200b", "/ｐroc"} {
		c := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/x"}}}
		d := &container.Driver{}
		if err := d.Validate(c); err != nil {
			t.Errorf("host path %q = %v", p, err)
		} else if len(d.HostAccess(c)) == 0 {
			t.Errorf("host path %q is no host access", p)
		}
	}
}

// networks: anything that isn't a Docker network's plain name is refused.
func TestNetworkSpellings(t *testing.T) {
	d := &container.Driver{}
	for _, n := range []string{"container:abc", "service:x", "host:x", "/host", "host/", " host", "ns:/proc/1/ns/net"} {
		c := &resourcev1.Container{Image: "nginx:1.27", Networks: []string{n}}
		if err := d.Validate(c); err == nil {
			t.Errorf("network %q passes", n)
		}
	}
	// A container on its own has only Docker's networks, none of which Dockwell
	// made, so each is access, and the server's own is its own kind of it. Docker
	// puts a container on host alone.
	for _, c := range []struct {
		networks []string
		want     []resource.Access
	}{
		{[]string{"a", "b"}, []resource.Access{{Kind: resource.AccessExternalNetwork, Path: "a"}, {Kind: resource.AccessExternalNetwork, Path: "b"}}},
		{[]string{"host"}, []resource.Access{{Kind: resource.AccessNetwork}}},
	} {
		spec := &resourcev1.Container{Image: "nginx:1.27", Networks: c.networks}
		if err := d.Validate(spec); err != nil || !slices.Equal(d.HostAccess(spec), c.want) {
			t.Errorf("networks %v = %v, %v, want %v", c.networks, err, d.HostAccess(spec), c.want)
		}
	}
	e := dockertest.New(t)
	images(e)
	l := resource.Local{Drivers: drivers(e)}
	id := e.Network("host")
	t.Logf("the fake's host network: %+v", id)
	if id != nil {
		// Docker takes an ID here; the agent looks names up exactly.
		spec := &resourcev1.Container{Image: "postgres:17", Networks: []string{id.ID}}
		if err := d.Validate(spec); err != nil {
			t.Fatal(err)
		}
		ref := resource.Ref{Kind: container.Kind, Scope: srv, Name: "n"}
		steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
		if err == nil {
			err = l.Apply(t.Context(), steps[0], spec)
		}
		if err == nil {
			t.Errorf("the host network's ID as a network name applied with no access: %v", d.HostAccess(spec))
		} else {
			t.Logf("by ID = %v", err)
		}
	}
}

// Checks and plans compare these with each other, so a request that
// repeats them shouldn't take minutes.
func TestAContainerAsksForALimitedNumberOfMountsAndPorts(t *testing.T) {
	mounts := func(n int) *resourcev1.Container {
		c := &resourcev1.Container{Image: "nginx:1"}
		for i := range n {
			c.Mounts = append(c.Mounts, &resourcev1.Mount{
				Target: fmt.Sprintf("/m/%d", i),
				Source: &resourcev1.Mount_HostPath{HostPath: fmt.Sprintf("/srv/%d", i)},
			})
		}
		return c
	}
	if err := container.Validate(mounts(container.MaxMounts)); err != nil {
		t.Errorf("%d mounts: %v", container.MaxMounts, err)
	}
	if err := container.Validate(mounts(container.MaxMounts + 1)); err == nil {
		t.Errorf("%d mounts pass", container.MaxMounts+1)
	}
	beside := mounts(container.MaxMounts)
	beside.Tmpfs = []*resourcev1.Tmpfs{{Target: "/more"}}
	if err := container.Validate(beside); err == nil {
		t.Errorf("%d mounts and a tmpfs pass", container.MaxMounts)
	}

	ports := func(n int) *resourcev1.Container {
		c := &resourcev1.Container{Image: "nginx:1"}
		for i := range n {
			c.Ports = append(c.Ports, &resourcev1.PortMapping{HostPort: uint32(1 + i), ContainerPort: 80})
		}
		return c
	}
	if err := container.Validate(ports(container.MaxPorts)); err != nil {
		t.Errorf("%d ports: %v", container.MaxPorts, err)
	}
	if err := container.Validate(ports(container.MaxPorts + 1)); err == nil {
		t.Errorf("%d ports pass", container.MaxPorts+1)
	}
}
