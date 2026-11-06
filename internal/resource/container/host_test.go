// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
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
	project := docker.Project(env)
	first := mountedAt(e.Newest(project, "db"))

	tagged := postgres()
	tagged.GetApp().Env = map[string]string{"POSTGRES_DB": "app"}
	step := apply(t, l, ref, tagged)
	if step.Action != resource.ActionReplace || step.Destroys {
		t.Errorf("changing the environment = %s, destroys %v; want a replace that keeps the data", step.Action, step.Destroys)
	}
	if got := mountedAt(e.Newest(project, "db")); got != first {
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
	if got := mountedAt(e.Newest(project, "db")); got != first {
		t.Errorf("undoing the mount mounts %s at %s, not %s, which has the data", got, pgdata, first)
	}
}

// A recreated container is down for about 13 s, and may take as long as
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
	if step.Downtime != 13*time.Second || step.UntilHealthy != 13*time.Second+5*(10+30)*time.Second || step.HealthUnknown {
		t.Errorf("recreating postgres = down %s, healthy within %s, unknown %v; want 13s, and 3m33s with the image's check", step.Downtime, step.UntilHealthy, step.HealthUnknown)
	}
	spec.GetApp().Healthcheck = &resourcev1.Healthcheck{Test: []string{"CMD", "pg_isready"}, StartPeriod: "20s", Interval: "15s", Retries: 4}
	spec.GetApp().Env["A"] = "2"
	if step := apply(t, l, ref, spec); step.UntilHealthy != 13*time.Second+20*time.Second+4*(15+30)*time.Second {
		t.Errorf("recreating it with its own check = healthy within %s, want 3m33s", step.UntilHealthy)
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
	if err != nil || dec.UntilHealthy != 13*time.Second+3*time.Minute {
		t.Errorf("planning on the control plane = %s, %v; want 3m13s with Docker's defaults", dec.UntilHealthy, err)
	}
	// An image the server hasn't yet: its own check isn't known, so the
	// plan says the bound may be missing.
	other := postgres()
	other.GetApp().Image = "nginx:1.27"
	if step := apply(t, l, ref, other); !step.HealthUnknown || step.UntilHealthy != 0 {
		t.Errorf("recreating it on an image the server hasn't = unknown %v, healthy within %s; want it to say so", step.HealthUnknown, step.UntilHealthy)
	}
	// Verify waits as long as a check takes to give up, up to its limit, and so
	// does the estimate.
	long := &resourcev1.Healthcheck{Test: []string{"CMD", "true"}, Interval: "20m"}
	if b := container.HealthBound(long, nil); b != container.MaxHealthWait {
		t.Errorf("a check every 20 minutes = %s, want %s, as long as Verify waits", b, container.MaxHealthWait)
	}
}

