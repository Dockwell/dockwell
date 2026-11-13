// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// wordpressWithNeeds is the built-in WordPress as ADR 0084 rewrites it, at release:
// its database a need its own MariaDB meets, or the catalog's MySQL.
func wordpressWithNeeds(t *testing.T, release int, image string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "catalog", "snapshot", "entries", "wordpress.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	s = strings.NewReplacer(
		"WORDPRESS_DB_HOST: db", "WORDPRESS_DB_HOST: ${DB_ADDRESS}",
		"WORDPRESS_DB_USER: wordpress", "WORDPRESS_DB_USER: ${DB_USER}",
		"WORDPRESS_DB_NAME: wordpress", "WORDPRESS_DB_NAME: ${DB_NAME}",
		"MARIADB_DATABASE: wordpress", "MARIADB_DATABASE: ${DB_NAME}",
		"MARIADB_USER: wordpress", "MARIADB_USER: ${DB_USER}",
		"  - name: MARIADB_PASSWORD\n    label: Database password\n    type: INPUT_TYPE_SECRET\n    generate: 32\n", "",
	).Replace(s)
	i := strings.Index(s, "release: ")
	end := strings.Index(s[i:], "\n")
	s = s[:i] + "release: " + strconv.Itoa(release) + s[i+end:]
	if image != "" {
		start := strings.Index(s, "image: wordpress:")
		stop := strings.Index(s[start:], "\n")
		s = s[:start] + "image: " + image + s[start+stop:]
	}
	s = strings.Replace(s, "databases: [db]\n", `databases: [db]
needs:
  - name: db
    label: Database
    service: db
    database: wordpress
    user: wordpress
    accepts:
      - engine: ENGINE_MYSQL
        versions: ">=8.0"
      - engine: ENGINE_MARIADB
        versions: ">=10.6"
    sets:
      - name: DB_ADDRESS
        part: PART_ADDRESS
      - name: DB_NAME
        part: PART_DATABASE
      - name: DB_USER
        part: PART_USER
      - name: MARIADB_PASSWORD
        part: PART_PASSWORD
`, 1)
	return s
}

