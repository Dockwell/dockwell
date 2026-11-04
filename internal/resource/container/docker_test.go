// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
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

const env = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"

// images are what the fake registry has, as Docker Hub's images look.
func images(e *dockertest.Engine) {
	e.Registry["nginx:1.27"] = dockertest.Image{Size: 48 << 20, Config: docker.Config{
		Cmd: []string{"nginx", "-g", "daemon off;"}, Entrypoint: []string{"/docker-entrypoint.sh"},
		Env:          []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "NGINX_VERSION=1.27.3"},
		Labels:       map[string]string{"maintainer": "NGINX Docker Maintainers"},
		ExposedPorts: map[string]struct{}{"80/tcp": {}},
	}}
	e.Registry["postgres:17"] = dockertest.Image{Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{
		Cmd: []string{"postgres"}, Entrypoint: []string{"docker-entrypoint.sh"}, Env: []string{"PGDATA=/var/lib/postgresql/data"},
		User: "postgres", WorkingDir: "/",
		Healthcheck: &docker.Healthcheck{Test: []string{"CMD", "pg_isready"}, Interval: 10 * time.Second, Retries: 5},
	}}
	e.Registry["crash:1"] = dockertest.Image{Exits: true, Config: docker.Config{Cmd: []string{"false"}}}
	e.Registry["sick:1"] = dockertest.Image{Unhealthy: true, Config: docker.Config{
		Cmd: []string{"sleep", "inf"}, Healthcheck: &docker.Healthcheck{Test: []string{"CMD", "false"}},
	}}
	e.Registry["slow:1"] = dockertest.Image{Starting: true, Config: docker.Config{
		Cmd: []string{"sleep", "inf"}, Healthcheck: &docker.Healthcheck{Test: []string{"CMD", "true"}},
	}}
}

func drivers(e *dockertest.Engine) *resource.Registry {
	kept := &volume.Kept{}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, HealthWait: time.Second, Deleted: kept}
	c := container.Driver{Runtime: rt}
	return resource.NewRegistry(&c, &service.Driver{Containers: c},
		&volume.Driver{Runtime: &volume.Docker{Client: rt.Client, Kept: kept}})
}

// apply plans one resource against the engine and applies the step, as
// the engine's agent does. It returns the step it took.
func apply(t *testing.T, l resource.Local, ref resource.Ref, spec proto.Message) resource.Step {
	t.Helper()
	if spec != nil {
		if err := l.Drivers.Validate(ref.Kind, spec); err != nil {
			t.Fatalf("validate %s: %v", ref, err)
		}
	}
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
	if err != nil {
		t.Fatalf("plan %s: %v", ref, err)
	}
	if steps[0].Action != resource.ActionNone {
		if err := l.Apply(t.Context(), steps[0], spec); err != nil {
			t.Fatalf("apply %s %s: %v", steps[0].Action, ref, err)
		}
	}
	return steps[0]
}

// unchanged checks a resource reads back as its spec: planning it again
// finds nothing to do.
func unchanged(t *testing.T, l resource.Local, ref resource.Ref, spec proto.Message) {
	t.Helper()
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != resource.ActionNone {
		t.Errorf("%s drifted right after it was applied: %s %v", ref, steps[0].Action, steps[0].Changes)
	}
}

// What Docker fills in from the image, and what Dockwell and Compose label
// a container with, never reads back as drift, and neither does a spec
// that repeats its image.
func TestContainersReadBackAsTheirSpec(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	reg := drivers(e)
	l := resource.Local{Drivers: reg}
	for name, spec := range map[string]*resourcev1.Container{
		"bare": {Image: "nginx:1.27"},
		"full": {
			Image: "nginx:1.27", Command: []string{"nginx", "-T"}, Env: map[string]string{"A": "1", "B": "two=2"},
			Labels: map[string]string{"team": "web"}, User: "101", WorkingDir: "/srv/../srv/www",
			Ports: []*resourcev1.PortMapping{
				{HostPort: 8443, ContainerPort: 443, HostIp: "127.0.0.1"},
				{HostPort: 8080, ContainerPort: 80},
				{HostPort: 5353, ContainerPort: 53, Protocol: resourcev1.Protocol_PROTOCOL_UDP},
			},
			Mounts: []*resourcev1.Mount{
				{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/conf"}, Target: "/etc/nginx/conf.d", ReadOnly: true},
				{Source: &resourcev1.Mount_Volume{Volume: "cache"}, Target: "/var/cache/nginx"},
			},
			Restart:     resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS,
			Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "curl", "-f", "http://localhost"}, Interval: "30000ms"},
		},
		// Every value here is the image's own, which Docker would fill in
		// anyway: the spec still reads back as it is.
		"repeats-image": {
			Image: "nginx:1.27", Command: []string{"nginx", "-g", "daemon off;"}, Entrypoint: []string{"/docker-entrypoint.sh"},
			Env:    map[string]string{"NGINX_VERSION": "1.27.3"},
			Labels: map[string]string{"maintainer": "NGINX Docker Maintainers"},
		},
		"entrypoint-only": {Image: "nginx:1.27", Entrypoint: []string{"/bin/sh", "-c", "sleep inf"}},
		"partial-health":  {Image: "postgres:17", Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD-SHELL", "pg_isready -U app"}}},
		"no-health":       {Image: "postgres:17", Healthcheck: &resourcev1.Healthcheck{Test: []string{"NONE"}}},
	} {
		t.Run(name, func(t *testing.T) {
			ref := resource.Ref{Kind: container.Kind, Scope: "srv_01KAT4ZR6E3V0Y5XBKZB5D9Q6M", Name: name}
			if step := apply(t, l, ref, spec); step.Action != resource.ActionCreate {
				t.Fatalf("first apply = %s, want create", step.Action)
			}
			unchanged(t, l, ref, spec)
		})
	}
}

