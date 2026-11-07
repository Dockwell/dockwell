// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/volumetool"
)

// A volume holds a database only when it's the data directory or has it inside, and
// where that is comes from the container: postgres:18 keeps it under its volume.
func TestADatabaseIsInTheVolumeThatHoldsItsDataAndNoOther(t *testing.T) {
	pg18 := []string{"PGDATA=/var/lib/postgresql/18/docker"}
	pw := []string{"MYSQL_ROOT_PASSWORD=s"}
	for _, c := range []struct {
		image, dest string
		env         []string
		want        string
	}{
		{"postgres:17", "/var/lib/postgresql/data", nil, "PostgreSQL"},
		{"postgres:17", "/var/lib/postgresql/data/", nil, "PostgreSQL"},
		{"postgres:17", "/var/lib/postgresql", nil, "PostgreSQL"},
		{"postgres:17", "/var/lib/postgresql/data/pg_wal", nil, ""},
		{"postgres:17", "/var/lib/postgresql/archive", nil, ""},
		{"postgres:17", "/var/lib/postgresql-backups", nil, ""},
		{"postgres:18", "/var/lib/postgresql", pg18, "PostgreSQL"},
		{"postgres:18", "/var/lib/postgresql/18/docker", pg18, "PostgreSQL"},
		{"postgres:18", "/var/lib/postgresql/data", pg18, ""},
		{"postgres:17", "/srv/pg", []string{"PGDATA=/srv/pg/data"}, "PostgreSQL"},
		{"postgres:17", "/var/lib/postgresql/data", []string{"PGDATA=/srv/pg/data"}, ""},
		{"mysql:8.4", "/var/lib/mysql", pw, "MySQL"},
		{"mysql:8.4", "/var/lib/mysql-files", pw, ""},
		{"mysql:8.4", "/var/lib/mysql-keyring", pw, ""},
	} {
		ct := &docker.Container{Config: docker.Config{Image: c.image, Env: c.env}, Mounts: []docker.MountPoint{{Name: "v", Destination: c.dest}}}
		got := ""
		if k := kindOf(ct, "v"); k != nil {
			got = k.name
		}
		if got != c.want {
			t.Errorf("%s at %s with %v is %q, want %q", c.image, c.dest, c.env, got, c.want)
		}
	}
}

// A volume above the data's own volume has the directory's name in it and none of the
// data, so restoring it mustn't load a dump over the database.
func TestOnlyTheDeepestMountHoldsADatabase(t *testing.T) {
	ct := &docker.Container{Config: docker.Config{Image: "postgres:17", Env: []string{"PGDATA=/var/lib/postgresql/data"}}, Mounts: []docker.MountPoint{
		{Name: "home", Destination: "/var/lib/postgresql"},
		{Name: "pgdata", Destination: "/var/lib/postgresql/data/"},
		{Name: "wal", Destination: "/var/lib/postgresql/data/pg_wal"},
	}}
	for volume, want := range map[string]bool{"home": false, "pgdata": true, "wal": false} {
		if got := kindOf(ct, volume) != nil; got != want {
			t.Errorf("%s holds the database: %v, want %v", volume, got, want)
		}
	}
}

// dying stops a restore at its nth helper, as an agent that died would: what it did
// stays on the disk, and the Runner that comes back knows only that.
func dying(t *testing.T, b *bench, source resource.RestoreSource, nth int32) *Runner {
	t.Helper()
	dec, _, err := b.runner.JudgeRestore(t.Context(), ref, source)
	if err != nil {
		t.Fatal(err)
	}
	var helpers atomic.Int32
	died, gone, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		close(gone)
		<-ended
	})
	b.engine.Intercept(func(r *http.Request) error {
		if strings.Contains(r.URL.Path, "/images/create") && helpers.Add(1) == nth {
			close(died)
			<-gone
			return errors.New("the agent died")
		}
		return nil
	})
	go func() {
		defer close(ended)
		_ = b.runner.ApplyRestore(context.Background(), resource.Step{Ref: ref, Decision: dec}, source)
	}()
	<-died
	b.engine.Intercept(nil)
	back := &Runner{Client: b.client, Root: b.runner.Root, Binary: b.runner.Binary}
	if err := back.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	return back
}