// An install meets its need as asked: by the entry's own MariaDB, or by the catalog's
// MySQL in its place, with what the need sets as the environment's variables, the
// password a generated secret, both used by reference, a link that says how, and the
// pin keeping it, so an upgrade keeps the MySQL.
func TestAnInstallMeetsItsNeeds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	write := func(release int, image string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "wordpress.yaml"), []byte(wordpressWithNeeds(t, release, image)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(5, "")
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)

	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "wordpress"}))
	if err != nil {
		t.Fatal(err)
	}
	var choices []string
	for _, o := range got.Msg.GetNeeds() {
		choices = append(choices, o.GetNeed()+"="+o.GetChoice())
	}
	if !slices.Equal(choices, []string{"db=new:mariadb", "db=new:mysql"}) {
		t.Fatalf("the entry's need options: %v", choices)
	}
	if len(got.Msg.GetParts()) == 0 {
		t.Error("an entry with needs says nothing of what it installs")
	}

	for _, refused := range []*dockwellv1.InstallCatalogEntryRequest{
		{Id: "wordpress", Project: "x", Values: map[string]string{"DB_NAME": "mine"}},
		{Id: "wordpress", Project: "x", Needs: map[string]string{"db": "new:postgresql"}},
		{Id: "wordpress", Project: "x", Needs: map[string]string{"cache": "new"}},
	} {
		if _, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(refused)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%v = %v", refused, err)
		}
	}

	install := func(project string, needs map[string]string) (*dockwellv1.Change, string) {
		t.Helper()
		planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "wordpress", Project: project, Needs: needs})
		if got := apply(t, c, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("installing %s ended %v: %s", project, got.GetState(), got.GetError())
		}
		for _, r := range planned.GetPlan().GetResources() {
			if r.GetResource().GetKind() == "service" {
				return planned, docker.Project(r.GetResource().GetScope())
			}
		}
		t.Fatal("the install planned no service")
		return nil, ""
	}
	made := func(ch *dockwellv1.Change) map[string]bool {
		out := map[string]bool{}
		for _, r := range ch.GetPlan().GetResources() {
			if r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE {
				out[r.GetResource().GetKind()+" "+r.GetResource().GetName()] = true
			}
		}
		return out
	}
	env := func(project, service string) map[string]string {
		t.Helper()
		ct := e.Newest(project, service)
		if ct == nil {
			t.Fatalf("%s has no %s running: %v", project, service, e.Containers())
		}
		out := map[string]string{}
		for _, kv := range ct.Config.Env {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		return out
	}

	// By its own MariaDB.
	own, ownProject := install("blog", nil)
	for _, want := range []string{"link db", "variable DB_ADDRESS", "variable DB_NAME", "variable DB_USER", "secret MARIADB_PASSWORD", "service db", "service wordpress"} {
		if !made(own)[want] {
			t.Errorf("by its own MariaDB, the plan makes no %s: %v", want, made(own))
		}
	}
	app, db := env(ownProject, "wordpress"), env(ownProject, "db")
	if app["WORDPRESS_DB_HOST"] != "db:3306" || app["WORDPRESS_DB_NAME"] != "wordpress" || app["WORDPRESS_DB_PASSWORD"] == "" ||
		app["WORDPRESS_DB_PASSWORD"] != db["MARIADB_PASSWORD"] || db["MARIADB_DATABASE"] != "wordpress" || db["MARIADB_USER"] != "wordpress" {
		t.Errorf("by its own MariaDB, WordPress reads %v and its database %v", app, db)
	}
	if !strings.Contains(e.Newest(ownProject, "wordpress").Config.Labels["dockwell.uses"], "WORDPRESS_DB_HOST=variable:DB_ADDRESS") {
		t.Errorf("WordPress doesn't use DB_ADDRESS by reference: %v", e.Newest(ownProject, "wordpress").Config.Labels)
	}

	// By the catalog's MySQL in its place.
	mysql, mysqlProject := install("blog-mysql", map[string]string{"db": "new:mysql"})
	if !made(mysql)["link db"] || made(mysql)["service phpmyadmin"] {
		t.Errorf("with a new MySQL, the plan makes %v", made(mysql))
	}
	app, db = env(mysqlProject, "wordpress"), env(mysqlProject, "db")
	if !strings.HasPrefix(e.Newest(mysqlProject, "db").Config.Image, "mysql:") {
		t.Errorf("the new database runs %s", e.Newest(mysqlProject, "db").Config.Image)
	}
	if app["WORDPRESS_DB_HOST"] != "db:3306" || db["MYSQL_DATABASE"] != "wordpress" || db["MYSQL_USER"] != "wordpress" ||
		app["WORDPRESS_DB_PASSWORD"] == "" || app["WORDPRESS_DB_PASSWORD"] != db["MYSQL_PASSWORD"] || db["MYSQL_ROOT_PASSWORD"] == "" || db["MYSQL_ROOT_PASSWORD"] == db["MYSQL_PASSWORD"] {
		t.Errorf("with a new MySQL, WordPress reads %v and its database %v", app, db)
	}
	if got := e.Newest(mysqlProject, "db").HostConfig.PortBindings; len(got) != 0 {
		t.Errorf("the new MySQL publishes %v", got)
	}
	installs, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range installs.Msg.GetInstalls() {
		p := in.GetPin().GetNeeds()["db"]
		switch in.GetProject() {
		case "blog":
			if p.GetEntry() != "" || len(p.GetFile()) != 0 {
				t.Errorf("blog's pin says %v", p)
			}
		case "blog-mysql":
			if p.GetEntry() != "mysql" || p.GetService() != "db" || len(p.GetFile()) == 0 {
				t.Errorf("blog-mysql's pin says %v", p)
			}
		}
	}

	// A person points WordPress at another database by hand, and an upgrade keeps it.
	var mysqlEnv string
	for _, r := range mysql.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" {
			mysqlEnv = r.GetResource().GetScope()
		}
	}
	apply(t, c, propose(t, c, "Point WordPress elsewhere", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: mysqlEnv, Name: "DB_ADDRESS"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "elsewhere.example.com:3306"}}},
	}))

	// An upgrade keeps each as it was met: the MySQL stays, as it ran.
	write(6, "wordpress:7.1.4-apache@sha256:"+digest2)
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "blog-mysql", SkipBackup: true}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		name := r.GetResource().GetKind() + " " + r.GetResource().GetName()
		if name != "service wordpress" && name != "project blog-mysql" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			t.Errorf("upgrading WordPress plans %v for %s", r.GetAction(), name)
		}
	}
	dbBefore := e.Newest(mysqlProject, "db")
	if got := apply(t, c, res.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the upgrade ended %v: %s", got.GetState(), got.GetError())
	}
	if got := e.Newest(mysqlProject, "db"); got == nil || got.ID != dbBefore.ID || !strings.HasPrefix(got.Config.Image, "mysql:") {
		t.Errorf("the upgrade touched the MySQL: %v", got)
	}
	if got := env(mysqlProject, "wordpress"); got["WORDPRESS_DB_PASSWORD"] != db["MYSQL_PASSWORD"] || got["WORDPRESS_DB_HOST"] != "elsewhere.example.com:3306" {
		t.Errorf("after the upgrade WordPress reads %v", got)
	}
	if notes := notesOf(res.Msg.GetChange(), catalogapi.NewNeedNote); len(notes) != 0 {
		t.Errorf("an upgrade that needs nothing new says %q", notes)
	}

	// A release that needs what the install didn't makes a database no one asked
	// for, which the plan says, and an automatic update leaves for a person.
	next := strings.Replace(wordpressWithNeeds(t, 7, "wordpress:7.1.4-apache@sha256:"+digest2), "needs:\n", `needs:
  - name: cache
    label: Cache
    accepts:
      - engine: ENGINE_MYSQL
    sets:
      - name: CACHE_HOST
        part: PART_HOST
`, 1)
	next = strings.Replace(next, "WORDPRESS_DB_HOST: ${DB_ADDRESS}", "WORDPRESS_DB_HOST: ${DB_ADDRESS}\n        WORDPRESS_CACHE_HOST: ${CACHE_HOST}", 1)
	if err := os.WriteFile(filepath.Join(dir, "wordpress.yaml"), []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "blog-mysql", SkipBackup: true}))
	if err != nil {
		t.Fatal(err)
	}
	if notes := notesOf(res.Msg.GetChange(), catalogapi.NewNeedNote); len(notes) != 1 || !strings.Contains(notes[0], "needs a cache, which it didn't before, so a new MySQL") {
		t.Errorf("a release that adds a need says %q", notes)
	}
}

// Deleting an app takes its link, with the values only it read, as the link stood for
// its need: what meets it stays, with the values it starts with, and can go after, as
// nothing reads it any more. A database can't go while an app reads it through its
// link, and the refusal names the app, not the link, which has the database's name
// (ADR 0084).
func TestALinkGoesWithItsApp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "wordpress.yaml"), []byte(wordpressWithNeeds(t, 5, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "wordpress", Project: "blog"})
	if got := apply(t, c, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	var env string
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" {
			env = r.GetResource().GetScope()
		}
	}
	deleting := func(names ...string) (*dockwellv1.Change, error) {
		var edits []*dockwellv1.Edit
		for _, name := range names {
			edits = append(edits, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: env, Name: name}, Delete: true})
		}
		res, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete " + strings.Join(names, " and "), Edits: edits}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}

	if _, err := deleting("db"); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "service db can't go while wordpress reads it through the link db") {
		t.Errorf("deleting the database WordPress reads = %v", err)
	}

	app, err := deleting("wordpress")
	if err != nil {
		t.Fatal(err)
	}
	gone := map[string]bool{}
	for _, r := range app.GetPlan().GetResources() {
		if r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_DELETE {
			gone[r.GetResource().GetKind()+" "+r.GetResource().GetName()] = true
		}
	}
	for _, want := range []string{"service wordpress", "link db", "variable DB_ADDRESS"} {
		if !gone[want] {
			t.Errorf("deleting WordPress leaves %s: %v", want, gone)
		}
	}
	// The database starts with its user's name and password, so those are its now.
	for _, kept := range []string{"secret MARIADB_PASSWORD", "variable DB_NAME", "variable DB_USER", "service db"} {
		if gone[kept] {
			t.Errorf("deleting WordPress takes %s, which its database uses: %v", kept, gone)
		}
	}
	if got := apply(t, c, app); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("deleting WordPress ended %v: %s", got.GetState(), got.GetError())
	}
	db, err := deleting("db")
	if err != nil {
		t.Fatalf("the database once WordPress is gone: %v", err)
	}
	if got := apply(t, c, db); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("deleting the database ended %v: %s", got.GetState(), got.GetError())
	}
}

