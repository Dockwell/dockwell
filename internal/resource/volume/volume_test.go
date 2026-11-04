// SPDX-License-Identifier: AGPL-3.0-only

package volume_test

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

const env = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"

func plan(t *testing.T, l resource.Local, ref resource.Ref, spec *resourcev1.Volume) resource.Step {
	t.Helper()
	var desired []resource.Desired
	if spec == nil {
		desired = []resource.Desired{{Ref: ref}}
	} else {
		if err := l.Drivers.Validate(volume.Kind, spec); err != nil {
			t.Fatal(err)
		}
		desired = []resource.Desired{{Ref: ref, Spec: spec}}
	}
	steps, err := l.Plan(t.Context(), desired)
	if err != nil {
		t.Fatal(err)
	}
	return steps[0]
}

func apply(t *testing.T, l resource.Local, step resource.Step, spec *resourcev1.Volume) {
	t.Helper()
	var err error
	if spec == nil {
		err = l.Apply(t.Context(), step, nil)
	} else {
		err = l.Apply(t.Context(), step, spec)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A volume is named and labelled as Compose names and labels a project's,
// and reads back as its spec. Deleting it destroys its data, so the plan
// says so, but it stays for 7 days: undoing the deletion brings the same
// volume back, data and all, and a prune removes it after.
func TestDeletedVolumesAreKept(t *testing.T) {
	e := dockertest.New(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	rt := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{Path: filepath.Join(t.TempDir(), "agent", "deleted-volumes.json")}, Now: func() time.Time { return now }}
	l := resource.Local{Drivers: resource.NewRegistry(&volume.Driver{Runtime: rt})}
	data := resource.Ref{Kind: volume.Kind, Scope: env, Name: "db-data"}
	spec := &resourcev1.Volume{Labels: map[string]string{"backup": "daily"}}

	step := plan(t, l, data, spec)
	if step.Action != resource.ActionCreate {
		t.Fatalf("first plan = %s", step.Action)
	}
	apply(t, l, step, spec)
	name := "env_01kat4zr6e3v0y5xbkzb5d9q6m_db-data"
	v := e.Volume(name)
	if v == nil || v.Labels[docker.LabelProject] != "env_01kat4zr6e3v0y5xbkzb5d9q6m" || v.Labels[docker.LabelVolume] != "db-data" || v.Driver != "local" {
		t.Fatalf("Docker has %+v, want %s with Compose's labels", v, name)
	}
	if step := plan(t, l, data, spec); step.Action != resource.ActionNone {
		t.Errorf("the volume drifted right after it was made: %s %v", step.Action, step.Changes)
	}

	// Docker can't change a volume, and making it again would empty it.
	changed := &resourcev1.Volume{Labels: map[string]string{"backup": "hourly"}}
	_ = l.Drivers.Validate(volume.Kind, changed)
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: data, Spec: changed}}); err == nil || !strings.Contains(err.Error(), "docker can't change a volume once it is made") {
		t.Errorf("planning a change to a volume = %v, want it refused", err)
	}

	del := plan(t, l, data, nil)
	if del.Action != resource.ActionDelete || !del.Destroys {
		t.Errorf("deleting a volume = %+v, want a delete that destroys data", del.Decision)
	}
	apply(t, l, del, nil)
	if e.Volume(name) == nil {
		t.Fatal("deleting the volume removed it at once; it should stay for 7 days")
	}
	if step := plan(t, l, data, spec); step.Action != resource.ActionCreate {
		t.Errorf("planning the deleted volume again = %s, want a create, as it is deleted", step.Action)
	}

	// Undo: the same volume comes back, with its data.
	before := e.Volume(name).CreatedAt
	apply(t, l, plan(t, l, data, spec), spec)
	if v := e.Volume(name); v == nil || v.CreatedAt != before {
		t.Errorf("undoing the deletion made a new volume, %+v; want the kept one back", v)
	}
	if step := plan(t, l, data, spec); step.Action != resource.ActionNone {
		t.Errorf("the volume brought back drifted: %s", step.Action)
	}

	// Brought back another way, it can't keep its data, so it's refused.
	apply(t, l, plan(t, l, data, nil), nil)
	other := &resourcev1.Volume{Labels: map[string]string{"backup": "never"}}
	err := l.Apply(t.Context(), plan(t, l, data, other), other)
	if err == nil || !strings.Contains(err.Error(), "was deleted 2026-11-04, and is kept with its data until 2026-11-11, but was made another way") {
		t.Errorf("making the kept volume again another way = %v", err)
	}

	// A prune within 7 days keeps it; after, it goes.
	logger := slog.New(slog.DiscardHandler)
	now = now.Add(6 * 24 * time.Hour)
	if err := rt.Prune(t.Context(), logger); err != nil {
		t.Fatal(err)
	}
	if e.Volume(name) == nil {
		t.Fatal("a prune 6 days after removed the deleted volume")
	}
	now = now.Add(24 * time.Hour)
	if err := rt.Prune(t.Context(), logger); err != nil {
		t.Fatal(err)
	}
	if e.Volume(name) != nil {
		t.Error("a prune 7 days after kept the deleted volume")
	}
	if _, kept, _ := rt.Kept.Since(name); kept {
		t.Error("the pruned volume is still listed as kept")
	}
}

// A kept volume a container still uses stays until none does.
func TestPruneSkipsVolumesInUse(t *testing.T) {
	e := dockertest.New(t)
	e.Registry["busybox:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"sh"}}}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	rt := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{}, Now: func() time.Time { return now }}
	l := resource.Local{Drivers: resource.NewRegistry(&volume.Driver{Runtime: rt})}
	data := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	spec := &resourcev1.Volume{}
	apply(t, l, plan(t, l, data, spec), spec)
	if err := e.Client().PullImage(t.Context(), "busybox:1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Client().CreateContainer(t.Context(), "by-hand", &docker.CreateRequest{
		Config:     docker.Config{Image: "busybox:1"},
		HostConfig: docker.HostConfig{Mounts: []docker.Mount{{Type: "volume", Source: "env_01kat4zr6e3v0y5xbkzb5d9q6m_data", Target: "/d"}}},
	}); err != nil {
		t.Fatal(err)
	}
	apply(t, l, plan(t, l, data, nil), nil)
	now = now.Add(8 * 24 * time.Hour)
	if err := rt.Prune(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if e.Volume("env_01kat4zr6e3v0y5xbkzb5d9q6m_data") == nil {
		t.Error("a prune removed a volume a container uses")
	}
}
