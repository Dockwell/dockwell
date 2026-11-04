// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// A replace that mounts something else where the image keeps its data
// takes that data out of use, and the plan says so, with where; the
// server keeps it for 7 days, and undoing the replace mounts it again.
// Changing anything else keeps it, as Compose keeps an image's volumes.
func TestAMountOverAnImagesDataKeepsIt(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reg, _ := keptDrivers(e, &now)
	l := resource.Local{Drivers: reg}
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}
	apply(t, l, ref, postgres())
	name := service.ContainerName(ref)
	first := mountedAt(e.Container(name))

	tagged := postgres()
	tagged.GetApp().Env = map[string]string{"POSTGRES_DB": "app"}
	step := apply(t, l, ref, tagged)
	if step.Action != resource.ActionReplace || step.Destroys {
		t.Errorf("changing the environment = %s, destroys %v; want a replace that keeps the data", step.Action, step.Destroys)
	}
	if got := mountedAt(e.Container(name)); got != first {
		t.Errorf("the replace mounts %s at %s, not %s", got, pgdata, first)
	}

	apply(t, l, resource.Ref{Kind: volume.Kind, Scope: env, Name: "pg"}, &resourcev1.Volume{})
	covered := postgres()
	covered.GetApp().Env = tagged.GetApp().Env
	covered.GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "pg"}, Target: pgdata}}
	step = apply(t, l, ref, covered)
	if !step.Destroys || !slices.Equal(step.Kept, []string{pgdata}) {
		t.Errorf("mounting a volume over the image's data = destroys %v, kept %v; want it to say the data at %s is out of use", step.Destroys, step.Kept, pgdata)
	}
	if e.Volume(first) == nil {
		t.Fatal("the image's volume went with the old container")
	}
	step = apply(t, l, ref, tagged)
	if !step.Restores {
		t.Error("undoing the mount doesn't say it brings the image's data back")
	}
	if got := mountedAt(e.Container(name)); got != first {
		t.Errorf("undoing the mount mounts %s at %s, not %s, which has the data", got, pgdata, first)
	}
}

// A recreated container is down for about 12 s, and may take as long as
// its health check allows to be healthy again: its start period, then its
// retries, an interval apart, the image's check included.
func TestDowntimeCountsTheHealthCheck(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}
	apply(t, l, ref, postgres())
	// postgres:17's own check, in the fake registry: every 10 s, 5 retries.
	spec := postgres()
	spec.GetApp().Env = map[string]string{"A": "1"}
	step := apply(t, l, ref, spec)
	if step.Downtime != 12*time.Second || step.UntilHealthy != 12*time.Second+50*time.Second {
		t.Errorf("recreating postgres = down %s, healthy within %s; want 12s, and 1m2s with the image's check", step.Downtime, step.UntilHealthy)
	}
	spec.GetApp().Healthcheck = &resourcev1.Healthcheck{Test: []string{"CMD", "pg_isready"}, StartPeriod: "20s", Interval: "15s", Retries: 4}
	spec.GetApp().Env["A"] = "2"
	if step := apply(t, l, ref, spec); step.UntilHealthy != 12*time.Second+20*time.Second+60*time.Second {
		t.Errorf("recreating it with its own check = healthy within %s, want 1m32s", step.UntilHealthy)
	}
	spec.GetApp().Healthcheck = &resourcev1.Healthcheck{Test: []string{"NONE"}}
	spec.GetApp().Env["A"] = "3"
	if step := apply(t, l, ref, spec); step.UntilHealthy != 0 {
		t.Errorf("recreating it without a check = healthy within %s, want nothing beyond the downtime", step.UntilHealthy)
	}
	// The control plane, which can't see the image, counts the spec's
	// check, with Docker's defaults where it says nothing.
	dec, err := (&container.Driver{}).Plan(&resourcev1.Container{Image: "nginx:1.27"}, &resourcev1.Container{
		Image: "nginx:1.28", Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "true"}},
	}, []resource.Change{{Path: "image"}})
	if err != nil || dec.UntilHealthy != 12*time.Second+90*time.Second {
		t.Errorf("planning on the control plane = %s, %v; want 1m42s with Docker's defaults", dec.UntilHealthy, err)
	}
	// Verify waits 5 minutes at most, and so does the estimate.
	long := &resourcev1.Healthcheck{Test: []string{"CMD", "true"}, Interval: "10m"}
	if b := container.HealthBound(long, nil); b != container.DefaultHealthWait {
		t.Errorf("a check every 10 minutes = %s, want %s, as long as Verify waits", b, container.DefaultHealthWait)
	}
}

