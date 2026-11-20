// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// restored is the new database's environment, beside the original's.
const restored = "env_01restored"

// seedBench is a bench whose original database runs PostgreSQL 17 on data, with a
// backup of it taken by the ID the control plane gives, and a new, empty volume
// data in another environment for a restore into a new database to fill.
func seedBench(t *testing.T, stop bool) (*bench, Seed, string) {
	t.Helper()
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE notes;")
	db := b.run("db", "postgres:17", "/var/lib/postgresql/data")
	if stop {
		if err := b.client.StopContainer(t.Context(), db, 0); err != nil {
			t.Fatal(err)
		}
	}
	backupID := id.New(id.Backup)
	if _, err := b.runner.Take(t.Context(), Take{ID: backupID, Ref: ref, Stop: stop}); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if stop {
		if err := b.client.StartContainer(t.Context(), db); err != nil {
			t.Fatal(err)
		}
	}
	// What the original has since, which the new database mustn't.
	b.write("data.sql", "CREATE TABLE notes; CREATE TABLE since;")
	s := Seed{
		Service: resource.Ref{Kind: "service", Scope: restored, Name: "db"},
		Backup:  backupID, From: ref, Change: "chg_seed",
		App: &resourcev1.Container{
			Image: "postgres:17", Env: map[string]string{"POSTGRES_PASSWORD": "example"},
			Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/var/lib/postgresql/data"}},
		},
	}
	if _, err := b.client.CreateVolume(t.Context(), &docker.VolumeCreate{Name: volume.TargetOf(s.to()).Name}); err != nil {
		t.Fatal(err)
	}
	return b, s, db
}

