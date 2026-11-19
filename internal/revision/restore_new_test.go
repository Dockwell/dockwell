// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// postgres is a PostgreSQL service named db in env, keeping its data in the volume
// named.
func postgres(env, data string) Edit {
	return Edit{
		Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "db"},
		Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: "postgres:17", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: data}, Target: "/var/lib/postgresql/data"}},
		}}},
		Servers: []string{"srv_A"},
	}
}

// A restore into a new database is planned only onto a database the change makes,
// with its data volume made in the same change, never onto what's here: not a
// service that runs, not a volume that's here, not the environment the backup was
// taken in, and not an app that isn't a database. The volume it fills is the new
// database's own, whatever the backup's volume is called, as a move to a new release
// names it anew (ADR 0106, the review's M2 and M3).
func TestARestoreIntoANewDatabaseOnlyFillsWhatTheChangeMakes(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, engines, db := setupDocker(t, b, "srv_A")
		withSeeds(e, engines)
		prod := production(t, db)
		// The original, moved to a volume of a new name.
		apply(t, e, propose(t, e, "Run db", Edit{Ref: resource.Ref{Kind: volume.Kind, Scope: prod, Name: "data-17"}, Spec: &resourcev1.Volume{}, Servers: []string{"srv_A"}}, postgres(prod, "data-17")))
		project := tree(t, db).Projects[0].ID
		apply(t, e, propose(t, e, "Add staging", environment(project, "staging"), environment(project, "test")))
		_, staging, err := tree(t, db).Resolve("", "staging")
		if err != nil {
			t.Fatal(err)
		}
		_, other, err := tree(t, db).Resolve("", "test")
		if err != nil {
			t.Fatal(err)
		}
		from := resource.Ref{Kind: volume.Kind, Scope: prod, Name: "data-17"}
		restore := func(ed Edit) Edit {
			ed.Restore = &resource.RestoreSource{Backup: "bkp_1", Server: "srv_A", From: from}
			return ed
		}
		refused := func(want string, edits ...Edit) {
			t.Helper()
			if _, err := e.Propose(t.Context(), Proposal{Summary: "Restore", Actor: "usr_test", Edits: edits}); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("proposing = %v, want %q", err, want)
			}
		}
		// The database that runs.
		refused("db is here already", restore(postgres(prod, "data-17")))
		// A new database in the original's own environment.
		other2 := postgres(prod, "data-18")
		other2.Ref.Name = "db2"
		refused("in the environment the backup was taken in", Edit{Ref: resource.Ref{Kind: volume.Kind, Scope: prod, Name: "data-18"}, Spec: &resourcev1.Volume{}, Servers: []string{"srv_A"}}, restore(other2))
		// An app that isn't a database, on a volume of its own.
		refused("isn't a database Dockwell knows", data(staging.ID, "srv_A"), restore(uses2(staging.ID)))
		// A new database whose volume nothing makes, which the references refuse first.
		refused("isn't in its environment", restore(postgres(staging.ID, "data")))
		// A new database on a volume that's here already.
		apply(t, e, propose(t, e, "Add data", data(other.ID, "srv_A")))
		refused("data is here already", restore(postgres(other.ID, "data")))
		// The catalog's file names the first volume, not the one it moved to: the new
		// database fills its own, data.
		if _, err := e.Propose(t.Context(), Proposal{Summary: "Restore", Actor: "usr_test", Edits: []Edit{data(staging.ID, "srv_A"), restore(postgres(staging.ID, "data"))}}); err != nil {
			t.Errorf("restoring a moved database's backup into a new one on its first volume's name = %v", err)
		}
	})
}
