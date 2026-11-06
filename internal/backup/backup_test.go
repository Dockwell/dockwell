// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

const env = "env_01backups"

var ref = resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}

type bench struct {
	t      *testing.T
	engine *dockertest.Engine
	client *docker.Client
	runner *Runner
	target volume.Target
	// fails says what the next load does, for a database that can't take a dump.
	loadFails bool
	dumpFails bool
	dumpShort bool
}

func newBench(t *testing.T) *bench {
	t.Helper()
	e := dockertest.New(t)
	binary := filepath.Join(t.TempDir(), "dockwell")
	if err := os.WriteFile(binary, []byte("#!agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &bench{t: t, engine: e, client: e.Client(), target: volume.TargetOf(ref)}
	b.runner = &Runner{Client: b.client, Root: t.TempDir(), Binary: binary}
	if _, err := b.client.CreateVolume(t.Context(), &docker.VolumeCreate{Name: b.target.Name}); err != nil {
		t.Fatal(err)
	}
	e.AddLocalImage("nginx:1", dockertest.Image{Config: docker.Config{Cmd: []string{"serve"}}})
	e.AddLocalImage("postgres:17", dockertest.Image{Exec: b.postgres, Config: docker.Config{Cmd: []string{"postgres"}}})
	return b
}

// postgres stands in for the database's tools: its data is one file of SQL.
func (b *bench) postgres(x dockertest.Exec) int {
	file := filepath.Join(x.Volumes["/var/lib/postgresql/data"], "data.sql")
	script := strings.Join(x.Cmd, " ")
	switch {
	case strings.Contains(script, "pg_dumpall"):
		if b.dumpFails {
			_, _ = io.WriteString(x.Stderr, "pg_dumpall: error: connection failed\n")
			return 1
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return 1
		}
		_, _ = x.Stdout.Write(data)
		if !b.dumpShort {
			_, _ = io.WriteString(x.Stdout, "\n--\n-- PostgreSQL database cluster dump complete\n--\n")
		}
		return 0
	case strings.Contains(script, "pg_isready"):
		return 0
	case strings.Contains(script, "psql"):
		data, err := io.ReadAll(x.Stdin)
		if err != nil {
			return 1
		}
		_, _ = io.WriteString(x.Stderr, "psql:<stdin>:3: ERROR:  current user cannot be dropped\n")
		_, _ = io.WriteString(x.Stderr, "psql:<stdin>:4: ERROR:  role \"postgres\" already exists\n")
		if b.loadFails {
			_, _ = io.WriteString(x.Stderr, "psql:<stdin>:9: ERROR:  relation \"users\" already exists\n")
			return 0 // psql goes on, as it's told to, and exits 0
		}
		if err := os.WriteFile(file, data, 0o600); err != nil {
			return 1
		}
		return 0
	}
	return 127
}

// run makes a running container of a service with the volume mounted at path.
func (b *bench) run(service, image, path string) string {
	b.t.Helper()
	ctx := b.t.Context()
	req := &docker.CreateRequest{Config: docker.Config{Image: image, Labels: map[string]string{
		docker.LabelProject: b.target.Project, docker.LabelService: service,
	}}}
	if path != "" {
		req.HostConfig.Mounts = []docker.Mount{{Type: "volume", Source: b.target.Name, Target: path}}
	}
	id, err := b.client.CreateContainer(ctx, docker.ContainerName(b.target.Project, service), req)
	if err != nil {
		b.t.Fatal(err)
	}
	if err := b.client.StartContainer(ctx, id); err != nil {
		b.t.Fatal(err)
	}
	return id
}

func (b *bench) running(id string) bool {
	b.t.Helper()
	c, err := b.client.InspectContainer(b.t.Context(), id)
	if err != nil {
		b.t.Fatal(err)
	}
	return c.State.Running
}

func (b *bench) write(name, content string) {
	b.t.Helper()
	path := filepath.Join(b.engine.VolumeDir(b.target.Name), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench) read(name string) string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.engine.VolumeDir(b.target.Name), name))
	if err != nil {
		b.t.Fatal(err)
	}
	return string(data)
}

