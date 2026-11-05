// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"context"
	"errors"
	"log/slog"
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
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

const env = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"

// images are what the fake registry has, as Docker Hub's images look.
// sickSays is what the check of the image sick:1 prints.
const sickSays = "curl: (22) The requested URL returned error: 500. It says so"

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
	e.Registry["busybox:1.37"] = dockertest.Image{Config: docker.Config{Cmd: []string{"sh"}}}
	e.Registry["sick:1"] = dockertest.Image{Unhealthy: true, CheckSays: sickSays, Config: docker.Config{
		Cmd: []string{"sleep", "inf"}, Healthcheck: &docker.Healthcheck{Test: []string{"CMD", "false"}},
	}}
	e.Registry["slow:1"] = dockertest.Image{Starting: true, Config: docker.Config{
		Cmd: []string{"sleep", "inf"}, Healthcheck: &docker.Healthcheck{Test: []string{"CMD", "true"}},
	}}
}

func drivers(e *dockertest.Engine) *resource.Registry {
	kept := &volume.Kept{}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second, Deleted: kept}
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
				{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/certs"}, Target: "/etc/certs", ReadOnly: true, Relabel: resourcev1.Relabel_RELABEL_SHARED},
				{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/run"}, Target: "/run/nginx", Relabel: resourcev1.Relabel_RELABEL_PRIVATE},
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
		// No entrypoint, not even the image's, as Compose's entrypoint: [].
		"no-entrypoint":         {Image: "nginx:1.27", NoEntrypoint: true, Command: []string{"sleep", "inf"}},
		"no-entrypoint-of-none": {Image: "busybox:1.37", NoEntrypoint: true},
		"partial-health":        {Image: "postgres:17", Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD-SHELL", "pg_isready -U app"}}},
		"no-health":             {Image: "postgres:17", Healthcheck: &resourcev1.Healthcheck{Test: []string{"NONE"}}},
	} {
		t.Run(name, func(t *testing.T) {
			ref := resource.Ref{Kind: container.Kind, Scope: "srv_01KAT4ZR6E3V0Y5XBKZB5D9Q6M", Name: name}
			if step := apply(t, l, ref, spec); step.Action != resource.ActionCreate {
				t.Fatalf("first apply = %s, want create", step.Action)
			}
			unchanged(t, l, ref, spec)
			if got := e.Container(name).Config.Entrypoint; spec.GetNoEntrypoint() && (got == nil || len(got) > 0) {
				t.Errorf("Docker runs the entrypoint %q, want none, not even the image's", got)
			}
			if name == "bare" {
				// Left unset, the image's entrypoint runs, and the spec
				// still reads back without one.
				if got := e.Container(name).Config.Entrypoint; !slices.Equal(got, []string{"/docker-entrypoint.sh"}) {
					t.Errorf("Docker runs the entrypoint %q, want the image's", got)
				}
			}
			if name == "full" {
				binds := e.Container(name).HostConfig.Binds
				if !slices.Contains(binds, "/srv/certs:/etc/certs:ro,z") || !slices.Contains(binds, "/srv/run:/run/nginx:Z") {
					t.Errorf("Docker got the binds %q, want the server paths relabelled", binds)
				}
			}
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
	if !slices.Contains(heard, "Pulling nginx:1.27") || !slices.ContainsFunc(heard, func(m string) bool { return strings.HasSuffix(m, " of 48 MB") }) {
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
		"crash:1": "crash stopped right after it started, with exit code 1",
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

// Each way a real person's first deploy goes wrong reads in plain words, and
// what Docker said stays behind them.
func TestFailuresReadPlainly(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	e.Registry["ghost:nosuchtag"] = dockertest.Image{PullError: "manifest for ghost:nosuchtag not found: manifest unknown: manifest unknown"}
	e.BindError = func(source string) string {
		if source == "/readonly/uploads" {
			return "mkdir /readonly/uploads: read-only file system"
		}
		return ""
	}
	l := resource.Local{Drivers: drivers(e)}
	run := func(name string, spec *resourcev1.Container) error {
		t.Helper()
		ref := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: name}
		if err := container.Validate(spec); err != nil {
			t.Fatal(err)
		}
		return l.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec)
	}
	port := func(n uint32) []*resourcev1.PortMapping {
		return []*resourcev1.PortMapping{{HostPort: n, ContainerPort: 80}}
	}
	if err := run("first", &resourcev1.Container{Image: "nginx:1.27", Ports: port(8080)}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		spec  *resourcev1.Container
		plain string
	}{
		{"ghost", &resourcev1.Container{Image: "ghost:1"}, "ghost:1 can't be pulled. The name may be wrong, or it may be private, which Dockwell can't pull yet. Check the name, then try again."},
		{"tagged", &resourcev1.Container{Image: "ghost:nosuchtag"}, "ghost:nosuchtag isn't on its registry, so it can't be pulled. The tag may be wrong. Check the name and tag, then try again."},
		{"crash", &resourcev1.Container{Image: "crash:1"}, "crash stopped right after it started, with exit code 1. Check its command and variables, then try again."},
		{"sick", &resourcev1.Container{Image: "sick:1"}, `sick runs, but fails its health check: the command exited with 1: "` + sickSays + `". Fix what it reports, or the check, then try again.`},
		{"second", &resourcev1.Container{Image: "nginx:1.27", Ports: port(8080)}, "Port 8080 is already in use on this server. Choose another port, or stop what uses it, then try again."},
		{"mounted", &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/readonly/uploads"}, Target: "/u"}}}, "mounted can't mount /readonly/uploads (read-only file system). Check the path on this server, then try again."},
	} {
		plain, detail := resource.Explain(run(tt.name, tt.spec))
		if plain != tt.plain {
			t.Errorf("%s reads %q, want %q", tt.name, plain, tt.plain)
		}
		if detail == "" || detail == plain {
			t.Errorf("%s has %q behind it, want what Docker said", tt.name, detail)
		}
		// A rollback takes the container away, so nothing sends a person to it.
		if strings.Contains(detail, "docker logs") || strings.Contains(detail, "docker inspect") {
			t.Errorf("%s has %q behind it, which sends you to a container that is gone", tt.name, detail)
		}
	}
}

// A service's failure names the service, not the container Docker runs it as,
// and what it is doing, as a step shows it.
func TestAServiceFailureNamesTheService(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	var heard []string
	var mu sync.Mutex
	ctx := resource.WithProgress(t.Context(), func(m string) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, m)
	})
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: service.Kind, Scope: "env_01M6MCCCXQW6G67PMQDK7PVYAY", Name: "web"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "crash:1"}}}
	plain, detail := resource.Explain(l.Apply(ctx, resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec))
	if plain != "web stopped right after it started, with exit code 1. Check its command and variables, then try again." {
		t.Errorf("plain = %q", plain)
	}
	if !strings.Contains(strings.ToLower(detail), "env_01m6mccc") {
		t.Errorf("detail = %q, want the chain with its IDs", detail)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(heard, "Starting web") || !slices.Contains(heard, "Checking that web stays up") {
		t.Errorf("heard %q, want what it was doing in its own name", heard)
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
		// The one that gives the new one its name, not the old one its mark.
		if strings.HasSuffix(r.URL.Path, "/rename") && r.URL.Query().Get("name") == name {
			once.Do(func() { err = errors.New("Error when allocating new name: the daemon is restarting") })
		}
		return err
	})
	spec.GetApp().Env["POSTGRES_DB"] = "shop"
	if err := container.Validate(spec.GetApp()); err != nil {
		t.Fatal(err)
	}
	err := l.Apply(t.Context(), resource.Step{Ref: db, Decision: resource.Decision{Action: resource.ActionReplace}}, spec)
	if err == nil || !strings.Contains(err.Error(), "removed the old db, but can't give the new one its name") {
		t.Fatalf("a replace whose rename fails = %v", err)
	}
	if got := e.Containers(); len(got) != 1 || got[0] == name {
		t.Fatalf("Docker has %v, want the new container left under the name it was made with", got)
	}

	if step := apply(t, l, db, spec); step.Action != resource.ActionReplace || len(step.Changes) != 0 {
		t.Errorf("planning db again = %s with %v, want it to finish the swap, with no change to its spec", step.Action, step.Changes)
	}
	if got := e.Containers(); len(got) != 1 || got[0] != name {
		t.Errorf("Docker has %v, want %s alone", got, name)
	}
	if c := e.Container(name); len(c.Mounts) != 1 || c.Mounts[0].Name != data || !c.State.Running {
		t.Errorf("%s mounts %+v and runs %t, want the image's volume %s and running", name, c.Mounts, c.State.Running, data)
	}
	unchanged(t, l, db, spec)
}