// newVolume reads a file of the new database's volume, or "" where it has none.
func (b *bench) newVolume(s Seed, name string) string {
	data, err := os.ReadFile(filepath.Join(b.engine.VolumeDir(volume.TargetOf(s.to()).Name), name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		b.t.Fatal(err)
	}
	return string(data)
}

// unchanged says the original runs as it did: the same container, started once,
// with what it holds now.
func (b *bench) unchanged(db string) {
	b.t.Helper()
	if !b.running(db) || b.engine.Started(docker.ContainerName(b.target.Project, "db")) != 1 {
		b.t.Error("the original database stopped or started again")
	}
	c, err := b.client.InspectContainer(b.t.Context(), docker.ContainerName(b.target.Project, "db"))
	if err != nil || c.ID != db {
		b.t.Errorf("the original's container is %v, %v, not %s", c, err, db)
	}
	if got := b.read("data.sql"); got != "CREATE TABLE notes; CREATE TABLE since;" {
		b.t.Errorf("the original holds %q", got)
	}
}

// A dump restores into a new database's own volume in a loader of its release, on
// no network, and nothing of the original changes: not its data, not its container.
func TestADumpRestoresIntoANewDatabaseAndLeavesTheOriginal(t *testing.T) {
	b, s, db := seedBench(t, false)
	plan, err := b.runner.JudgeSeed(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.IntoNew || plan.From != ref || plan.Method != resource.BackupDump || plan.Database != "PostgreSQL" || len(plan.Stops) != 0 || plan.Backup != s.Backup {
		t.Errorf("the plan = %+v", plan)
	}
	if err := b.runner.SeedNew(t.Context(), s); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if got := b.newVolume(s, "data.sql"); !strings.Contains(got, "CREATE TABLE notes;") || strings.Contains(got, "since") {
		t.Errorf("the new database holds %q", got)
	}
	b.unchanged(db)
	for _, h := range b.engine.Helpers() {
		if h.HostConfig.NetworkMode != "" && h.HostConfig.NetworkMode != "none" {
			t.Errorf("a helper ran on %s", h.HostConfig.NetworkMode)
		}
	}
	for _, name := range b.engine.Containers() {
		if strings.HasPrefix(name, "dockwell-move-") || strings.HasPrefix(name, "dockwell-helper") {
			t.Errorf("the loader %s is left", name)
		}
	}
	// A run of the same change that finds it done doesn't do it twice.
	b.write("data.sql", "CREATE TABLE notes; CREATE TABLE since;")
	if err := b.runner.SeedNew(t.Context(), s); err != nil {
		t.Errorf("running it again = %v", err)
	}
}

// A copy of files unpacks into the new database's volume, and the original keeps
// running as it is.
func TestACopyOfFilesRestoresIntoANewDatabase(t *testing.T) {
	b, s, db := seedBench(t, true)
	if err := b.runner.SeedNew(t.Context(), s); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if got := b.newVolume(s, "data.sql"); got != "CREATE TABLE notes;" {
		t.Errorf("the new database holds %q", got)
	}
	if !b.running(db) {
		t.Error("the original database stopped")
	}
	if got := b.read("data.sql"); got != "CREATE TABLE notes; CREATE TABLE since;" {
		t.Errorf("the original holds %q", got)
	}
}

// A dump that won't load leaves the new volume empty again, for the change's
// rollback to delete, and the original as it was.
func TestADumpThatWontLoadIntoANewDatabaseLeavesItEmpty(t *testing.T) {
	b, s, db := seedBench(t, false)
	b.loadFails = true
	err := b.runner.SeedNew(t.Context(), s)
	if err == nil || !strings.Contains(err.Error(), "The database it was taken of runs as before") {
		t.Fatalf("restoring = %v", err)
	}
	if got := b.newVolume(s, "data.sql"); got != "" {
		t.Errorf("the new volume still holds %q", got)
	}
	if _, err := os.Stat(b.runner.seedMarker(s)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the restore's note is left: %v", err)
	}
	b.unchanged(db)
}

// A damaged backup is found before anything is written.
func TestADamagedBackupFillsNothing(t *testing.T) {
	b, s, db := seedBench(t, false)
	file := filepath.Join(b.runner.dir(ref), s.Backup+"."+formatDump)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.SeedNew(t.Context(), s); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("restoring a damaged backup = %v", err)
	}
	if got := b.newVolume(s, "data.sql"); got != "" {
		t.Errorf("the new volume holds %q", got)
	}
	b.unchanged(db)
}

// The restore fills only a new volume: not one in the original's environment, among
// its volumes, nor one with data it didn't put there, as the original's own volume
// would be (the review's L5).
func TestARestoreIntoANewDatabaseFillsOnlyANewVolume(t *testing.T) {
	t.Run("in the original's environment", func(t *testing.T) {
		b, s, db := seedBench(t, false)
		s.Service.Scope = env // the new volume would be the original's own
		if _, err := b.runner.JudgeSeed(t.Context(), s); !errors.Is(err, errSameEnvironment) {
			t.Errorf("planning into the original's environment = %v", err)
		}
		if err := b.runner.SeedNew(t.Context(), s); !errors.Is(err, errSameEnvironment) {
			t.Errorf("restoring into the original's environment = %v", err)
		}
		b.unchanged(db)
	})
	t.Run("holds data", func(t *testing.T) {
		b, s, db := seedBench(t, false)
		dir := b.engine.VolumeDir(volume.TargetOf(s.to()).Name)
		if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := b.runner.SeedNew(t.Context(), s); err == nil || !strings.Contains(err.Error(), "already holds") {
			t.Errorf("restoring into a volume with data = %v", err)
		}
		if got := b.newVolume(s, "PG_VERSION"); got != "17" {
			t.Errorf("what the volume held is now %q", got)
		}
		b.unchanged(db)
	})
}

// Only a database Dockwell knows restores into a new copy of itself: a second copy
// of an app would act on whatever its secrets reach, beside the first (the review's
// M2).
func TestOnlyADatabaseRestoresIntoANewOne(t *testing.T) {
	b, s, db := seedBench(t, true)
	s.App = &resourcev1.Container{
		Image:  "ghcr.io/acme/mail-fetcher:2",
		Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/var/lib/fetcher"}},
	}
	if _, err := b.runner.JudgeSeed(t.Context(), s); !errors.Is(err, errNotADatabase) {
		t.Errorf("planning an app's volume into a new copy of it = %v", err)
	}
	if err := b.runner.SeedNew(t.Context(), s); !errors.Is(err, errNotADatabase) {
		t.Errorf("restoring an app's volume into a new copy of it = %v", err)
	}
	if got := b.newVolume(s, "data.sql"); got != "" {
		t.Errorf("the new volume holds %q", got)
	}
	if !b.running(db) {
		t.Error("the original stopped")
	}
}

// After a move to a new release, the database runs on a volume of a new name, and its
// backups are of that one, while its catalog file names the first: the restore fills
// the new database's own data volume, whatever the backup's volume is called (the
// review's M3).
func TestARestoreFillsTheNewDatabasesOwnDataVolume(t *testing.T) {
	b, s, db := seedBench(t, false)
	s.App.Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "pgdata"}, Target: "/var/lib/postgresql/data"}}
	into := resource.Ref{Kind: volume.Kind, Scope: restored, Name: "pgdata"}
	if _, err := b.client.CreateVolume(t.Context(), &docker.VolumeCreate{Name: volume.TargetOf(into).Name}); err != nil {
		t.Fatal(err)
	}
	if s.to() != into {
		t.Fatalf("the restore fills %v, want %v", s.to(), into)
	}
	if _, err := b.runner.JudgeSeed(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.SeedNew(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if got := b.newVolume(s, "data.sql"); !strings.Contains(got, "CREATE TABLE notes;") || strings.Contains(got, "since") {
		t.Errorf("the new database holds %q", got)
	}
	b.unchanged(db)
}

// A damaged backup is refused as it's planned, before anything is placed (the
// review's M5).
func TestADamagedBackupIsRefusedAsItsPlanned(t *testing.T) {
	b, s, _ := seedBench(t, false)
	file := filepath.Join(b.runner.dir(ref), s.Backup+"."+formatDump)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.runner.JudgeSeed(t.Context(), s); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("planning a damaged backup = %v", err)
	}
}

