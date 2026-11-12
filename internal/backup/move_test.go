// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// moveBench is a bench with PostgreSQL 16 on the volume, web beside it in its app,
// and a new volume for PostgreSQL 17 to move to.
func moveBench(t *testing.T) (*bench, Move, string) {
	t.Helper()
	b := newBench(t)
	b.engine.AddLocalImage("postgres:16", dockertest.Image{Exec: b.postgres, Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=16"}}})
	b.engine.AddLocalImage("postgres:17", dockertest.Image{Exec: b.postgres, Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=17"}}})
	to := volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: env, Name: "data-17"})
	if _, err := b.client.CreateVolume(t.Context(), &docker.VolumeCreate{Name: to.Name}); err != nil {
		t.Fatal(err)
	}
	b.write("data.sql", "CREATE TABLE orders (id int);\n")
	b.run("db", "postgres:16", "/var/lib/postgresql/data")
	web := b.run("web", "nginx:1", "")
	mv := Move{
		Service: resource.Ref{Kind: "service", Scope: env, Name: "db"},
		Asked:   resource.MoveAsked{Backup: "bak_move", From: "data", To: "data-17"},
		Change:  "chg_move",
		App: &resourcev1.Container{
			Image: "postgres:17", Env: map[string]string{"POSTGRES_PASSWORD": "example"},
			Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data-17"}, Target: "/var/lib/postgresql/data"}},
		},
	}
	return b, mv, web
}

// A move stops what else of the app runs, dumps the database as a backup of its
// volume, loads that into the new release on the new volume, and only then calls
// the replace, after which what stopped starts again. The old volume is as it was,
// and the loader is gone.
func TestAMoveLoadsTheDumpIntoTheNewRelease(t *testing.T) {
	b, mv, web := moveBench(t)
	plan, downtime, err := b.runner.JudgeMove(t.Context(), mv)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Database != "PostgreSQL" || plan.FromSeries != "16" || plan.ToSeries != "17" || !slices.Equal(plan.Stops, []string{"web"}) || plan.Backup != "bak_move" || downtime <= 0 {
		t.Errorf("the plan = %+v, down for %s", plan, downtime)
	}
	newDir := b.engine.VolumeDir(volume.TargetOf(mv.to()).Name)
	replaced := false
	err = b.runner.MoveDatabase(t.Context(), mv, func(context.Context) error {
		replaced = true
		if b.running(web) {
			t.Error("web runs while the database moves")
		}
		if got, err := os.ReadFile(filepath.Join(newDir, "data.sql")); err != nil || !strings.Contains(string(got), "CREATE TABLE orders") {
			t.Errorf("the new volume holds %q, %v, before the replace", got, err)
		}
		return nil
	})
	if err != nil || !replaced {
		t.Fatalf("the move = %v, replaced %v", err, replaced)
	}
	if !b.running(web) {
		t.Error("web didn't start again")
	}
	if got, _ := os.ReadFile(filepath.Join(b.engine.VolumeDir(b.target.Name), "data.sql")); string(got) != "CREATE TABLE orders (id int);\n" {
		t.Errorf("the old volume holds %q", got)
	}
	if _, err := readManifest(filepath.Join(b.runner.dir(ref), "bak_move.json")); err != nil {
		t.Errorf("the dump isn't kept as a backup of the old volume: %v", err)
	}
	for _, name := range b.engine.Containers() {
		if strings.HasPrefix(name, "dockwell-move-") {
			t.Errorf("the loader %s is left", name)
		}
	}
}

// A move whose replace fails starts what stopped, and its dump goes, as the data is
// where it was; what it says keeps what the replace said, with what that means.
func TestAMoveWhoseReplaceFailsStartsWhatStopped(t *testing.T) {
	b, mv, web := moveBench(t)
	err := b.runner.MoveDatabase(t.Context(), mv, func(context.Context) error {
		return resource.Plainly("db isn't healthy", errors.New("the check failed"))
	})
	if err == nil {
		t.Fatal("a failed replace moved")
	}
	if plain, _ := resource.Explain(err); plain != "db isn't healthy. data still holds what PostgreSQL 16 had, and the rollback puts that back." {
		t.Errorf("it says %q", plain)
	}
	if !b.running(web) {
		t.Error("web didn't start again")
	}
	if _, err := os.Stat(filepath.Join(b.runner.dir(ref), "bak_move.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dump of a move that didn't happen is kept: %v", err)
	}
}

// A database that isn't running can't be dumped, so its move is refused as it's
// planned, with what to do.
func TestAMoveOfAStoppedDatabaseIsRefused(t *testing.T) {
	b, mv, _ := moveBench(t)
	if err := b.client.StopContainer(t.Context(), docker.ContainerName(b.target.Project, "db"), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.runner.JudgeMove(t.Context(), mv); err == nil || !strings.Contains(err.Error(), "db isn't running") {
		t.Errorf("moving a stopped database = %v", err)
	}
}

// MariaDB is dumped with its own tools where its root password is in either of its
// variables, and not where neither has it.
func TestMariaDBNeedsARootPassword(t *testing.T) {
	mounts := []docker.MountPoint{{Name: "v", Destination: "/var/lib/mysql"}}
	for _, env := range [][]string{{"MARIADB_ROOT_PASSWORD=s"}, {"MYSQL_ROOT_PASSWORD=s"}} {
		c := &docker.Container{Config: docker.Config{Image: "mariadb:11.4", Env: env}, Mounts: mounts}
		if k := kindOf(c, "v"); k == nil || k.name != "MariaDB" {
			t.Errorf("with %v, kind = %+v", env, k)
		}
	}
	if kindOf(&docker.Container{Config: docker.Config{Image: "mariadb:11.4", Env: []string{"MARIADB_RANDOM_ROOT_PASSWORD=1"}}, Mounts: mounts}, "v") != nil {
		t.Error("it would dump MariaDB with no way to sign in")
	}
}
