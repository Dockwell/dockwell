// SPDX-License-Identifier: AGPL-3.0-only

package dockertest_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// The engine fills in a container's ID for a network as it first starts, so until
// then a filter finds it by the network's name and not by its ID, as Moby v28.5.2's
// daemon/list.go reads it. A check that asks by ID misses a container another change
// has made and not started yet, as the review of #124 found the fake hid.
func TestAContainerListsByItsNetworksIDOnceItStarts(t *testing.T) {
	e := dockertest.New(t)
	e.AddNetwork(docker.Network{ID: "feedface0123", Name: "edge", Driver: "bridge"})
	e.AddLocalImage("nginx:1.27", dockertest.Image{})
	c := e.Client()
	id, err := c.CreateContainer(t.Context(), "web", &docker.CreateRequest{
		Image:            "nginx:1.27",
		NetworkingConfig: docker.NetworkingConfig{EndpointsConfig: map[string]*docker.EndpointSettings{"edge": {}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	listed := func(by string) int {
		t.Helper()
		list, err := c.ListContainers(t.Context(), map[string][]string{"network": {by}})
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	if got := listed("edge"); got != 1 {
		t.Errorf("by the network's name, not started: %d containers, want 1", got)
	}
	if got := listed("feedface0123"); got != 0 {
		t.Errorf("by the network's ID, not started: %d containers, want 0", got)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for _, by := range []string{"edge", "feedface0123", "feed"} {
		if got := listed(by); got != 1 {
			t.Errorf("by %s, started: %d containers, want 1", by, got)
		}
	}
}

// A list names what a container was made from only while that name still means the
// image it runs, as Moby v28.5.2's refreshImage has it, and carries its mounts.
func TestAListSaysWhatRunsOnceItsTagMoves(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("nginx:1.27", dockertest.Image{Volumes: []string{"/data"}})
	c := e.Client()
	id, err := c.CreateContainer(t.Context(), "web", &docker.CreateRequest{Config: docker.Config{Image: "nginx:1.27"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []docker.ContainerSummary
	each := func(s *docker.ContainerSummary) error {
		got = append(got, *s)
		return nil
	}
	if err := c.EachContainer(t.Context(), map[string][]string{"id": {id}}, each); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Image != "nginx:1.27" || len(got[0].Mounts) != 1 || got[0].Mounts[0].Destination != "/data" {
		t.Fatalf("the list says %+v", got)
	}
	e.AddLocalImage("nginx:1.27", dockertest.Image{Config: docker.Config{Cmd: []string{"new"}}})
	got = nil
	if err := c.EachContainer(t.Context(), nil, each); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Image == "nginx:1.27" || !strings.HasPrefix(got[0].Image, "sha256:") {
		t.Errorf("once the tag means another image, the list says %+v", got)
	}
}

// Docker can't label a running container, so what was taken on reads as its service
// through the client alone: a list, a filter Docker can't apply to it, and an inspect.
// Only that goes when the container does, and the record outlives the process.
func TestWhatWasTakenOnReadsAsItsService(t *testing.T) {
	e := dockertest.New(t)
	path := filepath.Join(t.TempDir(), "adopted.json")
	c := e.Client()
	c.Adopted = &docker.Adopted{Path: path}
	made := e.AddContainer(docker.Container{ID: "a1", Name: "/by-hand", Config: docker.Config{Labels: map[string]string{docker.LabelProject: "shop", docker.LabelService: "db"}}})
	e.AddContainer(docker.Container{ID: "b2", Name: "/other", Config: docker.Config{Labels: map[string]string{docker.LabelProject: "env_x", docker.LabelService: "web"}}})
	byLabels := map[string][]string{"label": {docker.LabelProject + "=env_x", docker.LabelService + "=api"}}
	ids := func(c *docker.Client, filters map[string][]string) []string {
		t.Helper()
		list, err := c.ListContainers(t.Context(), filters)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range list {
			out = append(out, strings.TrimPrefix(s.Names[0], "/"))
		}
		return out
	}
	if got := ids(c, byLabels); len(got) != 0 {
		t.Fatalf("before it was taken on, the list has %v", got)
	}
	if err := c.Adopted.Take(made.ID, docker.Taken{Project: "env_x", Service: "api"}); err != nil {
		t.Fatal(err)
	}
	if got := ids(c, byLabels); !slices.Equal(got, []string{"by-hand"}) {
		t.Errorf("by its service's labels, the list has %v", got)
	}
	if got := ids(c, map[string][]string{"label": {docker.LabelProject + "=shop"}}); len(got) != 0 {
		t.Errorf("by the project it left, the list has %v", got)
	}
	inspected, err := c.InspectContainer(t.Context(), made.ID)
	if err != nil {
		t.Fatal(err)
	}
	l := inspected.Config.Labels
	if l[docker.LabelProject] != "env_x" || l[docker.LabelService] != "api" || l[docker.LabelNumber] != "1" || l[docker.LabelOneoff] != "False" || l[docker.LabelAdopted] != "by-hand" {
		t.Errorf("it reads with the labels %v", l)
	}

	again := e.Client()
	again.Adopted = &docker.Adopted{Path: path}
	if got := ids(again, byLabels); !slices.Equal(got, []string{"by-hand"}) {
		t.Errorf("after a restart, the list has %v", got)
	}
	if err := again.RemoveContainer(t.Context(), made.ID, true); err != nil {
		t.Fatal(err)
	}
	third := e.Client()
	third.Adopted = &docker.Adopted{Path: path}
	if got := ids(third, byLabels); len(got) != 0 {
		t.Errorf("with it gone, the list has %v", got)
	}
	if got := third.Adopted.Forget(made.ID); got != nil {
		t.Errorf("forgetting what's gone: %v", got)
	}
}
