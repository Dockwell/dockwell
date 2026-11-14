// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/backup/seal"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
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
	// dropRows has a load lose every row it was given, as a broken restore would.
	dropRows bool
	// preflightSays is what a move's preflight prints, and nativeSays what it prints
	// of mysql_native_password's users where it's asked.
	preflightSays, nativeSays string
	// onDump runs as a dump does.
	onDump func()
	// dumpedOne are the arguments each dump of one database ran with: the database,
	// then who it ran as, and as what role. admin is the SQL its administrator ran,
	// loads what each load into one database was given and who signed in, and
	// loadsFail fails that many loads into one database first.
	dumpedOne [][]string
	// dumpOneFails names a database whose dump alone fails.
	dumpOneFails string
	admin        []string
	loads        []oneLoad
	loadsFail    int
	// The server's roles, as its administrator's SQL leaves them: whether the owner
	// is kept out, and each role made to load. notOwned is what the database holds
	// that its owner doesn't, and adminFails fails that many of the administrator's
	// asks that hold failsOn: what comes before it in one stands, as psql stops
	// there, and what it says doesn't happen.
	ownerOut   bool
	loaders    map[string]bool
	notOwned   string
	adminFails int
	failsOn    string
	// MySQL and MariaDB: what the probe of a MariaDB says, how many times an upgrade
	// ran and whether it fails, and the commands each server was started with.
	probeSays    string
	upgrades     int
	upgradeFails bool
}

// sql stands in for MySQL's and MariaDB's tools: their data is one file of SQL.
func (b *bench) sql(x dockertest.Exec) int {
	file := filepath.Join(x.Volumes["/var/lib/mysql"], "data.sql")
	script := strings.Join(x.Cmd, " ")
	switch {
	case strings.Contains(script, "SYSTEM VERSIONED"):
		_, _ = io.WriteString(x.Stdout, b.probeSays)
	case strings.Contains(script, "SHOW REPLICA STATUS"):
		_, _ = io.WriteString(x.Stdout, b.preflightSays)
		if strings.Contains(script, "DW_NATIVE=1") {
			_, _ = io.WriteString(x.Stdout, b.nativeSays)
		}
	case strings.Contains(script, "information_schema.schemata"):
		data, _ := os.ReadFile(file)
		rows := 0
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "--") {
				rows++
			}
		}
		_, _ = fmt.Fprintf(x.Stdout, "database fake %d\n", rows)
	case strings.Contains(script, "upgrade"):
		b.upgrades++
		if b.upgradeFails {
			_, _ = io.WriteString(x.Stderr, "Phase 2/8: Installing used storage engines... failed\n")
			return 1
		}
	case strings.Contains(script, "--all-databases"):
		if b.onDump != nil {
			b.onDump()
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return 1
		}
		_, _ = x.Stdout.Write(data)
		_, _ = io.WriteString(x.Stdout, "\n-- Dump completed on 2026-11-12\n")
	case strings.Contains(script, "--binary-mode"):
		data, err := io.ReadAll(x.Stdin)
		if err != nil || os.WriteFile(file, data, 0o600) != nil {
			return 1
		}
	}
	return 0
}

