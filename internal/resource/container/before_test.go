// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"errors"
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
		BeforeReplace: func(_ context.Context, old *docker.Container, spec *resourcev1.Container) error {
			seen = append(seen, old.Config.Image+">"+spec.GetImage())
			if !old.State.Running {
				t.Error("what was there had stopped")
			}
			if fail {
				return refuse
			}
			return nil
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