// A restore that has its copy doesn't need the room it looked for, and using that room
// up mustn't strand the app. Being refused was how it stayed down for good.
func TestAResumeWithItsCopyIsntRefusedForRoom(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "from the backup")
	id := b.run("web", "nginx:1", "/data")
	b.take(true)
	b.write("kept.txt", "changed since")
	source := resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}
	back := dying(t, b, source, 3)
	if st, err := back.stageOf(ref, "chg_1"); err != nil || st.Stage != stageCopied {
		t.Fatalf("stage = %+v, %v", st, err)
	}
	back.Disk = func() disk { return disk{free: 1 << 20, reserve: 1 << 30} }
	dec, _, err := back.JudgeRestore(t.Context(), ref, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := back.ApplyRestore(t.Context(), resource.Step{Ref: ref, Decision: dec}, source); err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if got := b.read("kept.txt"); got != "from the backup" {
		t.Errorf("kept.txt = %q", got)
	}
	if !b.running(id) {
		t.Error("the app stayed down")
	}
}

// A run that stopped the app and died before it had a copy still gets its room looked
// at, and a refusal then starts the app, and doesn't say nothing was stopped.
func TestARefusalOnAResumeStartsWhatStopped(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "from the backup")
	id := b.run("web", "nginx:1", "/data")
	b.take(true)
	b.write("kept.txt", "changed since")
	source := resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}
	back := dying(t, b, source, 2)
	if b.running(id) {
		t.Fatal("the agent started what a restore stopped")
	}
	back.Disk = func() disk { return disk{free: 1 << 30, reserve: 1 << 30} }
	dec, _, err := back.JudgeRestore(t.Context(), ref, source)
	if err != nil {
		t.Fatal(err)
	}
	err = back.ApplyRestore(t.Context(), resource.Step{Ref: ref, Decision: dec}, source)
	if err == nil || !strings.Contains(err.Error(), "takes up to") {
		t.Fatalf("resuming = %v", err)
	}
	if strings.Contains(err.Error(), "nothing was stopped") {
		t.Errorf("it says nothing was stopped, and it stopped web: %v", err)
	}
	if !strings.Contains(err.Error(), "what stopped started again") {
		t.Errorf("it doesn't say web is back: %v", err)
	}
	if !b.running(id) {
		t.Error("a refusal left the app down")
	}
}

// What's gone or damaged by the time a change resumes can't strand the app either.
func TestAResumeThatCantGoOnStartsWhatStopped(t *testing.T) {
	for name, spoil := range map[string]func(file string) error{
		"gone": os.Remove,
		"damaged": func(file string) error {
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			data[len(data)/2] ^= 0xff
			return os.WriteFile(file, data, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBench(t)
			b.write("kept.txt", "from the backup")
			id := b.run("web", "nginx:1", "/data")
			b.take(true)
			b.write("kept.txt", "changed since")
			source := resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}
			back := dying(t, b, source, 3)
			dec, _, err := back.JudgeRestore(t.Context(), ref, source)
			if err != nil {
				t.Fatal(err)
			}
			if err := spoil(filepath.Join(back.dir(ref), "bkp_1.tar.gz")); err != nil {
				t.Fatal(err)
			}
			err = back.ApplyRestore(t.Context(), resource.Step{Ref: ref, Decision: dec}, source)
			if err == nil {
				t.Fatal("it restored")
			}
			if strings.Contains(err.Error(), "nothing was changed or stopped") {
				t.Errorf("it says nothing was stopped, and it stopped web: %v", err)
			}
			if got := b.read("kept.txt"); got != "changed since" {
				t.Errorf("kept.txt = %q", got)
			}
			if !b.running(id) {
				t.Error("the app stayed down")
			}
		})
	}
}