// What a spec can ask of the server: paths no container may mount are
// refused when a change is proposed, saying why and what to mount
// instead; others are host access, which a person approves.
func TestServerPathsNoContainerMayMount(t *testing.T) {
	for path, want := range map[string]string{
		"/":                         "is the server's whole file system",
		"/proc/1":                   "is in /proc",
		"/sys":                      "is in /sys",
		"/dev/sda":                  "is in /dev",
		"/boot/grub":                "is in /boot",
		"/var/lib/dockwell":         "is in Dockwell's data directory, /var/lib/dockwell",
		"/var/lib/dockwell/agent":   "is in Dockwell's data directory",
		"/var/lib/docker/volumes/x": "is in Docker's data directory, /var/lib/docker",
		"/var":                      "holds Dockwell's data directory",
		"/var/lib":                  "holds Dockwell's data directory",
		"//var//lib//dockwell//":    "is in Dockwell's data directory",
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
		"rootless": {
			&resourcev1.Container{Image: "alpine", Mounts: []*resourcev1.Mount{
				{Source: &resourcev1.Mount_HostPath{HostPath: "/run/user"}, Target: "/a"},
				{Source: &resourcev1.Mount_HostPath{HostPath: "/run/user/1000"}, Target: "/b"},
				{Source: &resourcev1.Mount_HostPath{HostPath: "/run/user/1000/app"}, Target: "/c"},
			}},
			[]resource.Access{{Kind: resource.AccessDocker, Path: "/run/user"}, {Kind: resource.AccessDocker, Path: "/run/user/1000"}, {Kind: resource.AccessPath, Path: "/run/user/1000/app", SameAsRoot: true}},
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
	// . and .. are refused however they fall: the kernel reads them through
	// symlinks, so a cleaned path may not be the path it mounts.
	for _, p := range []string{"/var/lib/../lib/dockwell/.", "/srv/./app", "/var/run/../srv"} {
		c := &resourcev1.Container{Image: "alpine", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/host"}}}
		if err := container.Validate(c); err == nil || !strings.Contains(err.Error(), "which the server reads through symlinks") {
			t.Errorf("mounting %s = %v, want it refused for how it is spelled", p, err)
		}
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
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Paths: &container.Paths{Client: e.Client(), DataDir: data}}
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

	// Docker's data root is asked for each time: a Docker restarted with
	// another one is followed, with no restart of the agent.
	moved := filepath.Join(dir, "srv", "docker")
	mount := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: filepath.Join(moved, "volumes")}, Target: "/v"}}}
	if err := c.Validate(mount); err != nil {
		t.Fatal(err)
	}
	other := resource.Ref{Kind: container.Kind, Scope: srv, Name: "other"}
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: other, Spec: mount}}); err != nil {
		t.Fatalf("before Docker moved: %v", err)
	}
	e.SetDockerRoot(moved)
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: other, Spec: mount}}); err == nil || !strings.Contains(err.Error(), "is in Docker's data directory") {
		t.Errorf("once Docker's data root moved = %v, want it refused", err)
	}
}

// A volume that binds a path is checked when a container mounts it too,
// as a symlink may have changed since the volume was made.
func TestABindVolumesSymlinksAreCheckedWhenMounted(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	dir := t.TempDir()
	data := filepath.Join(dir, "dockwell")
	for _, d := range []string{data, filepath.Join(dir, "srv")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "srv"), link); err != nil {
		t.Fatal(err)
	}
	paths := &container.Paths{Client: e.Client(), DataDir: data}
	c := container.Driver{Runtime: &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Paths: paths}}
	vols := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{}, Paths: paths}
	l := resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: c}, &volume.Driver{Runtime: vols})}
	v := &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": link}}
	apply(t, l, resource.Ref{Kind: volume.Kind, Scope: env, Name: "nas"}, v)

	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_Volume{Volume: "nas"}, Target: "/nas"},
	}}}}
	if err := l.Drivers.Validate(service.Kind, spec); err != nil {
		t.Fatal(err)
	}
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: "web"}
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}}); err != nil {
		t.Fatalf("while the link leads somewhere fine: %v", err)
	}
	// The link moves after the volume was made.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(data, link); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}}); err == nil || !strings.Contains(err.Error(), "that the volume nas binds") {
		t.Errorf("planning a mount once the link leads to the data directory = %v, want it refused", err)
	}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}
	if err := l.Apply(t.Context(), step, spec); err == nil || !strings.Contains(err.Error(), "that the volume nas binds") {
		t.Errorf("applying it = %v, want it refused", err)
	}
}

