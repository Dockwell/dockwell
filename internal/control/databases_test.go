// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func databaseClient(c connect.HTTPClient, origin string) dockwellv1connect.DatabaseServiceClient {
	return dockwellv1connect.NewDatabaseServiceClient(c, origin, connect.WithProtoJSON())
}

// readablePostgres answers a reading as the official image's psql would, and counts
// how often it's asked. What it's asked with is kept, to see no password is in it.
func readablePostgres(asked *atomic.Int32, cmds *[]string) dockertest.Image {
	return dockertest.Image{
		Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}},
		Exec: func(x dockertest.Exec) int {
			sql, _ := io.ReadAll(x.Stdin)
			if !strings.Contains(string(sql), "json_build_object") {
				return 1
			}
			asked.Add(1)
			*cmds = append(*cmds, strings.Join(x.Cmd, " "))
			_, _ = io.WriteString(x.Stdout, `{"version":"17.2 (Debian 17.2-1.pgdg120+1)","user":"postgres","connections":4,"max_connections":100,"uptime":7200,"hit":99,"read":1,"size":9000000,"database_count":2,"user_count":2,`+
				`"databases":[{"name":"postgres","owner":"postgres","size":8000000},{"name":"shop","owner":"shop","size":1000000}],`+
				`"users":[{"name":"postgres","super":true,"login":true},{"name":"shop","super":false,"login":true}]}`+"\n")
			return 0
		},
	}
}

// readableRedis answers INFO as redis-cli would, and keeps the command it ran.
func readableRedis(cmds *[]string) dockertest.Image {
	return dockertest.Image{
		Volumes: []string{"/data"}, Config: docker.Config{Cmd: []string{"redis-server"}},
		Exec: func(x dockertest.Exec) int {
			*cmds = append(*cmds, strings.Join(x.Cmd, " "))
			_, _ = io.WriteString(x.Stdout, "# Server\r\nredis_version:7.4.1\r\nuptime_in_seconds:60\r\n# Clients\r\nconnected_clients:1\r\nmaxclients:10000\r\n# Memory\r\nused_memory:2048\r\n# Keyspace\r\ndb0:keys=5,expires=0,avg_ttl=0\r\n")
			return 0
		},
	}
}

const databaseStack = `
services:
  web:
    image: ghcr.io/acme/shop:1.4
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes: [db-data:/var/lib/postgresql/data]
  cache:
    image: redis:7
    command: ["redis-server", "--port", "6380"]
volumes:
  db-data: {}
`

// The services that run a database are listed, each with what it says of itself,
// read in its own container with what its environment holds: no password goes
// anywhere, a reading is asked for once however many read it, and a server that's
// away says so.
func TestDatabasesAreTheServicesThatRunOne(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testDatabasesAreTheServicesThatRunOne)
}

func testDatabasesAreTheServicesThatRunOne(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	var asked atomic.Int32
	var pgCmds, redisCmds []string
	e.Registry["postgres:17"] = readablePostgres(&asked, &pgCmds)
	e.Registry["redis:7"] = readableRedis(&redisCmds)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	ch := changeClient(b, b.origin)
	const password = "a-password-nobody-reads"
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": password}, false))
	dc := databaseClient(b, b.origin)

	server := f.lastDocker
	server.stop()
	waitConnected(t, f, server.id.ServerID, false)
	away, err := dc.ListDatabases(t.Context(), connect.NewRequest(&dockwellv1.ListDatabasesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range away.Msg.GetDatabases() {
		if r := d.GetReadings(); len(r) != 1 || r[0].GetUnread() != "hz-1 isn't connected, so it can't say how its databases are" || r[0].GetServerName() != "hz-1" {
			t.Errorf("with hz-1 away, %s reads %v", d.GetResource().GetName(), r)
		}
	}
	server.connect(t, f)

	listed, err := dc.ListDatabases(t.Context(), connect.NewRequest(&dockwellv1.ListDatabasesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	dbs := listed.Msg.GetDatabases()
	if len(dbs) != 2 || dbs[0].GetResource().GetName() != "cache" || dbs[1].GetResource().GetName() != "db" {
		t.Fatalf("the databases are %v, want cache and db, and not web", dbs)
	}
	cache, db := dbs[0], dbs[1]
	if db.GetEngine() != dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL || db.GetVersion() != "17" || db.GetVolume() != "db-data" || db.GetVolumeId() == "" ||
		db.GetPort() != 5432 || db.GetHost() != "db" || !slices.Equal(db.GetBeside(), []string{"cache", "web"}) || db.GetPasswordSecret() != "" {
		t.Errorf("db is %v", db)
	}
	r := db.GetReadings()
	if len(r) != 1 || r[0].GetUnread() != "" || r[0].GetVersion() != "17.2" || r[0].GetUser() != "postgres" || r[0].GetConnections() != 4 ||
		r[0].GetSizeBytes() != 9000000 || len(r[0].GetDatabases()) != 2 || r[0].GetDatabases()[1].GetName() != "shop" || r[0].GetReadTime() == nil {
		t.Errorf("db reads %v", r)
	}
	if cache.GetEngine() != dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS || cache.GetPort() != 6380 || cache.GetVolume() != "" {
		t.Errorf("cache is %v", cache)
	}
	if r := cache.GetReadings(); len(r) != 1 || r[0].GetVersion() != "7.4.1" || r[0].GetKeys() != 5 {
		t.Errorf("cache reads %v", r)
	}
	if len(redisCmds) == 0 || !strings.HasPrefix(redisCmds[0], "env DW_PORT=6380 sh -c") {
		t.Errorf("redis-cli ran as %q, without the port its command sets", redisCmds)
	}

	// Its tools read the password where the image does, so it's in no command.
	for _, cmd := range append(pgCmds, redisCmds...) {
		if strings.Contains(cmd, password) {
			t.Errorf("a reading ran with the password in its command: %q", cmd)
		}
	}
	text, err := protojson.Marshal(listed.Msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), password) {
		t.Error("the list holds the password")
	}

	// A reading stands for a moment, so several readers ask once.
	before := asked.Load()
	read := databaseClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}, b.origin)
	byName, err := read.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"}))
	if err != nil {
		t.Fatalf("a read-only token can't read a database: %v", err)
	}
	if byName.Msg.GetDatabase().GetId() != db.GetId() || len(byName.Msg.GetDatabase().GetReadings()[0].GetUsers()) != 2 {
		t.Errorf("by name, db is %v", byName.Msg.GetDatabase())
	}
	byID, err := dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: db.GetId()}))
	if err != nil || byID.Msg.GetDatabase().GetResource().GetName() != "db" {
		t.Errorf("by ID, db is %v, %v", byID, err)
	}
	if got := asked.Load(); got != before {
		t.Errorf("PostgreSQL was asked %d more times within a moment", got-before)
	}

	_, err = dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "web"}))
	if connect.CodeOf(err) != connect.CodeNotFound || !strings.Contains(err.Error(), "there's no database web") {
		t.Errorf("web as a database: %v", err)
	}
}

