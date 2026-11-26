// SPDX-License-Identifier: AGPL-3.0-only

// Command fakedocker serves the fake Docker Engine the tests use on a unix
// socket, for the panel's end-to-end test on a machine without Docker: point
// dockwell at it with DOCKER_HOST=unix://<socket>. It pulls any image. So a
// test can see each way a deploy goes wrong:
//
//   - a name it can't find, "doesnotexist", can't be pulled;
//   - the tag "nosuchtag" isn't on its registry;
//   - the tag "broken" exits as soon as it starts;
//   - the tag "unhealthy" runs, and fails its health check;
//   - the tag "slow" takes 3 s to pull, for a deploy seen in flight;
//   - the tags "answers-ok" and "answers-503" run at 127.0.0.1 and 127.0.0.2, so a
//     check the agent makes reaches what the test listens with on those addresses;
//   - a server path under /readonly can't be mounted;
//   - the tag "prints" writes "legacy says hello" as it starts, as a container
//     run by hand has something in its log;
//   - a language's image that an app from a folder runs on (ADR 0058) runs at
//     127.0.0.1, so its check reaches what the test listens with on its port;
//   - postgres keeps its data as one file, and dumps and loads it as its tools do,
//     for a backup to go through a database's own dump, and answers a reading of
//     itself as psql would, for the Databases section;
//   - /info says Docker keeps its data at FAKEDOCKER_ROOT, when that's set;
//   - a pull takes FAKEDOCKER_PULL_TIME, such as 30s, when that's set, for a stand
//     that shows an update in flight: of the images whose names hold one of
//     FAKEDOCKER_SLOW, a comma-separated list, or of every image without it.
//
// It is never part of a release. CI's Panel end to end on Docker job runs the
// same flows on real Docker.
//
//	go run ./scripts/fakedocker /tmp/fake/docker.sock
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// tb is what dockertest asks of a test: a place for the socket, and what to
// do when the process ends.
type tb struct {
	testing.TB
	dir     string
	cleanup []func()
}

func (t *tb) Helper()                         {}
func (t *tb) TempDir() string                 { return t.dir }
func (t *tb) Cleanup(f func())                { t.cleanup = append(t.cleanup, f) }
func (t *tb) Fatal(args ...any)               { log.Fatal(args...) }
func (t *tb) Fatalf(format string, a ...any)  { log.Fatalf(format, a...) }
func (t *tb) Errorf(format string, a ...any)  { log.Printf(format, a...) }
func (t *tb) Logf(format string, args ...any) { log.Printf(format, args...) }

// stateDir is the folder this fake was given, for files of its own.
var stateDir string