// Only what is as good as root asks for more than a click, so the warning means
// something when it comes: an app's own folder doesn't, and what holds the
// system or Docker does, read-only or not. A time zone read from the server
// says nothing worth having.
func TestWhatIsAsGoodAsRoot(t *testing.T) {
	for _, tt := range []struct {
		path     string
		readOnly bool
		want     bool
	}{
		{path: "/var/run/docker.sock", want: true},
		{path: "/var/run/docker.sock", readOnly: true, want: true},
		{path: "/run/containerd/containerd.sock", want: true},
		{path: "/", want: true},
		{path: "/", readOnly: true, want: true},
		{path: "/etc", want: true},
		{path: "/etc", readOnly: true, want: true},
		{path: "/etc/ssh", readOnly: true, want: true},
		{path: "/root/.ssh", readOnly: true, want: true},
		{path: "/usr/local/bin", want: true},
		{path: "/proc", readOnly: true, want: true},
		{path: "/sys/fs/cgroup", readOnly: true, want: true},
		{path: "/boot", want: true},
		{path: "/dev/mem", want: true},
		{path: "/var/lib/docker/volumes", want: true},
		// What holds one holds that too.
		{path: "/var", want: true},
		{path: "/var/lib", readOnly: true, want: true},
		{path: "/srv/site/html", readOnly: true},
		{path: "/srv/site/html"},
		{path: "/var/www/html"},
		{path: "/var/log", readOnly: true},
		{path: "/home/mira/site"},
		{path: "/mnt/media"},
		{path: "/opt/app/config", readOnly: true},
		{path: "/etc/localtime", readOnly: true},
		{path: "/etc/timezone", readOnly: true},
		{path: "/usr/share/zoneinfo", readOnly: true},
		{path: "/usr/share/zoneinfo/Asia/Tokyo", readOnly: true},
		// Written to, they are the system's again.
		{path: "/etc/localtime", want: true},
		{path: "/usr/share/zoneinfo", want: true},
		// A name that only starts like one is no such thing.
		{path: "/etcetera"},
		{path: "/usrdata"},
	} {
		if got := container.PathAccess(tt.path, tt.readOnly).AsGoodAsRoot(); got != tt.want {
			t.Errorf("%s (read-only %v): same as root = %v, want %v", tt.path, tt.readOnly, got, tt.want)
		}
	}
	if !(resource.Access{Kind: resource.AccessNetwork}).AsGoodAsRoot() {
		t.Error("the server's own network doesn't count as root")
	}
	for _, a := range []resource.Access{
		{Kind: resource.AccessMount, Type: "nfs", Address: "10.0.0.2", Path: ":/export"},
		{Kind: resource.AccessNetworkDriver, Driver: "macvlan", Path: "eth0"},
	} {
		if a.AsGoodAsRoot() {
			t.Errorf("%+v counts as root", a)
		}
	}
}

// A symlink reaches what it leads to, so the agent says where each server path
// leads, from the server as it is, for a bind and for a volume that binds. A path
// that leads nowhere else isn't mentioned.
func TestTheAgentSaysWhereAPathLeads(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	dir := t.TempDir()
	dirs := map[string]string{"site": filepath.Join(dir, "site")}
	if err := os.MkdirAll(dirs["site"], 0o700); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{"to-etc": "/etc", "to-socket": "/run/docker.sock", "to-site": dirs["site"]}
	for link, to := range links {
		if err := os.Symlink(to, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	paths := &container.Paths{Client: e.Client(), DataDir: filepath.Join(dir, "dockwell")}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Paths: paths}
	c := container.Driver{Runtime: rt}
	l := resource.Local{Drivers: resource.NewRegistry(&c, &service.Driver{Containers: c})}
	plan := func(t *testing.T, ref resource.Ref, host string) map[string]string {
		t.Helper()
		app := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: host}, Target: "/x"}}}
		var spec proto.Message = app
		if ref.Kind == service.Kind {
			spec = &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}
		}
		steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
		if err != nil {
			t.Fatal(err)
		}
		return steps[0].Leads
	}
	web := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}
	svc := resource.Ref{Kind: service.Kind, Scope: "env_01M6MCCCXQW6G67PMQDK7PVYAY", Name: "web"}
	for link, to := range links {
		host := filepath.Join(dir, link)
		for _, ref := range []resource.Ref{web, svc} {
			if got := plan(t, ref, host); got[host] != to || len(got) != 1 {
				t.Errorf("%s of %s: leads = %v, want it to lead to %s", ref.Kind, link, got, to)
			}
		}
	}
	if got := plan(t, web, dirs["site"]); len(got) != 0 {
		t.Errorf("a folder that is what it says leads to %v", got)
	}

	// A volume that binds a link: its own step says, and so does a service that
	// mounts one already there.
	volumes := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{}, Paths: paths}
	vol := &resourcev1.Volume{Driver: "local", Options: map[string]string{"type": "none", "o": "bind", "device": filepath.Join(dir, "to-etc")}}
	vl := resource.Local{Drivers: resource.NewRegistry(&volume.Driver{Runtime: volumes})}
	vref := resource.Ref{Kind: volume.Kind, Scope: svc.Scope, Name: "data"}
	steps, err := vl.Plan(t.Context(), []resource.Desired{{Ref: vref, Spec: vol}})
	if err != nil {
		t.Fatal(err)
	}
	if got := steps[0].Leads[filepath.Join(dir, "to-etc")]; got != "/etc" {
		t.Errorf("a volume that binds a link: leads = %v", steps[0].Leads)
	}
	apply(t, vl, vref, vol)
	mounted := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/x"}},
	}}}
	steps, err = l.Plan(t.Context(), []resource.Desired{{Ref: svc, Spec: mounted}})
	if err != nil {
		t.Fatal(err)
	}
	if got := steps[0].Leads[filepath.Join(dir, "to-etc")]; got != "/etc" {
		t.Errorf("a service that mounts a volume that binds a link: leads = %v", steps[0].Leads)
	}
}