func newBench(t *testing.T) *bench {
	t.Helper()
	e := dockertest.New(t)
	helper := filepath.Join(t.TempDir(), "dockwell")
	if err := os.WriteFile(helper, []byte("#!agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &bench{t: t, engine: e, client: e.Client(), target: volume.TargetOf(ref)}
	b.runner = &Runner{Client: b.client, Root: t.TempDir(), Binary: helper, Work: 1000}
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
	case strings.Contains(script, "noconnect"):
		// A move's preflight, which says what it was told to.
		_, _ = io.WriteString(x.Stdout, b.preflightSays)
		return 0
	case strings.Contains(script, "pg_roles"):
		// A move's census: what it holds, by its one file.
		data, _ := os.ReadFile(file)
		rows := 0
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "--") {
				rows++
			}
		}
		_, _ = fmt.Fprintf(x.Stdout, "database fake %d\n", rows)
		return 0
	case strings.Contains(script, "pg_dumpall"):
		if b.onDump != nil {
			b.onDump()
		}
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
	case strings.Contains(script, "pg_dump "):
		// One database alone, as its owner.
		b.dumpedOne = append(b.dumpedOne, slices.Clone(x.Cmd[4:]))
		if b.dumpOneFails != "" && x.Cmd[4] == b.dumpOneFails {
			_, _ = io.WriteString(x.Stderr, "pg_dump: error: connection to server failed\n")
			return 1
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return 1
		}
		_, _ = x.Stdout.Write(data)
		_, _ = io.WriteString(x.Stdout, "\n--\n-- PostgreSQL database dump complete\n--\n")
		return 0
	case slices.Equal(x.Cmd, []string{"psql", "-V"}):
		_, _ = io.WriteString(x.Stdout, "psql (PostgreSQL) 17.6\n")
		return 0
	case strings.Contains(script, " -w ") && strings.Contains(script, "SELECT 1"):
		_, _ = io.WriteString(x.Stderr, "psql: error: fe_sendauth: no password supplied\n")
		return 2
	case strings.Contains(script, `-d "$d"`):
		// A load into one database, through the copy's path.
		data, err := io.ReadAll(x.Stdin)
		if err != nil {
			return 1
		}
		b.loads = append(b.loads, oneLoad{args: slices.Clone(x.Cmd[4:]), env: slices.Clone(x.Env), sql: string(data)})
		// It has no volume of its own: what it loads goes into the server's.
		into := filepath.Join(b.engine.VolumeDir(b.target.Name), "data.sql")
		if b.loadsFail > 0 {
			b.loadsFail--
			_, _ = io.WriteString(x.Stderr, "psql:<stdin>:4: ERROR:  relation \"orders\" already exists\n")
			if !strings.Contains(x.Cmd[2], " -1 ") {
				// Each statement before it stands, and a dump with --clean drops
				// each table first: the database is left empty.
				_ = os.WriteFile(into, nil, 0o600)
			}
			return 3
		}
		// What it holds is the dump, after the restrict, the role it sets, and what
		// empties the database first.
		sql := string(data)
		if _, after, ok := strings.Cut(sql, "$dw_empty$;\n"); ok {
			sql = after
		}
		lines := strings.SplitAfter(sql, "\n")
		for len(lines) > 0 && (strings.HasPrefix(lines[0], `\restrict `) || strings.HasPrefix(lines[0], "SET ROLE ")) {
			lines = lines[1:]
		}
		if err := os.WriteFile(into, []byte(strings.Join(lines, "")), 0o600); err != nil {
			return 1
		}
		return 0
	case strings.Contains(script, " -A -t ") && strings.Contains(script, " -f -"):
		// Its administrator's SQL from its input, which reads and changes users, not
		// the data. A query on its command line, as a drill asks, isn't this.
		data, _ := io.ReadAll(x.Stdin)
		sql := string(data)
		b.admin = append(b.admin, sql)
		if b.loaders == nil {
			b.loaders = map[string]bool{}
		}
		switch {
		case strings.Contains(sql, "'login', r.rolcanlogin"):
			loader := "null"
			for name := range b.loaders {
				loader = `"` + name + `"`
			}
			_, _ = fmt.Fprintf(x.Stdout, `{"owner":"shop","login":%t,"super":false,"loader":%s}`+"\n", !b.ownerOut, loader)
			return 0
		case strings.Contains(sql, "json_build_object('owner'"):
			_, _ = io.WriteString(x.Stdout, `{"owner":"shop"}`+"\n")
			return 0
		case strings.Contains(sql, "FROM pg_extension x WHERE x.oid >= 16384 AND x.extowner <>"):
			_, _ = io.WriteString(x.Stdout, b.notOwned)
			return 0
		}
		// What it changes of the roles, statement by statement.
		for _, m := range regexp.MustCompile(`CREATE ROLE "(dw_restore_[0-9a-f]+)"`).FindAllStringSubmatch(sql, -1) {
			b.loaders[m[1]] = true
		}
		for _, m := range regexp.MustCompile(`DROP ROLE IF EXISTS "(dw_restore_[0-9a-f]+)"`).FindAllStringSubmatch(sql, -1) {
			if b.adminFails == 0 || !strings.Contains(sql, b.failsOn) {
				delete(b.loaders, m[1])
			}
		}
		if strings.Contains(sql, `ALTER ROLE "shop" NOLOGIN`) {
			b.ownerOut = true
		}
		if b.adminFails > 0 && b.failsOn != "" && strings.Contains(sql, b.failsOn) {
			b.adminFails--
			_, _ = io.WriteString(x.Stderr, "psql:<stdin>:2: ERROR:  canceling statement due to statement timeout\n")
			return 3
		}
		if strings.Contains(sql, `ALTER ROLE "shop" LOGIN;`) {
			b.ownerOut = false
		}
		return 0
	case strings.Contains(script, "pg_isready"):
		return 0
	case len(x.Cmd) > 0 && x.Cmd[0] == "psql" && slices.Contains(x.Cmd, "-c"):
		return b.query(x, file)
	case strings.Contains(script, "psql"):
		data, err := io.ReadAll(x.Stdin)
		if err != nil {
			return 1
		}

		if b.dropRows {
			data = withoutRows(data)
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
func (b *bench) run(service, image, path string, cmd ...string) string {
	b.t.Helper()
	ctx := b.t.Context()
	req := &docker.CreateRequest{Config: docker.Config{Image: image, Cmd: cmd, Labels: map[string]string{
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
	// One to measure the volume before anything stops, and one to read it.
	helpers := b.engine.Helpers()
	if len(helpers) != 2 {
		t.Fatalf("made %d helpers", len(helpers))
	}
	h := helpers[1]
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

// A token can have a backup stop what mounts a volume, and start it again, so this is
// the one start anyone can ask for at will. It's no way round where a server path
// leads: a folder that became a link since keeps what mounts it down, and says why.
// What else it stopped starts as it should.
func TestWhatABackupStoppedStaysDownOnAPathThatMoved(t *testing.T) {
	b := newBench(t)
	b.runner.Paths = &container.Paths{Client: b.client, DataDir: t.TempDir()}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "conf")
	if err := os.Mkdir(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	running := func(service string, binds ...string) string {
		t.Helper()
		id, err := b.client.CreateContainer(ctx, docker.ContainerName(b.target.Project, service), &docker.CreateRequest{
			Config: docker.Config{Image: "nginx:1", Labels: map[string]string{
				docker.LabelProject: b.target.Project, docker.LabelService: service, docker.LabelLeads: "{}",
			}},
			HostConfig: docker.HostConfig{Binds: binds, Mounts: []docker.Mount{{Type: "volume", Source: b.target.Name, Target: "/data"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.client.StartContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	moved, fine := running("moved", conf+":/conf"), running("fine")
	b.take(true)
	if !b.running(moved) || !b.running(fine) {
		t.Fatalf("with nothing moved it started moved: %v, fine: %v", b.running(moved), b.running(fine))
	}

	b.engine.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/stop") && strings.Contains(r.URL.Path, moved) {
			if err := os.Remove(conf); err != nil {
				return err
			}
			if err := os.Symlink("/etc", conf); err != nil {
				return err
			}
		}
		return nil
	})
	_, err = b.runner.Take(ctx, Take{ID: "bkp_2", Ref: ref, Stop: true})
	b.engine.Intercept(nil)
	var down downList
	if !errors.As(err, &down) || !strings.Contains(err.Error(), conf+" leads to /etc now") {
		t.Fatalf("a backup over a path that moved = %v", err)
	}
	if b.running(moved) {
		t.Error("what mounts a link made since was started")
	}
	if !b.running(fine) {
		t.Error("what mounts nothing that moved stayed down")
	}
	// docker start would follow the link, so that's not what it says to do.
	if strings.Contains(err.Error(), "docker start") {
		t.Errorf("it says to start it by hand: %v", err)
	}
}

// bound replaces the bench's volume with one that binds device.
func (b *bench) bound(device string) {
	b.t.Helper()
	if err := b.client.RemoveVolume(b.t.Context(), b.target.Name); err != nil {
		b.t.Fatal(err)
	}
	if _, err := b.client.CreateVolume(b.t.Context(), &docker.VolumeCreate{
		Name: b.target.Name, Driver: "local", DriverOpts: map[string]string{"type": "none", "o": "bind", "device": device},
	}); err != nil {
		b.t.Fatal(err)
	}
}

// A backup is tied to a volume by name alone, so it says what that volume reached, by
// where its path led, and a restore's plan says so. Undoing that puts back what was in
// the volume, which isn't what the backup held.
func TestABackupSaysWhatItsVolumeReached(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link, folder := filepath.Join(dir, "link"), filepath.Join(dir, "folder")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		device, share string
		want          []string
	}{
		"an ordinary volume": {},
		"a folder":           {device: folder},
		"the system":         {device: "/etc", want: []string{"/etc"}},
		"a link to it":       {device: link, want: []string{"/etc"}},
		"an NFS share":       {share: ":/share"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBench(t)
			b.runner.Paths = &container.Paths{Client: b.client, DataDir: t.TempDir()}
			if tt.device != "" {
				b.bound(tt.device)
			}
			if tt.share != "" {
				if err := b.client.RemoveVolume(t.Context(), b.target.Name); err != nil {
					t.Fatal(err)
				}
				if _, err := b.client.CreateVolume(t.Context(), &docker.VolumeCreate{
					Name: b.target.Name, Driver: "local", DriverOpts: map[string]string{"type": "nfs", "o": "addr=10.0.0.5", "device": tt.share},
				}); err != nil {
					t.Fatal(err)
				}
			}
			m := b.take(false)
			if !slices.Equal(m.Reached, tt.want) {
				t.Fatalf("it says it reached %v, want %v", m.Reached, tt.want)
			}
			dec, _, err := b.runner.JudgeRestore(t.Context(), ref, resource.RestoreSource{Backup: m.ID, Server: "srv_A", Change: "chg_1"})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(dec.Restore.Reached, tt.want) {
				t.Errorf("a restore's plan says %v, want %v", dec.Restore.Reached, tt.want)
			}
		})
	}
}

// Docker follows a volume's path when a helper mounts it, and a restore writes over
// everything there. So a link made since the plan stops it before anything stops,
// where it costs nothing, and planned again it says where the path leads.
func TestARestoreChecksWhereItsPathLeads(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "conf")
	if err := os.Mkdir(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	b := newBench(t)
	b.runner.Paths = &container.Paths{Client: b.client, DataDir: t.TempDir()}
	b.bound(conf)
	id := b.run("web", "nginx:1", "/data")
	m := b.take(true)
	if err := os.Remove(conf); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", conf); err != nil {
		t.Fatal(err)
	}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Restore: &resource.RestorePlan{Backup: m.ID}}}
	source := resource.RestoreSource{Backup: m.ID, Server: "srv_A", Change: "chg_1"}
	started := b.engine.Started(docker.ContainerName(b.target.Project, "web"))
	err = b.runner.ApplyRestore(t.Context(), step, source)
	if err == nil || !strings.Contains(err.Error(), conf+" leads to /etc now") || !strings.Contains(err.Error(), "nothing was changed or stopped") {
		t.Fatalf("a restore over a link made since its plan = %v", err)
	}
	if !b.running(id) || b.engine.Started(docker.ContainerName(b.target.Project, "web")) != started {
		t.Error("something stopped")
	}
	// What mounts it hasn't been approved for that, so it goes first.
	if err := b.client.RemoveContainer(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	step.PlannedLeads = map[string]string{conf: "/etc"}
	if err := b.runner.ApplyRestore(t.Context(), step, source); err != nil {
		t.Fatalf("a plan that says where it leads: %v", err)
	}
}

// A restore checks where its volume's path leads again right before it writes, after
// the stops and the copy of what's there, as a link made in that time would be
// followed. Nothing is written over, and it says so.
func TestARestoreChecksAgainBeforeItWrites(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "conf")
	if err := os.Mkdir(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	b := newBench(t)
	b.runner.Paths = &container.Paths{Client: b.client, DataDir: t.TempDir()}
	b.bound(conf)
	m := b.take(false)
	starts := 0
	b.engine.Intercept(func(r *http.Request) error {
		// The first helper to start is the copy of what's there.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start") {
			starts++
			if starts == 1 {
				if err := os.Remove(conf); err != nil {
					return err
				}
				return os.Symlink("/etc", conf)
			}
		}
		return nil
	})
	step := resource.Step{Ref: ref, Decision: resource.Decision{Restore: &resource.RestorePlan{Backup: m.ID}}}
	err = b.runner.ApplyRestore(t.Context(), step, resource.RestoreSource{Backup: m.ID, Server: "srv_A", Change: "chg_1"})
	b.engine.Intercept(nil)
	if err == nil || !strings.Contains(err.Error(), conf+" leads to /etc now") || !strings.Contains(err.Error(), "nothing was written over") {
		t.Fatalf("a link made during the copy of what's there = %v", err)
	}
}

// What a backup reached is worked out again once its bytes are read, and it says what
// either says: a link made while they were read is what they hold.
func TestABackupSaysWhatItsVolumeReachedOnceItsRead(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder, link := filepath.Join(dir, "folder"), filepath.Join(dir, "link")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(folder, link); err != nil {
		t.Fatal(err)
	}
	b := newBench(t)
	b.runner.Paths = &container.Paths{Client: b.client, DataDir: t.TempDir()}
	b.bound(link)
	b.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start") {
			if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return os.Symlink("/etc", link)
		}
		return nil
	})
	m := b.take(false)
	b.engine.Intercept(nil)
	if !slices.Equal(m.Reached, []string{"/etc"}) {
		t.Errorf("it says it reached %v, want /etc", m.Reached)
	}
}

// Recover takes each run on its own. One that a moved path keeps down stays down, and
// says so, and is done with: no other run waits on it, and it isn't tried again.
func TestRecoverGoesPastWhatAMovedPathKeepsDown(t *testing.T) {
	b := newBench(t)
	paths := &container.Paths{Client: b.client, DataDir: t.TempDir()}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "conf")
	if err := os.Mkdir(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	running := func(name string, binds ...string) *user {
		t.Helper()
		id, err := b.client.CreateContainer(t.Context(), docker.ContainerName(b.target.Project, name), &docker.CreateRequest{
			Config:     docker.Config{Image: "nginx:1", Labels: map[string]string{docker.LabelLeads: "{}"}},
			HostConfig: docker.HostConfig{Binds: binds, RestartPolicy: docker.RestartPolicy{Name: "always"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.client.StartContainer(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		return &user{id: id, name: name, stopWait: time.Second}
	}
	held, fine := running("held", conf+":/conf"), running("fine")
	if err := b.runner.stop(t.Context(), "bkp_a", []*user{held}, false); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.stop(t.Context(), "bkp_b", []*user{fine}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(conf); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", conf); err != nil {
		t.Fatal(err)
	}

	again := &Runner{Client: b.client, Root: b.runner.Root, Paths: paths}
	if err := again.Recover(t.Context()); err != nil {
		t.Fatalf("recovering = %v", err)
	}
	if !b.running(fine.id) {
		t.Error("what the other run stopped stayed down")
	}
	if b.running(held.id) {
		t.Error("what mounts a link made since was started")
	}
	if c, err := b.client.InspectContainer(t.Context(), held.id); err != nil || c.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("it can start by itself: %+v, %v", c, err)
	}
	if list, err := again.readStopped(); err != nil || len(list) != 0 {
		t.Errorf("what's left to start: %v, %v", list, err)
	}
}

// What restores replaced goes on time whatever Recover makes of what a run left: one
// that can't finish doesn't keep the disk full.
func TestPruningDoesntWaitOnRecovering(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "x")
	b.take(false)
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
		t.Fatal(err)
	}
	b.runner.Now = func() time.Time { return time.Now().Add(KeepFor + time.Hour) }
	b.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json") {
			return errors.New("the engine can't list containers")
		}
		return nil
	})
	defer b.engine.Intercept(nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go b.runner.Keep(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for slices.Contains(b.files(), "before-chg_1.tar.gz") {
		if time.Now().After(deadline) {
			t.Fatal("what a restore replaced is still there, as recovering never finished")
		}
		time.Sleep(10 * time.Millisecond)
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
	for _, h := range b.engine.Helpers() {
		if slices.Contains(h.Cmd, "pack") {
			t.Error("a dump went through a helper")
		}
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
	if err == nil || !strings.Contains(err.Error(), "relation") || !strings.Contains(err.Error(), "What was there is back") {
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
	dec, _, err := b.runner.JudgeRestore(t.Context(), ref, source)
	if err != nil {
		t.Fatal(err)
	}

	// The agent dies as it's about to unpack: it stopped the app and copied what's
	// there, through the real path, and its last helper never comes.
	var helpers atomic.Int32
	dying, gone, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		close(gone)
		<-ended
	})
	b.engine.Intercept(func(r *http.Request) error {
		if !strings.Contains(r.URL.Path, "/images/create") {
			return nil
		}
		if helpers.Add(1) == 3 { // to measure, to copy, and to unpack
			close(dying)
			<-gone
			return errors.New("the agent died")
		}
		return nil
	})
	go func() {
		defer close(ended)
		_ = b.runner.ApplyRestore(context.Background(), resource.Step{Ref: ref, Decision: dec}, source)
	}()
	<-dying
	b.engine.Intercept(nil)

	// The agent comes back, knowing nothing but the disk. Its restore is its change's
	// to finish.
	back := &Runner{Client: b.client, Root: b.runner.Root, Binary: b.runner.Binary}
	if err := back.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.running(id) {
		t.Fatal("the agent started an app whose restore isn't finished")
	}
	st, err := back.stageOf(ref, "chg_1")
	if err != nil || st.Stage != stageCopied {
		t.Fatalf("stage = %+v, %v", st, err)
	}
	copied, err := os.ReadFile(back.beforeFile(ref, "chg_1"))
	if err != nil {
		t.Fatal(err)
	}

	dec, left, err := back.JudgeRestore(t.Context(), ref, source)
	if err != nil || !left {
		t.Fatalf("JudgeRestore = %v, %v", left, err)
	}
	if err := back.ApplyRestore(t.Context(), resource.Step{Ref: ref, Decision: dec}, source); err != nil {
		t.Fatal(err)
	}
	if got := b.read("kept.txt"); got != "from the backup" {
		t.Errorf("kept.txt = %q", got)
	}
	if !b.running(id) {
		t.Error("the app didn't start once it finished")
	}
	if now, err := os.ReadFile(back.beforeFile(ref, "chg_1")); err != nil || !bytes.Equal(now, copied) {
		t.Errorf("it copied over the copy it took: %v", err)
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

// What a backup will cost is said first, and it never takes the last of the disk.
func TestABackupSaysWhatItCostsAndLeavesRoom(t *testing.T) {
	b := newBench(t)
	b.write("data.bin", strings.Repeat("x", 1<<20))
	b.runner.Disk = func() disk { return disk{free: 8 << 30, reserve: 1 << 30} }
	p, err := b.runner.Preview(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if p.Estimate != 1<<20 || p.Free != 8<<30 || p.Reserve != 1<<30 {
		t.Errorf("before the first, preview = %+v", p)
	}
	m := b.take(true)
	p, err = b.runner.Preview(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if p.Estimate != m.Size {
		t.Errorf("after the first, the estimate is %d, want the last one's %d", p.Estimate, m.Size)
	}
	b.runner.Disk = func() disk { return disk{free: 1 << 30, reserve: 1 << 30} }
	id := b.run("web", "nginx:1", "/data")
	starts := b.engine.Started(docker.ContainerName(b.target.Project, "web"))
	_, err = b.runner.Take(t.Context(), Take{ID: "bkp_2", Ref: ref, Stop: true})
	if err == nil || !strings.Contains(err.Error(), "free") || !strings.Contains(err.Error(), "keep fewer") {
		t.Fatalf("Take with no room = %v", err)
	}
	if !b.running(id) || b.engine.Started(docker.ContainerName(b.target.Project, "web")) != starts {
		t.Error("a backup with no room stopped the app")
	}
}

// destination is a fake bucket, and what reaches it.
func (b *bench) destination(t *testing.T) (*Destination, *s3test.Server) {
	t.Helper()
	store := s3test.New(t)
	b.runner.HTTP = store.Client()
	return &Destination{Endpoint: store.URL(), Bucket: "acme", Prefix: "dockwell", AccessKey: "key", SecretKey: "a-secret", Passphrase: "a long passphrase"}, store
}

// sent takes a backup and sends it.
func (b *bench) sent(t *testing.T, id string, d *Destination) (*Manifest, *Copied) {
	t.Helper()
	m, err := b.runner.Take(t.Context(), Take{ID: id, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.runner.Send(t.Context(), ref, id, d)
	if err != nil {
		t.Fatal(err)
	}
	return m, c
}

// gone is the server losing a backup.
func (b *bench) gone(t *testing.T, id string) {
	t.Helper()
	if _, err := b.runner.Delete(t.Context(), ref, id, "", nil); err != nil {
		t.Fatal(err)
	}
}

// A backup leaves sealed, and comes back whole on a server that's lost its copy.
func TestABackupGoesToItsDestinationAndComesBack(t *testing.T) {
	b := newBench(t)
	b.runner.Work = 0 // as an agent has it
	b.write("orders.db", strings.Repeat("the orders ", 1<<18))
	d, store := b.destination(t)
	m, copied := b.sent(t, "bkp_1", d)
	wantKey := "dockwell/" + env + "/data/bkp_1.tar.gz.sealed"
	if copied.Key != wantKey {
		t.Errorf("key = %q, want %q", copied.Key, wantKey)
	}
	objects := store.Objects()
	sealed := objects["/acme/"+wantKey]
	if int64(len(sealed)) != copied.Bytes {
		t.Errorf("stored %d bytes, says %d", len(sealed), copied.Bytes)
	}
	for _, path := range []string{"/acme/" + wantKey, "/acme/" + wantKey + ".json"} {
		if !bytes.Contains(objects[path][:32], binary.BigEndian.AppendUint32(nil, seal.Iterations)) {
			t.Errorf("%s isn't sealed at %d iterations", path, seal.Iterations)
		}
	}
	local, err := os.ReadFile(filepath.Join(b.runner.dir(ref), "bkp_1.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, local[:64]) {
		t.Error("the bucket holds the backup as it is")
	}
	for path, object := range objects {
		if bytes.Contains(object, []byte("a-secret")) || bytes.Contains(object, []byte("a long passphrase")) || strings.Contains(path, "secret") {
			t.Errorf("%s holds what signs in", path)
		}
		for _, said := range []string{m.SHA256, m.Volume + `"`, `"volume"`} {
			if bytes.Contains(object, []byte(said)) {
				t.Errorf("%s says %q to anyone who can read the bucket", path, said)
			}
		}
	}

	// The server loses it, and someone asks for it back.
	b.gone(t, "bkp_1")
	got, err := b.runner.Fetch(t.Context(), ref, "bkp_1", copied.Key, m.SHA256, d)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != m.SHA256 || got.Size != m.Size {
		t.Errorf("fetched %+v, want %+v", got, m)
	}
	b.write("orders.db", "lost")
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
		t.Fatalf("restoring what came back: %v", err)
	}
	if got := b.read("orders.db"); !strings.HasPrefix(got, "the orders ") {
		t.Errorf("orders.db = %q", got[:min(len(got), 20)])
	}

	// Deleted from both.
	if copyError, err := b.runner.Delete(t.Context(), ref, "bkp_1", copied.Key, d); err != nil || copyError != "" {
		t.Fatal(err, copyError)
	}
	if got := store.Objects(); len(got) != 0 {
		t.Errorf("left in the bucket: %d objects", len(got))
	}
}

// Storage that says no fails the send, not the backup, which is whole on the server.
func TestABackupStaysWhenItsCopyFails(t *testing.T) {
	b := newBench(t)
	b.write("orders.db", "the orders")
	d, store := b.destination(t)
	store.Refuse = "AccessDenied"
	m := b.take(false)
	_, err := b.runner.Send(t.Context(), ref, m.ID, d)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || !strings.Contains(err.Error(), "Check the destination's keys and bucket") {
		t.Errorf("Send = %v", err)
	}
	if err != nil && (strings.Contains(err.Error(), "a-secret") || strings.Contains(err.Error(), "a long passphrase")) {
		t.Errorf("the error holds what signs in: %q", err)
	}
	if got := b.files(); !slices.Contains(got, "bkp_1.tar.gz") {
		t.Errorf("on disk: %v", got)
	}
}

// Storage nobody can reach is said in the shape everything else is, with what to do.
func TestStorageNobodyCanReachSaysWhatToCheck(t *testing.T) {
	b := newBench(t)
	b.write("orders.db", "the orders")
	d, _ := b.destination(t)
	d.Endpoint = "http://127.0.0.1:1"
	b.runner.HTTP = nil
	m := b.take(false)
	_, err := b.runner.Send(t.Context(), ref, m.ID, d)
	if err == nil || !strings.HasSuffix(err.Error(), "Check the destination's endpoint, and that this server can reach it") {
		t.Errorf("Send = %v", err)
	}
}

// What comes back has to be what went: the wrong passphrase, or a bucket's copy
// changed, leaves nothing on the disk.
func TestAFetchThatDoesntMatchLeavesNothing(t *testing.T) {
	b := newBench(t)
	b.write("orders.db", "the orders")
	d, _ := b.destination(t)
	m, copied := b.sent(t, "bkp_1", d)
	b.gone(t, "bkp_1")
	wrong := *d
	wrong.Passphrase = "another long passphrase"
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_1", copied.Key, m.SHA256, &wrong); err == nil || !strings.Contains(err.Error(), "passphrase is wrong") {
		t.Errorf("Fetch with the wrong passphrase = %v", err)
	}
	if got := b.files(); len(got) != 0 {
		t.Errorf("it left %v", got)
	}
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_other", copied.Key, m.SHA256, d); err == nil {
		t.Error("it took one backup for another")
	}
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_1", copied.Key, strings.Repeat("0", 64), d); err == nil || !strings.Contains(err.Error(), "isn't the backup") {
		t.Errorf("Fetch of something the record doesn't name = %v", err)
	}
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_1", copied.Key, "", d); err == nil {
		t.Error("a fetch with nothing to check against went ahead")
	}
	if got := b.files(); len(got) != 0 {
		t.Errorf("it left %v", got)
	}
}

// Whoever has the passphrase can seal anything under a backup's name, and say what they
// like beside it. What comes back still has to hash to what's on record.
func TestAFetchComesDownToTheHashOnRecord(t *testing.T) {
	b := newBench(t)
	b.write("orders.db", "the orders")
	d, store := b.destination(t)
	m, copied := b.sent(t, "bkp_1", d)
	b.gone(t, "bkp_1")
	other, err := seal.Small([]byte("something else"), d.Passphrase, binding(ref, "bkp_1"), b.runner.Work)
	if err != nil {
		t.Fatal(err)
	}
	store.Set("/acme/"+copied.Key, other)
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_1", copied.Key, m.SHA256, d); err == nil || !strings.Contains(err.Error(), "what came back hashes to") {
		t.Errorf("Fetch of something else with the right manifest = %v", err)
	}
	if got := b.files(); len(got) != 0 {
		t.Errorf("it left %v", got)
	}
}

// A fetch can't touch a backup the server has, whatever storage holds: what's on the
// disk is what the record says, and that's the copy to keep.
func TestAFetchLeavesTheServersOwnCopyAlone(t *testing.T) {
	b := newBench(t)
	b.write("orders.db", "the orders")
	d, _ := b.destination(t)
	m, copied := b.sent(t, "bkp_1", d)
	path := filepath.Join(b.runner.dir(ref), "bkp_1.tar.gz")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_1", copied.Key, m.SHA256, d); err == nil || !strings.Contains(err.Error(), "still has") {
		t.Errorf("Fetch of a backup this server has = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the fetch changed the server's own copy")
	}

	// What's beside it says nothing of what's left: a server that lost the file can fetch.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_1", copied.Key, m.SHA256, d); err != nil {
		t.Errorf("Fetch of a backup the server lost = %v", err)
	}
}

// Someone who can write to the bucket can put an older backup under a newer name, and
// it has the right passphrase. It doesn't open as what it isn't.
func TestAnOlderObjectUnderANewerNameWontOpen(t *testing.T) {
	b := newBench(t)
	b.write("orders.db", "the old orders")
	d, store := b.destination(t)
	_, older := b.sent(t, "bkp_1", d)
	b.write("orders.db", "the new orders")
	m2, newer := b.sent(t, "bkp_2", d)
	objects := store.Objects()
	store.Set("/acme/"+newer.Key, objects["/acme/"+older.Key])
	b.gone(t, "bkp_2")
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_2", newer.Key, m2.SHA256, d); err == nil {
		t.Error("the older backup opened as the newer one")
	}
	if got := b.files(); slices.Contains(got, "bkp_2.tar.gz") {
		t.Errorf("it left %v", got)
	}
	store.Set("/acme/"+newer.Key, objects["/acme/"+newer.Key])
	store.Set("/acme/"+newer.Key+".json", objects["/acme/"+older.Key+".json"])
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_2", newer.Key, m2.SHA256, d); err == nil {
		t.Error("the older manifest opened as the newer one")
	}
	store.Set("/acme/"+newer.Key+".json", objects["/acme/"+newer.Key])
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_2", newer.Key, m2.SHA256, d); err == nil {
		t.Error("the backup opened as its own manifest")
	}
}