// pgState is what a fake PostgreSQL holds of its users and databases, and every
// statement it was given, to see what a password became. makeNone has it take SQL
// and make nothing.
type pgState struct {
	mu        sync.Mutex
	roles     map[string]string // a role's password, as it was set
	databases map[string]string // a database's owner
	said      []string
	makeNone  bool
	// sizes has its reading list its databases, each this large, where set.
	sizes map[string]int64
	// loaded is what each load into one database was given, by database.
	loaded map[string]string
	// passwords are what a sign-in from another host takes, by role, as a role made
	// by hand has it.
	passwords map[string]string
}

var (
	createRole   = regexp.MustCompile(`SELECT 'CREATE ROLE "([a-z0-9_]+)" LOGIN' WHERE NOT EXISTS`)
	alterRole    = regexp.MustCompile(`ALTER ROLE "([a-z0-9_]+)" (?:LOGIN )?PASSWORD '([^']*)'`)
	createDB     = regexp.MustCompile(`SELECT 'CREATE DATABASE "([a-z0-9_]+)" OWNER "([a-z0-9_]+)"' WHERE NOT EXISTS`)
	createDBOnly = regexp.MustCompile(`SELECT 'CREATE DATABASE "([a-z0-9_]+)"' WHERE NOT EXISTS`)
	alterDBOwner = regexp.MustCompile(`ALTER DATABASE "([a-z0-9_]+)" OWNER TO "([a-z0-9_]+)"`)
	dropDB       = regexp.MustCompile(`DROP DATABASE IF EXISTS "([a-z0-9_]+)"`)
	dropRole     = regexp.MustCompile(`DROP ROLE IF EXISTS "([a-z0-9_]+)"`)
	askLockout   = regexp.MustCompile(`'login', r.rolcanlogin, 'super', r.rolsuper, 'loader', .* FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = '([a-z0-9_]+)'`)
	askReach     = regexp.MustCompile(`pg_has_role\('([a-z0-9_]+)', r\.oid, 'MEMBER'\).*FROM pg_database WHERE datname = '([a-z0-9_]+)'`)
	askOwner     = regexp.MustCompile(`'owner', r.rolname\) FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = '([a-z0-9_]+)'`)
	askRole      = regexp.MustCompile(`rolname = '([a-z0-9_]+)'\), 'owner', coalesce\(\(SELECT pg_get_userbyid\(datdba\) FROM pg_database WHERE datname = '([a-z0-9_]+)'`)
	askLogical   = regexp.MustCompile(`'exists', EXISTS \(SELECT 1 FROM pg_database WHERE datname = '([a-z0-9_]+)'\), 'owner_exists', (true|EXISTS \(SELECT 1 FROM pg_roles WHERE rolname = '([a-z0-9_]+)'\))`)
)