// A symlink read by name alone reads as what it is called: the spec can't say
// more, which is why an agent has to.
func TestAPathReadsAsWrittenWithoutAnAgent(t *testing.T) {
	a := container.PathAccess("/srv/app/link", false)
	if a.AsGoodAsRoot() {
		t.Errorf("%+v counts as root, from its name alone", a)
	}
	if !a.Leading("/etc").AsGoodAsRoot() {
		t.Error("told it leads to /etc, it doesn't count as root")
	}
}

// A check the agent can't make is refused when it is planned, by what is in the
// way, and says what to use: the agent reaches a container by the address Docker
// reports, which a server can't always do.
func TestACheckTheAgentCantMakeIsRefusedAtPlanTime(t *testing.T) {
	httpCheck := &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 8080, Path: "/healthz"}}
	tcpCheck := &resourcev1.Healthcheck{Tcp: &resourcev1.TcpCheck{Port: 5432}}
	command := &resourcev1.Healthcheck{Test: []string{"CMD", "true"}}
	target := container.Target{Project: "env_x", Name: "web", Scoped: true}
	for name, tc := range map[string]struct {
		rootless bool
		driver   string
		check    *resourcev1.Healthcheck
		want     string
	}{
		"an HTTP check on a bridge":   {driver: "bridge", check: httpCheck},
		"a TCP check on a bridge":     {driver: "bridge", check: tcpCheck},
		"a network not yet made":      {check: httpCheck},
		"no check":                    {rootless: true, driver: "macvlan"},
		"a command check, rootless":   {rootless: true, check: command},
		"a command check, on macvlan": {driver: "macvlan", check: command},
		"an HTTP check, rootless":     {rootless: true, check: httpCheck, want: "this server's Docker runs rootless, so the agent can't reach a container's address. An HTTP check can't run here. Use a command check"},
		"a TCP check, rootless":       {rootless: true, check: tcpCheck, want: "A TCP check can't run here. Use a command check"},
		"an HTTP check on macvlan":    {driver: "macvlan", check: httpCheck, want: "the network front uses the macvlan driver, so the agent can't reach a container's address on it. An HTTP check can't run here. Use a command check"},
		"a TCP check on an overlay":   {driver: "overlay", check: tcpCheck, want: "the overlay driver"},
	} {
		t.Run(name, func(t *testing.T) {
			e := dockertest.New(t)
			e.Rootless = tc.rootless
			if tc.driver != "" {
				e.AddNetwork(docker.Network{Name: target.DockerName("front"), Driver: tc.driver})
			}
			d := &container.Docker{Client: e.Client()}
			err := d.CheckHost(t.Context(), target, &resourcev1.Container{Image: "app:1", Networks: []string{"front"}, Healthcheck: tc.check})
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("it is refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("it says %v, want it refused with %q", err, tc.want)
			}
		})
	}
}

