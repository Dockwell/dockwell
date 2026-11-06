// SPDX-License-Identifier: AGPL-3.0-only

package docker_test

import (
	"context"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// Docker clears OOMKilled as a restart policy starts a container again, so what says
// why every site dropped has to come from its events. A container that exits by
// itself starts again the same way, and says nothing of the kind.
func TestARestartForMemoryIsInDockersEventsOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	project := docker.Project(dockertest.Scope(t))
	dockertest.Cleanup(t, client, project)
	const image = "nginx:1.27-alpine"
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	restarted := func(name, command string) *docker.Container {
		t.Helper()
		req := &docker.CreateRequest{
			Config: docker.Config{
				Image: image, Entrypoint: []string{"sh", "-c"}, Cmd: []string{command},
				Labels: map[string]string{docker.LabelProject: project},
			},
			HostConfig: docker.HostConfig{
				Memory: 16 << 20, MemorySwap: 16 << 20,
				RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			},
		}
		id, err := client.CreateContainer(t.Context(), project+"-"+name, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.StartContainer(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		for ctx.Err() == nil {
			got, err := client.InspectContainer(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.RestartCount >= 1 && got.State.Running && !got.State.Restarting {
				return got
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s didn't start again in 60 s", name)
		return nil
	}
	for name, tc := range map[string]struct {
		command string
		want    bool
	}{
		"past-its-memory": {"sleep 1; tail /dev/zero", true},
		"exits":           {"sleep 1; exit 3", false},
	} {
		got := restarted(name, tc.command)
		went, err := client.WentPastMemory(t.Context(), got.ID, got.State.StartedAt.Add(-2*time.Minute), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if went != tc.want {
			t.Errorf("%s: WentPastMemory = %v, want %v", name, went, tc.want)
		}
	}
}
