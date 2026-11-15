// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// mongoState is what a fake MongoDB holds of the users its root user made, each by
// the database it owns, with its password, and every script it was given.
type mongoState struct {
	mu        sync.Mutex
	owns      map[string]string // a user's database
	passwords map[string]string
	scripts   []string
}

var (
	mongoUser = regexp.MustCompile(`const name = ("[^"]*");`)
	mongoDB   = regexp.MustCompile(`const target = m\.getDB\(("[^"]*")\);`)
	mongoPwd  = regexp.MustCompile(`const pwd = ("(?:[^"\\]|\\.)*");`)
)

// image is a mongo whose root user makes users as mongosh would, answering only
// one who signs in, as the official image does once it has made its root user.
func (s *mongoState) image() dockertest.Image {
	return dockertest.Image{
		Volumes: []string{"/data/db", "/data/configdb"}, Config: docker.Config{Cmd: []string{"mongod"}},
		Exec: func(x dockertest.Exec) int {
			b, _ := io.ReadAll(x.Stdin)
			script := string(b)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.scripts = append(s.scripts, script)
			unquote := func(re *regexp.Regexp) string {
				var v string
				if m := re.FindStringSubmatch(script); m != nil {
					_ = json.Unmarshal([]byte(m[1]), &v)
				}
				return v
			}
			name, db := unquote(mongoUser), unquote(mongoDB)
			root := slices.ContainsFunc(x.Env, func(kv string) bool {
				return strings.HasPrefix(kv, "MONGO_INITDB_ROOT_USERNAME=") && len(kv) > len("MONGO_INITDB_ROOT_USERNAME=")
			})
			switch {
			case strings.Contains(script, "listDatabases"):
				out, _ := json.Marshal(map[string]bool{"root": root, "answering": true, "open": !root})
				_, _ = io.WriteString(x.Stdout, string(out)+"\n")
			case strings.Contains(script, "createUser("):
				s.owns[name], s.passwords[name] = db, unquote(mongoPwd)
				_, _ = io.WriteString(x.Stdout, `{"made":true}`+"\n")
			case strings.Contains(script, "const owns"):
				owner := ""
				if s.owns[name] == db {
					owner = name
				}
				_, has := s.owns[name]
				out, _ := json.Marshal(map[string]any{"user": has, "owner": owner})
				_, _ = io.WriteString(x.Stdout, string(out)+"\n")
			case strings.Contains(script, "dropDatabase()"):
				delete(s.owns, name)
				_, _ = io.WriteString(x.Stdout, `{"dropped":true}`+"\n")
			}
			return 0
		},
	}
}

// notesEntry is an app whose need takes MongoDB, which brings none of its own.
const notesEntry = `id: notes
name: Notes
summary: Notes on the web
category: productivity
version: "1.0"
release: 1
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
web:
  service: app
inputs:
  - name: PORT
    label: Port
    type: INPUT_TYPE_PORT
    defaultValue: "18191"
needs:
  - name: db
    label: Database
    database: notes
    accepts:
      - engine: ENGINE_MONGODB
        versions: ">=6"
    sets:
      - name: MONGO_HOST
        part: PART_HOST
      - name: MONGO_USER
        part: PART_USER
      - name: MONGO_PASSWORD
        part: PART_PASSWORD
      - name: MONGO_URL
        part: PART_URL
compose: |
  services:
    app:
      image: ghcr.io/acme/notes:1@sha256:` + digest1 + `
      environment:
        MONGO_HOST: ${MONGO_HOST}
        MONGO_USER: ${MONGO_USER}
        MONGO_PASSWORD: ${MONGO_PASSWORD}
        MONGO_URL: ${MONGO_URL}
      ports:
        - "${PORT}:3000"
`

