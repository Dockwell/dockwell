// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// clusterDump is what pg_dumpall writes, as the bench's database holds it: a database
// with a table it fills and one it leaves empty.
const clusterDump = `--
-- PostgreSQL database cluster dump
--
CREATE ROLE postgres;
\connect template1
CREATE DATABASE orders WITH TEMPLATE = template0 ENCODING = 'UTF8';
\connect -reuse-previous=on "dbname='orders'"
CREATE TABLE public.items (
    id integer NOT NULL
);
CREATE TABLE "Sales"."Big Table" (
    id integer
);
CREATE TABLE public.empty (
    id integer
);
COPY public.items (id) FROM stdin;
1
2
\.
COPY "Sales"."Big Table" (id) FROM stdin;
7
\.
COPY public.empty (id) FROM stdin;
\.
`

// withoutRows is a dump with every COPY's rows gone, as a load that lost them leaves.
func withoutRows(data []byte) []byte {
	var out bytes.Buffer
	copying := false
	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case copying && line == `\.`:
			copying = false
		case copying:
			continue
		case strings.HasPrefix(line, "COPY ") && strings.HasSuffix(line, "FROM stdin;"):
			copying = true
		}
		out.WriteString(line + "\n")
	}
	return out.Bytes()
}

var emptyPart = regexp.MustCompile(`SELECT (\d+) WHERE NOT EXISTS \(SELECT 1 FROM "((?:[^"]|"")*)"\."((?:[^"]|"")*)" LIMIT 1\)`)

// query answers a drill's question the way PostgreSQL would, from what was loaded.
func (b *bench) query(x dockertest.Exec, file string) int {
	arg := func(flag string) string {
		for i, a := range x.Cmd {
			if a == flag && i+1 < len(x.Cmd) {
				return x.Cmd[i+1]
			}
		}
		return ""
	}
	database, q := arg("-d"), arg("-c")
	data, err := os.ReadFile(file)
	if err != nil {
		return 2
	}
	held := newHeldDump(false)
	_, _ = held.Write(data)
	_ = held.Close()
	out := x.Stdout
	switch {
	case q == "SELECT 1":
		_, _ = io.WriteString(out, "1\n")
	case q == "SELECT datname FROM pg_database":
		_, _ = io.WriteString(out, "postgres\ntemplate0\n")
		for _, d := range held.databases {
			_, _ = io.WriteString(out, d+"\n")
		}
	case q == "SELECT schemaname, tablename FROM pg_tables":
		for _, t := range held.tables {
			if t.database == database {
				_, _ = io.WriteString(out, t.schema+"\t"+t.name+"\n")
			}
		}
	case strings.Contains(q, "WHERE NOT EXISTS"):
		for _, m := range emptyPart.FindAllStringSubmatch(q, -1) {
			schema, name := strings.ReplaceAll(m[2], `""`, `"`), strings.ReplaceAll(m[3], `""`, `"`)
			t := held.byName[database+"\x00"+schema+"\x00"+name]
			if t == nil || !t.filled {
				_, _ = io.WriteString(out, m[1]+"\n")
			}
		}
	default:
		_, _ = io.WriteString(x.Stderr, "ERROR:  the bench doesn't know "+q+"\n")
		return 1
	}
	return 0
}