// image is a postgres that keeps users and databases as psql would make them.
func (s *pgState) image() dockertest.Image {
	return dockertest.Image{
		Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}},
		Exec: func(x dockertest.Exec) int {
			// Anyone signing in from another host needs a password, as the official
			// image's pg_hba.conf has it.
			if script := strings.Join(x.Cmd, " "); strings.Contains(script, " -w ") && strings.Contains(script, "SELECT 1") && x.Cmd[len(x.Cmd)-2] != "127.0.0.1" {
				// One that gives a role's own password signs in, as taking a user on checks.
				user := x.Cmd[len(x.Cmd)-4]
				s.mu.Lock()
				want := s.passwords[user]
				s.mu.Unlock()
				for _, kv := range x.Env {
					if pw, ok := strings.CutPrefix(kv, "PGPASSWORD="); ok && want != "" && pw == want {
						return 0
					}
				}
				_, _ = io.WriteString(x.Stderr, "psql: error: fe_sendauth: no password supplied\n")
				return 2
			}
			if script := strings.Join(x.Cmd, " "); strings.Contains(script, "pg_dumpall") {
				// The whole server, which reads nothing.
				_, _ = io.WriteString(x.Stdout, "CREATE ROLE blog;\n--\n-- PostgreSQL database cluster dump complete\n--\n")
				return 0
			}
			if script := strings.Join(x.Cmd, " "); strings.Contains(script, "pg_dump ") {
				// One database alone, which a dump says it ended.
				s.mu.Lock()
				defer s.mu.Unlock()
				s.said = append(s.said, script)
				cur := "CREATE TABLE posts (id int);\n"
				if s.loaded["shop"] != "" {
					cur = s.loaded["shop"] + "\n"
				}
				_, _ = io.WriteString(x.Stdout, cur+"--\n-- PostgreSQL database dump complete\n--\n")
				return 0
			}
			b, _ := io.ReadAll(x.Stdin)
			sql := string(b)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.said = append(s.said, strings.Join(x.Cmd, " ")+"\n"+sql)
			if script := strings.Join(x.Cmd, " "); strings.Contains(script, `-d "$d"`) {
				if s.loaded == nil {
					s.loaded = map[string]string{}
				}
				s.loaded[x.Cmd[5]] = sql
				return 0
			}
			if strings.HasPrefix(strings.TrimSpace(sql), "SELECT pg_terminate_backend") && !strings.Contains(sql, "DROP DATABASE") {
				return 0
			}
			if m := regexp.MustCompile(`^SELECT 1 FROM pg_database WHERE datname = '([a-z0-9_]+)'`).FindStringSubmatch(strings.TrimSpace(sql)); m != nil {
				if _, ok := s.databases[m[1]]; ok {
					_, _ = io.WriteString(x.Stdout, "1\n")
				}
				return 0
			}
			if script := strings.Join(x.Cmd, " "); strings.Contains(script, `-d "$DW_DB"`) {
				if strings.Contains(sql, "BROKEN_DUMP") {
					_, _ = io.WriteString(x.Stderr, "ERROR:  syntax error at or near \"BROKEN_DUMP\"\n")
					return 3
				}
				if s.loaded == nil {
					s.loaded = map[string]string{}
				}
				dbName := "shop"
				for _, arg := range x.Cmd {
					if v, ok := strings.CutPrefix(arg, "DW_DB="); ok {
						dbName = v
					}
				}
				s.loaded[dbName] = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), "--\n-- PostgreSQL database dump complete\n--"))
				return 0
			}
			if m := askLockout.FindStringSubmatch(sql); m != nil {
				_, _ = fmt.Fprintf(x.Stdout, `{"owner":%q,"login":true,"super":false,"loader":null}`+"\n", s.databases[m[1]])
				return 0
			}
			if m := askReach.FindStringSubmatch(sql); m != nil {
				// What a user reaches: postgres reaches past every database, and a user its
				// own database where it owns it.
				one := map[bool]string{true: "1", false: "0"}
				_, _ = io.WriteString(x.Stdout, one[m[1] == "postgres"]+" "+one[s.databases[m[2]] == m[1]]+"\n")
				return 0
			}
			if m := askOwner.FindStringSubmatch(sql); m != nil {
				_, _ = fmt.Fprintf(x.Stdout, `{"owner":%q}`+"\n", s.databases[m[1]])
				return 0
			}
			if m := askLogical.FindStringSubmatch(sql); m != nil {
				owner, exists := s.databases[m[1]]
				ownerExists := true
				if m[3] != "" {
					_, ownerExists = s.roles[m[3]]
				}
				_, _ = fmt.Fprintf(x.Stdout, `{"exists":%t,"owner_exists":%t,"owner":%q}`+"\n", exists, ownerExists, owner)
				return 0
			}
			if m := askRole.FindStringSubmatch(sql); m != nil {
				_, has := s.roles[m[1]]
				_, _ = fmt.Fprintf(x.Stdout, `{"user":%t,"owner":%q}`+"\n", has, s.databases[m[2]])
				return 0
			}
			if strings.Contains(sql, "json_build_object") {
				var users []string
				for name := range s.roles {
					users = append(users, fmt.Sprintf(`{"name":%q,"super":%t,"login":true}`, name, name == "postgres"))
				}
				slices.Sort(users)
				var dbs []string
				if s.sizes != nil {
					for name, size := range s.sizes {
						dbs = append(dbs, fmt.Sprintf(`{"name":%q,"owner":%q,"size":%d}`, name, s.databases[name], size))
					}
				} else {
					for name, owner := range s.databases {
						dbs = append(dbs, fmt.Sprintf(`{"name":%q,"owner":%q,"size":1048576,"connections":1}`, name, owner))
					}
				}
				slices.Sort(dbs)
				_, _ = fmt.Fprintf(x.Stdout, `{"version":"17.2","user":"postgres","databases":[%s],"users":[%s]}`+"\n", strings.Join(dbs, ","), strings.Join(users, ","))
				return 0
			}
			if s.makeNone {
				return 0
			}
			// \gexec runs what each SELECT makes only where it isn't yet.
			for _, m := range createRole.FindAllStringSubmatch(sql, -1) {
				if _, has := s.roles[m[1]]; !has {
					s.roles[m[1]] = ""
				}
			}
			for _, m := range alterRole.FindAllStringSubmatch(sql, -1) {
				s.roles[m[1]] = m[2]
			}
			for _, m := range createDB.FindAllStringSubmatch(sql, -1) {
				if _, has := s.databases[m[1]]; !has {
					s.databases[m[1]] = m[2]
				}
			}
			for _, m := range createDBOnly.FindAllStringSubmatch(sql, -1) {
				if _, has := s.databases[m[1]]; !has {
					s.databases[m[1]] = "postgres"
				}
			}
			for _, m := range alterDBOwner.FindAllStringSubmatch(sql, -1) {
				if _, has := s.databases[m[1]]; has {
					s.databases[m[1]] = m[2]
				}
			}
			for _, m := range dropDB.FindAllStringSubmatch(sql, -1) {
				delete(s.databases, m[1])
			}
			for _, m := range dropRole.FindAllStringSubmatch(sql, -1) {
				delete(s.roles, m[1])
			}
			return 0
		},
	}
}

