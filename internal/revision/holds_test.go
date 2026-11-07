// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// followingPaths is an engine with one server whose agent follows its server paths on
// this disk, as an agent does on its own server.
func followingPaths(t *testing.T, dataDir string) (*Engine, *store.DB) {
	t.Helper()
	db, _ := storetest.Open(t, store.SQLite)
	if _, err := Setup(t.Context(), db, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	agents := onDocker{}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateServer(t.Context(), store.Server{ID: "srv_A", Name: "srv_A", CreatedAt: now, CertSerial: "srv_A", CertNotAfter: now}); err != nil {
			return err
		}
		e := dockertest.New(t)
		e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
		client, kept := e.Client(), &volume.Kept{}
		paths := &container.Paths{Client: client, DataDir: dataDir}
		containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Deleted: kept, Paths: paths}}
		agents["srv_A"] = resource.Local{Drivers: resource.NewRegistry(&containers, &service.Driver{Containers: containers},
			&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: kept, Paths: paths}}, &network.Driver{Client: client})}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&container.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{})
	return New(db, kinds, agents, secretstest.Keeper(db, kinds), slog.New(slog.DiscardHandler)), db
}

// One folder can be written two ways, through a link and not. What holds another's
// source counts however each is written, and whichever is in the plan: the agent says
// where every path on its server leads, not only the plan's. A source that's a link
// itself is still a file in a folder the holder can write, so that counts too.
func TestAHoldWrittenThroughALinkIsCounted(t *testing.T) {
	for name, tt := range map[string]struct {
		holder, held func(disk, srv string) string
		// first says which goes first. The other is the plan, and then new code in
		// the holder is, with nothing of the held one in it.
		heldFirst bool
	}{
		"a source through a link, the holder first": {
			holder: func(disk, _ string) string { return disk },
			held:   func(_, srv string) string { return filepath.Join(srv, "app", "conf") },
		},
		"a source through a link, the holder second": {
			holder:    func(disk, _ string) string { return disk },
			held:      func(_, srv string) string { return filepath.Join(srv, "app", "conf") },
			heldFirst: true,
		},
		"a holder through a link": {
			holder:    func(_, srv string) string { return srv },
			held:      func(disk, _ string) string { return filepath.Join(disk, "app", "conf") },
			heldFirst: true,
		},
		"a source that's a link itself, written through another": {
			holder:    func(disk, _ string) string { return disk },
			held:      func(_, srv string) string { return filepath.Join(srv, "app", "current") },
			heldFirst: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			disk, srv := filepath.Join(dir, "disk"), filepath.Join(dir, "srv")
			if err := os.MkdirAll(filepath.Join(disk, "app", "conf"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(disk, srv); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(disk, "app", "current")); err != nil {
				t.Fatal(err)
			}
			e, db := followingPaths(t, filepath.Join(dir, "dockwell"))
			env := production(t, db)
			app := func(p string, command ...string) Edit {
				return Edit{Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
					Image: "nginx:1.27", Command: command, Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/m"}},
				}}}}
			}
			named := func(name string, ed Edit) Edit {
				ed.Ref = resource.Ref{Kind: service.Kind, Scope: env, Name: name}
				return ed
			}
			holder, held := named("holder", app(tt.holder(disk, srv))), named("held", app(tt.held(disk, srv)))
			first, then := holder, held
			if tt.heldFirst {
				first, then = held, holder
			}
			apply(t, e, propose(t, e, "First", first))
			layout := propose(t, e, "Then", then)
			if !layout.AsGoodAsRoot() {
				t.Fatalf("the layout reads as ordinary: %+v", layout.Resources)
			}
			apply(t, e, layout)
			newCode := propose(t, e, "New code", named("holder", app(tt.holder(disk, srv), "sh")))
			if !newCode.AsGoodAsRoot() {
				t.Errorf("new code in the holder asks nothing: %+v", newCode.Resources)
			}
		})
	}
}
