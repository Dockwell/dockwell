// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

// rowsData is a table with rows, as a dump writes them, which a load can lose.
const rowsData = "CREATE TABLE orders (id int);\nCOPY public.orders (id) FROM stdin;\n1\n2\n\\.\n"

// creates records each container the engine is asked to make, from here on.
func creates(b *bench) func() []docker.CreateRequest {
	var made []docker.CreateRequest
	b.engine.Intercept(func(r *http.Request) error {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/containers/create") {
			return nil
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req docker.CreateRequest
		if json.Unmarshal(body, &req) == nil {
			made = append(made, req)
		}
		return nil
	})
	return func() []docker.CreateRequest { return made }
}

// H2: the old release is dumped with everything stopped, the database too, in a
// container of its own, of the image that ran, on no network, so nothing can write
// to it after the dump, from the server or out.
func TestAMoveDumpsTheOldReleaseSealedOff(t *testing.T) {
	b, mv, _ := moveBench(t)
	ctx := t.Context()
	name := docker.ContainerName(b.target.Project, "db")
	old, err := b.client.InspectContainer(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	dbRan, sealed := true, false
	b.onDump = func() {
		dbRan = b.running(name)
		list, err := b.client.ListContainers(ctx, map[string][]string{"label": {labelHelper}})
		if err != nil {
			t.Error(err)
			return
		}
		for i := range list {
			c, err := b.client.InspectContainer(ctx, list[i].ID)
			if err == nil && c.Image == old.Image && c.HostConfig.NetworkMode == "none" && c.State.Running {
				sealed = true
			}
		}
	}
	if err := b.runner.MoveDatabase(ctx, mv, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if dbRan || !sealed {
		t.Errorf("as it was dumped, the database ran: %v, and a sealed container of its image did: %v", dbRan, sealed)
	}
}

// M4: what a move stops is noted as a restore's is, so a crash doesn't start it
// against either release, and a resume finishes it: it starts what stopped and
// reports the dump. A volume that something else mounts isn't filled.
func TestAMovesStopsWaitForItsChange(t *testing.T) {
	b, mv, web := moveBench(t)
	ctx := t.Context()
	var noted stoppedRun
	err := b.runner.MoveDatabase(ctx, mv, func(context.Context) error {
		list, err := b.runner.readStopped()
		noted = list[moveRun(mv.change())]
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !noted.Restore || !slices.Contains(noted.Containers, web) {
		t.Errorf("what the move stopped is noted as %+v, want web's, as a restore's", noted)
	}

	// A run cut short after its replace: what it stopped is still stopped, and its
	// dump unreported. Recover leaves it; the finishing step starts it and reports.
	if err := b.client.StopContainer(ctx, web, 0); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.noteStopped(func(l stoppedFile) {
		l[moveRun(mv.change())] = stoppedRun{Containers: []string{web}, Restore: true}
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if b.running(web) {
		t.Error("the agent's start started what a move stopped")
	}
	if left, err := b.runner.MoveFinishes(mv); err != nil || !left {
		t.Fatalf("a move cut short after its replace has nothing left: %v", err)
	}
	var told *resource.BackupTaken
	finishing := resource.WithOutcome(ctx, func(o resource.Outcome) { told = firstBackup(told, o.Backup) })
	if err := b.runner.FinishMove(finishing, mv); err != nil {
		t.Fatal(err)
	}
	if !b.running(web) || told == nil || told.ID != "bak_move" {
		t.Errorf("finishing: web runs %v, the dump reported %+v", b.running(web), told)
	}
}

func firstBackup(a, b *resource.BackupTaken) *resource.BackupTaken {
	if a != nil {
		return a
	}
	return b
}

// M4 and L6: the new volume isn't filled while something else mounts it, nor where
// it holds data no run of this move put there, as Docker takes on a volume of a
// name it has.
func TestAMoveFillsOnlyANewVolume(t *testing.T) {
	b, mv, web := moveBench(t)
	ctx := t.Context()
	to := volume.TargetOf(mv.to())
	if err := os.WriteFile(filepath.Join(b.engine.VolumeDir(to.Name), "someone.dat"), []byte("not the move's"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := b.runner.MoveDatabase(ctx, mv, func(context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "already holds") || !strings.Contains(err.Error(), "that this move didn't put there") {
		t.Errorf("filling a volume with data in = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(b.engine.VolumeDir(to.Name), "someone.dat")); string(got) != "not the move's" {
		t.Errorf("what was in %s is %q", to.Key, got)
	}
	if !b.running(web) {
		t.Error("web didn't start again")
	}
	if err := os.Remove(filepath.Join(b.engine.VolumeDir(to.Name), "someone.dat")); err != nil {
		t.Fatal(err)
	}
	id, err := b.client.CreateContainer(ctx, "intruder", &docker.CreateRequest{
		Config:     docker.Config{Image: "nginx:1"},
		HostConfig: docker.HostConfig{Mounts: []docker.Mount{{Type: "volume", Source: to.Name, Target: "/srv"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.runner.MoveDatabase(ctx, mv, func(context.Context) error { return nil }); err == nil || !strings.Contains(err.Error(), "is mounted by intruder") {
		t.Errorf("filling a volume something mounts = %v", err)
	}
	_ = id
}

// M3: a move refuses one without room for its dump and the new release's copy, and
// one that fails empties the new volume rather than keep a partial copy.
func TestAMoveNeedsRoomAndLeavesNoCopyBehind(t *testing.T) {
	b, mv, _ := moveBench(t)
	ctx := t.Context()
	// The disk the volume is on holds backups too, and has room for one of the two.
	v, err := b.runner.tool(ctx, volume.TargetOf(mv.from()), "measure", docker.Streams{})
	if err != nil {
		t.Fatal(err)
	}
	b.runner.Disk = func() disk {
		return disk{free: 1 << 20, reserve: 1<<20 - v.Bytes - v.Bytes/2, filesystem: v.Filesystem}
	}
	if _, _, err := b.runner.JudgeMove(ctx, mv); err == nil || !strings.Contains(err.Error(), "for the new release's copy") {
		t.Errorf("moving with no room = %v", err)
	}
	b.runner.Disk = nil
	b.write("data.sql", rowsData)
	b.dropRows = true
	if err := b.runner.MoveDatabase(ctx, mv, func(context.Context) error { return nil }); err == nil {
		t.Fatal("a load that lost its rows moved")
	}
	entries, err := os.ReadDir(b.engine.VolumeDir(volume.TargetOf(mv.to()).Name))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a move that didn't happen left %d entries in the new volume", len(entries))
	}
}

// L1: a database that takes no connections, which pg_dumpall leaves out, is refused
// as the move is planned; and one whose load lost what it held fails the move, as
// the new release has to hold what the old one did.
func TestAMoveHoldsTheNewReleaseToWhatTheOldHeld(t *testing.T) {
	b, mv, _ := moveBench(t)
	ctx := t.Context()
	b.preflightSays = "noconnect archive\n"
	if _, _, err := b.runner.JudgeMove(ctx, mv); err == nil || !strings.Contains(err.Error(), "takes no connections to archive") {
		t.Errorf("a database that takes no connections = %v", err)
	}
	b.preflightSays = ""
	b.write("data.sql", rowsData)
	b.dropRows = true
	err := b.runner.MoveDatabase(ctx, mv, func(context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "doesn't hold what data held after the load") {
		t.Errorf("a load that lost its rows = %v", err)
	}
}

// L4: a database in replication, either way, can't move by a dump.
func TestAMoveRefusesReplication(t *testing.T) {
	b, mv, _ := moveBench(t)
	for says, want := range map[string]string{
		"replicas 1\n":          "has replicas or replication slots",
		"slot standby_1\n":      "replication slots (standby_1)",
		"subscription orders\n": "has replicas or replication slots",
		"replica\n":             "is a replica of another server",
	} {
		b.preflightSays = says
		if _, _, err := b.runner.JudgeMove(t.Context(), mv); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("with %q = %v, want %q", says, err, want)
		}
	}
}

// sqlBench is a bench with MySQL or MariaDB from on the volume, to move to to.
func sqlBench(t *testing.T, repo, from, to string) (*bench, Move) {
	t.Helper()
	b := newBench(t)
	for _, tag := range []string{from, to} {
		b.engine.AddLocalImage(repo+":"+tag, dockertest.Image{Exec: b.sql, Config: docker.Config{Cmd: []string{"mysqld"}}})
	}
	if _, err := b.client.CreateVolume(t.Context(), &docker.VolumeCreate{Name: volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: env, Name: "data-" + to}).Name}); err != nil {
		t.Fatal(err)
	}
	b.write("data.sql", "CREATE TABLE shop.orders (id int);\n")
	id, err := b.client.CreateContainer(t.Context(), docker.ContainerName(b.target.Project, "db"), &docker.CreateRequest{
		Config: docker.Config{Image: repo + ":" + from, Env: []string{"MYSQL_ROOT_PASSWORD=example"}, Labels: map[string]string{
			docker.LabelProject: b.target.Project, docker.LabelService: "db",
		}},
		HostConfig: docker.HostConfig{Mounts: []docker.Mount{{Type: "volume", Source: b.target.Name, Target: "/var/lib/mysql"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return b, Move{
		Service: resource.Ref{Kind: "service", Scope: env, Name: "db"},
		Asked:   resource.MoveAsked{Backup: "bak_move", From: "data", To: "data-" + to, Change: "chg_move"},
		App: &resourcev1.Container{
			Image: repo + ":" + to, Env: map[string]string{"MYSQL_ROOT_PASSWORD": "example"},
			Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data-" + to}, Target: "/var/lib/mysql"}},
		},
	}
}

// M5: MySQL starts again on what it loaded with --upgrade=FORCE, so its system
// tables take the new layout, and MariaDB runs its upgrade; one that fails fails
// the move.
func TestAMovedSQLServerTakesItsNewLayout(t *testing.T) {
	b, mv := sqlBench(t, "mysql", "8.0", "8.4")
	made := creates(b)
	if err := b.runner.MoveDatabase(t.Context(), mv, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(made(), func(r docker.CreateRequest) bool {
		return r.Image == "mysql:8.4" && slices.Contains(r.Cmd, "--upgrade=FORCE") && r.HostConfig.NetworkMode == "none"
	}) {
		t.Errorf("MySQL 8.4 never started on what it loaded with --upgrade=FORCE: %+v", made())
	}

	b, mv = sqlBench(t, "mariadb", "10.11", "11.4")
	b.probeSays = "ok"
	if err := b.runner.MoveDatabase(t.Context(), mv, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if b.upgrades != 1 {
		t.Errorf("MariaDB's upgrade ran %d times", b.upgrades)
	}
	b, mv = sqlBench(t, "mariadb", "10.11", "11.4")
	b.probeSays, b.upgradeFails = "ok", true
	if err := b.runner.MoveDatabase(t.Context(), mv, func(context.Context) error { return nil }); err == nil || !strings.Contains(err.Error(), "couldn't bring what it loaded up to its own layout") {
		t.Errorf("a failed upgrade = %v", err)
	}
}

// L3: MySQL 8.4 doesn't load mysql_native_password, so a move to it refuses users
// who sign in with it; a move within 8.0 doesn't ask.
func TestAMoveToMySQL84RefusesNativePasswords(t *testing.T) {
	b, mv := sqlBench(t, "mysql", "8.0", "8.4")
	b.nativeSays = "native app@%\n"
	if _, _, err := b.runner.JudgeMove(t.Context(), mv); err == nil || !strings.Contains(err.Error(), "signs app@% in with mysql_native_password, which MySQL 8.4 doesn't load") {
		t.Errorf("a user on mysql_native_password = %v", err)
	}
}

// L2: a MariaDB whose dump would lose a system-versioned table's history is backed
// up as files, as before it was dumped, and can't move by a dump; one whose tool
// keeps it is dumped, with --dump-history.
func TestAMariaDBDumpKeepsHistoryOrIsCopied(t *testing.T) {
	b, mv := sqlBench(t, "mariadb", "10.6", "10.11")
	b.probeSays = ""
	p, err := b.runner.Preview(t.Context(), mv.from())
	if err != nil {
		t.Fatal(err)
	}
	if p.Method == resource.BackupDump {
		t.Error("a MariaDB whose dump loses history is dumped")
	}
	if _, _, err := b.runner.JudgeMove(t.Context(), mv); err == nil || !strings.Contains(err.Error(), "system-versioned table") {
		t.Errorf("moving it = %v", err)
	}
	b.probeSays = "ok"
	if p, err = b.runner.Preview(t.Context(), mv.from()); err != nil || p.Method != resource.BackupDump {
		t.Errorf("one whose dump keeps history is backed up by %s, %v", p.Method, err)
	}
	if k := kindNamed("MariaDB"); !strings.Contains(strings.Join(k.dump, " "), "--dump-history") {
		t.Error("MariaDB's dump doesn't ask for its history")
	}
}
