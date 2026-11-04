// SPDX-License-Identifier: AGPL-3.0-only

package volume_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// Undoing a volume's deletion plans a restore while the server keeps the
// volume, and a create of an empty one once it is gone: the two differ,
// so a plan reviewed on day 6 can't apply on day 8 as the other. Applied
// then, the restore fails, saying the kept copy is gone, rather than
// making the volume empty without a word.
func TestAnUndoAfterThePruneSaysTheDataIsGone(t *testing.T) {
	e := dockertest.New(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	rt := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{Path: filepath.Join(t.TempDir(), "d.json")}, Now: func() time.Time { return now }}
	l := resource.Local{Drivers: resource.NewRegistry(&volume.Driver{Runtime: rt})}
	ref := resource.Ref{Kind: volume.Kind, Scope: env, Name: "db-data"}
	spec := &resourcev1.Volume{}
	apply(t, l, plan(t, l, ref, spec), spec)
	apply(t, l, plan(t, l, ref, nil), nil)

	now = now.Add(6 * 24 * time.Hour)
	undo := plan(t, l, ref, spec)
	if undo.Action != resource.ActionCreate || !undo.Restores {
		t.Fatalf("the undo on day 6 = %s, restores %v; want a create that restores", undo.Action, undo.Restores)
	}
	now = now.Add(2 * 24 * time.Hour)
	if err := rt.Prune(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	again := plan(t, l, ref, spec)
	if again.Action != resource.ActionCreate || again.Restores {
		t.Errorf("the undo on day 8 = %s, restores %v; want a create of an empty volume", again.Action, again.Restores)
	}
	err := l.Apply(t.Context(), undo, spec)
	if err == nil || !strings.Contains(err.Error(), "it's gone now, so this change can't bring the data back") {
		t.Errorf("applying the day-6 undo on day 8 = %v, want it refused, saying the kept copy is gone", err)
	}
	if e.Volume("env_01kat4zr6e3v0y5xbkzb5d9q6m_db-data") != nil {
		t.Error("the refused undo made an empty volume")
	}
	apply(t, l, again, spec)
}

// A volume that binds a server path is host access, with the path, and
// one that binds a path no container may mount is refused, on the
// control plane as written, and on the agent where its symlinks lead.
// Other options are host access too, such as an NFS share.
func TestAVolumesOptionsReachTheServer(t *testing.T) {
	d := &volume.Driver{}
	for name, tc := range map[string]struct {
		opts map[string]string
		want []resource.Access
	}{
		"bind":      {map[string]string{"type": "none", "o": "bind", "device": "/srv/media/"}, []resource.Access{{Kind: resource.AccessPath, Path: "/srv/media"}}},
		"read-only": {map[string]string{"type": "none", "o": "bind,ro", "device": "/srv/media"}, []resource.Access{{Kind: resource.AccessPath, Path: "/srv/media", ReadOnly: true}}},
		"socket":    {map[string]string{"type": "none", "o": "bind", "device": "/run/docker.sock"}, []resource.Access{{Kind: resource.AccessDocker, Path: "/run/docker.sock"}}},
		"nfs":       {map[string]string{"type": "nfs", "o": "addr=10.0.0.2", "device": ":/export"}, []resource.Access{{Kind: resource.AccessMount, Type: "nfs", Path: ":/export"}}},
		"none":      {nil, nil},
	} {
		v := &resourcev1.Volume{Options: tc.opts}
		if err := d.Validate(v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := d.HostAccess(v); !slices.Equal(got, tc.want) {
			t.Errorf("%s reaches %+v, want %+v", name, got, tc.want)
		}
	}
	for dev, want := range map[string]string{
		"/":                       "is the server's whole file system",
		"/var/lib/docker/volumes": "is in Docker's data directory",
		"/var":                    "holds Dockwell's data directory",
		"srv/data":                "isn't absolute",
	} {
		v := &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": dev}}
		if err := d.Validate(v); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("a volume that binds %s = %v, want it refused, as it %s", dev, err, want)
		}
	}

	e := dockertest.New(t)
	dir := t.TempDir()
	data := filepath.Join(dir, "dockwell")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(data, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	rt := &volume.Docker{Client: e.Client(), Kept: &volume.Kept{}, Paths: &container.Paths{Client: e.Client(), DataDir: data}}
	l := resource.Local{Drivers: resource.NewRegistry(&volume.Driver{Runtime: rt})}
	v := &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": filepath.Join(dir, "link", "agent")}}
	if err := d.Validate(v); err != nil {
		t.Fatal(err)
	}
	ref := resource.Ref{Kind: volume.Kind, Scope: env, Name: "keys"}
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: v}}); err == nil || !strings.Contains(err.Error(), "is in Dockwell's data directory") {
		t.Errorf("planning a volume that binds the agent's data through a symlink = %v, want it refused", err)
	}
	if err := l.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, v); err == nil {
		t.Error("applying it anyway made the volume")
	}
}