// Docker stops the checks of a paused container, so what it last said of one is as it
// left it, and the container serves nothing. Waiting for it passes nothing.
func TestAPausedContainerPassesNoCheck(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	reg := drivers(e)
	l := resource.Local{Drivers: reg}
	web := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}
	spec := &resourcev1.Container{Image: "nginx:1.27"}
	_ = container.Validate(spec)
	if err := l.Apply(t.Context(), resource.Step{Ref: web, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
		t.Fatal(err)
	}
	e.SetHealth("web", "healthy", "ok")
	e.Edit("web", func(c *docker.Container) { c.State.Paused, c.State.Status = true, "paused" })
	d, err := reg.Driver(container.Kind)
	if err != nil {
		t.Fatal(err)
	}
	err = d.Verify(t.Context(), web, spec)
	if err == nil {
		t.Fatal("a paused container passed its check")
	}
	if plain, _ := resource.Explain(err); !strings.Contains(plain, "web is paused") || !strings.Contains(plain, "Unpause it") {
		t.Errorf("it says %q", plain)
	}
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
			rt := &container.Docker{Client: e.Client(), Poll: time.Millisecond, Soak: -1, HealthWait: tc.wait}
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

// The Docker drivers' readiness is whether one Docker answers, so a
// registry of them asks it once per check.
func TestReadyAsksDockerOnce(t *testing.T) {
	e := dockertest.New(t)
	client := e.Client()
	containers := container.Driver{Runtime: &container.Docker{Client: client}}
	reg := resource.NewRegistry(&containers, &service.Driver{Containers: containers},
		&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: &volume.Kept{}}}, &network.Driver{Client: client})
	pings := func() int {
		n := 0
		for _, r := range e.Seen() {
			if strings.HasSuffix(r, "/version") {
				n++
			}
		}
		return n
	}
	ready, unavailable := reg.Ready(t.Context())
	if len(ready) != 4 || len(unavailable) != 0 {
		t.Fatalf("ready %v, unavailable %v, want the four Docker kinds ready", ready, unavailable)
	}
	if n := pings(); n != 1 {
		t.Errorf("one check asked Docker %d times, want once", n)
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

// docker compose lists a service's containers, and a container on its
// own, as its project's, by the filters it lists them with.
func TestComposeListsContainers(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	l := resource.Local{Drivers: drivers(e)}
	svc := resource.Ref{Kind: service.Kind, Scope: env, Name: "web"}
	apply(t, l, svc, &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}})
	own := resource.Ref{Kind: container.Kind, Scope: "srv_01KAT4ZR6E3V0Y5XBKZB5D9Q6M", Name: "cache"}
	apply(t, l, own, &resourcev1.Container{Image: "nginx:1.27"})
	for _, c := range []struct{ project, service, name string }{
		{docker.Project(env), "web", service.ContainerName(svc)},
		{docker.Project(own.Scope), "cache", "cache"},
	} {
		list, err := e.Client().ListContainers(t.Context(), dockertest.ComposeFilters(c.project, c.service))
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || !slices.Contains(list[0].Names, "/"+c.name) {
			t.Errorf("docker compose -p %s ps %s lists %+v, want %s", c.project, c.service, list, c.name)
		}
	}
	// The hash changes with the spec, and never holds its environment.
	a := container.ConfigHash(&resourcev1.Container{Image: "nginx:1.27", Env: map[string]string{"KEY": "secret-1"}})
	b := container.ConfigHash(&resourcev1.Container{Image: "nginx:1.27", Env: map[string]string{"KEY": "secret-2"}})
	c := container.ConfigHash(&resourcev1.Container{Image: "nginx:1.28", Env: map[string]string{"KEY": "secret-1"}})
	if a != b || a == c {
		t.Errorf("config hashes %s, %s, %s: want the same for values alone, another for an image", a, b, c)
	}
}

