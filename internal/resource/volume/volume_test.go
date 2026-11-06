// SPDX-License-Identifier: AGPL-3.0-only

package volume_test

import (
	"cmp"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// A kept volume a container uses again, made by hand since it was
// deleted, stays until none does.
func TestPruneSkipsVolumesInUse(t *testing.T) {
	e := dockertest.New(t)
	e.Registry["busybox:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"sh"}}}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	rt := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{}, Now: func() time.Time { return now }}
	l := resource.Local{Drivers: resource.NewRegistry(&volume.Driver{Runtime: rt})}
	data := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	spec := &resourcev1.Volume{}
	apply(t, l, plan(t, l, data, spec), spec)
	apply(t, l, plan(t, l, data, nil), nil)
	byHand(t, e, "env_01kat4zr6e3v0y5xbkzb5d9q6m_data")
	now = now.Add(8 * 24 * time.Hour)
	if err := rt.Prune(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if e.Volume("env_01kat4zr6e3v0y5xbkzb5d9q6m_data") == nil {
		t.Error("a prune removed a volume a container uses")
	}
}

// byHand makes a container that mounts a volume, by its name in Docker, as
// someone running docker by hand would.
func byHand(t *testing.T, e *dockertest.Engine, name string) {
	t.Helper()
	if _, ok := e.Registry["busybox:1"]; !ok {
		e.Registry["busybox:1"] = dockertest.Image{Config: docker.Config{Cmd: []string{"sh"}}}
	}
	if err := e.Client().PullImage(t.Context(), "busybox:1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Client().CreateContainer(t.Context(), "by-hand", &docker.CreateRequest{
		Config:     docker.Config{Image: "busybox:1"},
		HostConfig: docker.HostConfig{Mounts: []docker.Mount{{Type: "volume", Source: name, Target: "/d"}}},
	}); err != nil {
		t.Fatal(err)
	}
}

// A volume a container mounts can't be deleted: Docker would refuse to
// remove it once it was kept for long enough, and the container would go
// on using a volume the environment no longer has.
func TestVolumesInUseCantBeDeleted(t *testing.T) {
	e := dockertest.New(t)
	rt := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{}}
	l := resource.Local{Drivers: resource.NewRegistry(&volume.Driver{Runtime: rt})}
	data := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	spec := &resourcev1.Volume{}
	apply(t, l, plan(t, l, data, spec), spec)
	byHand(t, e, "env_01kat4zr6e3v0y5xbkzb5d9q6m_data")
	err := l.Apply(t.Context(), plan(t, l, data, nil), nil)
	if err == nil || !strings.Contains(err.Error(), "containers still mount the volume env_01kat4zr6e3v0y5xbkzb5d9q6m_data (by-hand), so it can't go") {
		t.Errorf("deleting a volume a container mounts = %v", err)
	}
	if kept, _ := rt.Kept.Has("env_01kat4zr6e3v0y5xbkzb5d9q6m_data"); kept {
		t.Error("the volume in use was deleted, and kept")
	}
}

// The list of deleted volumes is read from its file once, and each change
// is written through: the agent keeps it, and the next agent reads it.
func TestTheKeptListIsReadOnce(t *testing.T) {
	e := dockertest.New(t)
	path := filepath.Join(t.TempDir(), "deleted-volumes.json")
	rt := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{Path: path}}
	l := resource.Local{Drivers: resource.NewRegistry(&volume.Driver{Runtime: rt})}
	data := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	spec := &resourcev1.Volume{}
	apply(t, l, plan(t, l, data, spec), spec)
	apply(t, l, plan(t, l, data, nil), nil)
	name := "env_01kat4zr6e3v0y5xbkzb5d9q6m_data"

	again := &volume.Kept{Path: path}
	if kept, err := again.Has(name); err != nil || !kept {
		t.Fatalf("an agent started again finds %s kept = %t, %v; want it written through", name, kept, err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if step := plan(t, l, data, spec); step.Action != resource.ActionCreate {
		t.Errorf("planning the deleted volume = %s, want a create, from the list in memory", step.Action)
	}
	if _, err := (&volume.Kept{Path: path}).Has(name); err == nil || !strings.Contains(err.Error(), "Remove the file to forget them") {
		t.Errorf("reading a broken list = %v, want how to fix it", err)
	}
}

// Prune and an undo never cross: a volume an undo brings back while the
// prune is removing it either stays, data and all, or the undo fails,
// saying the data is gone. It is never made anew, empty, without a word.
func TestPruneNeverRemovesAVolumeBroughtBack(t *testing.T) {
	e := dockertest.New(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	rt := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{}, Now: func() time.Time { return now }}
	l := resource.Local{Drivers: resource.NewRegistry(&volume.Driver{Runtime: rt})}
	data := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	spec := &resourcev1.Volume{}
	apply(t, l, plan(t, l, data, spec), spec)
	apply(t, l, plan(t, l, data, nil), nil)
	name := "env_01kat4zr6e3v0y5xbkzb5d9q6m_data"
	undo := plan(t, l, data, spec)
	now = now.Add(volume.KeepFor)

	// The undo, planned before, applies as the prune asks Docker to remove
	// the volume, and has a moment to finish first, as it can if the prune
	// lets go of the list.
	var once sync.Once
	undone := make(chan error, 1)
	e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/volumes/"+name) {
			once.Do(func() {
				go func() { undone <- l.Apply(t.Context(), undo, spec) }()
				select {
				case err := <-undone:
					undone <- err
				case <-time.After(200 * time.Millisecond):
				}
			})
		}
		return nil
	})
	if err := rt.Prune(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	// The prune holds the list as it removes the volume, so it wins here:
	// the undo, which expected the data back, says it is gone.
	if err := <-undone; err == nil || !strings.Contains(err.Error(), "it's gone now") {
		t.Fatalf("the undo = %v, want it refused, as the prune removed the data first", err)
	}
	if e.Volume(name) != nil {
		t.Error("the refused undo made the volume anew, empty")
	}
	if kept, _ := rt.Kept.Has(name); kept {
		t.Error("the pruned volume is still listed as deleted")
	}
	// An undo that comes first keeps the data: one brought back is never
	// pruned.
	apply(t, l, plan(t, l, data, spec), spec)
	apply(t, l, plan(t, l, data, nil), nil)
	apply(t, l, plan(t, l, data, spec), spec)
	now = now.Add(2 * volume.KeepFor)
	if err := rt.Prune(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if e.Volume(name) == nil {
		t.Error("the prune removed a volume an undo brought back")
	}
}

// An edit that gives a volume as it is but for a password keeps the password it was
// made with, and one that gives no options keeps them all, as an export leaves them
// out. An edit that gives a password and differs in anything else is refused, so
// the password goes nowhere it hasn't been. What it does depends only on what the
// edit gave.
func TestAVolumeKeepsItsPassword(t *testing.T) {
	made := map[string]string{"type": "cifs", "device": "//nas/share", "o": "addr=10.0.0.2,username=svc,password=real,pass=also-real,password2=next", "password": "plugin-real"}
	guess := func(o map[string]string) map[string]string {
		out := maps.Clone(made)
		out["o"], out["password"] = "addr=10.0.0.2,username=svc,password=guess,pass=guess2,password2=guess3", "guess4"
		maps.Copy(out, o)
		return out
	}
	for name, c := range map[string]struct {
		driver     string
		edit, want map[string]string
		reason     string
		refused    bool
	}{
		"a wrong password": {edit: guess(nil), want: made, reason: "kept_password"},
		"the right one":    {edit: maps.Clone(made), want: made, reason: "kept_password"},
		"no options":       {edit: nil, want: made, reason: "kept_options"},
		"no password": {
			edit: map[string]string{"type": "nfs", "o": "addr=10.0.0.2"},
			want: map[string]string{"type": "nfs", "o": "addr=10.0.0.2"},
		},
		"a word like one":               {edit: map[string]string{"o": "passwd=x,passive"}, want: map[string]string{"o": "passwd=x,passive"}},
		"no options, on another driver": {driver: "evil/plugin"},
		"another driver":                {driver: "evil/plugin", edit: guess(nil), refused: true},
		"another address":               {edit: guess(map[string]string{"o": "addr=10.0.0.3,username=svc,password=guess,pass=guess2,password2=guess3"}), refused: true},
		"another device":                {edit: guess(map[string]string{"device": "//evil/share"}), refused: true},
		"another type":                  {edit: guess(map[string]string{"type": "nfs"}), refused: true},
		"an option more":                {edit: guess(map[string]string{"sshcmd": "x"}), refused: true},
		"another order":                 {edit: guess(map[string]string{"o": "username=svc,addr=10.0.0.2,password=guess,pass=guess2,password2=guess3"}), refused: true},
		"a password more":               {edit: guess(map[string]string{"o": "addr=10.0.0.2,username=svc,password=guess,pass=guess2,password2=guess3,password=guess5"}), refused: true},
		"a name in capitals":            {edit: guess(map[string]string{"o": "addr=10.0.0.2,username=svc,PASSWORD=guess,pass=guess2,password2=guess3"}), refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			current := &resourcev1.Volume{Driver: "local", Options: maps.Clone(made)}
			desired := &resourcev1.Volume{Driver: cmp.Or(c.driver, "local"), Options: maps.Clone(c.edit)}
			kept, err := (&volume.Driver{}).Keep(current, desired)
			switch {
			case c.refused && err == nil:
				t.Fatalf("it keeps %v, want it refused", desired.GetOptions())
			case c.refused:
				for _, value := range []string{"real", "plugin", "guess", "evil", "10.0.0"} {
					if strings.Contains(err.Error(), value) {
						t.Errorf("the refusal holds %q: %v", value, err)
					}
				}
				if !maps.Equal(desired.GetOptions(), c.edit) {
					t.Errorf("it refused, and changed the edit to %v", desired.GetOptions())
				}
			case err != nil:
				t.Fatalf("it refuses: %v", err)
			case !maps.Equal(desired.GetOptions(), c.want):
				t.Errorf("it keeps %v, want %v", desired.GetOptions(), c.want)
			}
			if kept.Reason != c.reason {
				t.Errorf("it says it kept %q, want %q", kept.Reason, c.reason)
			}
			if (kept.Reason != "") != (kept.Stays != "") {
				t.Errorf("it kept %q and says %q of where that goes", kept.Reason, kept.Stays)
			}
			if !maps.Equal(current.GetOptions(), made) {
				t.Errorf("it changed the volume as it is: %v", current.GetOptions())
			}
		})
	}
	if kept, err := (&volume.Driver{}).Keep(&resourcev1.Volume{}, &resourcev1.Volume{}); kept.Reason != "" || err != nil {
		t.Errorf("a volume with no options, edited with none, = %v, %v", kept, err)
	}
}