// blogEntry is an app as a converted store's would be: it brings no database, and
// reads a PostgreSQL by the names its need sets.
const blogEntry = `id: blog
name: Blog
summary: Writing on the web
category: cms
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
    defaultValue: "18190"
needs:
  - name: db
    label: Database
    database: blog
    accepts:
      - engine: ENGINE_POSTGRESQL
        versions: ">=15"
    sets:
      - name: PANEL_DB_HOST
        part: PART_HOST
      - name: PANEL_DB_NAME
        part: PART_DATABASE
      - name: PANEL_DB_USER
        part: PART_USER
      - name: PANEL_DB_USER_PASSWORD
        part: PART_PASSWORD
      - name: DATABASE_URL
        part: PART_URL
compose: |
  services:
    app:
      image: ghcr.io/acme/blog:1@sha256:` + digest1 + `
      environment:
        PANEL_DB_HOST: ${PANEL_DB_HOST}
        PANEL_DB_NAME: ${PANEL_DB_NAME}
        PANEL_DB_USER: ${PANEL_DB_USER}
        PANEL_DB_USER_PASSWORD: ${PANEL_DB_USER_PASSWORD}
        DATABASE_URL: ${DATABASE_URL}
      ports:
        - "127.0.0.1:${PORT}:80"
`

