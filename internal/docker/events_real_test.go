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
// why every site dropped has to come from its events, which may not be there yet as
// the restart shows. A container that exits by itself starts again the same way, and
// says nothing of the kind.
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
	read := func(got *docker.Container) bool {
		t.Helper()
		went, err := client.WentPastMemory(t.Context(), got.ID, got.State.StartedAt.Add(-2*time.Minute), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return went
	}

	// Docker can show it running again before it has the event, so the agent lets
	// 30 s pass before no event means there wasn't one. This holds that it comes
	// well inside that, and says how late it was, whichever way the read fell.
	got := restarted("past-its-memory", "sleep 1; tail /dev/zero")
	shown := time.Now()
	first := read(got)
	for !read(got) {
		if time.Since(shown) > 20*time.Second {
			t.Fatal("no oom event in 20 s, where the agent gives up on one after 30 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("the event read %v as the restart showed, and was there %v after", first, time.Since(shown).Round(time.Millisecond))

	got = restarted("exits", "sleep 1; exit 3")
	time.Sleep(2 * time.Second)
	if read(got) {
		t.Error("a container that exited by itself reads as past its memory")
	}
}
