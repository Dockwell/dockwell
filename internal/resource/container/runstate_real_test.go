// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"context"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// Opt in on a host that can run a second Docker daemon. The test restarts only
// the daemon it starts, using a private socket, data root and containerd.
func TestManagedStopSurvivesDaemonRestartOnRealDocker(t *testing.T) {
	client, launch := dockertest.PrivateDaemon(t)
	runRecords := t.TempDir()

	local := func() resource.Local {
		runtime := &container.Docker{Client: client, RunStateDir: runRecords}
		return resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: runtime}})}
	}
	stopDaemon := launch()
	ref := resource.Ref{Kind: service.Kind, Scope: dockertest.Scope(t), Name: "workload"}
	app := &resourcev1.Container{
		Image: "busybox:1.37", Command: []string{"sleep", "600"},
		Restart: resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS,
	}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}
	apply(t, local(), ref, spec)
	before, err := client.ContainerNamed(t.Context(), service.ContainerName(ref))
	if err != nil {
		t.Fatal(err)
	}
	app.DesiredRunState = resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED
	apply(t, local(), ref, spec)
	stopDaemon()
	_ = launch()

	after, err := client.InspectContainer(t.Context(), before.ID)
	if err != nil || after.State.Running || after.HostConfig.RestartPolicy.Name != "no" {
		t.Fatalf("the same container should remain stopped with Docker's policy disabled: %v", err)
	}
	unchanged(t, local(), ref, spec)
}

func TestManagedPauseAndUnpauseKeepEngineID(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	testManagedPauseAndUnpauseKeepEngineID(t, e.Client())
}

func TestManagedPauseAndUnpauseKeepEngineIDOnRealDocker(t *testing.T) {
	testManagedPauseAndUnpauseKeepEngineID(t, dockertest.Real(t))
}

func testManagedPauseAndUnpauseKeepEngineID(t *testing.T, d *docker.Client) {
	t.Helper()
	ref := resource.Ref{Kind: service.Kind, Scope: dockertest.Scope(t), Name: "paused-workload"}
	runtime := &container.Docker{Client: d, RunStateDir: t.TempDir(), Soak: -1, Settle: time.Millisecond}
	local := resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: runtime}})}
	app := &resourcev1.Container{Image: "busybox:1.37", Command: []string{"sleep", "600"}}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = d.EachContainer(ctx, map[string][]string{"label": {docker.LabelProject + "=" + docker.Project(ref.Scope)}}, func(item *docker.ContainerSummary) error {
			return d.RemoveOneOff(ctx, item.ID)
		})
	})
	apply(t, local, ref, spec)
	before, err := d.ContainerNamed(t.Context(), service.ContainerName(ref))
	if err != nil {
		t.Fatal(err)
	}
	app.DesiredRunState = resourcev1.DesiredRunState_DESIRED_RUN_STATE_PAUSED
	apply(t, local, ref, spec)
	unchanged(t, local, ref, spec)
	held, err := d.InspectContainer(t.Context(), before.ID)
	if err != nil || !held.State.Paused || held.State.StartedAt != before.State.StartedAt {
		t.Fatalf("expected same instance paused in place: %v, %v", held, err)
	}
	app.DesiredRunState = resourcev1.DesiredRunState_DESIRED_RUN_STATE_RUNNING
	apply(t, local, ref, spec)
	got, err := d.InspectContainer(t.Context(), before.ID)
	if err != nil || got.State.Paused || !got.State.Running || got.State.StartedAt != before.State.StartedAt {
		t.Fatalf("expected same instance unpaused in place: %v, %v", got, err)
	}
}