// An app that brings no database shares a PostgreSQL another project runs, because a
// person chose it: one plan makes the app a database and a user of its own there, a
// secret in the app's project whose value comes from the user's, and a link network
// the database joins in place, answering to the need's name. A new PostgreSQL stays
// the default: sharing is only ever chosen.
func TestAnInstallSharesAServerYouRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(blogEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	e.Registry["postgres:17"] = pg.image()
	e.Registry["ghcr.io/acme/shop:1.4"] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	e.Registry["redis:7"] = dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}}
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	ch := changeClient(b, b.origin)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "the-administrators"}, false))
	cat := catalogClient(b)

	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	if first := got.Msg.GetNeeds()[0]; first.GetChoice() != "new:postgresql" {
		t.Errorf("the default is %v, want a new PostgreSQL", first)
	}
	for _, o := range got.Msg.GetNeeds() {
		if o.GetService() == "db" {
			choice = o.GetChoice()
			if o.GetWhy() != "" || o.GetVersion() != "17.2" {
				t.Errorf("the shared PostgreSQL reads %v", o)
			}
		}
	}
	if choice == "" {
		t.Fatalf("the PostgreSQL this install runs isn't offered: %v", got.Msg.GetNeeds())
	}

	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog", Needs: map[string]string{"db": choice}})
	made := map[string]string{}
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE {
			made[r.GetResource().GetKind()+" "+r.GetResource().GetName()] = r.GetResource().GetScope()
		}
	}
	shared := made["service db"]
	for _, want := range []string{"database.user blog", "secret BLOG_DATABASE_PASSWORD", "link db", "network db-link", "secret PANEL_DB_USER_PASSWORD", "variable PANEL_DB_HOST", "service app"} {
		if _, ok := made[want]; !ok {
			t.Errorf("sharing, the plan makes no %s: %v", want, made)
		}
	}
	if made["database.user blog"] == made["link db"] || made["database.user blog"] == "" {
		t.Errorf("the user and the link are in %s and %s, want the server's environment and the app's", made["database.user blog"], made["link db"])
	}
	_ = shared
	if got := apply(t, ch, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	pg.mu.Lock()
	verifier, owner := pg.roles["blog"], pg.databases["blog"]
	pg.mu.Unlock()
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$") || owner != "blog" {
		t.Errorf("on the shared PostgreSQL, blog is %q and owns %q", verifier, owner)
	}
	app := e.Newest(docker.Project(made["link db"]), "app")
	if app == nil {
		t.Fatalf("the blog doesn't run: %v", e.Containers())
	}
	env := map[string]string{}
	for _, kv := range app.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["PANEL_DB_HOST"] != "db" || env["PANEL_DB_NAME"] != "blog" || env["PANEL_DB_USER"] != "blog" || env["PANEL_DB_USER_PASSWORD"] == "" {
		t.Errorf("the blog reads %v", env)
	}
	// Its URL is written from its parts, the password from the shared server's secret.
	if want := "postgres://" + url.UserPassword("blog", env["PANEL_DB_USER_PASSWORD"]).String() + "@db:5432/blog"; env["DATABASE_URL"] != want {
		t.Errorf("the blog's URL is %q, want %q", env["DATABASE_URL"], want)
	}
	linkNet := docker.ScopedName(docker.Project(made["link db"]), "db-link")
	if _, ok := app.NetworkSettings.Networks[linkNet]; !ok {
		t.Errorf("the blog isn't on its link network: %v", app.NetworkSettings.Networks)
	}
	db := e.Newest(docker.Project(made["database.user blog"]), "db")
	if ep := db.NetworkSettings.Networks[linkNet]; ep == nil || !slices.Contains(ep.Aliases, "db") {
		t.Errorf("the shared PostgreSQL is on the link network as %v", db.NetworkSettings.Networks)
	}

	// A database an app shares can't go from under it.
	_, err = ch.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete the database", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: made["database.user blog"], Name: "db"}, Delete: true,
	}}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "while the app in blog through its link db shares it") {
		t.Errorf("deleting the shared database = %v", err)
	}

	// A new password for the blog's user, on the shared server, reaches the blog in the
	// same change: its password and its URL, written from the other environment.
	dbEnv, appEnv := made["database.user blog"], made["link db"]
	rotated := propose(t, ch, "A new password for the blog", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: dbEnv, Name: "BLOG_DATABASE_PASSWORD"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "a-new-password-for-the-blog"}}},
	})
	restarts := false
	for _, r := range rotated.GetPlan().GetResources() {
		restarts = restarts || r.GetResource().GetScope() == appEnv && r.GetResource().GetName() == "app" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE
	}
	if !restarts {
		t.Errorf("a new password for the blog's user leaves the blog as it is: %v", rotated.GetPlan().GetResources())
	}
	if got := apply(t, ch, rotated); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the new password ended %v: %s", got.GetState(), got.GetError())
	}
	app = e.Newest(docker.Project(appEnv), "app")
	env = map[string]string{}
	for _, kv := range app.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["PANEL_DB_USER_PASSWORD"] != "a-new-password-for-the-blog" || !strings.Contains(env["DATABASE_URL"], ":a-new-password-for-the-blog@") {
		t.Errorf("after the new password the blog reads %v", env)
	}
	pg.mu.Lock()
	again := pg.roles["blog"]
	pg.mu.Unlock()
	if again == verifier {
		t.Error("the blog's user kept its old password")
	}

	// Someone who holds the database's project alone gives the blog's user a new
	// password: the blog restarts with it, as Dockwell's own item, in a project they
	// hold no role in, which their view of the change counts and doesn't name. The
	// blog's own developer sees its restart, and nothing of the database's project.
	// Where the blog's environment is protected, applying waits for an admin (ADR
	// 0084, amending ADR 0049).
	protect := func(env string, p resourcev1.Protection) {
		t.Helper()
		apply(t, ch, propose(t, ch, "Protect an environment", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: envProject(t, f, env), Name: "production"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{Protection: p}}},
		}))
	}
	rotate := func(value string) *dockwellv1.Change {
		t.Helper()
		return propose(t, ch, "Rotate the blog's database password", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: dbEnv, Name: "BLOG_DATABASE_PASSWORD"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: value}}},
		})
	}
	scopes := func(c *dockwellv1.Change) map[string]bool {
		out := map[string]bool{}
		for _, r := range c.GetPlan().GetResources() {
			out[r.GetResource().GetScope()] = true
		}
		for _, sp := range c.GetPlan().GetServers() {
			for _, step := range sp.GetSteps() {
				out[step.GetResource().GetScope()] = true
			}
		}
		return out
	}
	protect(dbEnv, resourcev1.Protection_PROTECTION_OPEN)
	protect(appEnv, resourcev1.Protection_PROTECTION_OPEN)
	hold := aMember(t, f)
	dbDeveloper := store.Grant{ProjectID: envProject(t, f, dbEnv), Role: store.RoleDeveloper}
	// The choice lists only what the person may use: a viewer of the blog alone, or a
	// developer of it, sees nothing of the database's project, and the database's
	// developer sees the server without the name of the project that shares it.
	offered := func() []*dockwellv1.CatalogNeedOption {
		t.Helper()
		got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
		if err != nil {
			t.Fatal(err)
		}
		return slices.DeleteFunc(got.Msg.GetNeeds(), func(o *dockwellv1.CatalogNeedOption) bool { return o.GetService() != "db" })
	}
	for _, role := range []string{store.RoleViewer, store.RoleDeveloper} {
		hold(store.Grant{ProjectID: envProject(t, f, appEnv), Role: role})
		if got := offered(); len(got) != 0 {
			t.Errorf("a %s of the blog alone is offered %v", role, got)
		}
	}
	hold(dbDeveloper)
	if got := offered(); len(got) != 1 || len(got[0].GetSharedWith()) != 0 {
		t.Errorf("the database's developer is offered %v", got)
	}
	// Deleting the server is refused while the blog shares it, naming the blog only to
	// someone who holds its project.
	deleteDB := func() error {
		_, err := ch.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete db", Edits: []*dockwellv1.Edit{
			{Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: dbEnv, Name: "db"}, Delete: true},
		}}))
		return err
	}
	if err := deleteDB(); err == nil || !strings.Contains(err.Error(), "can't go while an app in projects you can't see share it") || strings.Contains(err.Error(), "blog") {
		t.Errorf("deleting the shared server, the database's developer reads %v", err)
	}
	hold(dbDeveloper, store.Grant{ProjectID: envProject(t, f, appEnv), Role: store.RoleViewer})
	if err := deleteDB(); err == nil || !strings.Contains(err.Error(), "can't go while the app in blog through its link db shares it") {
		t.Errorf("deleting the shared server, someone who sees the blog reads %v", err)
	}
	hold(dbDeveloper)
	theirs := rotate("a-password-from-the-database-side")
	if got := scopes(theirs); got[appEnv] || !got[dbEnv] {
		t.Errorf("the database's developer sees the plan in %v", got)
	}
	if e := theirs.GetElsewhere(); e.GetRestarts() != 1 || e.GetItems() < 1 || e.GetEditsElsewhere() || theirs.GetSummary() != "Rotate the blog's database password" {
		t.Errorf("the database's developer reads %q with elsewhere %v", theirs.GetSummary(), e)
	}
	if got := apply(t, ch, theirs); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || got.GetElsewhere() == nil {
		t.Fatalf("the database's developer's change ended %v: %s (elsewhere %v)", got.GetState(), got.GetError(), got.GetElsewhere())
	}
	if read := e.Newest(docker.Project(appEnv), "app"); read == nil || !slices.Contains(read.Config.Env, "PANEL_DB_USER_PASSWORD=a-password-from-the-database-side") {
		t.Errorf("the blog doesn't read the new password: %v", read)
	}

	// The blog's developer reads the change as their app's restart.
	hold(store.Grant{ProjectID: envProject(t, f, appEnv), Role: store.RoleDeveloper})
	read, err := ch.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: theirs.GetId()}))
	if err != nil {
		t.Fatalf("the blog's developer can't read the change that restarted the blog: %v", err)
	}
	seen := read.Msg.GetChange()
	if sc := scopes(seen); sc[dbEnv] || !sc[appEnv] {
		t.Errorf("the blog's developer sees the plan in %v", sc)
	}
	if !seen.GetElsewhere().GetEditsElsewhere() || seen.GetSummary() != "A change in a project you can't see" || seen.GetProposedBy() != nil || seen.GetAppliedBy() != nil {
		t.Errorf("the blog's developer reads %q by %v, elsewhere %v", seen.GetSummary(), seen.GetProposedBy(), seen.GetElsewhere())
	}
	if _, err := ch.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: theirs.GetId()})); err == nil {
		t.Error("the blog's developer undid a change to the database's project")
	}
	// A failure out of sight says nothing of what failed, even beside one in sight.
	blogDeveloper := place.WithCaller(t.Context(), place.Caller{Grants: []store.Grant{{ProjectID: envProject(t, f, appEnv), Role: store.RoleDeveloper}}})
	failed := func(scope, name string) *dockwellv1.StepProgress {
		return &dockwellv1.StepProgress{Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: scope, Name: name}, State: dockwellv1.StepState_STEP_STATE_FAILED}
	}
	for _, steps := range [][]*dockwellv1.StepProgress{{failed(dbEnv, "db")}, {failed(appEnv, "app"), failed(dbEnv, "db")}} {
		c := &dockwellv1.Change{Id: theirs.GetId(), Error: "db in data failed: the disk of hz-1 is full", Steps: steps}
		if err := f.store.View(t.Context(), func(tx *store.Tx) error { return changes.Narrow(blogDeveloper, tx, c) }); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(c.GetError(), "db in data") || !strings.Contains(c.GetError(), "in a project you can't see") {
			t.Errorf("with %d steps failed, the blog's developer reads %q", len(steps), c.GetError())
		}
	}

	// The database's developer undoes it: the blog takes the old password back.
	hold(dbDeveloper)
	undone, err := ch.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: theirs.GetId()}))
	if err != nil {
		t.Fatalf("the database's developer can't undo their change: %v", err)
	}
	if back := apply(t, ch, undone.Msg.GetChange()); back.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %s", back.GetState(), back.GetError())
	}
	if read := e.Newest(docker.Project(appEnv), "app"); read == nil || !slices.Contains(read.Config.Env, "PANEL_DB_USER_PASSWORD=a-new-password-for-the-blog") {
		t.Errorf("after the undo the blog doesn't read its old password: %v", read)
	}

	// With the blog's environment protected, the change waits for an admin.
	hold(onInstall(store.RoleAdmin))
	protect(appEnv, resourcev1.Protection_PROTECTION_PROTECTED)
	hold(dbDeveloper)
	waiting := rotate("a-password-the-blog-waits-for")
	if !strings.Contains(waiting.GetWaits(), "an admin") {
		t.Errorf("a change that restarts a protected app says it waits for %q", waiting.GetWaits())
	}
	if _, err := ch.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: waiting.GetId()})); connect.CodeOf(err) != connect.CodePermissionDenied ||
		!strings.Contains(err.Error(), "restarts an app in a protected environment") || strings.Contains(err.Error(), "blog") {
		t.Errorf("applying a change that restarts a protected app = %v", err)
	}
}

