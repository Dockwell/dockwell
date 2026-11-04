// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"maps"
	"net"
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
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
		"repeats": {
			Image: image, Command: img.Config.Cmd, Entrypoint: img.Config.Entrypoint, Env: env, Labels: labels,
			WorkingDir: "/", User: "root",
		},
		"full": {
			Image: image, Command: []string{"nginx", "-g", "daemon off;"}, Env: map[string]string{"A": "1"},
			Ports:       []*resourcev1.PortMapping{{HostPort: uint32(freePort(t)), ContainerPort: 80, HostIp: "127.0.0.1"}},
			Mounts:      []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/etc/hostname"}, Target: "/srv/host-name", ReadOnly: true}},
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