// postgres stands in for the database's tools: its data is one file of SQL.
func postgres(x dockertest.Exec) int {
	// The database flows mount its data folder; a dump or restore run with
	// other mounts uses the one named for postgresql, then the first by name,
	// so a dump and the restore after it read the same file. With none it
	// keeps to this fake's own folder, never one shared with other runs.
	dir := x.Volumes["/var/lib/postgresql/data"]
	if dir == "" {
		keys := slices.Sorted(maps.Keys(x.Volumes))
		for _, k := range keys {
			if v := x.Volumes[k]; v != "" && strings.Contains(k, "postgresql") {
				dir = v
				break
			}
		}
		for _, k := range keys {
			if v := x.Volumes[k]; dir == "" && v != "" {
				dir = v
			}
		}
	}
	if dir == "" {
		dir = stateDir
	}
	file := filepath.Join(dir, "data.sql")
	script := strings.Join(x.Cmd, " ")
	switch {
	case strings.Contains(script, "pg_dumpall"):
		data, err := os.ReadFile(file) //nolint:gosec // a directory of this fake's own
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 1
		}
		_, _ = x.Stdout.Write(data)
		_, _ = io.WriteString(x.Stdout, "\n-- PostgreSQL database cluster dump complete\n")
	case strings.Contains(script, "pg_dump "):
		data, err := os.ReadFile(file) //nolint:gosec // a directory of this fake's own
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 1
		}
		_, _ = x.Stdout.Write(data)
		_, _ = io.WriteString(x.Stdout, "\n-- PostgreSQL database dump complete\n")
	case strings.Contains(script, "pg_restore"):
		data, err := io.ReadAll(x.Stdin)
		if err != nil {
			return 1
		}
		if strings.Contains(string(data), "FAIL_IMPORT") {
			_, _ = io.WriteString(x.Stderr, "pg_restore: error: could not execute query: ERROR: syntax error at or near \"FAIL_IMPORT\"\n")
			return 1
		}
		// PGArchiveToSQL runs pg_restore with no -d and writes SQL to stdout.
		if !slices.Contains(x.Cmd, "-d") && !strings.Contains(script, " -d ") {
			_, _ = x.Stdout.Write(data)
			return 0
		}
		if os.WriteFile(file, data, 0o600) != nil {
			return 1
		}
	case len(x.Cmd) > 0 && x.Cmd[0] == "psql" && slices.Contains(x.Cmd, "-V"):
		_, _ = io.WriteString(x.Stdout, "psql (PostgreSQL) 17.11\n")
		return 0
	case len(x.Cmd) > 0 && x.Cmd[0] == "psql" && slices.Contains(x.Cmd, "-c"):
		// A question a restore drill asks, psql itself with -c, which comes with nothing
		// on stdin (ADR 0068). A reading runs psql under sh -c, with its SQL on stdin.
		return postgresAnswer(x, file)
	case strings.Contains(script, "psql") && strings.Contains(script, " -w ") && strings.Contains(script, "SELECT 1"):
		// A sign-in from another host, as taking a user on checks (ADR 0084).
		return postgresSignsIn(x, filepath.Dir(file))
	case strings.Contains(script, "psql"):
		data, err := io.ReadAll(x.Stdin)
		if err != nil {
			return 1
		}
		switch sql := string(data); {
		case strings.HasPrefix(strings.TrimSpace(sql), "SELECT pg_terminate_backend") && !strings.Contains(sql, "DROP DATABASE"):
			return 0
		case strings.HasPrefix(strings.TrimSpace(sql), "SELECT 1 FROM pg_database WHERE datname ="):
			_, _ = io.WriteString(x.Stdout, "1\n")
			return 0
		case strings.Contains(sql, "FAIL_IMPORT"):
			_, _ = io.WriteString(x.Stderr, "ERROR:  syntax error at or near \"FAIL_IMPORT\"\nLINE 1: FAIL_IMPORT;\n")
			return 3
		case askedLockout.MatchString(sql):
			return postgresLockout(x, filepath.Dir(file), sql)
		case strings.HasPrefix(strings.TrimSpace(sql), "SELECT DISTINCT usename FROM pg_stat_activity"):
			// Active session check on database delete: no active sessions in fake DB.
			return 0
		case strings.Contains(sql, "FROM pg_extension x WHERE x.oid >= 16384"):
			// PgNotOwned check: everything in the fake DB belongs to its owner.
			return 0
		case askedLogical.MatchString(sql):
			return postgresLogicalThere(x, filepath.Dir(file), sql)
		case askedRole.MatchString(sql):
			return postgresUserThere(x, filepath.Dir(file), sql)
		case strings.Contains(sql, "pg_has_role("):
			// What one user reaches: postgres past every database, and app its own,
			// as postgresReading lists them.
			past, own := "0", "0"
			if strings.Contains(sql, "pg_has_role('postgres'") {
				past = "1"
			}
			if strings.Contains(sql, "pg_has_role('app'") && strings.Contains(sql, "datname = 'app'") {
				own = "1"
			}
			_, _ = io.WriteString(x.Stdout, past+" "+own+"\n")
		case strings.Contains(sql, "json_build_object"):
			return postgresReading(x, file)
		case strings.Contains(sql, "CREATE ROLE") || strings.Contains(sql, "ALTER ROLE") || strings.Contains(sql, "DROP ROLE") ||
			strings.Contains(sql, "CREATE DATABASE") || strings.Contains(sql, "DROP DATABASE") || strings.Contains(sql, "ALTER DATABASE"):
			return postgresUsers(x, filepath.Dir(file), sql)
		}
		if os.WriteFile(file, data, 0o600) != nil {
			return 1
		}
	}
	return 0
}