// An app that brings no database gets a new PostgreSQL from the catalog by default,
// whose image makes only its administrator: one plan makes the server, then the app's
// user and database in it, signing in as the administrator, and then the app, which
// signs in as its own user. The administrator's password is the server's alone.
func TestAnInstallMakesANewPostgreSQLWithAUserOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(blogEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	postgres, ok := c.Entry("postgres")
	image := regexp.MustCompile(`image: (postgres:\S+)`).FindStringSubmatch(postgres.GetCompose())
	if !ok || image == nil {
		t.Fatal("the catalog has no postgres")
	}
	e.Registry[image[1]] = pg.image()
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	ch := changeClient(b, b.origin)
	cat := catalogClient(b)

	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog"})
	var order []string
	scopes := map[string]string{}
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE {
			k := r.GetResource().GetKind() + " " + r.GetResource().GetName()
			order = append(order, k)
			scopes[k] = r.GetResource().GetScope()
		}
	}
	db, user, app := slices.Index(order, "service db"), slices.Index(order, "database.user blog"), slices.Index(order, "service app")
	if db < 0 || user < 0 || app < 0 || db > user || user > app {
		t.Fatalf("the plan makes %v, want the server, then its user, then the app", order)
	}
	if scopes["database.user blog"] != scopes["service app"] {
		t.Errorf("the user is in %s, the app in %s", scopes["database.user blog"], scopes["service app"])
	}
	if got := apply(t, ch, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	pg.mu.Lock()
	verifier, owner := pg.roles["blog"], pg.databases["blog"]
	pg.mu.Unlock()
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$") || owner != "blog" {
		t.Errorf("on the new PostgreSQL, blog is %q and owns %q", verifier, owner)
	}
	project := docker.Project(scopes["service app"])
	env := func(service string) map[string]string {
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
	blog, server := env("app"), env("db")
	if blog["PANEL_DB_HOST"] != "db" || blog["PANEL_DB_NAME"] != "blog" || blog["PANEL_DB_USER"] != "blog" || blog["PANEL_DB_USER_PASSWORD"] == "" {
		t.Errorf("the blog reads %v", blog)
	}
	if want := "postgres://" + url.UserPassword("blog", blog["PANEL_DB_USER_PASSWORD"]).String() + "@db:5432/blog"; blog["DATABASE_URL"] != want {
		t.Errorf("the blog's URL is %q, want %q", blog["DATABASE_URL"], want)
	}
	if server["POSTGRES_PASSWORD"] == "" || server["POSTGRES_PASSWORD"] == blog["PANEL_DB_USER_PASSWORD"] || server["POSTGRES_USER"] != "" {
		t.Errorf("the server starts with %v, the blog with %v", server, blog)
	}

	// An upgrade keeps the PostgreSQL, its user and its password as they are.
	next := strings.NewReplacer(`release: 1`, `release: 2`, `version: "1.0"`, `version: "1.1"`, "blog:1@sha256:"+digest1, "blog:1@sha256:"+digest2).Replace(blogEntry)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest2] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "blog", SkipBackup: true}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		name := r.GetResource().GetKind() + " " + r.GetResource().GetName()
		if name != "service app" && name != "project blog" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			t.Errorf("upgrading the blog plans %v for %s", r.GetAction(), name)
		}
	}
	if got := apply(t, ch, res.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the upgrade ended %v: %s", got.GetState(), got.GetError())
	}
	if got := env("app"); got["PANEL_DB_USER_PASSWORD"] != blog["PANEL_DB_USER_PASSWORD"] || got["DATABASE_URL"] != blog["DATABASE_URL"] {
		t.Error("after the upgrade the blog reads another password or URL")
	}

	// The install's pin keeps the PostgreSQL as it ran, apart from the blog's own file,
	// so another app may share it, with a user and a database of its own, named for
	// its project as the blog's has the entry's.
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range got.Msg.GetNeeds() {
		if o.GetService() == "db" && o.GetProject() == "blog" {
			if o.GetWhy() != "" {
				t.Errorf("the blog's PostgreSQL can't be shared: %s", o.GetWhy())
			}
			choice = o.GetChoice()
		}
	}
	if choice == "" {
		t.Fatalf("the blog's PostgreSQL isn't offered: %v", got.Msg.GetNeeds())
	}
	notes := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog", Project: "notes", Needs: map[string]string{"db": choice}})
	if got := apply(t, ch, notes); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("sharing the blog's PostgreSQL ended %v: %s", got.GetState(), got.GetError())
	}
	pg.mu.Lock()
	notesUser, notesOwner := pg.roles["notes"], pg.databases["notes"]
	pg.mu.Unlock()
	if notesUser == "" || notesOwner != "notes" {
		t.Errorf("on the blog's PostgreSQL, notes is %q and owns %q", notesUser, notesOwner)
	}
	var notesEnv string
	for _, r := range notes.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" {
			notesEnv = r.GetResource().GetScope()
		}
	}
	c2 := e.Newest(docker.Project(notesEnv), "app")
	if c2 == nil {
		t.Fatalf("notes doesn't run: %v", e.Containers())
	}
	read := map[string]string{}
	for _, kv := range c2.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		read[k] = v
	}
	if read["PANEL_DB_USER"] != "notes" || read["PANEL_DB_NAME"] != "notes" || !strings.HasSuffix(read["DATABASE_URL"], "@db:5432/notes") {
		t.Errorf("notes reads %v", read)
	}
}