// A fetch waits its turn on the volume, as everything that writes to it does.
func TestAFetchTakesTheVolumesLock(t *testing.T) {
	b := newBench(t)
	b.write("orders.db", "the orders")
	d, _ := b.destination(t)
	m, copied := b.sent(t, "bkp_1", d)
	b.gone(t, "bkp_1")
	release, err := b.runner.hold(volume.TargetOf(ref))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := b.runner.Fetch(t.Context(), ref, "bkp_1", copied.Key, m.SHA256, d); err == nil {
		t.Error("a fetch went ahead with the volume held")
	}
}

// A bucket that won't give up a copy doesn't keep the server's: that's what fills a
// disk. What it said goes back to be put on Activity.
func TestTheServersCopyGoesWhenTheBucketRefuses(t *testing.T) {
	b := newBench(t)
	b.write("orders.db", "the orders")
	d, store := b.destination(t)
	_, copied := b.sent(t, "bkp_1", d)
	store.Refuse = "AccessDenied"
	copyError, err := b.runner.Delete(t.Context(), ref, "bkp_1", copied.Key, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(copyError, "AccessDenied") {
		t.Errorf("what the bucket said = %q", copyError)
	}
	if got := b.files(); len(got) != 0 {
		t.Errorf("on disk: %v", got)
	}
}

func TestDeleteRemovesABackup(t *testing.T) {
	b := newBench(t)
	b.write("kept.txt", "x")
	b.take(false)
	b.gone(t, "bkp_1")
	if got := b.files(); len(got) != 0 {
		t.Errorf("on disk: %v", got)
	}
	b.gone(t, "bkp_1")
	if _, err := b.runner.Delete(t.Context(), ref, "../../other", "", nil); err == nil {
		t.Error("it took a path for an ID")
	}
}

// A backup that's a step of a change goes by the last one's size, which sets how long it
// gets. With none, and nothing to measure with, it says that's a guess.
func TestABackupStepSaysWhatItsGoingBy(t *testing.T) {
	b := newBench(t)
	b.write("orders.db", strings.Repeat("the orders ", 1<<16))
	b.engine.Intercept(func(r *http.Request) error {
		if strings.Contains(r.URL.Path, "/images/create") {
			return errors.New("no room for the helper")
		}
		return nil
	})
	dec, _, err := b.runner.JudgeBackup(t.Context(), ref, resource.BackupAsked{ID: "bkp_2"})
	if err != nil {
		t.Fatal(err)
	}
	if !dec.BackupFirst.Unmeasured || dec.BackupFirst.Estimate != 0 {
		t.Errorf("with nothing to go by = %+v", dec.BackupFirst)
	}
	b.engine.Intercept(nil)
	dec, _, err = b.runner.JudgeBackup(t.Context(), ref, resource.BackupAsked{ID: "bkp_2"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.BackupFirst.Unmeasured || dec.BackupFirst.Estimate == 0 {
		t.Errorf("measured = %+v", dec.BackupFirst)
	}

	m := b.take(false)
	b.engine.Intercept(func(r *http.Request) error {
		if strings.Contains(r.URL.Path, "/images/create") {
			return errors.New("no room for the helper")
		}
		return nil
	})
	dec, _, err = b.runner.JudgeBackup(t.Context(), ref, resource.BackupAsked{ID: "bkp_2"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.BackupFirst.Unmeasured || dec.BackupFirst.Estimate != m.Size {
		t.Errorf("after a backup of %d bytes = %+v", m.Size, dec.BackupFirst)
	}
}

// One database alone is a dump of it, as its owner, never the server's administrator
// (CVE-2024-7348), from its server running now, nothing stopping; a name that isn't
// one, a database the server keeps for itself, and a server that isn't running are
// refused (ADR 0084).
func TestOneDatabaseIsDumpedAsItsOwner(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE orders;")
	id := b.run("db", "postgres:17", "/var/lib/postgresql/data")
	m, err := b.runner.Take(t.Context(), Take{ID: "bkp_1", Ref: ref, Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if m.OneDatabase != "shop" || m.Method != resource.BackupDump || m.Format != formatDump || m.Database != "PostgreSQL" {
		t.Errorf("manifest = %+v", m)
	}
	if len(b.dumpedOne) != 1 || !slices.Equal(b.dumpedOne[0], []string{"shop", "shop"}) {
		t.Errorf("the dump ran with %v", b.dumpedOne)
	}
	if !b.running(id) {
		t.Error("a dump of one database stopped the server")
	}
	for i, name := range []string{"shop; DROP DATABASE x", "postgres"} {
		if _, err := b.runner.Take(t.Context(), Take{ID: "bkp_" + strconv.Itoa(i+2), Ref: ref, Database: name}); err == nil {
			t.Errorf("dumping %q alone: no error", name)
		}
	}
	if err := b.client.StopContainer(t.Context(), id, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := b.runner.Take(t.Context(), Take{ID: "bkp_9", Ref: ref, Database: "shop"}); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Errorf("dumping one database of a stopped server: %v", err)
	}
}

// oneLoad is a load into one database: who signed in, as the arguments after its
// script say, their environment, and what it was given.
type oneLoad struct {
	args, env []string
	sql       string
}

// One database alone restores without stopping anything: its user is kept out while
// it loads, what's there goes to a dump the server keeps, and the backup loads as a
// role made for it that sets the owner's role, through the copy's path, never as the
// administrator; then the user signs in again. Undoing it loads that dump back. One
// that fails to load puts back what was there and lets the user in; one whose put-back
// fails too keeps the user out (ADR 0084).
func TestOneDatabaseRestoresAlone(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE orders; -- as backed up")
	id := b.run("db", "postgres:17", "/var/lib/postgresql/data")
	if _, err := b.runner.Take(t.Context(), Take{ID: "bkp_1", Ref: ref, Database: "shop"}); err != nil {
		t.Fatal(err)
	}
	b.write("data.sql", "CREATE TABLE orders; -- written since")
	starts := b.engine.Started(docker.ContainerName(b.target.Project, "db"))
	dec, _, err := b.runner.JudgeRestore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err != nil {
		t.Fatal(err)
	}
	if r := dec.Restore; r == nil || r.OneDatabase != "shop" || len(r.Stops) != 0 || !dec.Destroys {
		t.Errorf("the plan says %+v", dec)
	}
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
		t.Fatal(err)
	}
	if got := b.read("data.sql"); !strings.Contains(got, "as backed up") {
		t.Errorf("shop holds %q", got)
	}
	if !b.running(id) || b.engine.Started(docker.ContainerName(b.target.Project, "db")) != starts {
		t.Error("restoring one database stopped its server")
	}
	// Kept out, dumped as the role that loads, loaded as it with the owner's role, let in.
	admin := strings.Join(b.admin, "")
	out, in := strings.Index(admin, `ALTER ROLE "shop" NOLOGIN`), strings.LastIndex(admin, `ALTER ROLE "shop" LOGIN;`)
	if out < 0 || in < out || !strings.Contains(admin, `DROP ROLE IF EXISTS "dw_restore_`) {
		t.Errorf("its administrator ran %q", admin)
	}
	if n := len(b.dumpedOne); n != 2 || len(b.dumpedOne[1]) != 3 || !strings.HasPrefix(b.dumpedOne[1][1], "dw_restore_") || b.dumpedOne[1][2] != "shop" {
		t.Errorf("the dumps ran as %v", b.dumpedOne)
	}
	if len(b.loads) != 1 {
		t.Fatalf("loads %+v", b.loads)
	}
	load := b.loads[0]
	if !strings.HasPrefix(load.args[0], "dw_restore_") || load.args[1] != "shop" || load.args[3] == "127.0.0.1" ||
		!strings.HasPrefix(load.sql, `\restrict `) || !strings.Contains(load.sql, "\nSET ROLE \"shop\";\n") {
		t.Errorf("the load ran as %v with %q", load.args, load.sql)
	}
	if slices.ContainsFunc(load.env, func(kv string) bool { return strings.HasPrefix(kv, "POSTGRES_PASSWORD=") }) {
		t.Errorf("the load had the server's environment: %v", load.env)
	}

	// Undoing it loads back what was there.
	if err := b.restore(resource.RestoreSource{Undoes: "chg_1", Change: "chg_2"}); err != nil {
		t.Fatal(err)
	}
	if got := b.read("data.sql"); !strings.Contains(got, "written since") {
		t.Errorf("after the undo shop holds %q", got)
	}

	// It loads as one transaction, after what the owner has there goes, so a load
	// that fails partway changes nothing: the user signs in again to what was there,
	// and nothing is put back, which could fail too.
	if !strings.Contains(load.sql, "SET ROLE \"shop\";\nDO $dw_empty$") || !strings.Contains(load.sql, "PERFORM lo_unlink") {
		t.Errorf("the load doesn't empty the database first: %q", load.sql)
	}
	b.admin, b.loads, b.loadsFail = nil, nil, 1
	err = b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_3"})
	if err == nil || !strings.Contains(err.Error(), "didn't load into shop") || !strings.Contains(err.Error(), "What was there is still in shop, and its user signs in again") {
		t.Errorf("a load that fails: %v", err)
	}
	if got := b.read("data.sql"); !strings.Contains(got, "written since") {
		t.Errorf("after a load that failed shop holds %q", got)
	}
	if len(b.loads) != 1 || b.ownerOut || len(b.loaders) != 0 {
		t.Errorf("after a load that failed: %d loads, kept out %t, roles that load %v", len(b.loads), b.ownerOut, b.loaders)
	}

	// Its user wrote on after that: the same restore, tried again, dumps what's there
	// again, so undoing it keeps what was written in between.
	b.write("data.sql", "CREATE TABLE orders; -- written after the failure")
	dumps := len(b.dumpedOne)
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_3"}); err != nil {
		t.Fatal(err)
	}
	if len(b.dumpedOne) != dumps+1 {
		t.Errorf("tried again, it dumped %d times, want once more", len(b.dumpedOne)-dumps)
	}
	if err := b.restore(resource.RestoreSource{Undoes: "chg_3", Change: "chg_3u"}); err != nil {
		t.Fatal(err)
	}
	if got := b.read("data.sql"); !strings.Contains(got, "written after the failure") {
		t.Errorf("undoing it brought back %q", got)
	}
}

// A restore of one database never leaves its app out with nothing to let it back in
// (ADR 0084): a database holding something its owner doesn't own is refused before
// anyone is kept out; a lockout that fails partway lets the user back in; an undo of
// a restore that couldn't let its user in takes up that restore's lockout, from its
// record or, with that gone, from the role it left; and a role that loaded and
// couldn't go is taken up by the next restore. What a restore kept goes after 7 days.
func TestARestoreOfOneDatabaseNeverLeavesItsAppOut(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE orders; -- as backed up")
	b.run("db", "postgres:17", "/var/lib/postgresql/data")
	if _, err := b.runner.Take(t.Context(), Take{ID: "bkp_1", Ref: ref, Database: "shop"}); err != nil {
		t.Fatal(err)
	}
	b.write("data.sql", "CREATE TABLE orders; -- written since")
	record := filepath.Join(b.runner.dir(ref), "lockout-shop.json")
	settled := func(what string) {
		t.Helper()
		if b.ownerOut || len(b.loaders) != 0 {
			t.Errorf("%s: shop kept out %t, roles that load %v", what, b.ownerOut, b.loaders)
		}
		if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the record of keeping shop out stays: %v", what, err)
		}
	}

	// Something of an administrator's in it: refused as it's planned, and as it's
	// applied, before anyone is kept out.
	b.notOwned = "the extension dblink\n"
	_, _, err := b.runner.JudgeRestore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err == nil || !strings.Contains(err.Error(), "shop holds the extension dblink, which isn't shop's") {
		t.Errorf("planning a restore of a database holding an administrator's extension = %v", err)
	}
	b.admin = nil
	err = b.runner.ApplyRestore(t.Context(), resource.Step{Ref: ref}, resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") || strings.Contains(strings.Join(b.admin, ""), "NOLOGIN") || len(b.loads) != 0 {
		t.Errorf("applying it = %v, its administrator ran %q", err, b.admin)
	}
	settled("refused")
	b.notOwned = ""

	// Keeping the user out fails after it's out: it's let back in.
	b.adminFails, b.failsOn = 1, "pg_terminate_backend"
	err = b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_2"})
	if err == nil || !strings.Contains(err.Error(), "can't keep shop's user out, so nothing was loaded") {
		t.Errorf("a lockout that fails partway = %v", err)
	}
	settled("a lockout that failed")

	// Letting it in fails, so it's left out, and says to try again; the undo the
	// change offers takes up its lockout, and lets it in as it was.
	b.adminFails, b.failsOn = 1, `ALTER ROLE "shop" LOGIN;`
	err = b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_3"})
	if err == nil || !strings.Contains(err.Error(), "Restore this backup again, or undo this change") || !b.ownerOut {
		t.Fatalf("a restore that can't let its user in = %v, kept out %t", err, b.ownerOut)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("a restore that left its user out keeps no record of it: %v", err)
	}
	made := len(b.loaders)
	if err := b.restore(resource.RestoreSource{Undoes: "chg_3", Change: "chg_4"}); err != nil {
		t.Fatal(err)
	}
	settled("the undo")
	if got := b.read("data.sql"); !strings.Contains(got, "written since") {
		t.Errorf("after the undo shop holds %q", got)
	}
	if n := strings.Count(strings.Join(b.admin, ""), "SELECT 'CREATE ROLE"); made != 1 || n == 0 {
		t.Errorf("roles made to load: %d, then %d asks to make one", made, n)
	}

	// With its record gone, the role it left says it's out, and is taken up.
	b.adminFails, b.failsOn = 1, `ALTER ROLE "shop" LOGIN;`
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_5"}); err == nil || !b.ownerOut {
		t.Fatalf("a restore that can't let its user in = %v", err)
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := b.restore(resource.RestoreSource{Undoes: "chg_5", Change: "chg_6"}); err != nil {
		t.Fatal(err)
	}
	settled("the undo with no record")

	// A role that loaded and couldn't go is only a warning: the user is in, and the
	// next restore takes the role up, and removes it.
	b.adminFails, b.failsOn = 1, "DROP ROLE IF EXISTS"
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_7"}); err != nil {
		t.Fatalf("a restore whose role that loaded stays = %v", err)
	}
	var left string
	for name := range b.loaders {
		left = name
	}
	if b.ownerOut || left == "" {
		t.Fatalf("after it, kept out %t, roles that load %v", b.ownerOut, b.loaders)
	}
	b.admin = nil
	if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_8"}); err != nil {
		t.Fatal(err)
	}
	settled("the next restore")
	if admin := strings.Join(b.admin, ""); !strings.Contains(admin, `DROP ROLE IF EXISTS "`+left+`"`) {
		t.Errorf("the next restore didn't take up %s: %q", left, admin)
	}

	// What each restore kept of shop goes after 7 days.
	b.runner.Now = func() time.Time { return time.Now().Add(KeepFor + 2*time.Hour) }
	if err := b.runner.Prune(); err != nil {
		t.Fatal(err)
	}
	if left := b.files(); slices.ContainsFunc(left, func(f string) bool { return strings.HasPrefix(f, beforePrefix) }) {
		t.Errorf("after 7 days the server keeps %v", left)
	}
}

// A step that dumps several apps' databases on one server alone takes each under its
// own ID, says each as it's taken, and is done only once all of them are (ADR 0084).
func TestAStepDumpsEachDatabaseAlone(t *testing.T) {
	b := newBench(t)
	b.write("data.sql", "CREATE TABLE orders;")
	b.run("db", "postgres:17", "/var/lib/postgresql/data")
	asked := resource.BackupAsked{ID: "bkp_a", Database: "shop", Also: []resource.DatabaseDump{{ID: "bkp_b", Database: "blog"}}}
	dec, left, err := b.runner.JudgeBackup(t.Context(), ref, asked)
	if err != nil || !left || dec.BackupFirst == nil || dec.BackupFirst.One != "shop" || len(dec.BackupFirst.Also) != 1 {
		t.Fatalf("judged %+v, %t, %v", dec, left, err)
	}
	var told []string
	ones := map[string]string{}
	ctx := resource.WithOutcome(t.Context(), func(o resource.Outcome) {
		if o.Backup != nil {
			told = append(told, o.Backup.ID)
			ones[o.Backup.ID] = o.Backup.One
		}
	})
	if err := b.runner.ApplyBackup(ctx, resource.Step{Ref: ref, Decision: dec}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(told, []string{"bkp_a", "bkp_b"}) || len(b.dumpedOne) != 2 || b.dumpedOne[0][0] != "shop" || b.dumpedOne[1][0] != "blog" {
		t.Errorf("it said %v, and dumped %v", told, b.dumpedOne)
	}
	// Each says the database it dumped alone, which is what the control plane records.
	if ones["bkp_a"] != "shop" || ones["bkp_b"] != "blog" {
		t.Errorf("it said it dumped %v", ones)
	}
	if _, left, err := b.runner.JudgeBackup(t.Context(), ref, asked); err != nil || left {
		t.Errorf("both taken, it judged %t, %v", left, err)
	}
	asked.Also = append(asked.Also, resource.DatabaseDump{ID: "bkp_c", Database: "tools"})
	dec, left, err = b.runner.JudgeBackup(t.Context(), ref, asked)
	if err != nil || !left {
		t.Fatalf("one still to take, it judged %t, %v", left, err)
	}

	// Taken again, as a step that resumes: what was taken isn't taken again, and each
	// is said, as the database it is. One that fails stops the step there.
	told = nil
	b.dumpedOne = nil
	if err := b.runner.ApplyBackup(ctx, resource.Step{Ref: ref, Decision: dec}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(told, []string{"bkp_a", "bkp_b", "bkp_c"}) || len(b.dumpedOne) != 1 || b.dumpedOne[0][0] != "tools" {
		t.Errorf("resumed, it said %v, and dumped %v", told, b.dumpedOne)
	}
	asked.Also = append(asked.Also, resource.DatabaseDump{ID: "bkp_d", Database: "logs"}, resource.DatabaseDump{ID: "bkp_e", Database: "mail"})
	dec, _, _ = b.runner.JudgeBackup(t.Context(), ref, asked)
	told, b.dumpOneFails = nil, "logs"
	if err := b.runner.ApplyBackup(ctx, resource.Step{Ref: ref, Decision: dec}); err == nil || !strings.Contains(err.Error(), "connection to server failed") {
		t.Errorf("a dump that fails = %v", err)
	}
	if slices.Contains(told, "bkp_d") || slices.Contains(told, "bkp_e") {
		t.Errorf("after a dump that failed, it said %v", told)
	}
}