// postgresRoles is the users a fake PostgreSQL made, beside its data: each user's
// password, as it was set, and the database each owns.
type postgresRoles struct {
	Passwords map[string]string `json:"passwords"`
	Owns      map[string]string `json:"owns"`
}

func readRoles(dir string) postgresRoles {
	r := postgresRoles{Passwords: map[string]string{}, Owns: map[string]string{}}
	if b, err := os.ReadFile(filepath.Join(dir, "roles.json")); err == nil { //nolint:gosec // a directory of this fake's own
		_ = json.Unmarshal(b, &r)
	}
	return r
}

// postgresAnswer answers what a restore drill asks of a database, from what was loaded:
// that it answers, its databases, its tables, and which of them have no rows, which
// here is none.
func postgresAnswer(x dockertest.Exec, file string) int {
	query := ""
	if i := slices.Index(x.Cmd, "-c"); i >= 0 && i+1 < len(x.Cmd) {
		query = x.Cmd[i+1]
	}
	data, err := os.ReadFile(file) //nolint:gosec // a directory of this fake's own
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 2
	}
	sql := string(data)
	switch {
	case query == "SELECT 1":
		_, _ = io.WriteString(x.Stdout, "1\n")
	case strings.Contains(query, "FROM pg_database"):
		_, _ = io.WriteString(x.Stdout, "postgres\ntemplate0\ntemplate1\n")
		for _, m := range dumpedDB.FindAllStringSubmatch(sql, -1) {
			_, _ = io.WriteString(x.Stdout, strings.Trim(m[1], `"'`)+"\n")
		}
	case strings.Contains(query, "FROM pg_tables"):
		for _, m := range dumpedTable.FindAllStringSubmatch(sql, -1) {
			_, _ = fmt.Fprintf(x.Stdout, "%s\t%s\n", strings.Trim(m[1], `"`), strings.Trim(m[2], `"`))
		}
	}
	return 0
}

var (
	dumpedDB     = regexp.MustCompile(`(?m)^(?:CREATE DATABASE |\\connect (?:-reuse-previous=on "dbname=)?)("?[^" ;]+"?|'[^']+')`)
	dumpedTable  = regexp.MustCompile(`(?m)^CREATE TABLE ("?[^". ]+"?)\.("?[^". (]+"?) \(`)
	madeRole     = regexp.MustCompile(`ALTER ROLE "([a-z0-9_]+)" LOGIN PASSWORD '([^']*)'`)
	madeDB       = regexp.MustCompile(`(?:SELECT ')?CREATE DATABASE "([a-z0-9_]+)"(?: OWNER "([a-z0-9_]+)")?(?:' WHERE NOT EXISTS|;)`)
	alteredDB    = regexp.MustCompile(`ALTER DATABASE "([a-z0-9_]+)" OWNER TO "([a-z0-9_]+)"`)
	droppedDB    = regexp.MustCompile(`DROP DATABASE IF EXISTS "([a-z0-9_]+)"`)
	droppedRole  = regexp.MustCompile(`DROP ROLE IF EXISTS "([a-z0-9_]+)"`)
	askedRole    = regexp.MustCompile(`rolname = '([a-z0-9_]+)'\), 'owner', coalesce\(\(SELECT pg_get_userbyid\(datdba\) FROM pg_database WHERE datname = '([a-z0-9_]+)'`)
	askedLogical = regexp.MustCompile(`'exists', EXISTS \(SELECT 1 FROM pg_database WHERE datname = '([a-z0-9_]+)'\), 'owner_exists', (true|EXISTS \(SELECT 1 FROM pg_roles WHERE rolname = '([a-z0-9_]+)'\))`)
	askedLockout = regexp.MustCompile(`FROM pg_database d JOIN pg_roles r ON r\.oid = d\.datdba WHERE d\.datname = '([a-z0-9_]+)'`)
)