// A service's container is named and labelled as Compose names and labels
// a service's first container, in its environment's project, so docker
// compose sees it, and other services reach it by its name.
func TestServicesRunAsComposeServices(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	e.AddNetwork(docker.Network{Name: "env_01kat4zr6e3v0y5xbkzb5d9q6m_backend", Driver: "bridge"})
	reg := drivers(e)
	l := resource.Local{Drivers: reg}
	data := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	apply(t, l, data, &resourcev1.Volume{})
	web := resource.Ref{Kind: service.Kind, Scope: env, Name: "web"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "nginx:1.27", Networks: []string{"backend"},
		Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}},
	}}}
	apply(t, l, web, spec)
	unchanged(t, l, web, spec)

	c := e.Container("env_01kat4zr6e3v0y5xbkzb5d9q6m-web-1")
	if c == nil {
		t.Fatalf("the service's container is missing; Docker has %v", e.Containers())
	}
	for k, want := range map[string]string{
		"com.docker.compose.project": "env_01kat4zr6e3v0y5xbkzb5d9q6m", "com.docker.compose.service": "web",
		"com.docker.compose.container-number": "1", "com.docker.compose.oneoff": "False",
		"maintainer": "NGINX Docker Maintainers", // the image's, as Docker merges them
	} {
		if got := c.Config.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
	if got := c.NetworkSettings.Networks["env_01kat4zr6e3v0y5xbkzb5d9q6m_backend"]; got == nil || !slices.Equal(got.Aliases, []string{"web"}) {
		t.Errorf("on the backend network as %+v, want the alias web", got)
	}
	if got := c.HostConfig.Mounts; len(got) != 1 || got[0].Source != "env_01kat4zr6e3v0y5xbkzb5d9q6m_data" {
		t.Errorf("mounts %+v, want the environment's data volume", got)
	}

	// A service can't make an unmanaged volume by mounting one that isn't
	// there, as Docker would.
	api := resource.Ref{Kind: service.Kind, Scope: env, Name: "api"}
	bad := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "uploads"}, Target: "/up"}},
	}}}
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: api, Spec: bad}})
	if err != nil {
		t.Fatal(err)
	}
	err = l.Apply(t.Context(), steps[0], bad)
	if err == nil || !strings.Contains(err.Error(), "the volume uploads isn't on this server, so api can't mount it") {
		t.Errorf("mounting a volume that isn't there = %v", err)
	}
	if e.Volume("env_01kat4zr6e3v0y5xbkzb5d9q6m_uploads") != nil {
		t.Error("Docker made the volume a service mounted, unmanaged")
	}
}

// A volume deleted through a plan stays on the server for its data, but
// isn't in the environment: a service can't mount it, or go on mounting
// it, until it is made again, which brings it back.
func TestDeletedVolumesCantBeMounted(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	l := resource.Local{Drivers: drivers(e)}
	data := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	apply(t, l, data, &resourcev1.Volume{})
	web := resource.Ref{Kind: service.Kind, Scope: env, Name: "web"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}},
	}}}
	apply(t, l, web, spec)
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: data}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(t.Context(), steps[0], nil); err == nil || !strings.Contains(err.Error(), "containers still mount the volume env_01kat4zr6e3v0y5xbkzb5d9q6m_data (env_01kat4zr6e3v0y5xbkzb5d9q6m-web-1)") {
		t.Errorf("deleting a volume a service mounts = %v", err)
	}
	apply(t, l, web, nil)
	apply(t, l, data, nil)

	steps, err = l.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(t.Context(), steps[0], spec); err == nil || !strings.Contains(err.Error(), "the volume data was deleted, so web can't mount it") {
		t.Errorf("mounting a deleted volume = %v", err)
	}
	if e.Container(service.ContainerName(web)) != nil {
		t.Error("the service runs, mounting a deleted volume")
	}
	apply(t, l, data, &resourcev1.Volume{})
	apply(t, l, web, spec)
}