// drillBench is a bench with a database whose dump has been taken, room on a disk of
// its own, and memory to spare.
func drillBench(t *testing.T) (*bench, string) {
	t.Helper()
	b := newBench(t)
	b.runner.DrillDisk = func() drillDisk { return drillDisk{free: 100 << 30, size: 200 << 30, filesystem: "docker"} }
	b.runner.DrillMemory = func() (int64, bool) { return 8 << 30, true }
	b.run("db", "postgres:17", "/var/lib/postgresql/data")
	b.write("data.sql", clusterDump)
	m, err := b.runner.Take(t.Context(), Take{ID: "bak_01dump", Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	return b, m.ID
}

// nothingLeft says a drill left no container and no volume of its own.
func (b *bench) nothingLeft() {
	b.t.Helper()
	ctx := b.t.Context()
	cs, err := b.client.ListContainers(ctx, map[string][]string{"label": {labelDrill}})
	if err != nil {
		b.t.Fatal(err)
	}
	vs, err := b.client.ListVolumes(ctx, labelDrill)
	if err != nil {
		b.t.Fatal(err)
	}
	if len(cs) > 0 || len(vs) > 0 {
		b.t.Errorf("a drill left %d containers and %d volumes", len(cs), len(vs))
	}
}

// A dump restores into a database of the drill's own, which holds every database and
// table the dump makes and a row in each it fills, and then goes, container and
// volume. The live database and its volume stay as they were.
func TestADumpRestoresInADrillOfItsOwn(t *testing.T) {
	b, backup := drillBench(t)
	got := b.runner.Drill(t.Context(), Drill{ID: "drl_01pass", Backup: backup, Ref: ref})
	if got.Outcome != DrillPassed || got.Tables != 3 || got.Restored == 0 {
		t.Fatalf("the drill = %+v, want passed with 3 tables", got)
	}
	b.nothingLeft()
	data, err := os.ReadFile(b.engine.VolumeDir(b.target.Name) + "/data.sql")
	if err != nil || string(data) != clusterDump {
		t.Errorf("the live volume holds %q, %v", data, err)
	}
}

// A dump that loads without its rows fails its drill by the table it lost, which is
// what a backup that doesn't restore looks like.
func TestADrillFailsATableThatLostItsRows(t *testing.T) {
	b, backup := drillBench(t)
	b.dropRows = true
	got := b.runner.Drill(t.Context(), Drill{ID: "drl_01rows", Backup: backup, Ref: ref})
	if got.Outcome != DrillFailed || got.Why != "orders.items has no rows, and the dump fills it" {
		t.Fatalf("the drill = %+v, want it failed on orders.items", got)
	}
	b.nothingLeft()
}

// A drill that can't run says why and checks nothing: no memory, no room, or another
// drill under way. None is a failure of the backup.
func TestADrillThatCantRunIsNotChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(*Runner)
		want string
	}{
		"memory": {func(r *Runner) { r.DrillMemory = func() (int64, bool) { return 512 << 20, true } }, "of memory free"},
		"room": {func(r *Runner) {
			r.DrillDisk = func() drillDisk { return drillDisk{free: 1 << 30, size: 20 << 30, filesystem: "docker"} }
		}, "the disk it restores to has"},
		"busy": {func(r *Runner) { r.drilling = &drillRun{id: "drl_01other", done: make(chan struct{})} }, "another restore check is running on this server"},
	} {
		t.Run(name, func(t *testing.T) {
			b, backup := drillBench(t)
			tc.set(b.runner)
			got := b.runner.Drill(t.Context(), Drill{ID: "drl_01" + name, Backup: backup, Ref: ref})
			if got.Outcome != DrillNotChecked || !strings.Contains(got.Why, tc.want) {
				t.Errorf("the drill = %+v, want not checked, saying %q", got, tc.want)
			}
			b.nothingLeft()
		})
	}
}