// A user and its database are made in PostgreSQL's own container through a plan, with
// a password the control plane generates and keeps as a secret, which goes to the
// server for that step alone and in as a SCRAM verifier, never as it is. Setting the
// secret anew gives the user the new one, and undoing it all drops both.
func TestADatabaseUserIsAPlanWithAGeneratedSecret(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testADatabaseUserIsAPlanWithAGeneratedSecret)
}

func testADatabaseUserIsAPlanWithAGeneratedSecret(t *testing.T, backend store.Backend) {
	logged := &syncBuffer{}
	f := startFullLogging(t, backend, nil, slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	e.Registry["postgres:17"] = pg.image()
	var redis []string
	e.Registry["redis:7"] = readableRedis(&redis)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	ch := changeClient(b, b.origin)
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "first-password"}, false))
	dc := databaseClient(b, b.origin)

	// A server that takes the SQL and makes nothing fails the step, and says so: the
	// change doesn't read as applied.
	pg.mu.Lock()
	pg.makeNone = true
	pg.mu.Unlock()
	nothing, err := dc.CreateDatabaseUser(t.Context(), connect.NewRequest(&dockwellv1.CreateDatabaseUserRequest{Id: "db", User: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, ch, nothing.Msg.GetChange()); got.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED ||
		!strings.Contains(got.GetError(), "has no such user") {
		t.Fatalf("a step that made no user ended %v: %v", got.GetState(), got.GetError())
	}
	pg.mu.Lock()
	pg.makeNone = false
	_, ghost := pg.roles["ghost"]
	pg.mu.Unlock()
	if ghost {
		t.Fatal("the fake made ghost after all")
	}

	// A reading kept from before the user is made is no reading of it after.
	if _, err := dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"})); err != nil {
		t.Fatal(err)
	}
	planned, err := dc.CreateDatabaseUser(t.Context(), connect.NewRequest(&dockwellv1.CreateDatabaseUserRequest{Id: "db", User: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	change := planned.Msg.GetChange()
	var kinds []string
	for _, r := range change.GetPlan().GetResources() {
		kinds = append(kinds, r.GetResource().GetKind()+" "+r.GetResource().GetName())
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"database.user shop", "secret SHOP_DATABASE_PASSWORD"}) {
		t.Fatalf("the plan makes %v", kinds)
	}
	done := apply(t, ch, change)
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("making the user ended %v: %v", done.GetState(), done.GetError())
	}
	pg.mu.Lock()
	verifier, owner := pg.roles["shop"], pg.databases["shop"]
	said := strings.Join(pg.said, "\n")
	pg.mu.Unlock()
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$4096:") || owner != "shop" {
		t.Fatalf("PostgreSQL has the role shop with %q, owning %q", verifier, owner)
	}

	// The password is the secret's, which only a passkey reveals, and nothing else shows.
	password := revealSecret(t, f, change.GetPlan().GetResources()[0].GetResource().GetScope(), "SHOP_DATABASE_PASSWORD")
	if len(password) != 32 {
		t.Fatalf("the generated password is %d characters", len(password))
	}
	if strings.Contains(said, password) {
		t.Error("what ran in PostgreSQL's container holds the password as it is")
	}
	if !scramFits(t, verifier, password) {
		t.Error("the SCRAM verifier isn't of the secret's password")
	}
	text, err := protojson.Marshal(done)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), password) {
		t.Error("the change holds the password")
	}
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		events, err := tx.ListEvents(t.Context(), 500)
		for i := range events {
			if b, _ := json.Marshal(&events[i]); strings.Contains(string(b), password) {
				t.Errorf("the event %s holds the password", events[i].Action)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), password) {
		t.Error("the control plane's log holds the password")
	}
	kept, err := os.ReadFile(filepath.Join(f.lastDocker.userDir, "database-users.json"))
	if err != nil || !strings.Contains(string(kept), "SHOP_DATABASE_PASSWORD") || strings.Contains(string(kept), password) {
		t.Errorf("the agent keeps %s, %v", kept, err)
	}

	read, err := dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	var shop *dockwellv1.DatabaseAccount
	for _, a := range read.Msg.GetDatabase().GetReadings()[0].GetUsers() {
		if a.GetName() == "shop" {
			shop = a
		}
	}
	if shop.GetSecret() != "SHOP_DATABASE_PASSWORD" || shop.GetResourceId() == "" {
		t.Errorf("the database says of shop %v", shop)
	}
	_, err = dc.CreateDatabaseUser(t.Context(), connect.NewRequest(&dockwellv1.CreateDatabaseUserRequest{Id: "db", User: "shop"}))
	var refused *connect.Error
	if !errors.As(err, &refused) || refused.Code() != connect.CodeInvalidArgument || refused.Meta().Get("Dockwell-Error") != "invalid_user" ||
		!strings.Contains(err.Error(), "a database user shop in this environment already") {
		t.Errorf("a second shop: %v", err)
	}
	_, err = dc.CreateDatabaseUser(t.Context(), connect.NewRequest(&dockwellv1.CreateDatabaseUserRequest{Id: "cache", User: "shop2"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "Redis, which has no users of its own") {
		t.Errorf("a user in Redis: %v", err)
	}

	// Setting the secret anew is a plan that reaches the user, as it reaches a service.
	rotated := apply(t, ch, propose(t, ch, "Rotate", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Name: "SHOP_DATABASE_PASSWORD"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "a-new-password-for-shop"}}},
	}))
	if rotated.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("rotating ended %v: %v", rotated.GetState(), rotated.GetError())
	}
	pg.mu.Lock()
	again := pg.roles["shop"]
	pg.mu.Unlock()
	if again == verifier || !scramFits(t, again, "a-new-password-for-shop") {
		t.Errorf("after the secret was set anew, shop signs in with %q", again)
	}

	// Undoing the rotation gives the user its first password back.
	back, err := ch.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: rotated.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, ch, back.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("undoing the rotation ended %v: %v", got.GetState(), got.GetError())
	}
	pg.mu.Lock()
	again = pg.roles["shop"]
	pg.mu.Unlock()
	if !scramFits(t, again, password) {
		t.Error("undoing the rotation didn't give shop its first password back")
	}

	// Deleting it drops the user and its database, which destroys data, so the plan says so.
	gone := propose(t, ch, "Delete shop",
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "database.user", Name: "shop"}, Delete: true},
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "secret", Name: "SHOP_DATABASE_PASSWORD"}, Delete: true})
	if !gone.GetPlan().GetDestructive() {
		t.Error("dropping a user and its database doesn't say it destroys data")
	}
	applyDestructive(t, ch, gone)
	pg.mu.Lock()
	_, role := pg.roles["shop"]
	_, db := pg.databases["shop"]
	pg.mu.Unlock()
	if role || db {
		t.Errorf("after the undo, PostgreSQL still has the role %t, the database %t", role, db)
	}
}