// Changing what Docker can't change in place recreates the container: the
// new one is made before the old stops, and keeps the volumes Docker made
// for its image, so their data stays. The restart policy changes in place.
func TestReplacingKeepsTheImagesVolumes(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	reg := drivers(e)
	l := resource.Local{Drivers: reg}
	db := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17", Env: map[string]string{"POSTGRES_DB": "app"}}}}
	apply(t, l, db, spec)
	name := service.ContainerName(db)
	before := e.Container(name)
	if len(before.Mounts) != 1 {
		t.Fatalf("mounts %+v, want the image's anonymous volume", before.Mounts)
	}

	spec.GetApp().Env["POSTGRES_DB"] = "shop"
	step := apply(t, l, db, spec)
	if step.Action != resource.ActionReplace || step.Downtime != container.DefaultRecreateDowntime || step.Destroys {
		t.Errorf("changing the environment = %+v, want a replace with %s of downtime that destroys nothing", step.Decision, container.DefaultRecreateDowntime)
	}
	after := e.Container(name)
	if after.ID == before.ID {
		t.Fatal("the container wasn't recreated")
	}
	if len(after.Mounts) != 1 || after.Mounts[0].Name != before.Mounts[0].Name {
		t.Errorf("the new container mounts %+v, want the old one's volume %s", after.Mounts, before.Mounts[0].Name)
	}
	if got := e.Containers(); len(got) != 1 {
		t.Errorf("Docker has %v after the replace, want only %s", got, name)
	}
	unchanged(t, l, db, spec)

	spec.GetApp().Restart = resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS
	if step := apply(t, l, db, spec); step.Action != resource.ActionUpdate || step.Downtime != 0 {
		t.Errorf("changing the restart policy = %+v, want an update in place", step.Decision)
	}
	if e.Container(name).ID != after.ID {
		t.Error("changing the restart policy recreated the container")
	}
}

// A container changed by hand reads back as it is, so it shows as drift.
func TestHandEditsAreDrift(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	reg := drivers(e)
	l := resource.Local{Drivers: reg}
	web := resource.Ref{Kind: service.Kind, Scope: env, Name: "web"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27", Env: map[string]string{"A": "1"}}}}
	apply(t, l, web, spec)
	e.Edit(service.ContainerName(web), func(c *docker.Container) {
		c.Config.Env = append(c.Config.Env, "DEBUG=1")
		c.Config.Image = "nginx:1.28"
		c.HostConfig.RestartPolicy.Name = "always"
	})
	observed, err := l.Observe(t.Context(), []resource.Ref{web})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range resource.Diff(spec, observed[0]) {
		paths = append(paths, c.Path)
	}
	if want := []string{"app.image", "app.env[DEBUG]", "app.restart"}; !slices.Equal(paths, want) {
		t.Errorf("drift = %v, want %v", paths, want)
	}
}

// Pulling an image reports how far it is, and an image that won't pull,
// start or pass its check fails its step with how to see why.
func TestPullsAndHealth(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	reg := drivers(e)
	l := resource.Local{Drivers: reg}
	var mu sync.Mutex
	var heard []string
	ctx := resource.WithProgress(t.Context(), func(m string) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, m)
	})
	web := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}
	spec := &resourcev1.Container{Image: "nginx:1.27"}
	_ = container.Validate(spec)
	if err := l.Apply(ctx, resource.Step{Ref: web, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if !slices.Contains(heard, "pulling nginx:1.27") || !slices.ContainsFunc(heard, func(m string) bool { return strings.HasSuffix(m, " of 48 MB") }) {
		t.Errorf("heard %q while pulling, want how far it was", heard)
	}
	mu.Unlock()
	if e.Pulls != 1 {
		t.Errorf("pulled %d times", e.Pulls)
	}
	// An image that is there already isn't pulled again.
	if err := l.Apply(t.Context(), resource.Step{Ref: web, Decision: resource.Decision{Action: resource.ActionReplace}}, spec); err != nil {
		t.Fatal(err)
	}
	if e.Pulls != 1 {
		t.Errorf("pulled %d times, want an image on the server used as it is", e.Pulls)
	}

	for image, want := range map[string]string{
		"ghost:1": "can't pull ghost:1: pull access denied for ghost:1",
		"crash:1": "crash stopped right after it started, with exit code 1. See why with docker logs crash",
		"sick:1":  "sick runs, but fails its health check",
	} {
		ref := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: strings.Split(image, ":")[0]}
		spec := &resourcev1.Container{Image: image}
		_ = container.Validate(spec)
		err := l.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("running %s = %v, want %q", image, err, want)
		}
	}
}