// postgresLockout answers ReadLockout and OwnerOf queries from roles.json.
func postgresLockout(x dockertest.Exec, dir, sql string) int {
	m := askedLockout.FindStringSubmatch(sql)
	if m == nil {
		return 1
	}
	r := readRoles(dir)
	db := m[1]
	owner := r.Owns[db]
	if db == "app" || db == "postgres" {
		owner = db
	}
	if owner == "" {
		return 0
	}
	super := owner == "postgres"
	if strings.Contains(sql, "'login'") {
		_, _ = fmt.Fprintf(x.Stdout, "{\"owner\":%q,\"login\":true,\"super\":%t,\"loader\":\"\"}\n", owner, super)
	} else {
		_, _ = fmt.Fprintf(x.Stdout, "{\"owner\":%q}\n", owner)
	}
	return 0
}

// postgresSignsIn signs a user in from another host: a user this fake made with the
// password it was given, or one its reading lists by hand, as app, with any password
// but an empty one or "wrong", so a stand can show both answers.
func postgresSignsIn(x dockertest.Exec, dir string) int {
	user := ""
	if len(x.Cmd) >= 4 {
		user = x.Cmd[len(x.Cmd)-4]
	}
	password := ""
	for _, kv := range x.Env {
		if v, ok := strings.CutPrefix(kv, "PGPASSWORD="); ok {
			password = v
		}
	}
	made, ok := readRoles(dir).Passwords[user]
	if ok && password != "" && password == made || !ok && password != "" && !strings.Contains(password, "wrong") {
		return 0
	}
	_, _ = fmt.Fprintf(x.Stderr, "psql: error: connection to server failed: FATAL:  password authentication failed for user %q\n", user)
	return 2
}

// postgresUsers makes, changes and drops users as psql would, making with \gexec
// only what isn't there yet.
func postgresUsers(_ dockertest.Exec, dir, sql string) int {
	r := readRoles(dir)
	for _, m := range madeRole.FindAllStringSubmatch(sql, -1) {
		r.Passwords[m[1]] = m[2]
	}
	for _, m := range madeDB.FindAllStringSubmatch(sql, -1) {
		if _, has := r.Owns[m[1]]; !has {
			owner := m[2]
			if owner == "" {
				owner = "postgres"
			}
			r.Owns[m[1]] = owner
		}
	}
	for _, m := range alteredDB.FindAllStringSubmatch(sql, -1) {
		r.Owns[m[1]] = m[2]
	}
	for _, m := range droppedDB.FindAllStringSubmatch(sql, -1) {
		delete(r.Owns, m[1])
	}
	for _, m := range droppedRole.FindAllStringSubmatch(sql, -1) {
		delete(r.Passwords, m[1])
		for db, owner := range r.Owns {
			if owner == m[1] {
				delete(r.Owns, db)
			}
		}
	}
	b, err := json.Marshal(r)
	if err != nil || os.WriteFile(filepath.Join(dir, "roles.json"), b, 0o600) != nil {
		return 1
	}
	return 0
}

// postgresUserThere answers whether a user and a database are there.
func postgresUserThere(x dockertest.Exec, dir, sql string) int {
	m := askedRole.FindStringSubmatch(sql)
	if m == nil {
		return 1
	}
	r := readRoles(dir)
	_, has := r.Passwords[m[1]]
	owner := r.Owns[m[2]]
	// The user and database its reading lists by hand, as postgresReading has them.
	has = has || m[1] == "app" || m[1] == "postgres"
	if m[2] == "app" || m[2] == "postgres" {
		owner = m[2]
	}
	_, _ = fmt.Fprintf(x.Stdout, "{\"user\":%t,\"owner\":%q}\n", has, owner)
	return 0
}

// postgresLogicalThere answers whether a logical database and its requested owner exist.
func postgresLogicalThere(x dockertest.Exec, dir, sql string) int {
	m := askedLogical.FindStringSubmatch(sql)
	if m == nil {
		return 1
	}
	r := readRoles(dir)
	db := m[1]
	owner := r.Owns[db]
	if db == "app" || db == "postgres" {
		owner = db
	}
	exists := owner != ""
	ownerExists := true
	if m[3] != "" {
		_, ok := r.Passwords[m[3]]
		ownerExists = ok || m[3] == "app" || m[3] == "postgres"
	}
	_, _ = fmt.Fprintf(x.Stdout, "{\"exists\":%t,\"owner_exists\":%t,\"owner\":%q}\n", exists, ownerExists, owner)
	return 0
}