// The agent checks and normalizes each desired spec again, as the control
// plane did: one that skipped that plans and applies as the control plane
// would have it, rather than failing to read back as asked for. The spec
// it was given stays as it was.
func TestTheAgentValidatesWhatItPlans(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: container.Kind, Scope: "srv_01KAT4ZR6E3V0Y5XBKZB5D9Q6M", Name: "web"}
	// No restart policy, which Validate fills in, and ports out of order.
	spec := &resourcev1.Container{Image: "nginx:1.27", Ports: []*resourcev1.PortMapping{
		{HostPort: 8443, ContainerPort: 443}, {HostPort: 8080, ContainerPort: 80},
	}}
	given := proto.CloneOf(spec)
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatalf("applying a spec that skipped validation: %v", err)
	}
	if !proto.Equal(spec, given) {
		t.Errorf("the agent changed the spec it was given: %v", spec)
	}
	unchanged(t, l, ref, spec)

	// One that isn't valid is refused, before anything runs.
	_, err = l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: &resourcev1.Container{}}})
	if err == nil || !strings.Contains(err.Error(), "needs an image") {
		t.Errorf("planning a container with no image: %v", err)
	}
}

// lockedBuffer is a log that tests read while the runtime writes it.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Removing the container a failed replace made takes CleanupWait at most,
// even when Docker doesn't answer, and what it leaves is logged with how
// to remove it.
func TestCleaningUpAFailedReplaceIsBounded(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	var log lockedBuffer
	rt := &container.Docker{
		Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second,
		CleanupWait: 50 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(&log, nil)),
	}
	l := resource.Local{Drivers: resource.NewRegistry(&container.Driver{Runtime: rt})}
	ref := resource.Ref{Kind: container.Kind, Scope: "srv_01KAT4ZR6E3V0Y5XBKZB5D9Q6M", Name: "web"}
	apply(t, l, ref, &resourcev1.Container{Image: "nginx:1.27"})
	old := e.Container("web").ID
	// The old container won't stop, and Docker never answers removing the
	// new one.
	e.Intercept(func(r *http.Request) error {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/"+old+"/stop"):
			return errors.New("the old one won't stop")
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/") && !strings.Contains(r.URL.Path, old):
			<-r.Context().Done()
			return r.Context().Err()
		}
		return nil
	})
	spec := &resourcev1.Container{Image: "nginx:1.27", Env: map[string]string{"A": "1"}}
	if err := l.Drivers.Validate(ref.Kind, spec); err != nil {
		t.Fatal(err)
	}
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = l.Apply(t.Context(), steps[0], spec)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the failed replace took %s, cleaning up as long as Docker didn't answer", took)
	}
	if err == nil || !strings.Contains(err.Error(), "can't stop the old web") {
		t.Errorf("the replace = %v, want it failed for the old container", err)
	}
	if got := log.String(); !strings.Contains(got, "is left on the server. Remove it with docker rm -f") || !strings.Contains(got, "container="+old[:12]+"_web") {
		t.Errorf("the log says %q, want what was left and how to remove it", got)
	}
}