// boardEntry is an app that keeps its queue in a Redis it doesn't bring.
const boardEntry = `id: board
name: Board
summary: Cards on a board
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
  - name: cache
    label: Cache
    accepts:
      - engine: ENGINE_REDIS
        versions: ">=6"
    sets:
      - name: REDIS_HOST
        part: PART_HOST
      - name: REDIS_PASSWORD
        part: PART_PASSWORD
compose: |
  services:
    app:
      image: ghcr.io/acme/board:1@sha256:` + digest1 + `
      environment:
        REDIS_HOST: ${REDIS_HOST}
        REDIS_PASSWORD: ${REDIS_PASSWORD}
      ports:
        - "127.0.0.1:${PORT}:80"
`

// An app that needs a Redis gets a new one from the catalog, its alone: the server's
// one password is the app's, and the Redis publishes nothing.
func TestAnInstallMakesANewRedisOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "board.yaml"), []byte(boardEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	ch := changeClient(b, b.origin)
	cat := catalogClient(b)
	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "board"})
	if got := apply(t, ch, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	var env string
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" {
			env = r.GetResource().GetScope()
		}
	}
	redis, app := e.Newest(docker.Project(env), "cache"), e.Newest(docker.Project(env), "app")
	if redis == nil || app == nil {
		t.Fatalf("the board and its Redis don't run: %v", e.Containers())
	}
	read := map[string]string{}
	for _, kv := range app.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		read[k] = v
	}
	cmd := strings.Join(redis.Config.Cmd, " ")
	if read["REDIS_HOST"] != "cache" || read["REDIS_PASSWORD"] == "" || !strings.Contains(cmd, "--requirepass "+read["REDIS_PASSWORD"]) {
		t.Errorf("the board reads %v; its Redis runs %q", read, cmd)
	}
	if len(redis.HostConfig.PortBindings) != 0 {
		t.Errorf("the Redis publishes %v", redis.HostConfig.PortBindings)
	}
}

// WordPress's own MariaDB follows WordPress's upgrades, so the plan that first shares
// it pins it apart in WordPress's install: offered with nothing in its way, and the
// plan that shares it changes WordPress's project, whose pin then keeps the database
// as it runs (ADR 0084).
func TestSharingAnInstallsOwnServerPinsItApart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "wordpress.yaml"), []byte(wordpressWithNeeds(t, 5, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	// A MariaDB whose client finds no such user or database, as a new one does.
	image := regexp.MustCompile(`image: (mariadb:\S+)`).FindStringSubmatch(wordpressWithNeeds(t, 5, ""))[1]
	e.Registry[image] = dockertest.Image{Config: docker.Config{Cmd: []string{"mariadbd"}}, Exec: func(x dockertest.Exec) int {
		_, _ = io.WriteString(x.Stdout, `{"user":false,"owner":""}`+"\n")
		return 0
	}}
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	if got := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "wordpress", Project: "blog"})); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "wordpress"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range got.Msg.GetNeeds() {
		if o.GetProject() == "blog" && o.GetService() == "db" {
			if o.GetWhy() != "" {
				t.Errorf("blog's own MariaDB can't be shared: %s", o.GetWhy())
			}
			choice = o.GetChoice()
		}
	}
	if choice == "" {
		t.Fatalf("blog's own MariaDB isn't offered: %v", got.Msg.GetNeeds())
	}
	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "wordpress", Project: "shop", Needs: map[string]string{"db": choice}})
	pins, user := false, ""
	for _, r := range planned.GetPlan().GetResources() {
		pins = pins || r.GetResource().GetKind() == "project" && r.GetResource().GetName() == "blog" && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_UPDATE
		if r.GetResource().GetKind() == "database.user" {
			user = r.GetResource().GetName()
		}
	}
	// WordPress's MariaDB has a wordpress user its image made, so shop's is shop.
	if user != "shop" {
		t.Errorf("shop's user on blog's MariaDB is %q", user)
	}
	if !pins {
		t.Errorf("sharing blog's own MariaDB leaves blog's pin as it is: %v", planned.GetPlan().GetResources())
	}
}

