// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// What runs before a container is replaced gets the one there, before anything
// stops it; one that fails stops the change, with the old one as it was.
func TestBeforeReplaceGetsTheOneThere(t *testing.T) {
	e := dockertest.New(t)
	for _, image := range []string{"mongo:7.0", "mongo:8.0"} {
		e.AddLocalImage(image, dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}})
	}
	var seen []string
	refuse := errors.New("MongoDB didn't raise its feature compatibility version")
	fail := false
	d := &Docker{
		Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1,
		BeforeReplace: func(_ context.Context, old *docker.Container, spec *resourcev1.Container) (string, error) {
			seen = append(seen, old.Config.Image+">"+spec.GetImage())
			if !old.State.Running {
				t.Error("what was there had stopped")
			}
			if fail {
				return "", refuse
			}
			return "", nil
		},
	}
	at := Target{Name: "env_01-docs-1", Project: "env_01", Service: "docs", Scoped: true}
	if err := d.Run(t.Context(), at, &resourcev1.Container{Image: "mongo:7.0"}, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Errorf("a first container ran it: %v", seen)
	}
	fail = true
	if err := d.Run(t.Context(), at, &resourcev1.Container{Image: "mongo:8.0"}, resource.Decision{}); !errors.Is(err, refuse) {
		t.Errorf("a failure before the replace gives %v", err)
	}
	c, err := e.Client().ContainerNamed(t.Context(), at.Name)
	if err != nil || c.Config.Image != "mongo:7.0" || !c.State.Running {
		t.Errorf("after the failure, what's there is %+v, %v", c, err)
	}
	fail = false
	if err := d.Run(t.Context(), at, &resourcev1.Container{Image: "mongo:8.0"}, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[1] != "mongo:7.0>mongo:8.0" {
		t.Errorf("it ran with %v", seen)
	}
}

func TestBeforeReplaceDoesNotStartAnImageThatChangedAfterTheCheck(t *testing.T) {
	e := dockertest.New(t)
	client := e.Client()
	const old, next = "events:25.8.33.6", "events:26.3.62.3"
	for _, image := range []string{old, next} {
		e.AddLocalImage(image, dockertest.Image{})
	}
	d := &Docker{
		Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1,
		BeforeReplace: func(ctx context.Context, _ *docker.Container, spec *resourcev1.Container) (string, error) {
			image, err := client.InspectImage(ctx, spec.GetImage())
			if err != nil {
				return "", err
			}
			e.AddLocalImage(spec.GetImage(), dockertest.Image{})
			return image.ID, nil
		},
	}
	at := Target{Name: "env_01-events-1", Project: "env_01", Service: "events", Scoped: true}
	if err := d.Run(t.Context(), at, &resourcev1.Container{Image: old}, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	// The previous instance can have a different name after a rollout.
	c, err := client.ContainerNamed(t.Context(), at.Name)
	if err != nil {
		t.Fatal(err)
	}
	const previous = "env_01-events-2"
	if err := client.RenameContainer(t.Context(), c.ID, previous); err != nil {
		t.Fatal(err)
	}
	if err := d.Run(t.Context(), at, &resourcev1.Container{Image: next}, resource.Decision{}); err == nil || !strings.Contains(err.Error(), "changed after it was checked") {
		t.Fatalf("replacing with a tag that moved after the check: %v", err)
	}
	c, err = client.ContainerNamed(t.Context(), previous)
	if err != nil || c.Config.Image != old || !c.State.Running {
		t.Fatalf("the old image is no longer running: %v, %v", c, err)
	}
}
