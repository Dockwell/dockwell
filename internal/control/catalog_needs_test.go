// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/docker"
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
