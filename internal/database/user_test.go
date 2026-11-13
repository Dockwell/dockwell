// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// fakePostgres keeps roles and databases as psql's \gexec would make them, and can
// be told to take SQL and make nothing, or to fail what makes a database.
type fakePostgres struct {
	mu        sync.Mutex
	roles     map[string]bool
	databases map[string]string // a database's owner
	makeNone  bool
	failDB    bool
	said      []string
	// starting is how many more times it answers as a server that's starting.
	starting int
	// dump is what its pg_dump writes, by database, and loaded what its psql was
	// given to load, by database.
	dump   map[string]string
	loaded map[string]string
}

var (
	fakeCreateRole = regexp.MustCompile(`SELECT 'CREATE ROLE "([a-z0-9_]+)" LOGIN'`)
	fakeCreateDB   = regexp.MustCompile(`SELECT 'CREATE DATABASE "([a-z0-9_]+)" OWNER "([a-z0-9_]+)"'`)
	fakeAsk        = regexp.MustCompile(`'user', EXISTS \(SELECT 1 FROM pg_roles WHERE rolname = '([a-z0-9_]+)'\), 'owner', coalesce\(\(SELECT pg_get_userbyid\(datdba\) FROM pg_database WHERE datname = '([a-z0-9_]+)'`)
	fakeDropDB     = regexp.MustCompile(`DROP DATABASE IF EXISTS "([a-z0-9_]+)"`)
	fakeDropRole   = regexp.MustCompile(`DROP ROLE IF EXISTS "([a-z0-9_]+)"`)
)

func (f *fakePostgres) exec(x dockertest.Exec) int {
	script := strings.Join(x.Cmd, " ")
	switch {
	case strings.Contains(script, "pg_dump"):
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = io.WriteString(x.Stdout, f.dump[x.Cmd[len(x.Cmd)-1]])
		return 0
	case strings.Contains(script, "psql") && strings.Contains(script, `-d "$1"`):
		b, _ := io.ReadAll(x.Stdin)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.loaded == nil {
			f.loaded = map[string]string{}
		}
		f.loaded[x.Cmd[len(x.Cmd)-1]] = string(b)
		return 0
	}
	b, _ := io.ReadAll(x.Stdin)
	sql := string(b)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.said = append(f.said, sql)
	if f.starting > 0 {
		f.starting--
		_, _ = io.WriteString(x.Stderr, "psql: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed: FATAL:  the database system is shutting down\n")
		return 2
	}
	if m := fakeAsk.FindStringSubmatch(sql); m != nil {
		_, _ = io.WriteString(x.Stdout, `{"user":`+map[bool]string{true: "true", false: "false"}[f.roles[m[1]]]+`,"owner":"`+f.databases[m[2]]+`"}`+"\n")
		return 0
	}
	if f.makeNone {
		return 0
	}
	for _, m := range fakeCreateRole.FindAllStringSubmatch(sql, -1) {
		f.roles[m[1]] = true
	}
	for _, m := range fakeCreateDB.FindAllStringSubmatch(sql, -1) {
		if f.failDB {
			_, _ = io.WriteString(x.Stderr, "psql:<stdin>:4: ERROR:  could not write to file \"base/16384/PG_VERSION\": No space left on device\n")
			return 3
		}
		if _, has := f.databases[m[1]]; !has {
			f.databases[m[1]] = m[2]
		}
	}
	for _, m := range fakeDropDB.FindAllStringSubmatch(sql, -1) {
		delete(f.databases, m[1])
	}
	for _, m := range fakeDropRole.FindAllStringSubmatch(sql, -1) {
		delete(f.roles, m[1])
	}
	return 0
}

// userStand is a PostgreSQL service's container on a fake Docker Engine, and a
// driver that makes users in it.
func userStand(t *testing.T) (*UserDriver, *fakePostgres, *dockertest.Engine) {
	t.Helper()
	pg := &fakePostgres{roles: map[string]bool{"postgres": true}, databases: map[string]string{"postgres": "postgres"}}
	e := dockertest.New(t)
	e.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: pg.exec})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-db-1", &docker.CreateRequest{Config: docker.Config{
		Image: "postgres:17", Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: "db", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return &UserDriver{Client: client, Dir: t.TempDir()}, pg, e
}