// Started again and cut off before it could say so, a restore has changed the volume
// though nothing's marked stopped, so nothing it says may claim otherwise.
func TestARestoreCutOffAsItEndedStillCounts(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "from the backup")
	b.take(false)
	b.write("kept.txt", "changed since")
	source := resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}
	if err := b.restore(source); err != nil {
		t.Fatal(err)
	}
	st, err := b.runner.stageOf(ref, "chg_1")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.runner.saveState(ref, st, stageReplaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(b.runner.dir(ref), "bkp_1.tar.gz")); err != nil {
		t.Fatal(err)
	}
	err = b.runner.ApplyRestore(t.Context(), resource.Step{Ref: ref}, source)
	if err == nil || !strings.Contains(err.Error(), "What was there is back") {
		t.Errorf("resuming = %v", err)
	}
	if got := b.read("kept.txt"); got != "changed since" {
		t.Errorf("kept.txt = %q", got)
	}
}

// With nothing in the volume there's nothing to make room for, whatever the disk says,
// and no refusal that would read "takes up to 0 B".
func TestAnEmptyVolumeNeedsNoRoomForItsCopy(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "from the backup")
	b.take(false)
	if err := os.Remove(filepath.Join(b.engine.VolumeDir(b.target.Name), "kept.txt")); err != nil {
		t.Fatal(err)
	}
	b.runner.Disk = func() disk { return disk{free: 1 << 20, reserve: 1 << 30} }
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
		t.Fatalf("restoring into an empty volume = %v", err)
	}
	if got := b.read("kept.txt"); got != "from the backup" {
		t.Errorf("kept.txt = %q", got)
	}
}

// A dump takes the place of what's there, so one smaller than that needs no more
// room, and no refusal that would read "needs 0 B more".
func TestADumpSmallerThanWhatItReplacesNeedsNoRoom(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE notes;")
	b.run("db", "postgres:17", "/var/lib/postgresql/data")
	b.take(false)
	b.write("data.sql", strings.Repeat("what the app has now", 1<<10))
	v, err := volumetool.Measure(b.engine.VolumeDir(b.target.Name))
	if err != nil {
		t.Fatal(err)
	}
	b.runner.Disk = func() disk { return disk{free: 1 << 62, reserve: 1 << 61, filesystem: v.Filesystem} }
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
		t.Fatalf("restoring = %v", err)
	}
}

// A dump's load cut short puts back what was there at once, and the change that
// resumes starts the restore over from that copy.
func TestALoadCutShortGoesBackThenStartsOver(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "the database at backup time")
	var once sync.Once
	loading := make(chan struct{})
	var cut atomic.Bool
	b.engine.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: func(x dockertest.Exec) int {
		if strings.Contains(strings.Join(x.Cmd, " "), "psql") && !cut.Load() {
			_ = os.WriteFile(filepath.Join(x.Volumes["/var/lib/postgresql/data"], "data.sql"), []byte("half of the dump"), 0o600)
			once.Do(func() { close(loading) })
			_, _ = io.Copy(io.Discard, x.Stdin)
			return 0
		}
		return b.postgres(x)
	}})
	db := b.run("db", "postgres:17", "/var/lib/postgresql/data")
	web := b.run("web", "nginx:1", "")
	b.take(false)
	b.write("data.sql", "orders taken since")
	source := resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}
	dec, _, err := b.runner.JudgeRestore(t.Context(), ref, source)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-loading
		cut.Store(true)
		cancel()
	}()
	err = b.runner.ApplyRestore(ctx, resource.Step{Ref: ref, Decision: dec}, source)
	if err == nil || !strings.Contains(err.Error(), "What was there is back") {
		t.Fatalf("a restore cut short said %v", err)
	}
	if got := b.read("data.sql"); got != "orders taken since" {
		t.Errorf("the database holds %q", got)
	}
	if !b.running(db) || !b.running(web) {
		t.Errorf("what stopped didn't start again: db %v, web %v", b.running(db), b.running(web))
	}
	if err := b.restore(source); err != nil {
		t.Fatalf("the resumed change: %v", err)
	}
	if got := b.read("data.sql"); !strings.Contains(got, "the database at backup time") {
		t.Errorf("after it resumed, the database holds %q", got)
	}
	copies := 0
	for _, h := range b.engine.Helpers() {
		if slices.Contains(h.Cmd, "pack") {
			copies++
		}
	}
	if copies != 1 {
		t.Errorf("it copied what was there %d times, over the copy it took", copies)
	}
}