// revealSecret reads a secret's value as Reveal does once a person has confirmed it.
func revealSecret(t *testing.T, f *fullServer, env, name string) string {
	t.Helper()
	var id string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), "secret", env, name)
		id = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := f.keeper.Reveal(t.Context(), id, "usr_test", "test", true, func(*store.Tx) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return got.Value
}

// applyDestructive applies a plan that destroys data, with its fingerprint, as a
// person confirms one.
func applyDestructive(t *testing.T, c dockwellv1connect.ChangeServiceClient, ch *dockwellv1.Change) {
	t.Helper()
	w := watch(t, c, ch.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("%s ended %v: %v", ch.GetSummary(), got.GetState(), got.GetError())
	}
}

// scramFits says a SCRAM-SHA-256 verifier is of password.
func scramFits(t *testing.T, verifier, password string) bool {
	t.Helper()
	m := regexp.MustCompile(`^SCRAM-SHA-256\$(\d+):([^$]+)\$([^:]+):(.+)$`).FindStringSubmatch(verifier)
	if m == nil {
		return false
	}
	salt, _ := base64.StdEncoding.DecodeString(m[2])
	salted, err := pbkdf2.Key(sha256.New, password, salt, 4096, sha256.Size)
	if err != nil {
		t.Fatal(err)
	}
	h := hmac.New(sha256.New, salted)
	h.Write([]byte("Client Key"))
	stored := sha256.Sum256(h.Sum(nil))
	return base64.StdEncoding.EncodeToString(stored[:]) == m[3]
}