func (b *bench) has(name string) bool {
	_, err := os.Lstat(filepath.Join(b.engine.VolumeDir(b.target.Name), name))
	return err == nil
}

func (b *bench) take(stop bool) *Manifest {
	b.t.Helper()
	m, err := b.runner.Take(b.t.Context(), Take{ID: "bkp_1", Ref: ref, Stop: stop})
	if err != nil {
		b.t.Fatalf("Take: %v", err)
	}
	return m
}

func (b *bench) restore(source resource.RestoreSource) error {
	b.t.Helper()
	dec, left, err := b.runner.JudgeRestore(b.t.Context(), ref, source)
	if err != nil {
		return err
	}
	if !left {
		b.t.Fatal("JudgeRestore says it's done")
	}
	step := resource.Step{Ref: ref, Decision: dec}
	return b.runner.ApplyRestore(b.t.Context(), step, source)
}

func (b *bench) files() []string {
	b.t.Helper()
	found, err := filepath.Glob(filepath.Join(b.runner.dir(ref), "*"))
	if err != nil {
		b.t.Fatal(err)
	}
	for i := range found {
		found[i] = filepath.Base(found[i])
	}
	return found
}

// A helper can reach nothing, keeps nothing, and leaves nothing.
func TestACopyGoesThroughAHelperThatLeavesNothing(t *testing.T) {
	b := newBench(t)
	b.write("page.html", "hello")
	m := b.take(false)
	if m.Method != resource.BackupCopy || m.Format != formatArchive || m.Size == 0 || m.SHA256 == "" {
		t.Errorf("manifest = %+v", m)
	}
	helpers := b.engine.Helpers()
	if len(helpers) != 1 {
		t.Fatalf("made %d helpers", len(helpers))
	}
	h := helpers[0]
	if h.HostConfig.NetworkMode != "none" || !h.HostConfig.ReadonlyRootfs {
		t.Errorf("a helper can reach out or write to itself: %+v", h.HostConfig)
	}
	if lc := h.HostConfig.LogConfig; lc == nil || lc.Type != "none" {
		t.Errorf("Docker would keep a copy of the archive in the helper's log: %+v", lc)
	}
	if len(h.HostConfig.Mounts) != 1 || !h.HostConfig.Mounts[0].ReadOnly || h.HostConfig.Mounts[0].Source != b.target.Name {
		t.Errorf("mounts = %+v", h.HostConfig.Mounts)
	}
	if got := b.engine.Containers(); len(got) != 0 {
		t.Errorf("containers left: %v", got)
	}
	if got := b.engine.Images(helperRepository); len(got) != 0 {
		t.Errorf("images left: %v", got)
	}
	if got := b.files(); !slices.Equal(got, []string{"bkp_1.json", "bkp_1.tar.gz"}) {
		t.Errorf("on disk: %v", got)
	}
	var saved Manifest
	raw, err := os.ReadFile(filepath.Join(b.runner.dir(ref), "bkp_1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.SHA256 != m.SHA256 || saved.Environment != env || saved.Volume != "data" {
		t.Errorf("the manifest on disk = %+v", saved)
	}
	fi, err := os.Stat(filepath.Join(b.runner.dir(ref), "bkp_1.tar.gz"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the backup's mode = %v, %v", fi.Mode().Perm(), err)
	}
}

// Removing a helper gets its own time once the helper is done, however long the copy
// took: a large volume mustn't leave one behind.
func TestAHelperGoesHoweverLongItRan(t *testing.T) {
	b := newBench(t)
	b.write("page.html", "hello")
	b.runner.CleanupWait = 500 * time.Millisecond
	var once sync.Once
	slow := writerFunc(func(p []byte) (int, error) {
		once.Do(func() { time.Sleep(700 * time.Millisecond) })
		return len(p), nil
	})
	if _, err := b.runner.tool(t.Context(), b.target, "pack", docker.Streams{Stdout: slow}); err != nil {
		t.Fatal(err)
	}
	if got := b.engine.Containers(); len(got) != 0 {
		t.Errorf("containers left: %v", got)
	}
	if got := b.engine.Images(helperRepository); len(got) != 0 {
		t.Errorf("images left: %v", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestACopyOfARunningAppSaysSo(t *testing.T) {
	b := newBench(t)
	b.write("page.html", "hello")
	id := b.run("web", "nginx:1", "/data")
	p, err := b.runner.Preview(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if p.Method != resource.BackupCopyRunning || !slices.Equal(p.Running, []string{"web"}) {
		t.Errorf("preview = %+v", p)
	}
	if m := b.take(false); m.Method != resource.BackupCopyRunning {
		t.Errorf("method = %q", m.Method)
	}
	if !b.running(id) || b.engine.Started(docker.ContainerName(b.target.Project, "web")) != 1 {
		t.Error("it stopped what it wasn't asked to")
	}
}

func TestACopyStopsWhatItWasAskedToAndStartsItAgain(t *testing.T) {
	b := newBench(t)
	b.write("page.html", "hello")
	id := b.run("web", "nginx:1", "/data")
	other := b.run("cache", "nginx:1", "")
	if m := b.take(true); m.Method != resource.BackupCopy {
		t.Errorf("method = %q", m.Method)
	}
	if !b.running(id) || !b.running(other) {
		t.Error("what stopped didn't start again")
	}
	if got := b.engine.Started(docker.ContainerName(b.target.Project, "web")); got != 2 {
		t.Errorf("web started %d times, want a start and a restart", got)
	}
	if got := b.engine.Started(docker.ContainerName(b.target.Project, "cache")); got != 1 {
		t.Errorf("what doesn't mount the volume restarted: %d", got)
	}
	if list, err := b.runner.readStopped(); err != nil || len(list) != 0 {
		t.Errorf("what stopped is still on the list: %v, %v", list, err)
	}
}

// What was stopped starts again, whatever comes of the backup.
func TestWhatStoppedStartsAgainIfTheCopyFails(t *testing.T) {
	b := newBench(t)
	b.write("page.html", "hello")
	id := b.run("web", "nginx:1", "/data")
	b.runner.Binary = filepath.Join(t.TempDir(), "gone")
	if _, err := b.runner.Take(t.Context(), Take{ID: "bkp_1", Ref: ref, Stop: true}); err == nil {
		t.Fatal("it took a backup with no helper")
	}
	if !b.running(id) {
		t.Error("what stopped stayed stopped")
	}
	if got := b.files(); len(got) != 0 {
		t.Errorf("a failed backup left %v", got)
	}
}

func TestARunningDatabaseIsDumpedAndNothingStops(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE notes;")
	id := b.run("db", "postgres:17", "/var/lib/postgresql/data")
	p, err := b.runner.Preview(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if p.Method != resource.BackupDump || p.Database != "PostgreSQL" {
		t.Errorf("preview = %+v", p)
	}
	m := b.take(true) // asking to stop changes nothing for a dump
	if m.Method != resource.BackupDump || m.Format != formatDump || m.Database != "PostgreSQL" || m.Image != "postgres:17" {
		t.Errorf("manifest = %+v", m)
	}
	if !b.running(id) || b.engine.Started(docker.ContainerName(b.target.Project, "db")) != 1 {
		t.Error("a dump stopped the database")
	}
	f, err := os.Open(filepath.Join(b.runner.dir(ref), "bkp_1.sql.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	text, err := io.ReadAll(gz)
	if err != nil || !strings.Contains(string(text), "CREATE TABLE notes;") {
		t.Errorf("the dump holds %q, %v", text, err)
	}
	if len(b.engine.Helpers()) != 0 {
		t.Error("a dump went through a helper")
	}
}

// A database that isn't running can't dump, and its files aren't changing, so a
// copy of them is exact.
func TestAStoppedDatabaseIsCopied(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE notes;")
	id := b.run("db", "postgres:17", "/var/lib/postgresql/data")
	if err := b.client.StopContainer(t.Context(), id, time.Second); err != nil {
		t.Fatal(err)
	}
	if m := b.take(false); m.Method != resource.BackupCopy || m.Image != "postgres:17" {
		t.Errorf("manifest = %+v", m)
	}
}

// Another image with a data path, or a database's other volumes, aren't known.
func TestOnlyAKnownDatabaseAtItsDataPathIsDumped(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "x")
	b.engine.AddLocalImage("ghcr.io/acme/postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}})
	b.run("custom", "ghcr.io/acme/postgres:17", "/var/lib/postgresql/data")
	b.run("seed", "postgres:17", "/docker-entrypoint-initdb.d")
	p, err := b.runner.Preview(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if p.Method != resource.BackupCopyRunning {
		t.Errorf("preview = %+v", p)
	}
}

func TestMySQLNeedsItsRootPassword(t *testing.T) {
	mounts := []docker.MountPoint{{Name: "v", Destination: "/var/lib/mysql"}}
	without := &docker.Container{Config: docker.Config{Image: "mysql:8.4.11@sha256:abc"}, Mounts: mounts}
	if kindOf(without, "v") != nil {
		t.Error("it would dump MySQL with no way to sign in")
	}
	with := &docker.Container{Config: docker.Config{Image: "mysql:8.4.11@sha256:abc", Env: []string{"MYSQL_ROOT_PASSWORD=s"}}, Mounts: mounts}
	if k := kindOf(with, "v"); k == nil || k.name != "MySQL" {
		t.Errorf("kind = %+v", k)
	}
}

// A dump that failed isn't a backup, and nor is one that stopped short.
func TestABadDumpLeavesNothing(t *testing.T) {
	for name, set := range map[string]func(*bench){
		"exits badly": func(b *bench) { b.dumpFails = true },
		"stops short": func(b *bench) { b.dumpShort = true },
	} {
		t.Run(name, func(t *testing.T) {
			b := newBench(t)
			b.write("data.sql", "x")
			b.run("db", "postgres:17", "/var/lib/postgresql/data")
			set(b)
			if _, err := b.runner.Take(t.Context(), Take{ID: "bkp_1", Ref: ref}); err == nil {
				t.Fatal("a bad dump counted as a backup")
			}
			if got := b.files(); len(got) != 0 {
				t.Errorf("it left %v", got)
			}
		})
	}
}

func TestOneBackupOfAVolumeAtATime(t *testing.T) {
	b := newBench(t)
	release, err := b.runner.hold(b.target)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := b.runner.Take(t.Context(), Take{ID: "bkp_1", Ref: ref}); err == nil || !strings.Contains(err.Error(), "under way") {
		t.Errorf("Take = %v", err)
	}
}

func TestARestoreOfACopyReplacesWhatsThereAndCanBeUndone(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "from the backup")
	id := b.run("web", "nginx:1", "/data")
	m := b.take(true)

	b.write("kept.txt", "changed since")
	b.write("new.txt", "made since")

	dec, left, err := b.runner.JudgeRestore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err != nil || !left {
		t.Fatalf("JudgeRestore = %v, %v", left, err)
	}
	if !dec.Destroys || dec.Action != resource.ActionReplace || dec.Downtime == 0 {
		t.Errorf("decision = %+v", dec)
	}
	want := resource.RestorePlan{Backup: "bkp_1", Method: resource.BackupCopy, Taken: m.Taken, Size: m.Size, Stops: []string{"web"}}
	if got := *dec.Restore; got.Backup != want.Backup || got.Method != want.Method || got.Size != want.Size || !got.Taken.Equal(want.Taken) || !slices.Equal(got.Stops, want.Stops) {
		t.Errorf("plan = %+v, want %+v", got, want)
	}

	source := resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}
	if err := b.runner.ApplyRestore(t.Context(), resource.Step{Ref: ref, Decision: dec}, source); err != nil {
		t.Fatalf("ApplyRestore: %v", err)
	}
	if got := b.read("kept.txt"); got != "from the backup" {
		t.Errorf("kept.txt = %q", got)
	}
	if b.has("new.txt") {
		t.Error("what was made since is still there")
	}
	if !b.running(id) {
		t.Error("the app didn't start again")
	}

	// The change did it, and can't do it twice.
	if _, left, err := b.runner.JudgeRestore(t.Context(), ref, source); err != nil || left {
		t.Errorf("JudgeRestore after it ran = %v, %v", left, err)
	}
	if err := b.runner.ApplyRestore(t.Context(), resource.Step{Ref: ref, Decision: dec}, source); err != nil {
		t.Errorf("a second ApplyRestore: %v", err)
	}

	// What it replaced comes back, and that can be undone too.
	if err := b.restore(resource.RestoreSource{Undoes: "chg_1", Change: "chg_2"}); err != nil {
		t.Fatalf("undoing: %v", err)
	}
	if got := b.read("kept.txt"); got != "changed since" {
		t.Errorf("kept.txt after undoing = %q", got)
	}
	if got := b.read("new.txt"); got != "made since" {
		t.Errorf("new.txt after undoing = %q", got)
	}
}

func TestARestoreOfADumpMakesTheDatabaseAnew(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE notes;")
	db := b.run("db", "postgres:17", "/var/lib/postgresql/data")
	web := b.run("web", "nginx:1", "")
	b.take(false)

	b.write("data.sql", "DROP TABLE notes;")
	b.write("base/made-since", "a whole database made since")

	dec, _, err := b.runner.JudgeRestore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dec.Restore.Stops, []string{"db", "web"}) || dec.Restore.Method != resource.BackupDump || dec.Restore.Database != "PostgreSQL" {
		t.Errorf("plan = %+v", dec.Restore)
	}
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if got := b.read("data.sql"); !strings.Contains(got, "CREATE TABLE notes;") {
		t.Errorf("the database holds %q", got)
	}
	if b.has("base/made-since") {
		t.Error("what was made since is still there")
	}
	if !b.running(db) || !b.running(web) {
		t.Error("the app didn't start again")
	}
	// Whichever way it was taken, undoing puts back the files.
	if err := b.restore(resource.RestoreSource{Undoes: "chg_1", Change: "chg_2"}); err != nil {
		t.Fatalf("undoing: %v", err)
	}
	if got := b.read("data.sql"); got != "DROP TABLE notes;" {
		t.Errorf("the database holds %q after undoing", got)
	}
	if !b.has("base/made-since") {
		t.Error("undoing didn't bring back what the restore replaced")
	}
}

func TestADumpThatWontLoadPutsBackWhatWasThere(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE notes;")
	db := b.run("db", "postgres:17", "/var/lib/postgresql/data")
	b.take(false)
	b.write("data.sql", "what the app has now")
	b.loadFails = true
	err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err == nil || !strings.Contains(err.Error(), "relation") || !strings.Contains(err.Error(), "holds what it held before") {
		t.Fatalf("restoring = %v", err)
	}
	if strings.Contains(err.Error(), "already exists\n") || strings.Contains(err.Error(), "current user") {
		t.Errorf("it blames what every load says: %v", err)
	}
	if got := b.read("data.sql"); got != "what the app has now" {
		t.Errorf("the database holds %q", got)
	}
	if !b.running(db) {
		t.Error("the database stayed stopped")
	}
}

// A load only says some of what can go wrong: a dump of a whole server into a
// new one always says these two.
func TestWhatEveryLoadSaysIsntAFailure(t *testing.T) {
	for line, failed := range map[string]bool{
		"psql:<stdin>:12: ERROR:  current user cannot be dropped":  false,
		`psql:<stdin>:13: ERROR:  role "postgres" already exists`:  false,
		"psql:<stdin>:13: NOTICE:  database \"x\" does not exist":  false,
		`psql:<stdin>:99: ERROR:  relation "users" already exists`: true,
		"psql:<stdin>:99: FATAL:  terminating connection":          true,
		`psql:<stdin>:99: ERROR:  syntax error at or near "x"`:     true,
	} {
		if got := postgresLoadFailed(line); got != failed {
			t.Errorf("%q: failed = %v, want %v", line, got, failed)
		}
	}
}

// Nothing stops for a backup that wouldn't restore.
func TestADamagedBackupStopsNothing(t *testing.T) {
	b := newBench(t)
	b.write("page.html", "hello")
	id := b.run("web", "nginx:1", "/data")
	b.take(true)
	path := filepath.Join(b.runner.dir(ref), "bkp_1.tar.gz")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	starts := b.engine.Started(docker.ContainerName(b.target.Project, "web"))
	err = b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("restoring = %v", err)
	}
	if !b.running(id) || b.engine.Started(docker.ContainerName(b.target.Project, "web")) != starts {
		t.Error("a damaged backup stopped the app")
	}
	if got := b.read("page.html"); got != "hello" {
		t.Errorf("the volume changed: %q", got)
	}
}

func TestAMissingBackupCantBePlanned(t *testing.T) {
	b := newBench(t)
	if _, _, err := b.runner.JudgeRestore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_gone", Change: "chg_1"}); err == nil || !strings.Contains(err.Error(), "no longer has") {
		t.Errorf("JudgeRestore = %v", err)
	}
	if _, _, err := b.runner.JudgeRestore(t.Context(), ref, resource.RestoreSource{Undoes: "chg_gone", Change: "chg_1"}); err == nil || !strings.Contains(err.Error(), "7 days") {
		t.Errorf("JudgeRestore of an undo = %v", err)
	}
}

// A dump needs its database. Without one, the plan says what to do.
func TestADumpNeedsADatabaseToLoadInto(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "x")
	id := b.run("db", "postgres:17", "/var/lib/postgresql/data")
	b.take(false)
	if err := b.client.RemoveContainer(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	_, _, err := b.runner.JudgeRestore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err == nil || !strings.Contains(err.Error(), "Deploy the service") {
		t.Errorf("JudgeRestore = %v", err)
	}
}

// A restore cut short goes on: what it stopped stays stopped until then, and it
// never copies what's there again, which by then may be half of what it restores.
func TestARestoreCutShortGoesOn(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "from the backup")
	id := b.run("web", "nginx:1", "/data")
	b.take(true)
	b.write("kept.txt", "changed since")

	source := resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}
	// As if the agent died after it copied what's there, and before it unpacked.
	st := &state{Manifest: Manifest{ID: beforePrefix + "chg_1", Environment: env, Volume: "data", Format: formatArchive, Method: resource.BackupCopy, Taken: time.Now()}, Change: "chg_1"}
	if err := b.runner.write(ref, &st.Manifest, func(w io.Writer) (int64, error) {
		stats, err := b.runner.tool(t.Context(), b.target, "pack", docker.Streams{Stdout: w})
		return stats.Changed, err
	}, verifyArchive); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.saveState(ref, st, stageCopied); err != nil {
		t.Fatal(err)
	}
	web := &user{id: id, name: "web", stopWait: time.Second}
	if err := b.runner.stop(t.Context(), "restore-chg_1", []*user{web}, true); err != nil {
		t.Fatal(err)
	}

	// The agent comes back. Its restore is its change's to finish.
	if err := b.runner.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.running(id) {
		t.Fatal("the agent started an app whose restore isn't finished")
	}
	b.write("half.txt", "what a half-done restore left")

	dec, left, err := b.runner.JudgeRestore(t.Context(), ref, source)
	if err != nil || !left {
		t.Fatalf("JudgeRestore = %v, %v", left, err)
	}
	if err := b.runner.ApplyRestore(t.Context(), resource.Step{Ref: ref, Decision: dec}, source); err != nil {
		t.Fatal(err)
	}
	if got := b.read("kept.txt"); got != "from the backup" {
		t.Errorf("kept.txt = %q", got)
	}
	if !b.running(id) {
		t.Error("the app didn't start once it finished")
	}
	// Its copy is still what was there before it began.
	if err := b.restore(resource.RestoreSource{Undoes: "chg_1", Change: "chg_2"}); err != nil {
		t.Fatal(err)
	}
	if got := b.read("kept.txt"); got != "changed since" || b.has("half.txt") {
		t.Errorf("undoing brought back %q, half.txt there: %v", got, b.has("half.txt"))
	}
}

func TestAnAgentThatDiedStartsWhatABackupStopped(t *testing.T) {
	b := newBench(t)
	id := b.run("web", "nginx:1", "/data")
	web := &user{id: id, name: "web", stopWait: time.Second}
	if err := b.runner.stop(t.Context(), "bkp_1", []*user{web}, false); err != nil {
		t.Fatal(err)
	}
	if b.running(id) {
		t.Fatal("it didn't stop")
	}
	if err := os.MkdirAll(b.runner.dir(ref), 0o700); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(b.runner.dir(ref), "bkp_1.partial")
	if err := os.WriteFile(partial, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.client.ImportImage(t.Context(), helperRepository+":left", strings.NewReader("tar")); err != nil {
		t.Fatal(err)
	}
	helper, err := b.client.CreateContainer(t.Context(), "left-behind", &docker.CreateRequest{Config: docker.Config{
		Image: helperRepository + ":left", Labels: map[string]string{labelHelper: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	// A new agent, with only what's on the disk.
	again := &Runner{Client: b.client, Root: b.runner.Root}
	if err := again.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !b.running(id) {
		t.Error("what a backup stopped stayed stopped")
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an unfinished file stayed: %v", err)
	}
	if _, err := b.client.InspectContainer(context.Background(), helper); !docker.IsNotFound(err) {
		t.Errorf("a helper stayed: %v", err)
	}
	if got := b.engine.Images(helperRepository); len(got) != 0 {
		t.Errorf("its image stayed: %v", got)
	}
}

// Recover runs as soon as Docker answers, which can be after a run has started.
func TestRecoverLeavesARunUnderWay(t *testing.T) {
	b := newBench(t)
	id := b.run("web", "nginx:1", "/data")
	release, err := b.runner.hold(b.target)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	web := &user{id: id, name: "web", stopWait: time.Second}
	if err := b.runner.stop(t.Context(), "bkp_1", []*user{web}, false); err != nil {
		t.Fatal(err)
	}
	if err := b.client.ImportImage(t.Context(), helperRepository+":under-way", strings.NewReader("tar")); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.running(id) {
		t.Error("it started what a run under way stopped")
	}
	if got := b.engine.Images(helperRepository); len(got) != 1 {
		t.Errorf("it removed a run's helper: %v", got)
	}
}

func TestWhatARestoreReplacedGoesAfter7Days(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "x")
	b.take(false)
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.Prune(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(b.files(), "before-chg_1.tar.gz") {
		t.Fatalf("it went early: %v", b.files())
	}
	b.runner.Now = func() time.Time { return time.Now().Add(KeepFor + time.Hour) }
	if err := b.runner.Prune(); err != nil {
		t.Fatal(err)
	}
	if got := b.files(); !slices.Equal(got, []string{"bkp_1.json", "bkp_1.tar.gz"}) {
		t.Errorf("on disk: %v", got)
	}
}

func TestDeleteRemovesABackup(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "x")
	b.take(false)
	if err := b.runner.Delete(t.Context(), ref, "bkp_1"); err != nil {
		t.Fatal(err)
	}
	if got := b.files(); len(got) != 0 {
		t.Errorf("on disk: %v", got)
	}
	if err := b.runner.Delete(t.Context(), ref, "bkp_1"); err != nil {
		t.Errorf("deleting one that's gone: %v", err)
	}
	if err := b.runner.Delete(t.Context(), ref, "../../other"); err == nil {
		t.Error("it took a path for an ID")
	}
}