// A copy of files that aren't a database's unpacks whole into the drill's volume,
// which is what it proves: "Unpacked whole".
func TestACopyUnpacksWhole(t *testing.T) {
	b := newBench(t)
	b.runner.DrillDisk = func() drillDisk { return drillDisk{free: 100 << 30, size: 200 << 30, filesystem: "docker"} }
	b.runner.DrillMemory = func() (int64, bool) { return 8 << 30, true }
	b.run("web", "nginx:1", "/data")
	b.write("site/index.html", "hello")
	b.write("site/about.html", "about us")
	m, err := b.runner.Take(t.Context(), Take{ID: "bak_01copy", Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	got := b.runner.Drill(t.Context(), Drill{ID: "drl_01copy", Backup: m.ID, Ref: ref})
	if got.Outcome != DrillPassed || !got.UnpackedOnly || got.Restored < int64(len("hello")+len("about us")) {
		t.Fatalf("the drill = %+v, want passed, unpacked only", got)
	}
	b.nothingLeft()
}

// A backup this server no longer has isn't checked.
func TestADrillOfAMissingBackupIsNotChecked(t *testing.T) {
	b := newBench(t)
	got := b.runner.Drill(t.Context(), Drill{ID: "drl_01gone", Backup: "bak_01none", Ref: ref})
	if got.Outcome != DrillNotChecked || !strings.Contains(got.Why, "this server no longer has the backup") {
		t.Errorf("the drill = %+v", got)
	}
}

// A backup that needs the room a drill holds stops the drill, and goes on: backups
// win (ADR 0068).
func TestABackupStopsADrillInItsWay(t *testing.T) {
	b := newBench(t)
	b.runner.Disk = func() disk { return disk{free: 10 << 30, reserve: 1 << 30, filesystem: "shared"} }
	ctx, stop := context.WithCancelCause(context.Background())
	run := &drillRun{id: "drl_01big", filesystem: "shared", need: 9 << 30, stop: stop, done: make(chan struct{})}
	go func() { <-ctx.Done(); close(run.done) }()
	b.runner.drilling = run
	b.write("a.txt", "a little")
	if err := b.runner.roomFor(t.Context(), Take{ID: "bak_01x", Ref: ref}, b.target); err != nil {
		t.Fatalf("the backup was refused: %v", err)
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errBackupWanted) {
		t.Errorf("the drill went on, or stopped with %v", cause)
	}

	// One on another disk holds nothing up.
	ctx, stop = context.WithCancelCause(context.Background())
	b.runner.drilling = &drillRun{id: "drl_01far", filesystem: "elsewhere", need: 9 << 30, stop: stop, done: make(chan struct{})}
	if err := b.runner.roomFor(t.Context(), Take{ID: "bak_01y", Ref: ref}, b.target); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Error("a backup stopped a drill on another disk")
	}
}

// A drill's container is the image, not the service: no network, nothing published,
// no mount but its scratch volume where the database keeps its data, only the image's
// setup variables with the drill's own password, the kernel's first to kill, and a
// low weight against the live app (ADR 0068).
func TestADrillRunsTheImageNotTheService(t *testing.T) {
	kind := kindNamed("PostgreSQL")
	env, user := drillEnv(kind, []string{
		"POSTGRES_PASSWORD=the-live-one", "POSTGRES_USER=shop", "APP_SECRET=nobody's business",
		"POSTGRES_INITDB_ARGS=--auth-local=scram-sha-256 --data-checksums -A md5", "PGDATA=/var/lib/postgresql/data/pg",
	}, "drill-password")
	if user != "shop" || !slices.Equal(env, []string{
		"POSTGRES_USER=shop", "PGDATA=/var/lib/postgresql/data/pg", "POSTGRES_INITDB_ARGS=--data-checksums", "POSTGRES_PASSWORD=drill-password",
	}) {
		t.Errorf("the drill's environment is %q as %s", env, user)
	}
	p := &drillPlan{
		kind: kind, image: "sha256:abc", tag: "postgres:17.2", cmd: []string{"postgres"}, entrypoint: []string{"docker-entrypoint.sh"},
		env: env, data: "/var/lib/postgresql/data", memory: 2 << 30, scratch: "dockwell-drill-drl_01", label: "drl_01", cpus: 1,
	}
	for _, files := range []bool{false, true} {
		req := drillRequest(p, files)
		h := req.HostConfig
		if h.NetworkMode != "none" || len(h.PortBindings) > 0 || h.Privileged || len(h.Binds) > 0 || len(h.CapAdd) > 0 {
			t.Errorf("files %t: it reaches out: %+v", files, h)
		}
		if len(h.Mounts) != 1 || h.Mounts[0] != (docker.Mount{Type: "volume", Source: p.scratch, Target: p.data}) {
			t.Errorf("files %t: it mounts %+v", files, h.Mounts)
		}
		if h.OomScoreAdj != 1000 || h.CPUShares != 256 || h.Memory != 2<<30 || h.MemorySwap != h.Memory || h.NanoCPUs != 5e8 {
			t.Errorf("files %t: its bounds are %+v", files, h)
		}
		if req.Labels[labelDrill] != "drl_01" || !slices.Equal(req.Env, env) {
			t.Errorf("files %t: it carries %v and %v", files, req.Labels, req.Env)
		}
		cmd := strings.Join(append(append([]string{}, req.Entrypoint...), req.Cmd...), " ")
		if !strings.Contains(cmd, "postgres -c fsync=off") || strings.Contains(cmd, "config_file") {
			t.Errorf("files %t: it runs %q", files, cmd)
		}
		if files && !strings.Contains(cmd, "> "+drillHBA+" && exec \"$@\" drill docker-entrypoint.sh postgres") {
			t.Errorf("a drill of files runs %q, want the image's own command after its pg_hba.conf", cmd)
		}
	}
	mysql, _ := drillEnv(kindNamed("MySQL"), []string{"MYSQL_ROOT_PASSWORD=live", "MYSQL_DATABASE=shop", "MYSQL_USER=app", "MYSQL_PASSWORD=apps"}, "drill")
	if !slices.Equal(mysql, []string{"MYSQL_DATABASE=shop", "MYSQL_ROOT_PASSWORD=drill"}) {
		t.Errorf("MySQL's drill environment is %q", mysql)
	}
}

// What a drill leaves, by its label, goes when the agent sweeps, but for the drill
// under way.
func TestASweepRemovesWhatDrillsLeft(t *testing.T) {
	b := newBench(t)
	ctx := t.Context()
	for _, id := range []string{"drl_01old", "drl_01now"} {
		if _, err := b.client.CreateVolume(ctx, &docker.VolumeCreate{Name: "dockwell-drill-" + id, Labels: map[string]string{labelDrill: id}}); err != nil {
			t.Fatal(err)
		}
		if _, err := b.client.CreateContainer(ctx, "dockwell-drill-"+id+"-0", &docker.CreateRequest{
			Config: docker.Config{Image: "postgres:17", Labels: map[string]string{labelDrill: id}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	b.runner.sweepDrills(ctx, "drl_01now")
	vs, err := b.client.ListVolumes(ctx, labelDrill)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := b.client.ListContainers(ctx, map[string][]string{"label": {labelDrill}})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Name != "dockwell-drill-drl_01now" || len(cs) != 1 || cs[0].Labels[labelDrill] != "drl_01now" {
		t.Errorf("after a sweep: volumes %v, containers %v", vs, cs)
	}
}

// A dump says what it holds as it goes by: PostgreSQL's databases by \connect and
// CREATE DATABASE, tables by CREATE TABLE with their schemas, quoted or not, and a
// COPY with a row fills one; MySQL's by USE, CREATE DATABASE, CREATE TABLE and INSERT.
func TestADumpSaysWhatItHolds(t *testing.T) {
	h := newHeldDump(false)
	// In pieces, as a stream comes.
	for chunk := range slices.Chunk([]byte(clusterDump), 7) {
		_, _ = h.Write(chunk)
	}
	_ = h.Close()
	if !slices.Equal(h.databases, []string{"template1", "orders"}) {
		t.Errorf("databases %v", h.databases)
	}
	var got []string
	for _, t := range h.tables {
		got = append(got, t.database+"|"+t.schema+"|"+t.name+"|"+map[bool]string{true: "filled", false: "empty"}[t.filled])
	}
	if !slices.Equal(got, []string{"orders|public|items|filled", "orders|Sales|Big Table|filled", "orders|public|empty|empty"}) {
		t.Errorf("tables %v", got)
	}

	m := newHeldDump(true)
	_, _ = m.Write([]byte("-- MySQL dump\n" +
		"CREATE DATABASE /*!32312 IF NOT EXISTS*/ `shop` /*!40100 DEFAULT CHARACTER SET utf8mb4 */;\n" +
		"USE `shop`;\n" +
		"CREATE TABLE `orders` (\n  `id` int NOT NULL\n) ENGINE=InnoDB;\n" +
		"INSERT INTO `orders` VALUES (1),(2);\n" +
		"CREATE TABLE `odd``name` (\n  `id` int\n);\n" +
		"/*!50001 CREATE VIEW `v` AS SELECT 1 AS `a` */;\n"))
	_ = m.Close()
	if !slices.Equal(m.databases, []string{"shop"}) || len(m.tables) != 2 || !m.tables[0].filled || m.tables[1].name != "odd`name" || m.tables[1].filled {
		t.Errorf("MySQL: databases %v, tables %+v %+v", m.databases, m.tables[0], m.tables[len(m.tables)-1])
	}
}

// A drill keeps at most maxTables in mind, and says when a dump makes more.
func TestADumpOfManyTablesIsBounded(t *testing.T) {
	h := newHeldDump(false)
	var b strings.Builder
	b.WriteString("\\connect big\n")
	for i := range maxTables + 5 {
		b.WriteString("CREATE TABLE public.t")
		b.WriteString(strings.Repeat("x", i%3))
		b.WriteString(strconv.Itoa(i))
		b.WriteString(" (\n")
	}
	_, _ = h.Write([]byte(b.String()))
	if len(h.tables) != maxTables || !h.over {
		t.Errorf("it holds %d tables, over %t", len(h.tables), h.over)
	}
}