// postgresReading says what PostgreSQL would of itself, as one line of JSON: the
// file of its data is its one database of its own, beside what users it made own.
func postgresReading(x dockertest.Exec, file string) int {
	var size int64
	if info, err := os.Stat(file); err == nil {
		size = info.Size()
	}
	const own = 7_512_000 // what an empty server's postgres database takes
	r := readRoles(filepath.Dir(file))
	databases := []string{fmt.Sprintf(`{"name":"app","owner":"app","size":%d,"connections":2}`, size+8_192), fmt.Sprintf(`{"name":"postgres","owner":"postgres","size":%d,"connections":1}`, own)}
	users := []string{`{"name":"app","super":false,"login":true}`, `{"name":"postgres","super":true,"login":true}`}
	for name := range r.Passwords {
		users = append(users, fmt.Sprintf(`{"name":%q,"super":false,"login":true}`, name))
	}
	for db, owner := range r.Owns {
		databases = append(databases, fmt.Sprintf(`{"name":%q,"owner":%q,"size":8192,"connections":0}`, db, owner))
	}
	slices.Sort(databases)
	slices.Sort(users)
	_, _ = fmt.Fprintf(x.Stdout, `{"version":"17.11 (Debian 17.11-1.pgdg120+1)","user":"postgres","connections":3,"max_connections":100,`+
		`"uptime":5400,"hit":9920,"read":80,"size":%d,"database_count":%d,"user_count":%d,"databases":[%s],"users":[%s]}`+"\n",
		own+size+8_192, len(databases), len(users), strings.Join(databases, ","), strings.Join(users, ","))
	return 0
}

// mysql stands in for MySQL's own tools: answers readings in batch mode, as mysql -N -B does,
// and writes a minimal dump when mysqldump runs. When FAKEDOCKER_MYSQL_UNREAD=1 is set, it
// refuses sign-in so stands can show an unread MySQL beside a read one.
func mysql(x dockertest.Exec) int {
	if os.Getenv("FAKEDOCKER_MYSQL_UNREAD") == "1" {
		_, _ = io.WriteString(x.Stderr, "ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: NO)\n")
		return 1
	}
	script := strings.Join(x.Cmd, " ")
	switch {
	case strings.Contains(script, "mysqldump"):
		_, _ = io.WriteString(x.Stdout, "-- MySQL dump\n-- Dump completed\n")
		return 0
	case strings.Contains(script, "--help"):
		_, _ = io.WriteString(x.Stdout, "  --system-command    Enable system command.\n  --init-command=name\n")
		return 0
	case strings.Contains(script, "mysql"):
		data, err := io.ReadAll(x.Stdin)
		if err != nil {
			return 1
		}
		if strings.Contains(string(data), "SELECT 'v', VERSION()") {
			_, _ = io.WriteString(x.Stdout,
				"v\t9.7.4\troot@localhost\t151\n"+
					"Innodb_buffer_pool_read_requests\t9800\n"+
					"Innodb_buffer_pool_reads\t200\n"+
					"Threads_connected\t3\n"+
					"Uptime\t3600\n"+
					"d\tinformation_schema\t0\n"+
					"d\tmysql\t2457600\n"+
					"d\tperformance_schema\t0\n"+
					"d\tsys\t16384\n"+
					"u\tmysql.sys\tlocalhost\tN\n"+
					"u\troot\tlocalhost\tY\n",
			)
		}
		return 0
	}
	return 0
}