// Use another database: the blog's need, met by a new PostgreSQL beside it, moves to
// a PostgreSQL another project runs, in one plan at the release it's on. The new
// server makes the blog a database and user, the blog's values point at it, under a
// name of its own, as the old one keeps running beside it, data and all, which the
// plan says (ADR 0084).
func TestUseAnotherDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(blogEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	own := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}, sizes: map[string]int64{"blog": 12 << 20}}
	shared := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	postgres, _ := c.Entry("postgres")
	e.Registry[regexp.MustCompile(`image: (postgres:\S+)`).FindStringSubmatch(postgres.GetCompose())[1]] = own.image()
	e.Registry["postgres:17"] = shared.image()
	e.Registry["ghcr.io/acme/shop:1.4"] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	e.Registry["redis:7"] = dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}}
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	ch := changeClient(b, b.origin)
	cat := catalogClient(b)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "the-administrators"}, false))
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog"})
	if got := apply(t, ch, installed); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	var env string
	for _, r := range installed.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" {
			env = r.GetResource().GetScope()
		}
	}
	entry, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range entry.Msg.GetNeeds() {
		if o.GetService() == "db" && o.GetProject() != "blog" && o.GetWhy() == "" {
			choice = o.GetChoice()
		}
	}
	if choice == "" {
		t.Fatalf("no PostgreSQL to move to: %v", entry.Msg.GetNeeds())
	}
	res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "blog", Need: "db", Choice: choice}))
	if err != nil {
		t.Fatal(err)
	}
	planned := res.Msg.GetChange()
	acts := map[string]dockwellv1.PlanAction{}
	for _, r := range planned.GetPlan().GetResources() {
		where := "there"
		if r.GetResource().GetScope() == env {
			where = "here"
		}
		acts[r.GetResource().GetKind()+" "+r.GetResource().GetName()+" "+where] = r.GetAction()
	}
	if acts["service db here"] == dockwellv1.PlanAction_PLAN_ACTION_DELETE || acts["database.user blog there"] != dockwellv1.PlanAction_PLAN_ACTION_CREATE ||
		acts["network db-link here"] != dockwellv1.PlanAction_PLAN_ACTION_CREATE || acts["variable PANEL_DB_HOST here"] != dockwellv1.PlanAction_PLAN_ACTION_UPDATE {
		t.Errorf("moving the blog's database plans %v", acts)
	}
	if notes := notesOf(planned, "catalog_need_moves"); len(notes) != 1 || !strings.Contains(notes[0], "db keeps running as it is") {
		t.Errorf("the plan says %q of the old database", notes)
	}
	if notes := notesOf(planned, "catalog_need_copies"); len(notes) != 1 || !strings.Contains(notes[0], "Blog's database of 12 MB") || !strings.Contains(notes[0], "keeps serving from db until the copy is done") {
		t.Errorf("the plan says %q of the copy", notes)
	}
	if got := apply(t, ch, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("moving the database ended %v: %s", got.GetState(), got.GetError())
	}
	read := map[string]string{}
	for _, kv := range e.Newest(docker.Project(env), "app").Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		read[k] = v
	}
	if read["PANEL_DB_HOST"] != "db-link" || !strings.Contains(read["DATABASE_URL"], "@db-link:5432/blog") {
		t.Errorf("the blog reads %v", read)
	}
	if old := e.Newest(docker.Project(env), "db"); old == nil || !old.State.Running {
		t.Error("the old PostgreSQL went with the move")
	}
	shared.mu.Lock()
	owner := shared.databases["blog"]
	// Loaded as blog, the user made for it, signing in as them.
	loaded := slices.ContainsFunc(shared.said, func(said string) bool {
		cmd, _, _ := strings.Cut(said, "\n")
		return strings.Contains(cmd, `psql`) && strings.Contains(cmd, `-U "$u" -d "$d"`) && strings.Contains(cmd, " sh blog blog 5432 ") && !strings.HasSuffix(cmd, " 127.0.0.1")
	})
	shared.mu.Unlock()
	own.mu.Lock()
	dumped := slices.ContainsFunc(own.said, func(said string) bool { return strings.Contains(said, "pg_dump") && strings.Contains(said, " blog\n") })
	own.mu.Unlock()
	if owner != "blog" || !loaded || !dumped {
		t.Errorf("on the shared PostgreSQL, blog owns %q; the blog's database was dumped %v and loaded as blog %v", owner, dumped, loaded)
	}
	if _, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "blog", Need: "db", Choice: choice})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("moving it to where it is = %v", err)
	}
}