// A network the server has is reached like any other, so a check the agent can't
// make on one is refused the same way, by the name the spec has for it.
func TestACheckOnANetworkTheServerHasIsHeldToTheSameReach(t *testing.T) {
	target := container.Target{Project: "env_x", Service: "web", Name: "web", Scoped: true}
	httpCheck := &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 8080, Path: "/healthz"}}
	for driver, want := range map[string]string{"bridge": "", "macvlan": "the network edge uses the macvlan driver", "overlay": "the network edge uses the overlay driver"} {
		t.Run(driver, func(t *testing.T) {
			e := dockertest.New(t)
			e.AddNetwork(docker.Network{Name: "edge", Driver: driver})
			d := &container.Docker{Client: e.Client()}
			err := d.CheckHost(t.Context(), target, &resourcev1.Container{Image: "app:1", ExternalNetworks: []string{"edge"}, Healthcheck: httpCheck})
			switch {
			case want == "" && err != nil:
				t.Errorf("it is refused: %v", err)
			case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
				t.Errorf("it says %v, want it refused with %q", err, want)
			}
		})
	}
}

// deadPath stands in for a mount that stopped answering: what asks the disk about a
// path under it doesn't come back until the test lets it.
func deadPath(t *testing.T, under string) (release func(), asked *atomic.Int32) {
	t.Helper()
	gone := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gone) }) }
	asked = &atomic.Int32{}
	restore := container.StopAnswering(50*time.Millisecond, func(p string) {
		if p == under || strings.HasPrefix(p, under+"/") {
			asked.Add(1)
			<-gone
		}
	})
	t.Cleanup(func() { release(); restore() })
	return release, asked
}

func TestAPlanOnAPathThatStopsAnsweringSaysWhichWithinItsBound(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, "nfs")
	if err := os.Mkdir(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	mounted := filepath.Join(dead, "uploads")
	release, asked := deadPath(t, dead)
	h := container.Host{Resolve: container.Resolve}

	check := func() error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- h.CheckPath(mounted, "the server path "+mounted+" mounted at /data") }()
		select {
		case err := <-done:
			return err
		case <-time.After(3 * time.Second):
			t.Fatal("the check is stuck on a path that won't answer")
			return nil
		}
	}
	err := check()
	if !errors.Is(err, resource.ErrPathNotAnswering) || !strings.Contains(err.Error(), mounted+" isn't answering") || !strings.Contains(err.Error(), "Check the mount") {
		t.Fatalf("got %v", err)
	}
	// The next plan hears at once, and holds nothing more.
	if err := check(); !errors.Is(err, resource.ErrPathNotAnswering) {
		t.Fatalf("again: %v", err)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("the disk was asked %d times, each holding a thread", n)
	}
	// Once it answers, so do plans, with nothing to restart.
	release()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := check(); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("it answers again, and the plan says %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A rollout's own way of following a path is bounded the same.
func TestARolloutOnAPathThatStopsAnsweringSaysWhich(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, "nfs")
	if err := os.Mkdir(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	deadPath(t, dead)
	done := make(chan error, 1)
	go func() {
		_, err := container.ResolveForRollout(&container.Docker{}, filepath.Join(dead, "uploads"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, resource.ErrPathNotAnswering) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("following a path for a rollout is stuck")
	}
}

// Each path left behind holds a thread, so they're bounded too.
func TestWhatsLeftBehindOfPathsIsBounded(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, "nfs")
	if err := os.Mkdir(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	_, asked := deadPath(t, dead)
	for i := range container.MaxStuckPaths + 5 {
		if _, err := container.Resolve(filepath.Join(dead, fmt.Sprint(i))); !errors.Is(err, resource.ErrPathNotAnswering) {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if n := int(asked.Load()); n != container.MaxStuckPaths {
		t.Errorf("%d paths left behind, want %d", n, container.MaxStuckPaths)
	}
}