// If what was there can't go back either, nothing starts on half the data, the error
// says where the copy is and what to run, and that's what gets the app back.
func TestARestoreThatCantPutBackStaysStoppedAndSaysHow(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE notes;")
	db := b.run("db", "postgres:17", "/var/lib/postgresql/data")
	web := b.run("web", "nginx:1", "")
	b.take(false)
	b.write("data.sql", "what the app has now")
	b.loadFails = true
	var helpers atomic.Int32
	b.engine.Intercept(func(r *http.Request) error {
		// To measure, to copy what's there and to empty it, then none to put it back.
		if strings.Contains(r.URL.Path, "/images/create") && helpers.Add(1) > 3 {
			return errors.New("no space left on device")
		}
		return nil
	})
	err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err == nil {
		t.Fatal("it restored")
	}
	for _, want := range []string{"the dump didn't load", "stays stopped", b.runner.beforeFile(ref, "chg_1"), "dockwell changes undo chg_1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v doesn't say %q", err, want)
		}
	}
	b.engine.Intercept(nil)
	if b.running(db) || b.running(web) {
		t.Errorf("something started on half the data: db %v, web %v", b.running(db), b.running(web))
	}
	back := &Runner{Client: b.client, Root: b.runner.Root, Binary: b.runner.Binary}
	if err := back.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.running(db) || b.running(web) {
		t.Error("the agent's next start ran the app on half the data")
	}

	// What the error says to run: undoing the change puts it back, and starts the app.
	b.loadFails = false
	b.runner = back
	if err := b.restore(resource.RestoreSource{Undoes: "chg_1", Change: "chg_2"}); err != nil {
		t.Fatalf("undoing it: %v", err)
	}
	if got := b.read("data.sql"); got != "what the app has now" {
		t.Errorf("the database holds %q", got)
	}
	if !b.running(db) || !b.running(web) {
		t.Errorf("the undo left the app down: db %v, web %v", b.running(db), b.running(web))
	}
}

// What was there goes back, and the app still won't start: the data is safe, so the
// agent's next start tries again, and the error names the container, not the service.
func TestARestoreThatStillCantStartSaysWhatToRun(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "from the backup")
	id := b.run("web", "nginx:1", "/data")
	b.take(true)
	b.write("kept.txt", "what the app has now")
	b.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/"+id+"/start") {
			return errors.New("port is already allocated")
		}
		return nil
	})
	err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err == nil {
		t.Fatal("it restored")
	}
	for _, want := range []string{"What was there is back", "so it's down", "docker start " + docker.ContainerName(b.target.Project, "web")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v doesn't say %q", err, want)
		}
	}
	if got := b.read("kept.txt"); got != "what the app has now" {
		t.Errorf("kept.txt = %q", got)
	}
	b.engine.Intercept(nil)
	if err := b.runner.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !b.running(id) {
		t.Error("the agent's next start left the app down, on data that's whole")
	}
	// It undid itself, so offering to undo it again would only be refused, and says why.
	if err := b.restore(resource.RestoreSource{Undoes: "chg_1", Change: "chg_2"}); err == nil || !strings.Contains(err.Error(), "nothing to undo") {
		t.Errorf("undoing a restore that undid itself = %v", err)
	}
}

// A copy that can't start what it stopped says the app is down, and how to start it.
func TestACopyThatCantStartWhatItStoppedSaysItsDown(t *testing.T) {
	b := newBench(t)
	b.write("page.html", "hello")
	id := b.run("web", "nginx:1", "/data")
	b.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/"+id+"/start") {
			return errors.New("port is already allocated")
		}
		return nil
	})
	_, err := b.runner.Take(t.Context(), Take{ID: "bkp_1", Ref: ref, Stop: true})
	if err == nil || !strings.Contains(err.Error(), "so it's down") || !strings.Contains(err.Error(), "docker start "+docker.ContainerName(b.target.Project, "web")) {
		t.Errorf("Take = %v", err)
	}
}