// How far a restore got is this change's alone, as its resume reads it (the review's
// M1). Once the step that made the database is done, its note goes, and the
// container that step made, which carries the change's ID, says it's done instead
// (the re-check's N1).
func TestARestoresNoteSaysHowFarItGotUntilItGoes(t *testing.T) {
	b, s, _ := seedBench(t, false)
	ctx := t.Context()
	if stage, err := b.runner.SeedStage(ctx, s); err != nil || stage != "" {
		t.Errorf("before it began, the stage is %q, %v", stage, err)
	}
	if err := b.runner.SeedNew(ctx, s); err != nil {
		t.Fatal(err)
	}
	if stage, err := b.runner.SeedStage(ctx, s); err != nil || stage != SeedDone {
		t.Errorf("once it loaded, the stage is %q, %v", stage, err)
	}
	other := s
	other.Change = "chg_other"
	if stage, err := b.runner.SeedStage(ctx, other); err != nil || stage != "" {
		t.Errorf("another change reads the stage %q, %v", stage, err)
	}
	if err := b.runner.ForgetSeed(s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.runner.seedMarker(s)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the note is left: %v", err)
	}
	// With no note, and no container this change made, it never began.
	if stage, err := b.runner.SeedStage(ctx, s); err != nil || stage != "" {
		t.Errorf("with the note gone and no container, the stage is %q, %v", stage, err)
	}
	// The container the step made says so.
	target := volume.TargetOf(s.to())
	made := &docker.CreateRequest{Config: docker.Config{Image: "postgres:17", Labels: map[string]string{
		docker.LabelProject: docker.Project(restored), docker.LabelService: "db", container.LabelChange: s.Change,
	}}}
	made.HostConfig.Mounts = []docker.Mount{{Type: "volume", Source: target.Name, Target: "/var/lib/postgresql/data"}}
	if _, err := b.client.CreateContainer(ctx, docker.ContainerName(docker.Project(restored), "db"), made); err != nil {
		t.Fatal(err)
	}
	if stage, err := b.runner.SeedStage(ctx, s); err != nil || stage != SeedDone {
		t.Errorf("with the container this change made, the stage is %q, %v", stage, err)
	}
	if stage, err := b.runner.SeedStage(ctx, other); err != nil || stage != "" {
		t.Errorf("another change reads the container as its own: %q, %v", stage, err)
	}
}