// A database's first user gets a new generated password through a plan: the secret
// its service starts with changes, the agent gives it to the user in the database as
// the service restarts, and undoing the change puts the old one back.
// Deleting a database's service with a user Dockwell made in it deletes the service
// first, so the user's step drops nothing: its database stays in the service's
// volume, which the deletion keeps, and undo brings back (ADR 0074). The other way
// round the user's database would go before anything kept it.
func TestDeletingADatabaseWithItsUserDropsNothingInIt(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testDeletingADatabaseWithItsUserDropsNothingInIt)
}

func testDeletingADatabaseWithItsUserDropsNothingInIt(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	e.Registry["postgres:17"] = pg.image()
	var redis []string
	e.Registry["redis:7"] = readableRedis(&redis)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	ch := changeClient(b, b.origin)
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "first-password"}, false))
	dc := databaseClient(b, b.origin)
	made, err := dc.CreateDatabaseUser(t.Context(), connect.NewRequest(&dockwellv1.CreateDatabaseUserRequest{Id: "db", User: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, ch, made.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("making shop ended %v: %v", got.GetState(), got.GetError())
	}

	planned := propose(t, ch, "Delete db",
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "database.user", Name: "shop"}, Delete: true},
		&dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "db"}, Delete: true},
	)
	var order []string
	for _, s := range planned.GetPlan().GetServers()[0].GetSteps() {
		order = append(order, s.GetResource().GetKind()+" "+s.GetResource().GetName())
		if s.GetResource().GetKind() == "database.user" && s.GetDestroysData() {
			t.Error("the user's step says it destroys data, though its server goes first")
		}
	}
	if !slices.Equal(order, []string{"service db", "database.user shop"}) {
		t.Fatalf("the steps run in the order %v, not the service first", order)
	}
	if got := apply(t, ch, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("deleting db ended %v: %v", got.GetState(), got.GetError())
	}
	pg.mu.Lock()
	_, role := pg.roles["shop"]
	owner := pg.databases["shop"]
	said := strings.Join(pg.said, "\n")
	pg.mu.Unlock()
	if !role || owner != "shop" || strings.Contains(said, "DROP") {
		t.Errorf("PostgreSQL has the role shop %t and its database owned by %q, and was told:\n%s", role, owner, said)
	}
}

func TestADatabasesFirstPasswordChangesThroughAPlan(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testADatabasesFirstPasswordChangesThroughAPlan)
}

