// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// An app from no catalog entry, in a project of its own, connects to the PostgreSQL
// another project runs, keeping the database and user it signs in as now: one plan
// takes the user on, with the password the app has, makes no role and changes none,
// links the app, sets each name it reads from the link, and reaches the server on a
// network of the app's own. Dockwell then never sets that user's password. A new
// database beside an app in its own environment is made with a user of its own. What
// can't be connected is refused before anything is planned (ADR 0084, Connect a
// database).
func TestAnAppFromNoEntryConnectsToADatabase(t *testing.T) {
	t.Parallel()
	logged := &syncBuffer{}
	f := startFullLogging(t, store.SQLite, nil, slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &pgState{
		roles:     map[string]string{"postgres": "", "blog": "made-by-hand"},
		databases: map[string]string{"postgres": "postgres", "blog": "blog"},
		sizes:     map[string]int64{"postgres": 8 << 20, "blog": 1 << 20},
		passwords: map[string]string{"blog": "the-blog-password"},
	}
	e.Registry["postgres:17"] = pg.image()
	var redis []string
	e.Registry["redis:7"] = readableRedis(&redis)
	e.Registry["ghcr.io/acme/shop:1.4"] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	ch := changeClient(b, b.origin)
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "first-password"}, false))
	dc := databaseClient(b, b.origin)

	// The blog, imported with the database and user it made by hand on data's server.
	apply(t, ch, propose(t, ch, "Add the tools project", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "tools"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Tools"}}},
	}))
	var dbEnv, toolsEnv string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		org, err := tx.Organization(t.Context())
		if err != nil {
			return err
		}
		p, err := tx.GetResource(t.Context(), "project", org.ID, "tools")
		if err != nil {
			return err
		}
		env, err := tx.GetResource(t.Context(), "environment", p.ID, "production")
		toolsEnv = env.ID
		if err != nil {
			return err
		}
		services, err := tx.ListKind(t.Context(), "service")
		for _, s := range services {
			if s.Name == "db" {
				dbEnv = s.Scope
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	protectEnv(t, ch, f, toolsEnv, resourcev1.Protection_PROTECTION_OPEN)
	apply(t, ch, propose(t, ch, "Run the blog",
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: toolsEnv, Name: "BLOG_PASSWORD"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "the-blog-password"}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: toolsEnv, Name: "BLOG_OLD_PASSWORD"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "not-the-blog-password"}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: toolsEnv, Name: "BLOG_URL"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "postgres://blog:the-blog-password@old-host:5432/blog"}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: toolsEnv, Name: "BLOG_HOST"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "old-host"}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: toolsEnv, Name: "blog"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/shop:1.4"}},
				Variables: map[string]string{"DB_HOST": "BLOG_HOST"},
				Secrets:   map[string]string{"DB_PASSWORD": "BLOG_PASSWORD", "DATABASE_URL": "BLOG_URL"},
			}}},
		},
	))
	sets := map[string]resourcev1.LinkPart{
		"BLOG_HOST": resourcev1.LinkPart_LINK_PART_HOST, "BLOG_PASSWORD": resourcev1.LinkPart_LINK_PART_PASSWORD, "BLOG_URL": resourcev1.LinkPart_LINK_PART_URL,
	}
	ask := func(m *dockwellv1.ConnectDatabaseRequest) (*dockwellv1.Change, error) {
		t.Helper()
		res, err := dc.ConnectDatabase(t.Context(), connect.NewRequest(m))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}

	// What can't be connected says why, and plans nothing.
	for _, tc := range []struct {
		name, reason, says string
		m                  *dockwellv1.ConnectDatabaseRequest
	}{
		{
			"the administrator", "invalid_user", "is db's administrator",
			&dockwellv1.ConnectDatabaseRequest{Id: "db", App: "blog", AppProject: "tools", Database: "blog", User: "postgres", PasswordSecret: "BLOG_PASSWORD", Sets: sets},
		},
		{
			"a name the app hasn't", "invalid_sets", "has no variable or secret BLOG_PORT",
			&dockwellv1.ConnectDatabaseRequest{Id: "db", App: "blog", AppProject: "tools", Database: "blog", User: "blog", PasswordSecret: "BLOG_PASSWORD", Sets: map[string]resourcev1.LinkPart{"BLOG_PORT": resourcev1.LinkPart_LINK_PART_PORT, "BLOG_PASSWORD": resourcev1.LinkPart_LINK_PART_PASSWORD}},
		},
		{
			"a password in a variable", "invalid_sets", "BLOG_HOST is a variable",
			&dockwellv1.ConnectDatabaseRequest{Id: "db", App: "blog", AppProject: "tools", Database: "blog", User: "blog", PasswordSecret: "BLOG_PASSWORD", Sets: map[string]resourcev1.LinkPart{"BLOG_HOST": resourcev1.LinkPart_LINK_PART_PASSWORD}},
		},
		{
			"a user it has without its password", "invalid_password_secret", "which of the app's secrets holds blog's password",
			&dockwellv1.ConnectDatabaseRequest{Id: "db", App: "blog", AppProject: "tools", Database: "blog", User: "blog", Sets: sets},
		},
		{
			"an app that isn't there", "invalid_app", "has no service wiki",
			&dockwellv1.ConnectDatabaseRequest{Id: "db", App: "wiki", AppProject: "tools", Sets: sets},
		},
		{
			"a cache", "", "an app connects to PostgreSQL, MySQL or MariaDB for now",
			&dockwellv1.ConnectDatabaseRequest{Id: "cache", App: "blog", AppProject: "tools", Sets: sets},
		},
	} {
		_, err := ask(tc.m)
		if err == nil || !strings.Contains(err.Error(), tc.says) || tc.reason != "" && reasonOf(err) != tc.reason {
			t.Errorf("connecting %s: %v", tc.name, err)
		}
	}

	// Signing in is checked as it's chosen, from another host, with the app's secret,
	// and nothing it answers holds the password.
	check := func(user, db, secret string) *dockwellv1.CheckDatabaseSignInResponse {
		t.Helper()
		res, err := dc.CheckDatabaseSignIn(t.Context(), connect.NewRequest(&dockwellv1.CheckDatabaseSignInRequest{
			Id: "db", User: user, Database: db, AppProject: "tools", PasswordSecret: secret,
		}))
		if err != nil {
			t.Fatalf("checking %s with %s: %v", user, secret, err)
		}
		return res.Msg
	}
	if got := check("blog", "blog", "BLOG_PASSWORD"); !got.GetSignsIn() || got.GetAdministrator() {
		t.Errorf("blog with its password checks as %v", got)
	}
	if got := check("blog", "blog", "BLOG_OLD_PASSWORD"); got.GetSignsIn() || strings.Contains(got.GetSaid(), "not-the-blog-password") || got.GetSaid() == "" {
		t.Errorf("blog with another password checks as %v", got)
	}
	if got := check("postgres", "postgres", "BLOG_PASSWORD"); !got.GetAdministrator() {
		t.Errorf("the administrator checks as %v", got)
	}
	if _, err := dc.CheckDatabaseSignIn(t.Context(), connect.NewRequest(&dockwellv1.CheckDatabaseSignInRequest{
		Id: "db", User: "blog", Database: "blog", AppProject: "tools", PasswordSecret: "BLOG_HOST",
	})); reasonOf(err) != "invalid_password_secret" {
		t.Errorf("checking with a variable: %v", err)
	}
	if text := logged.String(); strings.Contains(text, "the-blog-password") || strings.Contains(text, "not-the-blog-password") {
		t.Error("the control plane's log holds a password a check used")
	}

	// Taken on, across projects.
	pg.mu.Lock()
	before := len(pg.said)
	pg.mu.Unlock()
	change, err := ask(&dockwellv1.ConnectDatabaseRequest{Id: "db", App: "blog", AppProject: "tools", Database: "blog", User: "blog", PasswordSecret: "BLOG_PASSWORD", Sets: sets})
	if err != nil {
		t.Fatal(err)
	}
	var planned []string
	for _, r := range change.GetPlan().GetResources() {
		where := "tools"
		if r.GetResource().GetScope() == dbEnv {
			where = "data"
		}
		planned = append(planned, where+" "+r.GetResource().GetKind()+" "+r.GetResource().GetName()+" "+strings.TrimPrefix(r.GetAction().String(), "PLAN_ACTION_"))
	}
	slices.Sort(planned)
	want := []string{
		"data database.user blog CREATE", "data secret BLOG_DATABASE_PASSWORD CREATE",
		"tools link db CREATE", "tools network db-link CREATE", "tools secret BLOG_PASSWORD UPDATE", "tools secret BLOG_URL UPDATE",
		"tools service blog REPLACE", "tools variable BLOG_HOST UPDATE",
		// The parts the URL holds that the app reads no name of, as an install's are.
		"tools variable DB_DATABASE CREATE", "tools variable DB_PORT CREATE", "tools variable DB_USER CREATE",
	}
	if !slices.Equal(planned, want) {
		t.Fatalf("connecting the blog plans\n%s\nwant\n%s", strings.Join(planned, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(change.GetSummary(), "taking on blog on blog") {
		t.Errorf("the change says %q", change.GetSummary())
	}
	if done := apply(t, ch, change); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("connecting the blog ended %v: %s", done.GetState(), done.GetError())
	}
	pg.mu.Lock()
	for _, sql := range pg.said[before:] {
		if strings.Contains(sql, "CREATE ROLE") || strings.Contains(sql, "ALTER ROLE") || strings.Contains(sql, "CREATE DATABASE") || strings.Contains(sql, "DROP ") {
			t.Errorf("taking the blog's user on ran %s", sql)
		}
	}
	kept := pg.roles["blog"]
	pg.mu.Unlock()
	if kept != "made-by-hand" {
		t.Errorf("the blog's role is %q after it was taken on", kept)
	}
	blog := e.Newest(docker.Project(toolsEnv), "blog")
	if blog == nil {
		t.Fatalf("the blog doesn't run: %v", e.Containers())
	}
	env := map[string]string{}
	for _, kv := range blog.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["DB_HOST"] != "db" || env["DB_PASSWORD"] != "the-blog-password" || env["DATABASE_URL"] != "postgres://blog:the-blog-password@db:5432/blog" {
		t.Errorf("the blog reads %v", env)
	}
	if !slices.ContainsFunc(networksOf(blog), func(n string) bool { return strings.HasSuffix(n, "db-link") }) {
		t.Errorf("the blog isn't on its link network: %v", networksOf(blog))
	}

	// Its password is the person's: Dockwell doesn't change it.
	_, err = dc.ChangeDatabasePassword(t.Context(), connect.NewRequest(&dockwellv1.ChangeDatabasePasswordRequest{Id: "db", User: "blog"}))
	if err == nil || !strings.Contains(err.Error(), "taken on") {
		t.Errorf("changing a taken-on user's password: %v", err)
	}
	// And a second app can't read the same user through a link of its own.
	if _, err := ask(&dockwellv1.ConnectDatabaseRequest{Id: "db", App: "blog", AppProject: "tools", Link: "again", Database: "blog", User: "blog", PasswordSecret: "BLOG_PASSWORD", Sets: sets}); err == nil {
		t.Error("a second link took the same user on")
	}

	// A new database beside an app, in its own environment, with a user of its own.
	defaultEnv := dbEnv
	apply(t, ch, propose(t, ch, "Give the shop its names",
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: defaultEnv, Name: "SHOP_DB_PASSWORD"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "to-be-replaced"}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: defaultEnv, Name: "SHOP_DB_NAME"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "x"}}},
		},
	))
	fresh, err := ask(&dockwellv1.ConnectDatabaseRequest{Id: "db", App: "web", Sets: map[string]resourcev1.LinkPart{
		"SHOP_DB_PASSWORD": resourcev1.LinkPart_LINK_PART_PASSWORD, "SHOP_DB_NAME": resourcev1.LinkPart_LINK_PART_DATABASE,
	}})
	if err != nil {
		t.Fatal(err)
	}
	planned = nil
	for _, r := range fresh.GetPlan().GetResources() {
		planned = append(planned, r.GetResource().GetKind()+" "+r.GetResource().GetName()+" "+strings.TrimPrefix(r.GetAction().String(), "PLAN_ACTION_"))
	}
	slices.Sort(planned)
	if want := []string{"database.user web CREATE", "link db CREATE", "secret SHOP_DB_PASSWORD UPDATE", "variable SHOP_DB_NAME UPDATE"}; !slices.Equal(planned, want) {
		t.Fatalf("connecting the shop plans %v, want %v", planned, want)
	}
	if done := apply(t, ch, fresh); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("connecting the shop ended %v: %s", done.GetState(), done.GetError())
	}
	pg.mu.Lock()
	owner, made := pg.databases["web"], pg.roles["web"]
	pg.mu.Unlock()
	if owner != "web" || !strings.HasPrefix(made, "SCRAM-SHA-256$") {
		t.Errorf("the shop's database is %q's, its user %q", owner, made)
	}

	// A database kept for another app isn't taken on through a user made by hand: its
	// dumps and restores go by it, as its owner (ADR 0084).
	pg.mu.Lock()
	pg.roles["helper"], pg.sizes["web"] = "made-by-hand", 1<<20
	pg.passwords["helper"] = "the-helpers-password"
	pg.mu.Unlock()
	if _, err := ask(&dockwellv1.ConnectDatabaseRequest{Id: "db", App: "blog", AppProject: "tools", Link: "shop", Database: "web", User: "helper", PasswordSecret: "BLOG_OLD_PASSWORD", Sets: map[string]resourcev1.LinkPart{
		"BLOG_OLD_PASSWORD": resourcev1.LinkPart_LINK_PART_PASSWORD,
	}}); reasonOf(err) != "invalid_database" || !strings.Contains(fmt.Sprint(err), "kept for another app") {
		t.Errorf("taking on a database kept for another app: %v", err)
	}

	// One person's checks that reach an agent are held to 20 a minute, as more is a
	// script trying passwords; an answer kept for 30 seconds doesn't count.
	limited := -1
	for i := range 25 {
		_, err := dc.CheckDatabaseSignIn(t.Context(), connect.NewRequest(&dockwellv1.CheckDatabaseSignInRequest{
			Id: "db", User: fmt.Sprintf("probe%d", i), Database: "blog", AppProject: "tools", PasswordSecret: "BLOG_OLD_PASSWORD",
		}))
		if reasonOf(err) == "rate_limited" {
			limited = i
			break
		}
		if err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	// Two checks above reached the agent already.
	if limited != 18 {
		t.Errorf("checks were held back from the %dth, want the 19th of these", limited+1)
	}
}

// networksOf are the networks a container is on, by name.
func networksOf(c *docker.Container) []string {
	var out []string
	for name := range c.NetworkSettings.Networks {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