// What a spec can ask of the server: paths no container may mount are
// refused when a change is proposed, saying why and what to mount
// instead; others are host access, which a person approves.
func TestServerPathsNoContainerMayMount(t *testing.T) {
	for path, want := range map[string]string{
		"/":                          "is the server's whole file system",
		"/proc/1":                    "is in /proc",
		"/sys":                       "is in /sys",
		"/dev/sda":                   "is in /dev",
		"/boot/grub":                 "is in /boot",
		"/var/lib/dockwell":          "is in Dockwell's data directory, /var/lib/dockwell",
		"/var/lib/dockwell/agent":    "is in Dockwell's data directory",
		"/var/lib/docker/volumes/x":  "is in Docker's data directory, /var/lib/docker",
		"/var":                       "holds Dockwell's data directory",
		"/var/lib":                   "holds Dockwell's data directory",
		"/var/lib/../lib/dockwell/.": "is in Dockwell's data directory",
	} {
		c := &resourcev1.Container{Image: "alpine", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: path}, Target: "/host"}}}
		err := container.Validate(c)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "Mount only the directory the container needs") {
			t.Errorf("mounting %s = %v, want it refused, saying it %s", path, err, want)
		}
	}
	d := &container.Driver{}
	for name, tc := range map[string]struct {
		spec *resourcev1.Container
		want []resource.Access
	}{
		"config": {
			&resourcev1.Container{Image: "nginx", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/app/config"}, Target: "/etc/app", ReadOnly: true}}},
			[]resource.Access{{Kind: resource.AccessPath, Path: "/srv/app/config", ReadOnly: true}},
		},
		"socket": {
			&resourcev1.Container{Image: "docker:cli", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/var/run/docker.sock"}, Target: "/var/run/docker.sock", ReadOnly: true}}},
			[]resource.Access{{Kind: resource.AccessDocker, Path: "/var/run/docker.sock"}},
		},
		"run": {
			&resourcev1.Container{Image: "alpine", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/run"}, Target: "/host-run"}}},
			[]resource.Access{{Kind: resource.AccessDocker, Path: "/run"}},
		},
		"host network": {&resourcev1.Container{Image: "alpine", Networks: []string{"host"}}, []resource.Access{{Kind: resource.AccessNetwork}}},
		"volume":       {&resourcev1.Container{Image: "alpine", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}}}, nil},
	} {
		if err := d.Validate(tc.spec); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := d.HostAccess(tc.spec); !slices.Equal(got, tc.want) {
			t.Errorf("%s reaches %+v, want %+v", name, got, tc.want)
		}
	}
	// A service's networks are its environment's, so its host isn't the
	// server's.
	svc := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "alpine", Networks: []string{"host"}}}}
	if got := (&service.Driver{}).HostAccess(svc); len(got) != 0 {
		t.Errorf("a service on its environment's network host reaches %+v, want nothing", got)
	}
	// More access, or the same another way, is new; less isn't.
	rw := resource.Access{Kind: resource.AccessPath, Path: "/srv/a"}
	ro := resource.Access{Kind: resource.AccessPath, Path: "/srv/a", ReadOnly: true}
	if got := resource.NewAccess([]resource.Access{ro}, []resource.Access{rw}); !slices.Equal(got, []resource.Access{rw}) {
		t.Errorf("read-only to read-write = %+v, want it new", got)
	}
	if got := resource.NewAccess([]resource.Access{rw}, []resource.Access{ro}); len(got) != 0 {
		t.Errorf("read-write to read-only = %+v, want nothing new", got)
	}
}

// On the agent, a path is checked again as the server has it, as it
// plans and as it applies: where its symlinks lead, and where the agent
// and Docker keep their data.
func TestTheAgentFollowsSymlinks(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	dir := t.TempDir()
	data := filepath.Join(dir, "dockwell")
	e.DockerRoot = filepath.Join(dir, "docker")
	for _, d := range []string{data, e.DockerRoot, filepath.Join(dir, "srv")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for link, to := range map[string]string{"root": "/", "keys": data, "volumes": filepath.Join(e.DockerRoot, "volumes"), "fine": filepath.Join(dir, "srv")} {
		if err := os.Symlink(to, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Paths: &container.Paths{Client: e.Client(), DataDir: data}}
	c := container.Driver{Runtime: rt}
	l := resource.Local{Drivers: resource.NewRegistry(&c)}
	ref := resource.Ref{Kind: container.Kind, Scope: srv, Name: "web"}
	for path, want := range map[string]string{
		filepath.Join(dir, "root"):              "leads to / through a symlink",
		filepath.Join(dir, "root", "proc", "1"): "leads to /proc/1 through a symlink",
		filepath.Join(dir, "keys", "agent"):     "is in Dockwell's data directory",
		data:                                    "is in Dockwell's data directory",
		dir:                                     "holds Dockwell's data directory",
		filepath.Join(dir, "volumes", "x", "y"): "is in Docker's data directory",
		filepath.Join(e.DockerRoot, "x"):        "is in Docker's data directory",
	} {
		spec := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: path}, Target: "/host"}}}
		if err := c.Validate(spec); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("planning a mount of %s = %v, want it refused, as it %s", path, err, want)
		}
		// Applying checks again, as a plan made before a symlink was would.
		if err := l.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("applying a mount of %s = %v, want it refused, as it %s", path, err, want)
		}
	}
	if e.Container("web") != nil {
		t.Fatal("a refused mount made a container")
	}
	ok := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: filepath.Join(dir, "fine", "new")}, Target: "/srv"}}}
	if err := c.Validate(ok); err != nil {
		t.Fatal(err)
	}
	apply(t, l, ref, ok)
}