func testADatabasesFirstPasswordChangesThroughAPlan(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	e.Registry["postgres:17"] = pg.image()
	var redis []string
	e.Registry["redis:7"] = readableRedis(&redis)
	ch := changeClient(b, b.origin)
	// The password is a secret the database's service uses by reference, as a catalog
	// install's is.
	apply(t, ch, propose(t, ch, "Run the databases", secretSpec("DB_PASSWORD", "first-password"),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: "db"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17"}},
				Secrets: map[string]string{"POSTGRES_PASSWORD": "DB_PASSWORD"},
			}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: "cache"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "redis:7"}},
			}}},
		},
		// An app's own PostgreSQL, whose first user is the app's, as Umami's is.
		secretSpec("STATS_PASSWORD", "stats-password"),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: "stats-db"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17", Env: map[string]string{"POSTGRES_USER": "umami"}}},
				Secrets: map[string]string{"POSTGRES_PASSWORD": "STATS_PASSWORD"},
			}}},
		}))
	dc := databaseClient(b, b.origin)

	// The plan names the user the image made from POSTGRES_USER, as the agent changes theirs.
	stats, err := dc.ChangeDatabasePassword(t.Context(), connect.NewRequest(&dockwellv1.ChangeDatabasePasswordRequest{Id: "stats-db"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := stats.Msg.GetChange().GetSummary(); got != "Change the password of umami in stats-db" {
		t.Errorf("the change of an app's own PostgreSQL reads %q", got)
	}
	if _, err := ch.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: stats.Msg.GetChange().GetId()})); err != nil {
		t.Fatal(err)
	}

	planned, err := dc.ChangeDatabasePassword(t.Context(), connect.NewRequest(&dockwellv1.ChangeDatabasePasswordRequest{Id: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	change := planned.Msg.GetChange()
	if change.GetSummary() != "Change the password of postgres in db" {
		t.Errorf("the change reads %q", change.GetSummary())
	}
	var kinds []string
	for _, r := range change.GetPlan().GetResources() {
		kinds = append(kinds, r.GetResource().GetKind()+" "+r.GetResource().GetName())
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"secret DB_PASSWORD", "service db"}) {
		t.Fatalf("the plan changes %v", kinds)
	}
	if done := apply(t, ch, change); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("changing the password ended %v: %v", done.GetState(), done.GetError())
	}
	password := revealSecret(t, f, change.GetPlan().GetResources()[0].GetResource().GetScope(), "DB_PASSWORD")
	pg.mu.Lock()
	verifier, said := pg.roles["postgres"], strings.Join(pg.said, "\n")
	pg.mu.Unlock()
	if len(password) != 32 || !scramFits(t, verifier, password) {
		t.Fatalf("postgres signs in with %q, and the secret holds %d characters", verifier, len(password))
	}
	if strings.Contains(said, password) {
		t.Error("what ran in PostgreSQL's container holds the password as it is")
	}

	back, err := ch.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: change.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, ch, back.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("undoing it ended %v: %v", got.GetState(), got.GetError())
	}
	pg.mu.Lock()
	verifier = pg.roles["postgres"]
	pg.mu.Unlock()
	if !scramFits(t, verifier, "first-password") {
		t.Error("undoing the change didn't give postgres its first password back")
	}

	_, err = dc.ChangeDatabasePassword(t.Context(), connect.NewRequest(&dockwellv1.ChangeDatabasePasswordRequest{Id: "db", User: "ghost"}))
	var refused *connect.Error
	if !errors.As(err, &refused) || refused.Meta().Get("Dockwell-Error") != "invalid_user" || !strings.Contains(err.Error(), "no user ghost that Dockwell made") {
		t.Errorf("a user Dockwell didn't make: %v", err)
	}
	_, err = dc.ChangeDatabasePassword(t.Context(), connect.NewRequest(&dockwellv1.ChangeDatabasePasswordRequest{Id: "cache"}))
	if !errors.As(err, &refused) || refused.Meta().Get("Dockwell-Error") != "no_secret" {
		t.Errorf("a database whose password isn't in a secret: %v", err)
	}

	// Users says what Connect says of the first user: its password is in the database's own
	// secret, which the apps that sign in as it use, and the database that has it isn't one (#798).
	apply(t, ch, propose(t, ch, "Run the shop", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "web"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "redis:7"}},
			Secrets: map[string]string{"DATABASE_PASSWORD": "DB_PASSWORD"},
		}}},
	}))
	read, err := dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	var first *dockwellv1.DatabaseAccount
	for _, a := range read.Msg.GetDatabase().GetReadings()[0].GetUsers() {
		if a.GetName() == "postgres" {
			first = a
		}
	}
	if first.GetSecret() != "DB_PASSWORD" || !slices.Equal(first.GetUsedBy(), []string{"web"}) || first.GetResourceId() != "" {
		t.Errorf("db says of postgres %v, want its password in DB_PASSWORD, used by web", first)
	}

	// A server that's away can't say whether its agent changes the password as the
	// database restarts, so nothing is planned.
	f.lastDocker.stop()
	eventually(t, "the server to go", func() bool {
		_, ok := f.agents.Session(f.lastDocker.id.ServerID)
		return !ok
	})
	_, err = dc.ChangeDatabasePassword(t.Context(), connect.NewRequest(&dockwellv1.ChangeDatabasePasswordRequest{Id: "db"}))
	if !errors.As(err, &refused) || refused.Meta().Get("Dockwell-Error") != "unavailable" || !strings.Contains(err.Error(), "isn't connected") {
		t.Errorf("a database on a server that's away: %v", err)
	}
}

// One PostgreSQL engine hosts several logical databases: creating one with a new
// owner user+secret or with an existing owner, deleting an unlinked logical
// database without dropping its owner, refusing deletion while an app links to
// it, and offering the engine as a candidate for another app's database need.
func TestLogicalDatabasesInOneEngine(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testLogicalDatabasesInOneEngine)
}

