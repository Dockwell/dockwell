// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// Review of PR 175: a volume mounted anywhere under a database's data path, or at
// a path that only starts with the same letters, is taken for its data.
func TestReviewKindOfMatchesAnyPathUnderOrBesideTheDataPath(t *testing.T) {
	pw := []string{"MYSQL_ROOT_PASSWORD=s"}
	for _, c := range []struct {
		image, dest string
		env         []string
	}{
		{"postgres:17", "/var/lib/postgresql/archive", nil},
		{"postgres:17", "/var/lib/postgresql-backups", nil},
		{"mysql:8.4", "/var/lib/mysql-files", pw},
		{"mysql:8.4", "/var/lib/mysql-keyring", pw},
	} {
		ct := &docker.Container{Config: docker.Config{Image: c.image, Env: c.env}, Mounts: []docker.MountPoint{
			{Name: "other", Destination: "/var/lib/postgresql/data"}, {Name: "v", Destination: c.dest},
		}}
		if k := kindOf(ct, "v"); k != nil {
			t.Errorf("%s at %s is taken for %s's data", c.image, c.dest, k.name)
		}
	}
}

// End to end on the fake engine: a postgres container with its data in one volume
// and an archive in another. Backing up the archive dumps the database instead,
// and restoring it empties the archive and loads the dump over the live database
// in the other volume, which the plan doesn't name and the undo copy doesn't hold.
func TestReviewRestoringASecondVolumeOfADatabaseRewritesTheDatabase(t *testing.T) {
	b := newBench(t)
	ctx := t.Context()
	if _, err := b.client.CreateVolume(ctx, &docker.VolumeCreate{Name: b.target.Project + "_pgdata"}); err != nil {
		t.Fatal(err)
	}
	pgdata := b.engine.VolumeDir(b.target.Project + "_pgdata")
	if err := os.WriteFile(filepath.Join(pgdata, "data.sql"), []byte("the database at backup time"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.write("wal/0001", "an archived segment")
	req := &docker.CreateRequest{Config: docker.Config{Image: "postgres:17", Labels: map[string]string{
		docker.LabelProject: b.target.Project, docker.LabelService: "db",
	}}}
	req.HostConfig.Mounts = []docker.Mount{
		{Type: "volume", Source: b.target.Project + "_pgdata", Target: "/var/lib/postgresql/data"},
		{Type: "volume", Source: b.target.Name, Target: "/var/lib/postgresql/archive"},
	}
	id, err := b.client.CreateContainer(ctx, docker.ContainerName(b.target.Project, "db"), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.client.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	p, err := b.runner.Preview(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("preview of the archive volume: %+v", p)
	m := b.take(false)
	t.Logf("backup of the archive volume: method %s, database %q", m.Method, m.Database)
	if m.Method == resource.BackupDump {
		t.Errorf("the archive volume's backup is a dump of the database, and holds none of its files")
	}
	// Since the backup, the database moved on, in its own volume.
	if err := os.WriteFile(filepath.Join(pgdata, "data.sql"), []byte("orders taken since"), 0o600); err != nil {
		t.Fatal(err)
	}
	dec, _, err := b.runner.JudgeRestore(ctx, ref, resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the plan names %s and says it stops %v", ref.Name, dec.Restore.Stops)
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(pgdata, "data.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "orders taken since") {
		t.Errorf("restoring the archive volume replaced the database in another volume: it now holds %q", strings.SplitN(string(got), "\n", 2)[0])
	}
	if !b.has("wal/0001") {
		t.Errorf("restoring the archive volume emptied it, and the backup never held its files")
	}
}

// A restore that replaced the data and then can't start what it stopped fails, and
// puts back what was there. What it stopped mustn't stay stopped: the agent's Recover
// starts it, as no change resumes a restore that undid itself.
func TestReviewARestoreThatCantStartWhatItStoppedLeavesItDown(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "from the backup")
	id := b.run("web", "nginx:1", "/data")
	b.take(true)
	b.write("kept.txt", "what the app has now")
	starts := 0
	b.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/"+id+"/start") {
			starts++
			if starts == 1 { // set after the backup, so this is the restore's
				return errors.New("driver failed programming external connectivity: port is already allocated")
			}
		}
		return nil
	})
	err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	t.Logf("restore: %v", err)
	if err == nil {
		t.Fatal("it restored")
	}
	if got := b.read("kept.txt"); got != "what the app has now" {
		t.Fatalf("a failed restore left kept.txt = %q, and it undoes itself", got)
	}
	st, serr := b.runner.stageOf(ref, "chg_1")
	if serr != nil {
		t.Fatal(serr)
	}
	t.Logf("stage %q; the change fails with the data replaced", st.Stage)
	b.engine.Intercept(nil)
	if err := b.runner.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !b.running(id) {
		t.Errorf("after the agent's Recover, web is still stopped, and nothing will start it")
	}
}

// A dump restore that runs out of time while it loads returns at once, without
// putting back what was there, and starts the database and the rest of the app on
// what was loaded so far. On the control plane a call out of time fails the change,
// which then can't be undone, and the plan said "If it won't load, what's there
// now comes back".
func TestReviewADumpRestoreOutOfTimeStartsTheAppOnHalfALoad(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "the database at backup time")
	loading := make(chan struct{})
	b.engine.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: func(x dockertest.Exec) int {
		if strings.Contains(strings.Join(x.Cmd, " "), "psql") {
			file := filepath.Join(x.Volumes["/var/lib/postgresql/data"], "data.sql")
			_ = os.WriteFile(file, []byte("half of the dump"), 0o600)
			close(loading)
			_, _ = io.Copy(io.Discard, x.Stdin) // until the agent hangs up
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
	ctx, cancel := context.WithCancelCause(t.Context())
	go func() {
		<-loading
		cancel(context.DeadlineExceeded)
	}()
	err = b.runner.ApplyRestore(ctx, resource.Step{Ref: ref, Decision: dec}, source)
	t.Logf("restore: %v", err)
	t.Logf("data.sql now: %q; db running %v, web running %v", b.read("data.sql"), b.running(db), b.running(web))
	if b.read("data.sql") == "half of the dump" && (b.running(db) || b.running(web)) {
		t.Errorf("a restore cut short left half a load in the volume and started the app on it")
	}
}

// A copy that stops the app and then can't open its file, as on a disk that went
// read-only, never starts the app again: startAgain runs only inside take, and
// write returns before it calls take. Only the agent's next start does.
func TestReviewACopyThatCantOpenItsFileLeavesTheAppStopped(t *testing.T) {
	b := newBench(t)
	b.write("page.html", "hello")
	id := b.run("web", "nginx:1", "/data")
	dir := b.runner.dir(ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // as a disk remounted read-only
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := b.runner.Take(t.Context(), Take{ID: "bkp_1", Ref: ref, Stop: true})
	t.Logf("take: %v", err)
	if err == nil {
		t.Fatal("it took a backup")
	}
	if !b.running(id) {
		t.Errorf("the backup stopped web, failed, and left it stopped")
	}
}