// A replace that removed the old container but couldn't give the new one
// its name leaves it under the name it was made with. The next run takes
// it as the old one, so the volumes Docker made for the image, with their
// data, move on to the container that gets the name, and nothing is left.
func TestAFailedRenameIsFinished(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	l := resource.Local{Drivers: drivers(e)}
	db := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17", Env: map[string]string{"POSTGRES_DB": "app"}}}}
	apply(t, l, db, spec)
	name := service.ContainerName(db)
	data := e.Container(name).Mounts[0].Name

	var once sync.Once
	e.Intercept(func(r *http.Request) error {
		var err error
		if strings.HasSuffix(r.URL.Path, "/rename") {
			once.Do(func() { err = errors.New("Error when allocating new name: the daemon is restarting") })
		}
		return err
	})
	spec.GetApp().Env["POSTGRES_DB"] = "shop"
	if err := container.Validate(spec.GetApp()); err != nil {
		t.Fatal(err)
	}
	err := l.Apply(t.Context(), resource.Step{Ref: db, Decision: resource.Decision{Action: resource.ActionReplace}}, spec)
	if err == nil || !strings.Contains(err.Error(), "removed the old "+name+", but can't give the new one its name") {
		t.Fatalf("a replace whose rename fails = %v", err)
	}
	if got := e.Containers(); len(got) != 1 || got[0] == name {
		t.Fatalf("Docker has %v, want the new container left under the name it was made with", got)
	}

	if step := apply(t, l, db, spec); step.Action != resource.ActionCreate {
		t.Errorf("planning db again = %s, want a create, as nothing has its name", step.Action)
	}
	if got := e.Containers(); len(got) != 1 || got[0] != name {
		t.Errorf("Docker has %v, want %s alone", got, name)
	}
	if c := e.Container(name); len(c.Mounts) != 1 || c.Mounts[0].Name != data || !c.State.Running {
		t.Errorf("%s mounts %+v and runs %t, want the image's volume %s and running", name, c.Mounts, c.State.Running, data)
	}
	unchanged(t, l, db, spec)
}

// Waiting for health is bounded twice: by HealthWait, and by the step's
// own time. Each is blamed only when it is what ran out.
func TestHealthWaitsAreTold(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	ref := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "slow"}
	spec := &resourcev1.Container{Image: "slow:1"}
	if err := container.Validate(spec); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		wait, step time.Duration
		want       string
	}{
		"its own":    {50 * time.Millisecond, time.Minute, "slow didn't pass its health check within 0.05 s"},
		"the step's": {time.Minute, 100 * time.Millisecond, "the step's time ran out before slow passed its health check"},
	} {
		t.Run(name, func(t *testing.T) {
			rt := &container.Docker{Client: e.Client(), Poll: time.Millisecond, HealthWait: tc.wait}
			d := &container.Driver{Runtime: rt}
			ctx, cancel := context.WithTimeout(t.Context(), tc.step)
			defer cancel()
			err := d.Apply(ctx, resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec)
			if err == nil {
				err = d.Verify(ctx, ref, spec)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("waiting for a check that keeps starting = %v, want %q", err, tc.want)
			}
		})
	}
}

// A Docker that takes the connection but never answers makes the kinds
// unavailable with how to check it, never a bare context error.
func TestHungDocker(t *testing.T) {
	e := dockertest.New(t)
	e.Intercept(func(r *http.Request) error {
		<-r.Context().Done()
		return r.Context().Err()
	})
	d := &container.Driver{Runtime: &container.Docker{Client: e.Client()}}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := d.Ready(ctx)
	if !docker.IsUnavailable(err) || !strings.Contains(err.Error(), "Docker isn't answering on "+e.Socket) ||
		!strings.Contains(err.Error(), "Check it with systemctl status docker") || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Ready with Docker hung = %v, want it unavailable with how to check it", err)
	}
}

// Without Docker, the driver says why and how to fix it.
func TestNoDocker(t *testing.T) {
	c := docker.New(t.TempDir() + "/docker.sock")
	err := c.Ping(context.Background())
	if !docker.IsUnavailable(err) || !strings.Contains(err.Error(), "Docker isn't installed") || !strings.Contains(err.Error(), "Install Docker Engine") {
		t.Errorf("Ping without Docker = %v", err)
	}
}