func testLogicalDatabasesInOneEngine(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	e.Registry["postgres:17"] = pg.image()
	var redis []string
	e.Registry["redis:7"] = readableRedis(&redis)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	ch := changeClient(b, b.origin)
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "first-password"}, false))
	dc := databaseClient(b, b.origin)

	// 1. Create a database with default new owner user + secret.
	madeDefault, err := dc.CreateLogicalDatabase(t.Context(), connect.NewRequest(&dockwellv1.CreateLogicalDatabaseRequest{
		Id:       "db",
		Database: "analytics",
	}))
	if err != nil {
		t.Fatalf("CreateLogicalDatabase (default owner): %v", err)
	}
	if got := apply(t, ch, madeDefault.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("applying analytics ended %v: %v", got.GetState(), got.GetError())
	}

	// 2. Create a second database owned by that existing user (database.logical, no new role/secret).
	madeOwned, err := dc.CreateLogicalDatabase(t.Context(), connect.NewRequest(&dockwellv1.CreateLogicalDatabaseRequest{
		Id:       "db",
		Database: "reports",
		Owner:    "analytics",
	}))
	if err != nil {
		t.Fatalf("CreateLogicalDatabase (existing owner): %v", err)
	}
	if got := apply(t, ch, madeOwned.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("applying reports ended %v: %v", got.GetState(), got.GetError())
	}

	// 3. Create a third unlinked logical database owned by analytics so we can test Delete.
	madeTemp, err := dc.CreateLogicalDatabase(t.Context(), connect.NewRequest(&dockwellv1.CreateLogicalDatabaseRequest{
		Id:       "db",
		Database: "staging",
		Owner:    "analytics",
	}))
	if err != nil {
		t.Fatalf("CreateLogicalDatabase (staging): %v", err)
	}
	if got := apply(t, ch, madeTemp.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("applying staging ended %v: %v", got.GetState(), got.GetError())
	}

	pg.mu.Lock()
	if pg.databases["analytics"] != "analytics" || pg.databases["reports"] != "analytics" || pg.databases["staging"] != "analytics" {
		t.Errorf("pg databases = %v, want analytics, reports, staging owned by analytics", pg.databases)
	}
	pg.mu.Unlock()

	// 4. Link an app to reports; deletion of reports must be refused while linked.
	envScope := madeDefault.Msg.GetChange().GetPlan().GetResources()[0].GetResource().GetScope()
	apply(t, ch, propose(t, ch, "Link web to reports", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: envScope, Name: "web-reports"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
			Label:        "Database",
			Service:      "db",
			Engine:       resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL,
			Version:      "17",
			Accepts:      []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
			Sets:         []*resourcev1.LinkSet{{Name: "DB_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
			DatabaseName: "reports",
			User:         "analytics",
		}}},
	}))

	_, err = dc.DeleteLogicalDatabase(t.Context(), connect.NewRequest(&dockwellv1.DeleteLogicalDatabaseRequest{
		Id:       "db",
		Database: "reports",
	}))
	if err == nil || !strings.Contains(err.Error(), "reports") {
		t.Fatalf("deleting linked database reports = %v, want refusal naming reports", err)
	}

	// Reading the engine shows the logical databases, their managed resource IDs, and linked_by.
	read, err := dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*dockwellv1.InnerDatabase{}
	for _, inner := range read.Msg.GetDatabase().GetReadings()[0].GetDatabases() {
		byName[inner.GetName()] = inner
	}
	if byName["reports"].GetResourceKind() != "database.logical" || len(byName["reports"].GetLinkedBy()) == 0 {
		t.Errorf("reports reading = %v, want database.logical with linked_by", byName["reports"])
	}
	if byName["analytics"].GetResourceKind() != "database.user" || byName["analytics"].GetResourceId() == "" {
		t.Errorf("analytics reading = %v, want database.user with resource_id", byName["analytics"])
	}

	// 5. Deleting an unlinked logical database drops the database while keeping its owner role.
	delStaging, err := dc.DeleteLogicalDatabase(t.Context(), connect.NewRequest(&dockwellv1.DeleteLogicalDatabaseRequest{
		Id:       "db",
		Database: "staging",
	}))
	if err != nil {
		t.Fatalf("DeleteLogicalDatabase(staging): %v", err)
	}
	applyDestructive(t, ch, delStaging.Msg.GetChange())
	pg.mu.Lock()
	_, stagingStillThere := pg.databases["staging"]
	_, ownerStillThere := pg.roles["analytics"]
	pg.mu.Unlock()
	if stagingStillThere {
		t.Error("staging database was not dropped")
	}
	if !ownerStillThere {
		t.Error("deleting logical database staging dropped its owner role analytics")
	}

	// 6. Import a dump via BeginDatabaseImport + REST chunked PUT + PlanDatabaseImport + Apply + Undo.
	dumpPayload := []byte("INSERT INTO analytics_events VALUES (42);\n")
	began, err := dc.BeginDatabaseImport(t.Context(), connect.NewRequest(&dockwellv1.BeginDatabaseImportRequest{
		Id:        "db",
		Database:  "analytics",
		Filename:  "events.sql",
		SizeBytes: uint64(len(dumpPayload)),
	}))
	if err != nil {
		t.Fatalf("BeginDatabaseImport: %v", err)
	}
	// Stream in two chunks over the REST PUT streaming route.
	half := len(dumpPayload) / 2
	for _, part := range []struct {
		offset int
		data   []byte
	}{
		{0, dumpPayload[:half]},
		{half, dumpPayload[half:]},
	} {
		url := fmt.Sprintf("%s/v1/databases/db/imports/%s?offset=%d", b.origin, began.Msg.GetImportId(), part.offset)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, bytes.NewReader(part.data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Origin", b.origin)
		resp, err := b.client.Do(req)
		if err != nil {
			t.Fatalf("PUT chunk at %d: %v", part.offset, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("PUT chunk at %d returned %d: %s", part.offset, resp.StatusCode, string(body))
		}
	}

	plannedImport, err := dc.PlanDatabaseImport(t.Context(), connect.NewRequest(&dockwellv1.PlanDatabaseImportRequest{
		Id:       "db",
		ImportId: began.Msg.GetImportId(),
	}))
	if err != nil {
		t.Fatalf("PlanDatabaseImport: %v", err)
	}
	restorePlan := plannedImport.Msg.GetChange().GetPlan().GetServers()[0].GetSteps()[0].GetRestore()
	if restorePlan.GetImportFile() != "events.sql" || restorePlan.GetDatabaseName() != "analytics" {
		t.Fatalf("import RestorePlan = %v, want import_file=events.sql database_name=analytics", restorePlan)
	}
	applyDestructive(t, ch, plannedImport.Msg.GetChange())
	pg.mu.Lock()
	gotLoaded := pg.loaded["analytics"]
	pg.mu.Unlock()
	if !strings.Contains(gotLoaded, "INSERT INTO analytics_events VALUES (42);") {
		t.Errorf("analytics loaded = %q, want imported SQL", gotLoaded)
	}
}