// An app whose need takes MongoDB gets a new one from the catalog's mongo entry by
// default, whose image makes only its root user: one plan makes the server, then the
// app's user, owning the app's database alone, made as that root user, and then the
// app, which signs in as its own user. The root password is the server's alone, and
// an upgrade keeps it all as it is (ADR 0084).
func TestAnInstallMakesANewMongoDBWithAUserOfItsOwn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml"), []byte(notesEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	mg := &mongoState{owns: map[string]string{}, passwords: map[string]string{}}
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	mongo, ok := c.Entry("mongo")
	image := regexp.MustCompile(`image: (mongo:\S+)`).FindStringSubmatch(mongo.GetCompose())
	if !ok || image == nil {
		t.Fatal("the catalog has no mongo")
	}
	e.Registry[image[1]] = mg.image()
	e.Registry["ghcr.io/acme/notes:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"notes"}}}
	ch := changeClient(b, b.origin)
	cat := catalogClient(b)

	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "notes"}))
	if err != nil {
		t.Fatal(err)
	}
	if needs := got.Msg.GetNeeds(); len(needs) == 0 || needs[0].GetEntry() != "mongo" || !strings.HasPrefix(needs[0].GetChoice(), "new") {
		t.Fatalf("the need is offered %v, want a new MongoDB first", needs)
	}
	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "notes"})
	var order []string
	scopes := map[string]string{}
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE {
			k := r.GetResource().GetKind() + " " + r.GetResource().GetName()
			order = append(order, k)
			scopes[k] = r.GetResource().GetScope()
		}
	}
	db, user, app := slices.Index(order, "service db"), slices.Index(order, "database.user notes"), slices.Index(order, "service app")
	if db < 0 || user < 0 || app < 0 || db > user || user > app {
		t.Fatalf("the plan makes %v, want the server, then its user, then the app", order)
	}
	if got := apply(t, ch, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	project := docker.Project(scopes["service app"])
	env := func(service string) map[string]string {
		t.Helper()
		c := e.Newest(project, service)
		if c == nil {
			t.Fatalf("%s doesn't run: %v", service, e.Containers())
		}
		out := map[string]string{}
		for _, kv := range c.Config.Env {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		return out
	}
	notes, server := env("app"), env("db")
	mg.mu.Lock()
	owns, password, scripts := mg.owns["notes"], mg.passwords["notes"], slices.Clone(mg.scripts)
	mg.mu.Unlock()
	if owns != "notes" || password == "" || password != notes["MONGO_PASSWORD"] {
		t.Errorf("on the new MongoDB, notes owns %q, with the password the app has: %t", owns, password == notes["MONGO_PASSWORD"])
	}
	made := scripts[slices.IndexFunc(scripts, func(s string) bool { return strings.Contains(s, "createUser(") })]
	if !strings.Contains(made, `admin.auth(env.MONGO_INITDB_ROOT_USERNAME`) || !strings.Contains(made, `roles = [{ role: "dbOwner", db: target.getName() }]`) {
		t.Errorf("the user was made with %s", made)
	}
	if notes["MONGO_HOST"] != "db" || notes["MONGO_USER"] != "notes" {
		t.Errorf("the app reads %v", notes)
	}
	if want := "mongodb://" + url.UserPassword("notes", notes["MONGO_PASSWORD"]).String() + "@db:27017/notes"; notes["MONGO_URL"] != want {
		t.Errorf("the app's URL is %q, want %q", notes["MONGO_URL"], want)
	}
	if server["MONGO_INITDB_ROOT_USERNAME"] != "root" || server["MONGO_INITDB_ROOT_PASSWORD"] == "" || server["MONGO_INITDB_ROOT_PASSWORD"] == notes["MONGO_PASSWORD"] {
		t.Errorf("the server starts with %v, the app with %v", server, notes)
	}
	if c := e.Newest(project, "db"); c == nil || len(c.HostConfig.PortBindings) != 0 {
		t.Errorf("the server publishes %v", c.HostConfig.PortBindings)
	}

	// An upgrade keeps the MongoDB, its user and its password as they are.
	next := strings.NewReplacer(`release: 1`, `release: 2`, `version: "1.0"`, `version: "1.1"`, "notes:1@sha256:"+digest1, "notes:1@sha256:"+digest2).Replace(notesEntry)
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml"), []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
	e.Registry["ghcr.io/acme/notes:1@sha256:"+digest2] = dockertest.Image{Config: docker.Config{Cmd: []string{"notes"}}}
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "notes", SkipBackup: true}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		name := r.GetResource().GetKind() + " " + r.GetResource().GetName()
		if name != "service app" && name != "project notes" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			t.Errorf("upgrading notes plans %v for %s", r.GetAction(), name)
		}
	}
	if got := apply(t, ch, res.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the upgrade ended %v: %s", got.GetState(), got.GetError())
	}
	if got := env("app"); got["MONGO_PASSWORD"] != notes["MONGO_PASSWORD"] || got["MONGO_URL"] != notes["MONGO_URL"] {
		t.Error("after the upgrade the app reads another password or URL")
	}
}
