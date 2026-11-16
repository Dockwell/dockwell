// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// blogStand is blogEntry installed with a new PostgreSQL, ready for a release of it.
type blogStand struct {
	t     *testing.T
	dir   string
	pg    *pgState
	e     *dockertest.Engine
	ch    dockwellv1connect.ChangeServiceClient
	cat   dockwellv1connect.CatalogServiceClient
	scope string
}

func startBlog(t *testing.T) *blogStand {
	t.Helper()
	s := &blogStand{t: t, dir: t.TempDir()}
	useCatalogDir(t, s.dir)
	if err := os.WriteFile(filepath.Join(s.dir, "blog.yaml"), []byte(blogEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	s.e = f.addDockerServer(t)
	s.pg = &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	postgres, _ := c.Entry("postgres")
	s.e.Registry[regexp.MustCompile(`image: (postgres:\S+)`).FindStringSubmatch(postgres.GetCompose())[1]] = s.pg.image()
	s.e.Registry["ghcr.io/acme/blog:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	s.e.Registry["ghcr.io/acme/blog:1@sha256:"+digest2] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	s.ch = changeClient(b, b.origin)
	s.cat = catalogClient(b)
	planned := installApp(t, s.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog"})
	if got := apply(t, s.ch, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" && r.GetResource().GetName() == "app" {
			s.scope = r.GetResource().GetScope()
		}
	}
	return s
}

// env is what the blog's app reads.
func (s *blogStand) env() map[string]string {
	out := map[string]string{}
	for _, kv := range s.e.Newest(docker.Project(s.scope), "app").Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// upgrade releases blogEntry again, at release 2 with the replacements given, and
// plans its upgrade.
func (s *blogStand) upgrade(replace ...string) *dockwellv1.Change {
	s.t.Helper()
	next := strings.NewReplacer(append([]string{`release: 1`, `release: 2`, `version: "1.0"`, `version: "1.1"`, "blog:1@sha256:" + digest1, "blog:1@sha256:" + digest2}, replace...)...).Replace(blogEntry)
	if err := os.WriteFile(filepath.Join(s.dir, "blog.yaml"), []byte(next), 0o600); err != nil {
		s.t.Fatal(err)
	}
	res, err := s.cat.UpgradeCatalogInstall(s.t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "blog", SkipBackup: true}))
	if err != nil {
		s.t.Fatal(err)
	}
	return res.Msg.GetChange()
}

func (s *blogStand) applied(c *dockwellv1.Change) {
	s.t.Helper()
	if got := apply(s.t, s.ch, c); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		s.t.Fatalf("%s ended %v: %s", c.GetSummary(), got.GetState(), got.GetError())
	}
}

// A release that changes a need's URL template writes the new URL, with no note
// that the person set it (#570's check 2, H1).
func TestAChangedNeedURLTemplateIsWritten(t *testing.T) {
	t.Parallel()
	s := startBlog(t)
	up := s.upgrade("        part: PART_URL\n", "        part: PART_URL\n        template: postgres://{user}:{password}@{host}:{port}/{database}?sslmode=disable\n")
	if n := noteOf(up, "upgrade_kept_own"); n != nil {
		t.Errorf("the plan says the person set a URL they never touched: %s", n.GetMessage())
	}
	s.applied(up)
	if got := s.env()["DATABASE_URL"]; !strings.HasSuffix(got, "/blog?sslmode=disable") {
		t.Errorf("the blog's URL is %q, want the release's new template", got)
	}
}

// A need's URL a person pointed at their own database with a template, as Connect a
// database writes one, stays through an upgrade that changes the release's, and the
// plan says so (#570's recheck, N2, which held for a value with a secret inside
// alone).
func TestAPersonsOwnNeedURLTemplateStaysThroughAnUpgrade(t *testing.T) {
	t.Parallel()
	s := startBlog(t)
	password := s.env()["PANEL_DB_USER_PASSWORD"]
	s.applied(propose(t, s.ch, "Use my own database", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: s.scope, Name: "DATABASE_URL"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Template{Template: &resourcev1.ValueTemplate{
			Text: "postgres://me:${PANEL_DB_USER_PASSWORD}@elsewhere:5432/mine", Names: []*resourcev1.TemplateName{{Name: "PANEL_DB_USER_PASSWORD", Secret: true, Encoding: resourcev1.Encoding_ENCODING_USERINFO}},
		}}}}}},
	}))
	own := "postgres://me:" + url.PathEscape(password) + "@elsewhere:5432/mine"
	if got := s.env()["DATABASE_URL"]; got != own {
		t.Fatalf("after the edit the blog reads %q, want %q", got, own)
	}
	up := s.upgrade("        part: PART_URL\n", "        part: PART_URL\n        template: postgres://{user}:{password}@{host}:{port}/{database}?sslmode=disable\n")
	if n := noteOf(up, "upgrade_kept_own"); n == nil || !strings.Contains(n.GetMessage(), "DATABASE_URL holds a value you set") {
		t.Errorf("the plan's note on the person's URL: %v", n)
	}
	s.applied(up)
	if got := s.env()["DATABASE_URL"]; got != own {
		t.Errorf("after the upgrade the blog reads %q, want the person's %q", got, own)
	}
}