// With no room, a backup stops nothing, and it says what it needed. Going by what's
// in the volume, or the last backup, it leaves a margin.
func TestABackupThereIsNoRoomForStopsNothing(t *testing.T) {
	b := newBench(t)
	const size = 1 << 20
	b.write("data.bin", strings.Repeat("x", size))
	id := b.run("web", "nginx:1", "/data")
	name := docker.ContainerName(b.target.Project, "web")
	starts := b.engine.Started(name)
	b.runner.Disk = func() disk { return disk{free: 1<<30 + size, reserve: 1 << 30} }
	_, err := b.runner.Take(t.Context(), Take{ID: "bkp_1", Ref: ref, Stop: true})
	if err == nil || !strings.Contains(err.Error(), "free") || !strings.Contains(err.Error(), "takes about") {
		t.Fatalf("Take with no margin left = %v", err)
	}
	if !b.running(id) || b.engine.Started(name) != starts {
		t.Error("a backup with no room stopped the app")
	}
	if got := b.files(); len(got) != 0 {
		t.Errorf("on disk: %v", got)
	}
	b.runner.Disk = func() disk { return disk{free: 1<<30 + 2*size, reserve: 1 << 30} }
	if _, err := b.runner.Take(t.Context(), Take{ID: "bkp_1", Ref: ref, Stop: true}); err != nil {
		t.Errorf("Take with room = %v", err)
	}
}

// A backup larger than the last stops before it fills the disk.
func TestABackupStopsBeforeTheDiskFills(t *testing.T) {
	c := &counter{w: io.Discard, check: func() error { return errors.New("full") }}
	if _, err := c.Write(make([]byte, checkEvery-1)); err != nil {
		t.Fatalf("it looked too soon: %v", err)
	}
	if _, err := c.Write(make([]byte, 1)); err == nil {
		t.Error("it wrote past a full disk")
	}
}

// A backup that outgrows what was there for it stops, and leaves nothing behind.
func TestABackupWatchesTheDiskAsItWrites(t *testing.T) {
	b := newBench(t)
	if err := os.MkdirAll(b.runner.dir(ref), 0o700); err != nil {
		t.Fatal(err)
	}
	b.runner.Disk = func() disk { return disk{free: 1 << 20, reserve: 1 << 30} }
	err := b.runner.write(ref, &Manifest{ID: "bkp_1", Format: formatArchive}, func(w io.Writer) (int64, error) {
		_, err := w.Write(make([]byte, checkEvery))
		return 0, err
	}, verifyArchive)
	if err == nil || !strings.Contains(err.Error(), "free") {
		t.Fatalf("write = %v", err)
	}
	if got := b.files(); len(got) != 0 {
		t.Errorf("on disk: %v", got)
	}
}