func storageFixtures(e *dockertest.Engine, dir string) {
	const networkName = "fixture-network-with-many-attached-containers"
	const volumeName = "fixture-volume-with-a-long-mountpoint"
	mountpoint := filepath.Join(dir, "docker-volume-fixtures",
		"extraordinarily-long-folder-name-for-a-docker-volume-mountpoint", "nested-volume-data")
	if err := os.MkdirAll(mountpoint, 0o700); err != nil { //nolint:gosec // inside this fake Engine's directory
		log.Fatal(err)
	}
	e.AddVolume(docker.Volume{
		Name: volumeName, Driver: "local", Scope: "local",
		Mountpoint: mountpoint, CreatedAt: "2026-10-12T12:00:00Z",
	})
	n := docker.Network{Name: networkName, Driver: "bridge", Scope: "local", EnableIPv6: true}
	for i := 20; i < 28; i++ {
		n.IPAM.Config = append(n.IPAM.Config, docker.IPAMConfig{
			Subnet: fmt.Sprintf("10.104.%d.0/24", i), Gateway: fmt.Sprintf("10.104.%d.1", i),
		})
	}
	e.AddNetwork(n)
	id := e.Network(networkName).ID
	for i := 2; i <= 12; i++ {
		ctr := docker.Container{Name: fmt.Sprintf("/fixture-storage-workload-%02d", i)}
		ctr.State.Status = "exited"
		ctr.NetworkSettings.Networks = map[string]*docker.EndpointSettings{
			networkName: {NetworkID: id, IPAddress: fmt.Sprintf("10.104.20.%d", i)},
		}
		if i <= 4 {
			ctr.Mounts = []docker.MountPoint{{Type: "volume", Name: volumeName, Destination: "/var/lib/fixture", RW: i != 2}}
		}
		e.AddContainer(ctr)
	}
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("fakedocker takes the directory to put its socket in, such as /tmp/fake")
	}
	dir, err := filepath.Abs(os.Args[1])
	if err != nil {
		log.Fatalf("can't read that directory: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // the directory the person running it names
		log.Fatalf("can't make that directory: %v", err)
	}
	stateDir = dir
	t := &tb{dir: dir}
	e := dockertest.New(t)
	e.ArchiveLayer = os.Getenv("DOCKWELL_TEST_ARCHIVE_LAYER")
	if os.Getenv("FAKEDOCKER_CLEANUP_FIXTURE") == "1" {
		used := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
		e.SetBuildCache(
			docker.DiskUsageCache{ID: "fixtureidlebuildcache0001", Size: 48 << 20, CreatedAt: used, LastUsedAt: &used},
			docker.DiskUsageCache{ID: "fixturebusybuildcache0002", Size: 24 << 20, CreatedAt: used, LastUsedAt: &used, InUse: true},
		)
		e.AddLocalImage("fixture/cleanup-image-with-a-long-repository-name:review", dockertest.Image{
			Size: 52 << 20, Created: used, Layer: "sha256:" + strings.Repeat("d", 64),
		})
		e.AddLocalImage("", dockertest.Image{Size: 10 << 20, Created: used})
		e.AddNetwork(docker.Network{Name: "fixture-idle-network", Scope: "local", Driver: "bridge"})
		anonVol := strings.Repeat("f", 64)
		e.AddVolume(docker.Volume{
			Name: anonVol, Driver: "local", Scope: "local",
			Labels:    map[string]string{"com.docker.volume.anonymous": ""},
			CreatedAt: used.Format(time.RFC3339),
		})
		e.AddVolume(docker.Volume{
			Name: "fixture-unused-volume", Driver: "local", Scope: "local",
			CreatedAt: used.Format(time.RFC3339),
		})
		if e.VolumeSizes == nil {
			e.VolumeSizes = map[string]int64{}
		}
		e.VolumeSizes[anonVol] = 16 << 20
		e.VolumeSizes["fixture-unused-volume"] = 32 << 20
		e.VolumeCreatedAt = func() time.Time { return time.Now().UTC().Add(-5 * time.Minute) }
	}
	if fixtures := os.Getenv("FAKEDOCKER_STORAGE_FIXTURE"); fixtures != "" {
		storageFixtures(e, fixtures)
	}
	// Where /info says Docker keeps its data, for a run that wants the agent to add up
	// a disk this machine has: /var/lib/docker unless set.
	e.DockerRoot = os.Getenv("FAKEDOCKER_ROOT")
	pullTime, err := time.ParseDuration(cmpOr(os.Getenv("FAKEDOCKER_PULL_TIME"), "0s"))
	if err != nil {
		log.Fatalf("can't read FAKEDOCKER_PULL_TIME, such as 30s: %v", err)
	}
	e.Find = func(ref string) (dockertest.Image, bool) {
		name, _, _ := strings.Cut(ref, ":")
		if strings.HasSuffix(name, "doesnotexist") {
			return dockertest.Image{}, false
		}
		img := dockertest.Image{
			Exits: strings.HasSuffix(ref, ":broken"),
			Size:  48 << 20,
			Config: docker.Config{
				Cmd: []string{"serve"},
				Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin"},
			},
		}
		if strings.HasPrefix(ref, "postgres:") {
			img.Exec = postgres
		} else if strings.HasPrefix(ref, "mysql:") {
			img.Exec = mysql
		}
		if slow := os.Getenv("FAKEDOCKER_SLOW"); slow == "" || slices.ContainsFunc(strings.Split(slow, ","), func(s string) bool { return s != "" && strings.Contains(ref, s) }) {
			img.PullTime = pullTime
		}
		// What the stand-in registry has, the engine pulls as the registry names it.
		img.Digest = registryDigest(ref)
		if repo, digest, pinned := strings.Cut(ref, "@"); pinned && repo == "example/dockwell-pull" {
			fixture := fakeRepos[repo]
			for _, tag := range fixture.tags {
				config, manifest := configOf(repo, fixture, tag)
				if digestOfBytes(manifest) == digest {
					img.Digest, img.ConfigDigest = digest, digestOfBytes(config)
					break
				}
			}
		}

		// What it keeps private, a pull gets only with its login.
		if strings.Contains(name, "example/private") {
			img.Login = privateUser + ":" + privateToken
		}
		// A shell, run as this machine's /bin/sh, unless it's made to run one program.
		if !strings.HasSuffix(ref, ":distroless") {
			img.Shell = "/bin/sh"
		}
		switch {
		case strings.HasSuffix(ref, ":nosuchtag"):
			img.PullError = "manifest for " + ref + " not found: manifest unknown: manifest unknown"
		case strings.HasSuffix(ref, ":slow"):
			img.PullTime = 3 * time.Second
		case strings.HasSuffix(ref, ":unhealthy"):
			img.Unhealthy = true
			img.CheckSays = "the database refuses connections. Check its password"
			img.Config.Healthcheck = &docker.Healthcheck{Test: []string{"CMD", "false"}}
		case strings.HasSuffix(ref, ":answers-ok"):
			img.Address = "127.0.0.1"
		case strings.HasSuffix(ref, ":answers-503"):
			img.Address = "127.0.0.2"
		case strings.HasSuffix(ref, ":prints"):
			img.Prints = []string{"legacy says hello"}
		}
		if slices.ContainsFunc(service.Runtimes(), func(r service.Runtime) bool {
			return ref == r.Image || strings.HasPrefix(r.Image, ref+"@")
		}) {
			img.Address = "127.0.0.1"
		}
		return img, true
	}
	e.Hub = fakeHub
	// A registry for the agent's reads of images, where a test names one
	// (DOCKWELL_TEST_REGISTRY); none otherwise.
	if addr := os.Getenv("FAKEDOCKER_REGISTRY"); addr != "" {
		serveRegistry(addr)
	}
	// Cloudflare's API, for the control plane to write DNS through, where a test
	// names one (DOCKWELL_TEST_CLOUDFLARE, ADR 0088).
	if addr := os.Getenv("FAKEDOCKER_CLOUDFLARE"); addr != "" {
		serveCloudflare(addr)
	}
	// GitHub's pages and API, for the control plane to connect as an App, where a
	// test names one (DOCKWELL_TEST_GITHUB, ADR 0098).
	if addr := os.Getenv("FAKEDOCKER_GITHUB"); addr != "" {
		serveGitHub(addr)
	}
	e.BindError = func(source string) string {
		if strings.HasPrefix(source, "/readonly") {
			return "mkdir " + source + ": read-only file system"
		}
		return ""
	}
	fmt.Println(e.Socket)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	for _, f := range t.cleanup {
		f()
	}
}

// cmpOr is v, or or where v is empty.
func cmpOr(v, or string) string {
	if v == "" {
		return or
	}
	return v
}