// role is the blog's database user's password, as the database holds it.
func (s *blogStand) role() string {
	s.pg.mu.Lock()
	defer s.pg.mu.Unlock()
	return s.pg.roles["blog"]
}

// renamed releases blogEntry again with the need's password renamed, as a store
// renaming its variable has the converter do, and plans the upgrade.
func (s *blogStand) renamed() *dockwellv1.Change {
	s.t.Helper()
	return s.upgrade("PANEL_DB_USER_PASSWORD", "BLOG_DB_PASSWORD")
}

// A release that renames a need's password keeps its value under the new name, so
// the database user's password stays as it is, and Dockwell's URL reads the new name
// with the same password: no person's URL is left with one the database no longer
// has (#570's check 4, F1). The plan says so.
func TestARenamedNeedPasswordKeepsItsValue(t *testing.T) {
	t.Parallel()
	s := startBlog(t)
	old := s.env()["PANEL_DB_USER_PASSWORD"]
	up := s.renamed()
	if n := noteOf(up, "upgrade_kept_own"); n != nil {
		t.Errorf("the plan keeps Dockwell's own URL: %s", n.GetMessage())
	}
	if n := noteOf(up, "password_renamed"); n == nil || n.GetMessage() != "BLOG_DB_PASSWORD, this release's name for the database password, takes the value in PANEL_DB_USER_PASSWORD, so the password stays as it is." {
		t.Errorf("the plan's note on the rename: %v", n.GetMessage())
	}
	s.applied(up)
	env := s.env()
	if env["BLOG_DB_PASSWORD"] != old {
		t.Error("the renamed password took a new value")
	}
	if !scramFits(t, s.role(), old) {
		t.Error("the database user's password changed")
	}
	if want := "postgres://" + url.UserPassword("blog", old).String() + "@db:5432/blog"; env["DATABASE_URL"] != want {
		t.Errorf("the blog's URL is %q, want %q", env["DATABASE_URL"], want)
	}
}