// No room for a copy of what's there, or for what comes back, fails a restore before
// anything stops or changes.
func TestARestoreThereIsNoRoomForTouchesNothing(t *testing.T) {
	b := newBench(t)
	b.write("data.bin", strings.Repeat("x", 1<<20))
	id := b.run("web", "nginx:1", "/data")
	b.take(true)
	b.write("data.bin", "what the app has now")
	name := docker.ContainerName(b.target.Project, "web")
	starts := b.engine.Started(name)
	untouched := func(err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "nothing was stopped or changed") {
			t.Fatalf("restoring with no room = %v", err)
		}
		if !b.running(id) || b.engine.Started(name) != starts {
			t.Error("a restore with no room stopped the app")
		}
		if got := b.read("data.bin"); got != "what the app has now" {
			t.Errorf("data.bin = %q", got)
		}
		if st, err := b.runner.stageOf(ref, "chg_1"); err != nil || st.Stage != "" {
			t.Errorf("stage = %+v, %v", st, err)
		}
	}
	source := resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}

	// No room where backups go, for a copy of what's there.
	b.runner.Disk = func() disk { return disk{free: 1 << 30, reserve: 1 << 30} }
	untouched(b.restore(source), "a copy of what's in data takes up to")

	// No room in the volume for what comes back.
	b.runner.Disk = func() disk { return disk{free: 1 << 40, reserve: 1 << 30} }
	m, err := readManifest(filepath.Join(b.runner.dir(ref), "bkp_1.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.Unpacked = 1 << 62
	if err := writeManifest(filepath.Join(b.runner.dir(ref), "bkp_1.json"), m); err != nil {
		t.Fatal(err)
	}
	untouched(b.restore(source), "this backup needs")

	// Both on one disk: what comes back has to leave alone what backups do, which it
	// needn't on a disk of its own.
	v, err := volumetool.Measure(b.engine.VolumeDir(b.target.Name))
	if err != nil {
		t.Fatal(err)
	}
	m.Unpacked = 1
	if err := writeManifest(filepath.Join(b.runner.dir(ref), "bkp_1.json"), m); err != nil {
		t.Fatal(err)
	}
	b.runner.Disk = func() disk { return disk{free: 1 << 62, reserve: 1 << 61, filesystem: v.Filesystem} }
	untouched(b.restore(source), "this backup needs")
}

// A copy of a database's files opens only under the release that wrote it, so a plan
// that puts one under another says so. A dump loads into any.
func TestACopyUnderAnotherReleaseOfADatabaseIsSaid(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE notes;")
	b.engine.AddLocalImage("postgres:16.4", dockertest.Image{Exec: b.postgres, Config: docker.Config{Cmd: []string{"postgres"}}})
	stale := b.run("db", "postgres:16.4", "/var/lib/postgresql/data")
	if err := b.client.StopContainer(t.Context(), stale, time.Second); err != nil {
		t.Fatal(err)
	}
	m := b.take(false)
	if m.Format != formatArchive || m.Image != "postgres:16.4" {
		t.Fatalf("manifest = %+v", m)
	}
	if err := b.client.RemoveContainer(t.Context(), stale, true); err != nil {
		t.Fatal(err)
	}
	b.run("db", "postgres:17", "/var/lib/postgresql/data")
	dec, _, err := b.runner.JudgeRestore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err != nil {
		t.Fatal(err)
	}
	if r := dec.Restore; r.WrittenBy != "postgres:16.4" || r.RunsOn != "postgres:17" {
		t.Errorf("the plan says %+v", r)
	}
}

func TestSameSeriesOfADatabase(t *testing.T) {
	pg, my := kindNamed("PostgreSQL"), kindNamed("MySQL")
	for _, c := range []struct {
		k    *databaseKind
		a, b string
		want bool
	}{
		{pg, "postgres:17.2", "postgres:17.11", true},
		{pg, "postgres:17.11@sha256:abc", "postgres:17-alpine", true},
		{pg, "postgres:16.4", "postgres:17.11", false},
		{pg, "docker.io/library/postgres:16", "postgres:17", false},
		{my, "mysql:8.4.2", "mysql:8.4.11", true},
		{my, "mysql:8.0.40", "mysql:8.4.11", false},
		{pg, "postgres:latest", "postgres:latest", true},
		{pg, "postgres:latest", "postgres:17", false},
	} {
		if got := c.k.sameSeries(c.a, c.b); got != c.want {
			t.Errorf("sameSeries(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestPortOf(t *testing.T) {
	for _, c := range []struct {
		cmd  []string
		want string
	}{
		{[]string{"postgres"}, ""},
		{[]string{"postgres", "-p", "5433"}, "5433"},
		{[]string{"postgres", "-p5433"}, "5433"},
		{[]string{"postgres", "--port=5433"}, "5433"},
		{[]string{"postgres", "-c", "port=5433"}, "5433"},
		{[]string{"postgres", "-cport=5433"}, "5433"},
		{[]string{"postgres", "-c", "max_connections=200", "-c", "port=5433"}, "5433"},
		{[]string{"postgres", "-c", "shared_buffers=1GB"}, ""},
	} {
		if got := portOf(c.cmd); got != c.want {
			t.Errorf("portOf(%v) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// A server its command line moved to another port is asked there, by all its tools.
func TestADatabaseOnAnotherPortIsAskedThere(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE notes;")
	var mu sync.Mutex
	var ran []string
	b.engine.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: func(x dockertest.Exec) int {
		mu.Lock()
		ran = append(ran, strings.Join(x.Cmd, " "))
		mu.Unlock()
		return b.postgres(x)
	}})
	b.run("db", "postgres:17", "/var/lib/postgresql/data", "postgres", "-c", "port=5433")
	b.take(false)
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, tool := range []string{"pg_dumpall", "pg_isready", "psql"} {
		found := false
		for _, cmd := range ran {
			if strings.Contains(cmd, tool) {
				found = true
				if !strings.Contains(cmd, "PGPORT=5433") {
					t.Errorf("%s ran without its port: %s", tool, cmd)
				}
			}
		}
		if !found {
			t.Errorf("%s never ran: %v", tool, ran)
		}
	}
}

// Waiting on a database that has stopped, or tools that have said it's no use, spends
// the whole wait on a failure that's known.
func TestWhatWontBecomeReadyIsntWaitedFor(t *testing.T) {
	for name, c := range map[string]struct {
		ready func(b *bench, db *string, x dockertest.Exec) int
		want  string
	}{
		"it stopped": {
			ready: func(b *bench, db *string, _ dockertest.Exec) int {
				go func() { _ = b.client.StopContainer(context.Background(), *db, time.Second) }()
				return 1
			},
			want: "stopped while it set itself up",
		},
		"its tool gave up": {
			ready: func(*bench, *string, dockertest.Exec) int { return 3 },
			want:  "won't take connections",
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBench(t)
			b.write("data.sql", "CREATE TABLE notes;")
			var db string
			b.engine.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: func(x dockertest.Exec) int {
				if strings.Contains(strings.Join(x.Cmd, " "), "pg_isready") {
					return c.ready(b, &db, x)
				}
				return b.postgres(x)
			}})
			db = b.run("db", "postgres:17", "/var/lib/postgresql/data")
			b.take(false)
			source := resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}
			dec, _, err := b.runner.JudgeRestore(t.Context(), ref, source)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			err = b.runner.ApplyRestore(ctx, resource.Step{Ref: ref, Decision: dec}, source)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("restoring = %v", err)
			}
		})
	}
	if never := kindNamed("MySQL").neverReady; !never(1, "ERROR 1045 (28000): Access denied for user 'root'") || never(1, "Can't connect") {
		t.Error("MySQL's refusal isn't read as final")
	}
}

// What a database says of a load can quote rows, and a change's error is there for
// anyone who can read the change.
func TestAFailedLoadKeepsRowsOutOfItsError(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE notes;")
	b.engine.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: func(x dockertest.Exec) int {
		if strings.Contains(strings.Join(x.Cmd, " "), "psql") {
			_, _ = io.Copy(io.Discard, x.Stdin)
			_, _ = io.WriteString(x.Stderr, "ERROR 1062 (23000) at line 12: Duplicate entry 'alice@example.com' for key \"users.email\"\nand the row was ('alice', 'hunter2')\n")
			return 1
		}
		return b.postgres(x)
	}})
	b.run("db", "postgres:17", "/var/lib/postgresql/data")
	b.take(false)
	err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err == nil || !strings.Contains(err.Error(), "ERROR 1062 (23000) at line 12") {
		t.Fatalf("restoring = %v", err)
	}
	for _, leak := range []string{"alice", "hunter2", "users.email"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("%q is in the error: %v", leak, err)
		}
	}
}

// An undo can come for 7 days after a restore ends, however long it took, so what it
// puts back stays for that long.
func TestWhatARestoreReplacedOutlivesItsUndoWindow(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "x")
	b.take(false)
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
		t.Fatal(err)
	}
	st, err := b.runner.stageOf(ref, "chg_1")
	if err != nil {
		t.Fatal(err)
	}
	st.Taken = st.Taken.Add(-3 * time.Hour) // a long restore
	if err := b.runner.saveState(ref, st, stageFinished); err != nil {
		t.Fatal(err)
	}
	ended := st.Ended
	b.runner.Now = func() time.Time { return ended.Add(KeepFor) }
	if err := b.runner.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.runner.beforeFile(ref, "chg_1")); err != nil {
		t.Errorf("it went while its undo is still offered: %v", err)
	}
	b.runner.Now = func() time.Time { return ended.Add(KeepFor + 2*keepPast) }
	if err := b.runner.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.runner.beforeFile(ref, "chg_1")); err == nil {
		t.Error("it never went")
	}
}
