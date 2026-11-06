// SPDX-License-Identifier: AGPL-3.0-only

package dockertest_test

import (
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
