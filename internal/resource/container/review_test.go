// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

const srv = "srv_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"

// A container someone ran by hand, without the labels Dockwell gives its
// own, is never taken over by a container of the same name: planning one
// is refused, saying what is there and what to do, and it stays as it is.
func TestAContainerDockwellDidntMakeIsLeftAlone(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	e.AddContainer(docker.Container{
		Name: "/db", Image: "postgres:16",
		Config: docker.Config{Image: "postgres:16", Labels: map[string]string{"mine": "yes"}},
	})
	before := e.Container("db").ID
	l := resource.Local{Drivers: drivers(e)}
	ref := resource.Ref{Kind: container.Kind, Scope: srv, Name: "db"}
	for _, spec := range []*resourcev1.Container{{Image: "postgres:17"}, nil} {
		if spec != nil {
			_ = (&container.Driver{}).Validate(spec)
		}
		var desired []resource.Desired
		if spec == nil {
			desired = []resource.Desired{{Ref: ref}}
		} else {
			desired = []resource.Desired{{Ref: ref, Spec: spec}}
		}
		_, err := l.Plan(t.Context(), desired)
		if err == nil || !strings.Contains(err.Error(), "a container named db is on this server, but Dockwell didn't make it") ||
			!strings.Contains(err.Error(), "docker rename db") || !strings.Contains(err.Error(), "docker rm -f db") {
			t.Errorf("planning over a container run by hand = %v, want it refused, saying how to rename or remove it", err)
		}
	}
	// Applying anyway, as a plan made before someone started it would, is
	// refused too.
	for _, step := range []resource.Step{
		{Ref: ref, Decision: resource.Decision{Action: resource.ActionReplace}},
		{Ref: ref, Decision: resource.Decision{Action: resource.ActionUpdate}},
		{Ref: ref, Decision: resource.Decision{Action: resource.ActionDelete}},
	} {
		var spec *resourcev1.Container
		if step.Action != resource.ActionDelete {
			spec = &resourcev1.Container{Image: "postgres:17"}
			_ = (&container.Driver{}).Validate(spec)
		}
		var err error
		if spec == nil {
			err = l.Apply(t.Context(), step, nil)
		} else {
			err = l.Apply(t.Context(), step, spec)
		}
		if err == nil || !strings.Contains(err.Error(), "Dockwell didn't make it") {
			t.Errorf("applying a %s over a container run by hand = %v, want it refused", step.Action, err)
		}
	}
	if c := e.Container("db"); c == nil || c.ID != before {
		t.Error("the container run by hand is gone")
	}
}

// keptDrivers are the drivers with one list of kept volumes and a clock
// both share, as the agent has them.
func keptDrivers(e *dockertest.Engine, now *time.Time) (*resource.Registry, *volume.Docker) {
	clock := func() time.Time { return *now }
	kept := &volume.Kept{Now: clock}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, HealthWait: time.Second, Deleted: kept}
	c := container.Driver{Runtime: rt}
	vols := &volume.Docker{Client: e.Client(), Kept: kept, Now: clock}
	return resource.NewRegistry(&c, &service.Driver{Containers: c}, &volume.Driver{Runtime: vols}), vols
}

// mountedAt is the volume mounted at pgdata.
func mountedAt(c *docker.Container) string {
	for _, m := range c.Mounts {
		if m.Destination == pgdata {
			return m.Name
		}
	}
	return ""
}

func postgres() *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17"}}}
}

const pgdata = "/var/lib/postgresql/data"

// Deleting a service whose image keeps its data in a volume Docker made
// destroys that data, and the plan says so, with where; the server keeps
// the volume for 7 days, so undoing the deletion mounts it again, data
// and all, and the plan says it restores.
func TestDeletingAServiceKeepsItsImagesData(t *testing.T) {
	e := dockertest.New(t)
	images(e)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reg, vols := keptDrivers(e, &now)
	l := resource.Local{Drivers: reg}
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}
	apply(t, l, ref, postgres())
	name := service.ContainerName(ref)
	first := mountedAt(e.Container(name))

	del := apply(t, l, ref, nil)
	if !del.Destroys || !slices.Equal(del.Kept, []string{pgdata}) {
		t.Errorf("deleting the service = destroys %v, kept %v; want it to destroy the data at %s", del.Destroys, del.Kept, pgdata)
	}
	if e.Volume(first) == nil {
		t.Fatal("the image's volume went with the container")
	}
	undo := apply(t, l, ref, postgres())
	if undo.Action != resource.ActionCreate || !undo.Restores {
		t.Errorf("undoing the deletion = %s, restores %v; want a create that restores", undo.Action, undo.Restores)
	}
	if second := mountedAt(e.Container(name)); second != first {
		t.Errorf("the undo mounts %s at %s, not %s, which has the data", second, pgdata, first)
	}
	unchanged(t, l, ref, postgres())

	// An undo planned within the 7 days and applied after them, once the
	// volume is pruned, fails, rather than making the service empty; one
	// planned then makes it anew, and its plan differs.
	apply(t, l, ref, nil)
	again := postgres()
	_ = (&service.Driver{}).Validate(again)
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: again}})
	if err != nil || !steps[0].Restores {
		t.Fatalf("planning the undo on day 6 = %v, %+v", err, steps)
	}
	now = now.Add(8 * 24 * time.Hour)
	if err := vols.Prune(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if e.Volume(first) != nil {
		t.Fatal("the prune left the image's volume")
	}
	if err := l.Apply(t.Context(), steps[0], again); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Errorf("applying the undo planned before the prune = %v, want it refused, as the data is gone", err)
	}
	if e.Container(name) != nil {
		t.Error("the refused undo made the container anyway")
	}
	if step := apply(t, l, ref, again); step.Action != resource.ActionCreate || step.Restores {
		t.Errorf("planning the undo after the prune = %s, restores %v; want a create of its own", step.Action, step.Restores)
	}
}