// A later change's rollback that deletes a restored database's volume keeps what it
// holds, as any deleted volume's data is kept, whatever restore once filled it:
// nothing empties it, note or no note (the re-check's N1).
func TestALaterRollbackNeverEmptiesARestoredDatabase(t *testing.T) {
	b, s, _ := seedBench(t, false)
	ctx := t.Context()
	if err := b.runner.SeedNew(ctx, s); err != nil {
		t.Fatal(err)
	}
	// Weeks of use since, and the note left, as the worst case.
	dir := b.engine.VolumeDir(volume.TargetOf(s.to()).Name)
	if err := os.WriteFile(filepath.Join(dir, "weeks.sql"), []byte("INSERT INTO notes VALUES (1);"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &volume.Driver{Runtime: &volume.Docker{Client: b.client, Kept: &volume.Kept{Path: filepath.Join(t.TempDir(), "kept.json")}}, Restores: b.runner}
	later := resource.WithDeploy(ctx, resource.Deploy{Revision: 3, Change: "chg_later_undo", Rollback: true})
	if err := d.Apply(later, resource.Step{Ref: s.to(), Decision: resource.Decision{Action: resource.ActionDelete}}, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "weeks.sql")); err != nil || string(got) != "INSERT INTO notes VALUES (1);" {
		t.Errorf("after a later rollback, what was written since is %q, %v", got, err)
	}
	if got := b.newVolume(s, "data.sql"); !strings.Contains(got, "CREATE TABLE notes;") {
		t.Errorf("after a later rollback, the restored data is %q", got)
	}
}

// A dump loads only into a database of its kind, and a copy of files opens only
// under the release that wrote it.
func TestARestoreIntoANewDatabaseHasToFit(t *testing.T) {
	b, s, _ := seedBench(t, false)
	s.App = &resourcev1.Container{
		Image:  "mariadb:11.4",
		Env:    map[string]string{"MARIADB_ROOT_PASSWORD": "example"},
		Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/var/lib/mysql"}},
	}
	if _, err := b.runner.JudgeSeed(t.Context(), s); err == nil || !strings.Contains(err.Error(), "can't load it") {
		t.Errorf("a PostgreSQL dump into MariaDB = %v", err)
	}
	b, s, _ = seedBench(t, true)
	s.App.Image = "postgres:16"
	if _, err := b.runner.JudgeSeed(t.Context(), s); err == nil || !strings.Contains(err.Error(), "only that release opens") {
		t.Errorf("PostgreSQL 17's files under 16 = %v", err)
	}
	b, s, _ = seedBench(t, false)
	s.App.Mounts = nil
	if _, err := b.runner.JudgeSeed(t.Context(), s); !errors.Is(err, errNotADatabase) {
		t.Errorf("a new database that keeps no data volume = %v", err)
	}
}

// The agent reads a backup only from the backups of the volume the control plane
// names, inside this server's backups: never by a name that leads out of them, and
// never through a link (ADR 0106).
func TestARestoreIntoANewDatabaseReadsOnlyThatVolumesBackups(t *testing.T) {
	refused := func(t *testing.T, b *bench, s Seed) {
		t.Helper()
		if _, err := b.runner.JudgeSeed(t.Context(), s); !errors.Is(err, errOutside) {
			t.Errorf("planning = %v", err)
		}
		if err := b.runner.SeedNew(t.Context(), s); !errors.Is(err, errOutside) {
			t.Errorf("restoring = %v", err)
		}
		if got := b.newVolume(Seed{Service: resource.Ref{Scope: restored}, From: ref}, "data.sql"); got != "" {
			t.Errorf("the new volume holds %q", got)
		}
	}
	for name, change := range map[string]func(s *Seed){
		"a parent environment":      func(s *Seed) { s.From.Scope = ".." },
		"a parent volume":           func(s *Seed) { s.From.Name = ".." },
		"this folder":               func(s *Seed) { s.From.Name = "." },
		"a path for a volume":       func(s *Seed) { s.From.Name = "../../etc" },
		"a path for an environment": func(s *Seed) { s.From.Scope = env + "/../" + env },
		"an absolute path":          func(s *Seed) { s.From.Scope = "/etc" },
		"no environment":            func(s *Seed) { s.From.Scope = "" },
		"a backslash":               func(s *Seed) { s.From.Name = `..\data` },
		"a NUL":                     func(s *Seed) { s.From.Name = "data\x00" },
		"not a volume":              func(s *Seed) { s.From.Kind = "service" },
		"a path for a backup":       func(s *Seed) { s.Backup = "../" + s.Backup },
		"a name that isn't an ID":   func(s *Seed) { s.Backup = "passwd" },
		"another kind of ID":        func(s *Seed) { s.Backup = "chg" + strings.TrimPrefix(s.Backup, "bkp") },
		"a path for the new one":    func(s *Seed) { s.Service.Scope = "../" + env },
		"a path for the change":     func(s *Seed) { s.Change = "../../chg" },
	} {
		t.Run(name, func(t *testing.T) {
			b, s, _ := seedBench(t, false)
			change(&s)
			refused(t, b, s)
		})
	}

	// Links out of the backups, at each level: the environment's folder, the
	// volume's, the manifest and the file.
	outside := func(t *testing.T, b *bench) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.CopyFS(dir, os.DirFS(b.runner.dir(ref))); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	relink := func(t *testing.T, from, to string) {
		t.Helper()
		if err := os.RemoveAll(from); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(to, from); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a linked volume folder", func(t *testing.T) {
		b, s, _ := seedBench(t, false)
		relink(t, b.runner.dir(ref), outside(t, b))
		refused(t, b, s)
	})
	t.Run("a linked environment folder", func(t *testing.T) {
		b, s, _ := seedBench(t, false)
		elsewhere := filepath.Join(t.TempDir(), "env")
		if err := os.MkdirAll(elsewhere, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(outside(t, b), filepath.Join(elsewhere, ref.Name)); err != nil {
			t.Fatal(err)
		}
		relink(t, filepath.Join(b.runner.Root, env), elsewhere)
		refused(t, b, s)
	})
	for _, file := range []string{".json", "." + formatDump} {
		t.Run("a linked "+file, func(t *testing.T) {
			b, s, _ := seedBench(t, false)
			dir := outside(t, b)
			relink(t, filepath.Join(b.runner.dir(ref), s.Backup+file), filepath.Join(dir, s.Backup+file))
			refused(t, b, s)
		})
	}
	t.Run("a manifest of another backup", func(t *testing.T) {
		b, s, _ := seedBench(t, false)
		manifest := filepath.Join(b.runner.dir(ref), s.Backup+".json")
		m, err := readManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		m.ID = id.New(id.Backup)
		if err := writeManifest(manifest, m); err != nil {
			t.Fatal(err)
		}
		refused(t, b, s)
	})

	// A backup the server doesn't have is a plain fact, with what to do.
	b, s, _ := seedBench(t, false)
	s.Backup = id.New(id.Backup)
	if _, err := b.runner.JudgeSeed(t.Context(), s); err == nil || errors.Is(err, errOutside) || !strings.Contains(err.Error(), "no longer has") {
		t.Errorf("a backup the server doesn't have = %v", err)
	}
	s.From.Name = "other"
	if _, err := b.runner.JudgeSeed(t.Context(), s); err == nil || !strings.Contains(err.Error(), "has no backups of other") {
		t.Errorf("a volume with no backups = %v", err)
	}
}

// One that holds an app's database alone restores from that app's page.
func TestAnAppsDatabaseDoesntRestoreIntoANewDatabase(t *testing.T) {
	b, s, _ := seedBench(t, false)
	manifest := filepath.Join(b.runner.dir(ref), s.Backup+".json")
	m, err := readManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	m.OneDatabase = "shop"
	if err := writeManifest(manifest, m); err != nil {
		t.Fatal(err)
	}
	if _, err := b.runner.JudgeSeed(t.Context(), s); err == nil || !strings.Contains(err.Error(), "that app's page") {
		t.Errorf("an app's database = %v", err)
	}
}

// The plan-time read stops when its call ends: a read cancelled partway reads
// nothing more, and a plan whose call ended says so (the re-check's N3).
func TestAPlanTimeReadStopsWithItsCall(t *testing.T) {
	b, s, _ := seedBench(t, false)
	file := filepath.Join(b.runner.dir(ref), s.Backup+"."+formatDump)
	m, err := readManifest(filepath.Join(b.runner.dir(ref), s.Backup+".json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	after := 0
	err = checkWithin(ctx, file, m, func(r io.Reader) error {
		one := make([]byte, 1)
		if _, err := io.ReadFull(r, one); err != nil {
			return err
		}
		cancel()
		n, err := io.Copy(io.Discard, r)
		after = int(n)
		return err
	})
	if !errors.Is(err, context.Canceled) || after != 0 {
		t.Errorf("a read cancelled partway = %v, with %d bytes read after", err, after)
	}
	ended, stop := context.WithCancel(t.Context())
	stop()
	if _, err := b.runner.JudgeSeed(ended, s); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "took longer than planning may") {
		t.Errorf("planning in a call that ended = %v", err)
	}
}

// A backup past the plan-time bound has its header read as it's planned, and the
// whole of it checked as it loads: damage in its middle fails the load before
// anything is written, and a broken start is refused as it's planned (the re-check's
// N3).
func TestALargeBackupIsCheckedWholeAsItLoads(t *testing.T) {
	b, s, db := seedBench(t, false)
	b.runner.PlanCheckUpTo = 1
	file := filepath.Join(b.runner.dir(ref), s.Backup+"."+formatDump)
	// A backup larger than the header read, as a real one is: the same dump, after
	// lines of comments, written and hashed as a backup is.
	data := largerDump(t, b, s, file)
	damaged := slices.Clone(data)
	damaged[len(damaged)-6] ^= 0xff // in the gzip trailer's checksum, past what the header read reads
	if err := os.WriteFile(file, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.runner.JudgeSeed(t.Context(), s); err != nil {
		t.Errorf("planning a large backup read more than its header: %v", err)
	}
	if err := b.runner.SeedNew(t.Context(), s); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("loading a large damaged backup = %v", err)
	}
	if got := b.newVolume(s, "data.sql"); got != "" {
		t.Errorf("the new volume holds %q", got)
	}
	b.unchanged(db)
	broken := slices.Clone(data)
	broken[0] ^= 0xff
	if err := os.WriteFile(file, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.runner.JudgeSeed(t.Context(), s); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("planning a large backup with a broken start = %v", err)
	}
}

// largerDump rewrites a backup's dump with lines of comments ahead of it, so what it
// holds is larger than the header read, and its manifest to match, and says the file.
func largerDump(t *testing.T, b *bench, s Seed, file string) []byte {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(gz)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	for range 20_000 {
		_, _ = w.Write([]byte("-- a line of what a large dump holds\n"))
	}
	_, _ = w.Write(plain)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(b.runner.dir(ref), s.Backup+".json")
	m, err := readManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(out.Bytes())
	m.Size, m.SHA256 = int64(out.Len()), hex.EncodeToString(sum[:])
	if err := writeManifest(manifest, m); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
