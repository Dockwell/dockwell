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

// postgres stands in for the database's tools: its data is one file of SQL.
func postgres(x dockertest.Exec) int {
	file := filepath.Join(x.Volumes["/var/lib/postgresql/data"], "data.sql")
	script := strings.Join(x.Cmd, " ")
	switch {
	case strings.Contains(script, "pg_dumpall"):
		data, err := os.ReadFile(file) //nolint:gosec // a directory of this fake's own
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 1
		}
		_, _ = x.Stdout.Write(data)
		_, _ = io.WriteString(x.Stdout, "\n-- PostgreSQL database cluster dump complete\n")
	case len(x.Cmd) > 0 && x.Cmd[0] == "psql" && slices.Contains(x.Cmd, "-c"):
		// A question a restore drill asks, psql itself with -c, which comes with nothing
		// on stdin (ADR 0068). A reading runs psql under sh -c, with its SQL on stdin.
		return postgresAnswer(x, file)
	case strings.Contains(script, "psql"):
		data, err := io.ReadAll(x.Stdin)
		if err != nil {
			return 1
		}
		switch sql := string(data); {
		case askedRole.MatchString(sql):
			return postgresUserThere(x, filepath.Dir(file), sql)
		case strings.Contains(sql, "json_build_object"):
			return postgresReading(x, file)
		case strings.Contains(sql, "CREATE ROLE") || strings.Contains(sql, "ALTER ROLE") || strings.Contains(sql, "DROP ROLE"):
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
	dumpedDB    = regexp.MustCompile(`(?m)^(?:CREATE DATABASE |\\connect (?:-reuse-previous=on "dbname=)?)("?[^" ;]+"?|'[^']+')`)
	dumpedTable = regexp.MustCompile(`(?m)^CREATE TABLE ("?[^". ]+"?)\.("?[^". (]+"?) \(`)
	madeRole    = regexp.MustCompile(`ALTER ROLE "([a-z0-9_]+)" LOGIN PASSWORD '([^']*)'`)
	madeDB      = regexp.MustCompile(`SELECT 'CREATE DATABASE "([a-z0-9_]+)" OWNER "([a-z0-9_]+)"' WHERE NOT EXISTS`)
	droppedRole = regexp.MustCompile(`DROP ROLE IF EXISTS "([a-z0-9_]+)"`)
	askedRole   = regexp.MustCompile(`rolname = '([a-z0-9_]+)'\), 'owner', coalesce\(\(SELECT pg_get_userbyid\(datdba\) FROM pg_database WHERE datname = '([a-z0-9_]+)'`)
)

// postgresUsers makes, changes and drops users as psql would, making with \gexec
// only what isn't there yet.
func postgresUsers(_ dockertest.Exec, dir, sql string) int {
	r := readRoles(dir)
	for _, m := range madeRole.FindAllStringSubmatch(sql, -1) {
		r.Passwords[m[1]] = m[2]
	}
	for _, m := range madeDB.FindAllStringSubmatch(sql, -1) {
		if _, has := r.Owns[m[1]]; !has {
			r.Owns[m[1]] = m[2]
		}
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
	_, _ = fmt.Fprintf(x.Stdout, "{\"user\":%t,\"owner\":%q}\n", has, r.Owns[m[2]])
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
	databases := []string{fmt.Sprintf(`{"name":"app","owner":"app","size":%d}`, size+8_192), fmt.Sprintf(`{"name":"postgres","owner":"postgres","size":%d}`, own)}
	users := []string{`{"name":"app","super":false,"login":true}`, `{"name":"postgres","super":true,"login":true}`}
	for name := range r.Passwords {
		users = append(users, fmt.Sprintf(`{"name":%q,"super":false,"login":true}`, name))
	}
	for db, owner := range r.Owns {
		databases = append(databases, fmt.Sprintf(`{"name":%q,"owner":%q,"size":8192}`, db, owner))
	}
	slices.Sort(databases)
	slices.Sort(users)
	_, _ = fmt.Fprintf(x.Stdout, `{"version":"17.11 (Debian 17.11-1.pgdg120+1)","user":"postgres","connections":3,"max_connections":100,`+
		`"uptime":5400,"hit":9920,"read":80,"size":%d,"database_count":%d,"user_count":%d,"databases":[%s],"users":[%s]}`+"\n",
		own+size+8_192, len(databases), len(users), strings.Join(databases, ","), strings.Join(users, ","))
	return 0
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
	t := &tb{dir: dir}
	e := dockertest.New(t)
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
		}
		if slow := os.Getenv("FAKEDOCKER_SLOW"); slow == "" || slices.ContainsFunc(strings.Split(slow, ","), func(s string) bool { return s != "" && strings.Contains(ref, s) }) {
			img.PullTime = pullTime
		}
		// What the stand-in registry has, the engine pulls as the registry names it.
		img.Digest = registryDigest(ref)
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