func TestUseAnotherDatabaseStartingEmptyHoldsTheRoute(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	owned := strings.Replace(blogEntry, "web:\n  service: app\n", "web:\n  service: app\nfirstVisitorOwns: true\n", 1)
	// It publishes its port on every address, as a converted 1Panel app does, and
	// runs an admin service beside it, with a domain of its own.
	owned = strings.Replace(owned, "        - \"127.0.0.1:${PORT}:80\"\n", "        - \"${PORT}:80\"\n    admin:\n      image: ghcr.io/acme/blog:1@sha256:"+digest1+"\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(owned), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	own := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	shared := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	postgres, _ := c.Entry("postgres")
	e.Registry[regexp.MustCompile(`image: (postgres:\S+)`).FindStringSubmatch(postgres.GetCompose())[1]] = own.image()
	e.Registry["postgres:17"] = shared.image()
	e.Registry["ghcr.io/acme/shop:1.4"] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	e.Registry["redis:7"] = dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}}
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	ch := changeClient(b, b.origin)
	cat := catalogClient(b)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "the-administrators"}, false))
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog"})
	if got := apply(t, ch, installed); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	var env string
	for _, r := range installed.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" {
			env = r.GetResource().GetScope()
		}
	}
	entry, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range entry.Msg.GetNeeds() {
		if o.GetService() == "db" && o.GetProject() != "blog" && o.GetWhy() == "" {
			choice = o.GetChoice()
		}
	}
	if choice == "" {
		t.Fatalf("no PostgreSQL to move to: %v", entry.Msg.GetNeeds())
	}
	applyReviewed(t, ch, propose(t, ch, "Serve the blog", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "route", Scope: env, Name: "www"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "blog.example.com", Service: "app", Port: 80}}},
	}, &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "route", Scope: env, Name: "adm"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "admin.example.com", Service: "admin", Port: 80}}},
	}, &dockwellv1.Edit{
		// One to the port the blog publishes, on the server itself, which dials 127.0.0.1.
		Resource: &dockwellv1.ResourceRef{Kind: "route", Scope: env, Name: "local"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "local.example.com", HostPort: 18190}}},
	}))
	res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "blog", Need: "db", Choice: choice, StartEmpty: true}))
	if err != nil {
		t.Fatal(err)
	}
	planned := res.Msg.GetChange()
	if notes := notesOf(planned, "catalog_need_starts_empty"); len(notes) != 1 || !strings.Contains(notes[0], "its domains answer that it isn't open, and the ports it publishes listen on this server alone") {
		t.Errorf("the plan says %q of starting empty", notes)
	}
	if notes := notesOf(planned, "catalog_need_undo"); len(notes) != 1 || !strings.Contains(notes[0], "with what Blog wrote there after the move") {
		t.Errorf("the plan says %q of undoing it", notes)
	}
	// Held before the app restarts: a stranger never reaches it empty and open.
	steps := planned.GetPlan().GetServers()[0].GetSteps()
	at := func(kind, name string) int {
		return slices.IndexFunc(steps, func(s *dockwellv1.PlanStep) bool {
			return s.GetResource().GetKind() == kind && s.GetResource().GetName() == name
		})
	}
	if www, app := at("route", "www"), at("service", "app"); www < 0 || app < 0 || www > app {
		t.Errorf("the route is held at step %d, and the app restarts at %d", www, app)
	}
	if len(notesOf(planned, "catalog_need_copies")) != 0 {
		t.Error("a move that starts empty says it copies")
	}
	applyReviewed(t, ch, planned)
	for _, name := range []string{"www", "adm", "local"} {
		if got := ing.routes[resource.Ref{Kind: "route", Scope: env, Name: name}]; !got.GetHeld() {
			t.Errorf("the ingress has the blog's route %s as %v", name, got)
		}
	}
	bound := func() string {
		t.Helper()
		app := e.Newest(docker.Project(env), "app")
		if app == nil || len(app.HostConfig.PortBindings["80/tcp"]) != 1 {
			t.Fatalf("the blog publishes %v", app)
		}
		return app.HostConfig.PortBindings["80/tcp"][0].HostIP
	}
	if ip := bound(); ip != "127.0.0.1" {
		t.Errorf("held, the blog listens on %q", ip)
	}
	var held resourcev1.Route
	err = f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), "route", env, "www")
		if err != nil {
			return err
		}
		return proto.Unmarshal(r.Spec, &held)
	})
	if err != nil || !held.GetHeld() {
		t.Errorf("the blog's route after starting empty is %v, %v", &held, err)
	}
	shared.mu.Lock()
	loaded := slices.ContainsFunc(shared.said, func(said string) bool { return strings.Contains(said, `-U "$u" -d "$d"`) })
	shared.mu.Unlock()
	if loaded {
		t.Error("a move that starts empty copied the database")
	}

	// Its owner opens it: its routes answer, and its port listens where it did.
	opened, err := cat.OpenCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.OpenCatalogInstallRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, ch, opened.Msg.GetChange())
	for _, name := range []string{"www", "adm", "local"} {
		if got := ing.routes[resource.Ref{Kind: "route", Scope: env, Name: name}]; got.GetHeld() {
			t.Errorf("opened, the ingress has the blog's route %s as %v", name, got)
		}
	}
	if ip := bound(); ip != "" {
		t.Errorf("opened, the blog listens on %q", ip)
	}
	if _, err := cat.OpenCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.OpenCatalogInstallRequest{Id: "blog"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("opening it again = %v", err)
	}
}

// Deleting an app that shares a server takes its link with it, its values and the
// network it reached the server on, which the server leaves in place; the server
// keeps running, with the app's database and user, as they're its now (ADR 0084).
func TestDeletingAnAppTakesItsLinkNetwork(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(blogEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	e.Registry["postgres:17"] = pg.image()
	e.Registry["ghcr.io/acme/shop:1.4"] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	e.Registry["redis:7"] = dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}}
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	ch := changeClient(b, b.origin)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "the-administrators"}, false))
	cat := catalogClient(b)
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range got.Msg.GetNeeds() {
		if o.GetService() == "db" {
			choice = o.GetChoice()
		}
	}
	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog", Needs: map[string]string{"db": choice}})
	if got := apply(t, ch, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	var appEnv, dbEnv string
	for _, r := range planned.GetPlan().GetResources() {
		switch r.GetResource().GetKind() {
		case "link":
			appEnv = r.GetResource().GetScope()
		case "database.user":
			dbEnv = r.GetResource().GetScope()
		}
	}
	linkNet := docker.ScopedName(docker.Project(appEnv), "db-link")
	if db := e.Newest(docker.Project(dbEnv), "db"); db.NetworkSettings.Networks[linkNet] == nil {
		t.Fatalf("the shared PostgreSQL isn't on the blog's link network: %v", db.NetworkSettings.Networks)
	}

	deleting := propose(t, ch, "Delete the blog", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: appEnv, Name: "app"}, Delete: true,
	})
	gone := map[string]bool{}
	for _, r := range deleting.GetPlan().GetResources() {
		if r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_DELETE {
			gone[r.GetResource().GetKind()+" "+r.GetResource().GetName()] = true
		}
	}
	for _, want := range []string{"service app", "link db", "network db-link", "secret PANEL_DB_USER_PASSWORD", "variable PANEL_DB_HOST"} {
		if !gone[want] {
			t.Errorf("deleting the blog leaves %s: %v", want, gone)
		}
	}
	if got := apply(t, ch, deleting); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("deleting the blog ended %v: %s", got.GetState(), got.GetError())
	}
	db := e.Newest(docker.Project(dbEnv), "db")
	if db == nil || !db.State.Running || db.NetworkSettings.Networks[linkNet] != nil {
		t.Errorf("after the blog went, the shared PostgreSQL is %v on %v", db.State, db.NetworkSettings.Networks)
	}
	pg.mu.Lock()
	_, kept := pg.roles["blog"]
	pg.mu.Unlock()
	if !kept {
		t.Error("deleting the blog dropped its user on the shared server")
	}
}