func makeUser(ctx context.Context, d *UserDriver, name string) error {
	step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: "env_1", Name: name}, Action: resource.ActionCreate}
	ctx = resource.WithValues(ctx, map[string]string{passwordUse: "a-password-no-one-typed"})
	return d.Apply(ctx, step, &resourcev1.DatabaseUser{Service: "db", Password: "SHOP_DATABASE_PASSWORD"})
}

// A server that takes the SQL and makes nothing fails the step, and says so, rather
// than reading as applied.
func TestAStepThatMakesNoUserFails(t *testing.T) {
	d, pg, _ := userStand(t)
	pg.makeNone = true
	err := makeUser(t.Context(), d, "shop")
	if err == nil || !strings.Contains(err.Error(), "took the SQL that makes the user shop without an error, and has no such user") {
		t.Fatalf("a step that made no user: %v", err)
	}
	if got, _ := d.Observe(t.Context(), resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}); got != nil {
		t.Errorf("a user that wasn't made reads as %v", got)
	}
}

// A step that stops halfway leaves a user Dockwell knows it began: applying again
// finishes it, rather than refusing a user it says Dockwell didn't make.
func TestAHalfMadeUserFinishesNextTime(t *testing.T) {
	d, pg, _ := userStand(t)
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	pg.failDB = true
	err := makeUser(t.Context(), d, "shop")
	if err == nil || !strings.Contains(err.Error(), "PostgreSQL refused it: could not write to file") {
		t.Fatalf("a step whose database failed: %v", err)
	}
	if !pg.roles["shop"] {
		t.Fatal("the fake made no role before the database failed")
	}
	if got, err := d.Observe(t.Context(), ref); got != nil || err != nil {
		t.Errorf("a user without its database reads as %v, %v", got, err)
	}
	desired := &resourcev1.DatabaseUser{Service: "db", Password: "SHOP_DATABASE_PASSWORD"}
	if _, err := d.Judge(t.Context(), ref, nil, desired, nil); err != nil {
		t.Fatalf("planning it again: %v", err)
	}
	pg.failDB = false
	if err := makeUser(t.Context(), d, "shop"); err != nil {
		t.Fatalf("applying it again: %v", err)
	}
	if pg.databases["shop"] != "shop" {
		t.Errorf("the database shop is %q's", pg.databases["shop"])
	}

	// One Dockwell never began is still refused.
	pg.roles["someone"] = true
	if _, err := d.Judge(t.Context(), resource.Ref{Kind: UserKind, Scope: "env_1", Name: "someone"}, nil, desired, nil); err == nil ||
		!strings.Contains(err.Error(), "which Dockwell didn't make") {
		t.Errorf("a user made by hand: %v", err)
	}
}

// A user whose server the same change makes is planned with nothing to ask, and made
// once that server takes connections, as PostgreSQL's image takes a moment to on an
// empty volume, starting it twice (ADR 0084). One whose server is nowhere is refused.
func TestAUserIsMadeWithItsServer(t *testing.T) {
	d, _, e := userStand(t)
	d.Every = time.Millisecond
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	desired := &resourcev1.DatabaseUser{Service: "db", Password: "SHOP_DATABASE_PASSWORD"}
	e.Remove("env_1-db-1")
	if _, err := d.Judge(t.Context(), ref, nil, desired, nil); err == nil || !strings.Contains(err.Error(), "isn't on this server") {
		t.Errorf("a user whose server is nowhere: %v", err)
	}
	ctx := resource.WithDesired(t.Context(), []resource.Desired{
		{Ref: resource.Ref{Kind: "service", Scope: "env_1", Name: "db"}, Spec: &resourcev1.Service{}},
		{Ref: ref, Spec: desired},
	})
	dec, err := d.Judge(ctx, ref, nil, desired, nil)
	if err != nil || dec.Action != resource.ActionCreate {
		t.Fatalf("a user whose server this change makes: %v, %v", dec, err)
	}

	// The change makes the server, which starts, stops and starts again.
	d2, pg2, _ := userStand(t)
	d2.Every = time.Millisecond
	pg2.starting = 3
	if err := makeUser(t.Context(), d2, "shop"); err != nil {
		t.Fatalf("making a user while its server starts: %v", err)
	}
	if !pg2.roles["shop"] || pg2.databases["shop"] != "shop" {
		t.Errorf("roles %v, databases %v", pg2.roles, pg2.databases)
	}

	// One that's still starting when the wait runs out says so.
	d2.Wait = 5 * time.Millisecond
	pg2.starting = 1 << 20
	if err := makeUser(t.Context(), d2, "blog"); err == nil || !strings.Contains(err.Error(), "isn't taking connections yet") {
		t.Errorf("a server that never takes connections: %v", err)
	}
}

