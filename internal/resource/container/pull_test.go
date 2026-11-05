// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"errors"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Finding nothing newer is only "already so" of a container that's running: one
// that stopped since the plan was made still has to be started, whatever the plan
// said of it.
func TestAPullWithNothingNewerStartsWhatStoppedSinceItsPlan(t *testing.T) {
	e := dockertest.New(t)
	const image = "ghcr.io/acme/shop:latest"
	e.Registry[image] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	d := &container.Driver{Runtime: &container.Docker{Client: e.Client()}}
	ref := resource.Ref{Kind: container.Kind, Name: "web"}
	spec := &resourcev1.Container{Image: image}
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
		t.Fatal(err)
	}
	pull := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionReplace, Pull: true}}

	running := e.Container("web").ID
	if err := d.Apply(t.Context(), pull, spec); !errors.Is(err, resource.ErrAlreadySo) {
		t.Fatalf("with it running = %v, want it already so", err)
	}
	if e.Container("web").ID != running {
		t.Error("with it running and nothing newer, it was replaced")
	}

	e.Stop("web")
	if err := d.Apply(t.Context(), pull, spec); err != nil {
		t.Fatalf("with it stopped = %v", err)
	}
	if c := e.Container("web"); c == nil || c.ID == running || !c.State.Running {
		t.Errorf("with it stopped, it's now %+v", c)
	}
}
