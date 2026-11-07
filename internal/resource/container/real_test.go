// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"encoding/json"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// On a real Docker Engine, what Docker fills in from the image never reads
// back as drift, and neither does a spec that repeats its image, or a
// change in place.
func TestContainersOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	scope := dockertest.Scope(t)
	project := docker.Project(scope)
	dockertest.Cleanup(t, client, project)
	containers := container.Driver{Runtime: &container.Docker{Client: client}}
	l := resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: containers})}
	const image = "nginx:1.27-alpine"
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	img, err := client.InspectImage(t.Context(), image)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if len(img.Config.Env) > 0 {
		k, v, _ := strings.Cut(img.Config.Env[len(img.Config.Env)-1], "=")
		env = map[string]string{k: v}
	}
	var labels map[string]string
	if len(img.Config.Labels) > 0 {
		k := slices.Sorted(maps.Keys(img.Config.Labels))[0]
		labels = map[string]string{k: img.Config.Labels[k]}
	}
	for name, app := range map[string]*resourcev1.Container{
		"bare": {Image: image},
		// No entrypoint, not even the image's, as Compose's entrypoint: [].
		"no-entrypoint": {Image: image, NoEntrypoint: true, Command: []string{"nginx", "-g", "daemon off;"}},
		"repeats": {
			Image: image, Command: img.Config.Cmd, Entrypoint: img.Config.Entrypoint, Env: env, Labels: labels,
			WorkingDir: "/", User: "root",
		},
		"full": {
			Image: image, Command: []string{"nginx", "-g", "daemon off;"}, Env: map[string]string{"A": "1"},
			Ports: []*resourcev1.PortMapping{{HostPort: uint32(freePort(t)), ContainerPort: 80, HostIp: "127.0.0.1"}},
			Mounts: []*resourcev1.Mount{
				{Source: &resourcev1.Mount_HostPath{HostPath: "/etc/hostname"}, Target: "/srv/host-name", ReadOnly: true},
				{Source: &resourcev1.Mount_HostPath{HostPath: t.TempDir()}, Target: "/srv/shared", Relabel: resourcev1.Relabel_RELABEL_SHARED},
			},
			Restart:     resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
			Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD-SHELL", "wget -q -O /dev/null http://127.0.0.1/"}, Interval: "1s", Retries: 30},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ref := resource.Ref{Kind: service.Kind, Scope: scope, Name: name}
			spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}
			if step := apply(t, l, ref, spec); step.Action != resource.ActionCreate {
				t.Fatalf("first apply = %s", step.Action)
			}
			unchanged(t, l, ref, spec)
			app.Restart = resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS
			if step := apply(t, l, ref, spec); step.Action != resource.ActionUpdate {
				t.Errorf("changing the restart policy = %s, want an update in place", step.Action)
			}
			unchanged(t, l, ref, spec)
		})
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// On a real Docker Engine, a container's, image's or network's ID prefix
// finds it, as the fake engine has it; looking up a name never does, and
// the list's name filter takes the name literally.
func TestReferencesOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	scope := dockertest.Scope(t)
	dockertest.Cleanup(t, client, docker.Project(scope))
	references(t, &container.Docker{Client: client}, scope)
}

// The fake engine takes references as the real one does.
func TestReferences(t *testing.T) {
	e := dockertest.New(t)
	e.Registry["nginx:1.27-alpine"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	references(t, &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}, dockertest.Scope(t))
}