// A user made with a database to copy gets that database's dump from the other
// server's container, loaded into theirs as them, so what it makes is theirs; one whose
// source is another engine, or gone, fails the step and takes the user away again (ADR
// 0084).
func TestAUserIsMadeWithACopyOfAnotherDatabase(t *testing.T) {
	d, pg, e := userStand(t)
	old := &fakePostgres{
		roles: map[string]bool{"postgres": true}, databases: map[string]string{"postgres": "postgres", "blog": "blog"},
		dump: map[string]string{"blog": "CREATE TABLE posts (id int);\nINSERT INTO posts VALUES (1);\n"},
	}
	e.AddLocalImage("postgres:16", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: old.exec})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_2-db-1", &docker.CreateRequest{Config: docker.Config{
		Image: "postgres:16", Labels: map[string]string{docker.LabelProject: "env_2", docker.LabelService: "db", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	copying := func(name, env string) error {
		step := resource.Step{Ref: resource.Ref{Kind: UserKind, Scope: "env_1", Name: name}, Action: resource.ActionCreate}
		ctx := resource.WithValues(t.Context(), map[string]string{passwordUse: "a-password-no-one-typed"})
		return d.Apply(ctx, step, &resourcev1.DatabaseUser{Service: "db", Password: "BLOG_DATABASE_PASSWORD", CopyFrom: &resourcev1.DatabaseCopy{Environment: env, Service: "db", Database: "blog"}})
	}
	if err := copying("blog", "env_2"); err != nil {
		t.Fatal(err)
	}
	if got := pg.loaded["blog"]; got != "SET ROLE \"blog\";\nCREATE TABLE posts (id int);\nINSERT INTO posts VALUES (1);\n" {
		t.Errorf("the new server loaded %q", got)
	}
	if err := copying("wiki", "env_9"); err == nil || !strings.Contains(err.Error(), "has no container on this server") {
		t.Errorf("a copy from nowhere: %v", err)
	}
	if pg.roles["wiki"] {
		t.Error("a copy that failed left its user behind")
	}
}

// Deleting a user whose server's container is gone has nothing to drop it from, so it
// succeeds, and Dockwell forgets it.
func TestDeletingAUserWithItsContainerGoneForgetsIt(t *testing.T) {
	d, _, e := userStand(t)
	ref := resource.Ref{Kind: UserKind, Scope: "env_1", Name: "shop"}
	if err := makeUser(t.Context(), d, "shop"); err != nil {
		t.Fatal(err)
	}
	e.Remove("env_1-db-1")
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Action: resource.ActionDelete}, nil); err != nil {
		t.Fatalf("deleting it: %v", err)
	}
	if _, ok, _ := d.recorded(ref); ok {
		t.Error("Dockwell still keeps the user")
	}
	b, err := os.ReadFile(filepath.Join(d.Dir, "database-users.json"))
	if err != nil || strings.Contains(string(b), "shop") {
		t.Errorf("the agent keeps %s, %v", b, err)
	}
}

// The agent takes no name into SQL that it wouldn't take from the control plane.
func TestTheAgentChecksANameBeforeItsSQL(t *testing.T) {
	d, pg, _ := userStand(t)
	if err := makeUser(t.Context(), d, `shop"; DROP DATABASE postgres; --`); err == nil || !strings.Contains(err.Error(), "isn't a valid user name") {
		t.Errorf("a name that isn't one: %v", err)
	}
	if len(pg.said) != 0 {
		t.Errorf("PostgreSQL was given %q", pg.said)
	}
}
