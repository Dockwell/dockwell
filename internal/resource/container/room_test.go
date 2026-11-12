// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"context"
	"errors"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// A new image that won't fit where Docker keeps images is refused before anything
// is pulled or stopped, with the numbers; one that fits, a first deploy, and a disk
// that can't be read go ahead (ADR 0080).
func TestAPullWithNoRoomIsRefusedBeforeAnythingStops(t *testing.T) {
	e := dockertest.New(t)
	e.Registry["ghcr.io/acme/shop:1.4"] = dockertest.Image{Size: 400 << 20, Config: docker.Config{Cmd: []string{"shop"}}}
	e.Registry["ghcr.io/acme/shop:1.5"] = dockertest.Image{Size: 400 << 20, Config: docker.Config{Cmd: []string{"shop"}}}
	e.Registry["ghcr.io/acme/shop:1.6"] = dockertest.Image{Size: 400 << 20, Config: docker.Config{Cmd: []string{"shop"}}}
	free, size := uint64(300<<20), uint64(100<<30)
	var unreadable error
	d := &container.Driver{Runtime: &container.Docker{Client: e.Client(), Room: func(context.Context) (uint64, uint64, error) {
		return free, size, unreadable
	}}}
	ref := resource.Ref{Kind: container.Kind, Name: "web"}
	// A first deploy has nothing to compare with, and leaves it to Docker.
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, &resourcev1.Container{Image: "ghcr.io/acme/shop:1.4"}); err != nil {
		t.Fatal(err)
	}
	running := e.Container("web").ID
	pulls := e.Pulls

	next := &resourcev1.Container{Image: "ghcr.io/acme/shop:1.5"}
	replace := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionReplace}}
	err := d.Apply(t.Context(), replace, next)
	if resource.ReasonOf(err) != resource.ReasonDisk {
		t.Fatalf("with 300 MB free = %v, want it refused for room", err)
	}
	// 400 MB plus a quarter, and 5% of 100 GB kept free, held to 2 GB.
	plain, _ := resource.Explain(err)
	if want := "This server has 300 MB free where Docker keeps images, and pulling ghcr.io/acme/shop:1.5 needs about 2.5 GB, so nothing was pulled or stopped. Free some space, such as with docker image prune, then try again."; plain != want {
		t.Errorf("it says %q, want %q", plain, want)
	}
	if e.Pulls != pulls || e.Container("web").ID != running || !e.Container("web").State.Running {
		t.Error("a pull with no room pulled or stopped something")
	}

	unreadable = errors.New("no such file or directory")
	if err := d.Apply(t.Context(), replace, next); err != nil {
		t.Fatalf("with a disk it can't read = %v", err)
	}
	unreadable = nil

	free = 3 << 30
	if err := d.Apply(t.Context(), replace, &resourcev1.Container{Image: "ghcr.io/acme/shop:1.6"}); err != nil {
		t.Fatalf("with 3 GB free = %v", err)
	}
	if got := e.Container("web").Config.Image; got != "ghcr.io/acme/shop:1.6" {
		t.Errorf("with room, it runs %s", got)
	}
}