func references(t *testing.T, rt *container.Docker, scope string) {
	t.Helper()
	client := rt.Client
	const image = "nginx:1.27-alpine"
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	l := resource.Local{Drivers: resource.NewRegistry(&container.Driver{Runtime: rt}, &network.Driver{Client: client})}
	suffix := strings.ToLower(scope[len(scope)-8:])
	dotted := resource.Ref{Kind: container.Kind, Scope: scope, Name: "dw." + suffix}
	spec := &resourcev1.Container{Image: image}
	apply(t, l, dotted, spec)
	// Unquoted, the dot in dw.… would match dwa… too.
	apply(t, l, resource.Ref{Kind: container.Kind, Scope: scope, Name: "dwa" + suffix}, spec)
	list, err := client.ListContainers(t.Context(), map[string][]string{"name": {"^/" + regexp.QuoteMeta(dotted.Name) + "$"}})
	if err != nil || len(list) != 1 || !slices.Equal(list[0].Names, []string{"/" + dotted.Name}) {
		t.Errorf("listing %s by name = %+v, %v; want it alone", dotted.Name, list, err)
	}

	c, err := client.ContainerNamed(t.Context(), dotted.Name)
	if err != nil {
		t.Fatal(err)
	}
	prefix := c.ID[:12]
	if byPrefix, err := client.InspectContainer(t.Context(), prefix); err != nil || byPrefix.ID != c.ID {
		t.Errorf("Docker took %s as %+v, %v; the fake takes an ID's prefix for the container", prefix, byPrefix, err)
	}
	if _, err := client.ContainerNamed(t.Context(), prefix); !docker.IsNotFound(err) {
		t.Errorf("looking up the name %s, a prefix of %s's ID = %v, want none", prefix, dotted.Name, err)
	}
	ref := resource.Ref{Kind: container.Kind, Scope: scope, Name: prefix}
	if observed, err := l.Observe(t.Context(), []resource.Ref{ref}); err != nil || observed[0] != nil {
		t.Errorf("observing %s = %v, %v; want nothing", prefix, observed, err)
	}

	img, err := client.InspectImage(t.Context(), image)
	if err != nil {
		t.Fatal(err)
	}
	short := strings.TrimPrefix(img.ID, "sha256:")[:12]
	if byPrefix, err := client.InspectImage(t.Context(), short); err != nil || byPrefix.ID != img.ID {
		t.Errorf("Docker took the image %s as %+v, %v; the fake takes an ID's prefix for the image", short, byPrefix, err)
	}
	if _, err := client.InspectImage(t.Context(), docker.WithTag(short)); !docker.IsNotFound(err) {
		t.Errorf("Docker took %s with a tag as %v, want none", short, err)
	}

	back := resource.Ref{Kind: network.Kind, Scope: scope, Name: "back"}
	apply(t, l, back, &resourcev1.Network{})
	n, err := client.NetworkNamed(t.Context(), network.TargetOf(back).Name)
	if err != nil {
		t.Fatal(err)
	}
	if byPrefix, err := client.InspectNetwork(t.Context(), n.ID[:12]); err != nil || byPrefix.ID != n.ID {
		t.Errorf("Docker took the network %s as %+v, %v; the fake takes an ID's prefix for the network", n.ID[:12], byPrefix, err)
	}
	if _, err := client.NetworkNamed(t.Context(), n.ID[:12]); !docker.IsNotFound(err) {
		t.Errorf("looking up the network name %s, a prefix of an ID = %v, want none", n.ID[:12], err)
	}
}

// LeadsAsMade compares what a container mounts with where that leads now, so it rests
// on Docker reading a bind's source back as it was written, with the link still in it.
// Read back resolved, a moved path would look as it always did and pass. Only a real
// Docker Engine can say.
func TestAMovedPathStopsAStartOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	project := docker.Project(dockertest.Scope(t))
	dockertest.Cleanup(t, client, project)
	const image = "nginx:1.27-alpine"
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf, other, via := filepath.Join(dir, "conf"), filepath.Join(dir, "other"), filepath.Join(dir, "via")
	for _, p := range []string{conf, other} {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(conf, via); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]string{via: conf})
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.CreateContainer(t.Context(), project+"-moved-1", &docker.CreateRequest{
		Config: docker.Config{Image: image, Labels: map[string]string{
			docker.LabelProject: project, docker.LabelService: "moved", container.LabelLeads: string(record),
		}},
		HostConfig: docker.HostConfig{Binds: []string{via + ":/conf"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.InspectContainer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(c.Mounts, func(m docker.MountPoint) bool { return m.Type == "bind" && m.Source == via }) {
		t.Fatalf("Docker reads the bind back as %+v, want its source as written, %s", c.Mounts, via)
	}
	paths := &container.Paths{Client: client, DataDir: filepath.Join(dir, "dockwell")}
	if err := paths.LeadsAsMade(t.Context(), c, nil); err != nil {
		t.Fatalf("a path that leads where it did: %v", err)
	}
	if err := os.Remove(via); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, via); err != nil {
		t.Fatal(err)
	}
	var moved *container.PathMovedError
	if err := paths.LeadsAsMade(t.Context(), c, nil); !errors.As(err, &moved) || moved.To != other {
		t.Errorf("a path that moved = %v", err)
	}
}