// A person's template on the need's URL, kept through a rename, stays exactly as
// they wrote it and still signs in, whether it points at the need's database or at
// a server of their own: the old name still holds the same password (#570's check 4,
// F1; check 3's rewrite of such a template sent the new password elsewhere).
func TestAPersonsTemplateStaysAsWrittenThroughARename(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"the need's database": "postgres://blog:${PANEL_DB_USER_PASSWORD}@db:5432/blog?sslmode=disable",
		"their own server":    "postgres://me:${PANEL_DB_USER_PASSWORD}@elsewhere:5432/mine",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := startBlog(t)
			tmpl := &resourcev1.ValueTemplate{Text: text, Names: []*resourcev1.TemplateName{{Name: "PANEL_DB_USER_PASSWORD", Secret: true, Encoding: resourcev1.Encoding_ENCODING_USERINFO}}}
			s.applied(propose(t, s.ch, "My own URL", &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: s.scope, Name: "DATABASE_URL"},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Template{Template: tmpl}}}}},
			}))
			before, old := s.env()["DATABASE_URL"], s.env()["PANEL_DB_USER_PASSWORD"]
			up := s.renamed()
			if n := noteOf(up, "upgrade_kept_own"); n == nil || n.GetMessage() != "DATABASE_URL holds a value you set, so this keeps it, where the release writes it from its own template." {
				t.Errorf("the plan's note on the person's URL: %v", n.GetMessage())
			}
			s.applied(up)
			if got := s.env()["DATABASE_URL"]; got != before {
				t.Errorf("the blog's URL is %q, want it as it was, %q", got, before)
			}
			if !scramFits(t, s.role(), old) || s.env()["BLOG_DB_PASSWORD"] != old {
				t.Error("the database password changed")
			}
		})
	}
}

// A value of the person's own on the need's URL, such as one to a server of theirs,
// stays through a rename, with the note, and the upgrade isn't refused (#570's check
// 4, F2; check 3 refused it, telling them to do what the panel can't).
func TestAPersonsPlainNeedURLStaysThroughARename(t *testing.T) {
	t.Parallel()
	s := startBlog(t)
	const own = "postgres://me:theirs@elsewhere.example.com:5432/mine"
	s.applied(propose(t, s.ch, "Use my own database", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: s.scope, Name: "DATABASE_URL"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: own}}},
	}))
	up := s.renamed()
	if n := noteOf(up, "upgrade_kept_own"); n == nil {
		t.Error("the plan says nothing of the person's URL")
	}
	s.applied(up)
	if got := s.env()["DATABASE_URL"]; got != own {
		t.Errorf("the blog's URL is %q, want the person's %q", got, own)
	}
}

// On a PostgreSQL the project runs already, a release that renames the need's
// password has the new name read the server's password, as the old one did, so the
// app's URL still signs in there. On main it took a new value the server never had
// (#570's check 4).
func TestASharedDatabasesPasswordStaysTheServersThroughARename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
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
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest2] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	ch := changeClient(b, b.origin)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	apply(t, ch, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "the-administrators"}, false))
	s := &blogStand{t: t, dir: dir, pg: pg, e: e, ch: ch, cat: catalogClient(b)}
	got, err := s.cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range got.Msg.GetNeeds() {
		if o.GetService() == "db" {
			choice = o.GetChoice()
		}
	}
	planned := installApp(t, s.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog", Needs: map[string]string{"db": choice}})
	var serverScope string
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "secret" && r.GetResource().GetName() == "BLOG_DATABASE_PASSWORD" {
			serverScope = r.GetResource().GetScope()
		}
		if r.GetResource().GetKind() == "service" && r.GetResource().GetName() == "app" {
			s.scope = r.GetResource().GetScope()
		}
	}
	s.applied(planned)
	server := func() string {
		var v string
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResource(t.Context(), "secret", serverScope, "BLOG_DATABASE_PASSWORD")
			if err != nil {
				return err
			}
			d, err := f.keeper.Decode(t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			v = d.(*resourcev1.Secret).GetValue()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if want := "postgres://" + url.UserPassword("blog", server()).String() + "@"; !strings.HasPrefix(s.env()["DATABASE_URL"], want) {
		t.Fatalf("after the install the blog's URL doesn't hold the server's password")
	}
	up := s.renamed()
	if n := noteOf(up, "password_renamed"); n == nil {
		t.Error("the plan says nothing of the rename")
	}
	s.applied(up)
	env := s.env()
	if env["BLOG_DB_PASSWORD"] != server() {
		t.Error("the renamed password isn't the server's")
	}
	if want := "postgres://" + url.UserPassword("blog", server()).String() + "@"; !strings.HasPrefix(env["DATABASE_URL"], want) {
		t.Errorf("after the rename the blog's URL doesn't hold the server's password")
	}
}
