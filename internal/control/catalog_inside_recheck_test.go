// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/autoupdate"
	"github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// serviceEnv is what a service of the install reads, by name; nil where it doesn't run.
func serviceEnv(t *testing.T, u *upgrading, service string) map[string]string {
	t.Helper()
	c := u.e.Newest(docker.Project(u.env), service)
	if c == nil {
		return nil
	}
	out := map[string]string{}
	for _, kv := range c.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// withAPI is insideEntry with a service before web whose own URL, with the password
// inside, would take the name URL first in an install, so web's would be WEB_URL.
func withAPI(release int, version, image string) string {
	return strings.Replace(insideEntry(release, version, image, ""), "  services:\n    web:",
		"  services:\n    api:\n      image: "+image+"\n      environment:\n        URL: redis://:${PASSWORD}@cache:6379/0\n    web:", 1)
}

// ownURL is a value of the person's own, as setOwn sets it.
const ownURL = "postgres://me:own@elsewhere:5432/mine"

// setOwn has the install's secret name hold a value of the person's own, ownURL.
func setOwn(t *testing.T, u *upgrading, name string) {
	value := ownURL
	t.Helper()
	u.applied(propose(t, u.c, "Use my own database", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: u.env, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: value}}},
	}))
}

// A release that adds a service before web, whose own value with the password inside
// would take web's name, leaves web's value on the secret it has, and gives the new
// one a name of its own (#570's recheck, N1).
func TestAValueKeepsItsSecretWhereAReleaseAddsAServiceBefore(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	password := serviceEnv(t, u, "web")["PASSWORD"]
	u.release(withAPI(2, "2.0", secondRelease))
	up := u.upgrade()
	if planHas(up, "secret", "WEB_URL") {
		t.Error("the upgrade moves web's URL to a secret WEB_URL")
	}
	u.applied(up)
	web, api := serviceEnv(t, u, "web"), serviceEnv(t, u, "api")
	if want := "postgres://u:" + password + "@db:5432/x?port=18100"; web["URL"] != want {
		t.Errorf("web reads URL %q, want %q", web["URL"], want)
	}
	if want := "redis://:" + password + "@cache:6379/0"; api["URL"] != want {
		t.Errorf("api reads URL %q, want %q", api["URL"], want)
	}
}

// The same where the person set web's URL to their own: it stays with web, as the
// note says, and never reaches api (#570's recheck, N1).
func TestAPersonsOwnURLStaysWithItsServiceWhereAReleaseAddsOneBefore(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	const own = "postgres://me:own@elsewhere:5432/mine"
	setOwn(t, u, "URL")
	password := serviceEnv(t, u, "web")["PASSWORD"]
	u.release(withAPI(2, "2.0", secondRelease))
	up := u.upgrade()
	if n := noteOf(up, "upgrade_kept_own"); n == nil || !strings.Contains(n.GetMessage(), "URL holds a value you set") {
		t.Errorf("the plan's note on the person's URL: %v", n)
	}
	u.applied(up)
	web, api := serviceEnv(t, u, "web"), serviceEnv(t, u, "api")
	if web["URL"] != own {
		t.Errorf("web reads URL %q, want the person's %q", web["URL"], own)
	}
	if want := "redis://:" + password + "@cache:6379/0"; api["URL"] != want {
		t.Errorf("api reads URL %q, want %q", api["URL"], want)
	}
}

// The mirror: a release that drops the service before web leaves web's value, which
// an install named WEB_URL, on WEB_URL, with the person's value in it (#570's
// recheck, N1).
func TestAPersonsOwnURLStaysWithItsServiceWhereAReleaseDropsOneBefore(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, withAPI(1, "1.0", firstRelease))
	if _, ok := serviceEnv(t, u, "web")["URL"]; !ok {
		t.Fatal("web reads no URL")
	}
	const own = "postgres://me:own@elsewhere:5432/mine"
	setOwn(t, u, "WEB_URL")
	if got := serviceEnv(t, u, "web")["URL"]; got != own {
		t.Fatalf("after the edit web reads URL %q", got)
	}
	u.release(insideEntry(2, "2.0", secondRelease, ""))
	u.applied(u.upgrade())
	if got := serviceEnv(t, u, "web")["URL"]; got != own {
		t.Errorf("web reads URL %q, want the person's %q", got, own)
	}
}

// An automatic update of the release that adds a service before web, which no one
// reads the plan of, leaves the person's URL with web too (#570's recheck, N1).
func TestAnAutomaticUpdateLeavesAPersonsOwnURLWithItsService(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	const own = "postgres://me:own@elsewhere:5432/mine"
	setOwn(t, u, "URL")
	password := serviceEnv(t, u, "web")["PASSWORD"]
	u.release(withAPI(2, "1.1", secondRelease))
	builtInHeldSince(t, u.f.store, "thing", 2, longAgo)
	s := dockwellv1connect.NewServiceServiceClient(u.b, u.b.origin, connect.WithProtoJSON())
	list, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var web *dockwellv1.ServiceInfo
	for _, svc := range list.Msg.GetServices() {
		if svc.GetResource().GetName() == "web" {
			web = svc
		}
	}
	b := &bench{f: u.f, b: u.b, e: u.e, c: u.c, s: s}
	b.setAutoUpdate(t, web, &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_MINOR, At: "03:00", TimeZone: "UTC"})
	r := &autoupdate.Runner{
		DB: u.f.store, Keeper: u.f.keeper, Planner: u.f.server.autoupdates.Planner, Engine: u.f.engine,
		Updates: &newer{found: map[string]services.Newer{web.GetId(): {Image: secondRelease, Kind: "minor", Catalog: true, Found: longAgo}}},
		Logger:  slog.New(slog.DiscardHandler), Now: func() time.Time { return time.Date(2026, 11, 14, 3, 5, 0, 0, time.UTC) },
	}
	if err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := u.web().Config.Image; got != secondRelease {
		t.Fatalf("the automatic update didn't run: web runs %s", got)
	}
	web2, api := serviceEnv(t, u, "web"), serviceEnv(t, u, "api")
	if web2["URL"] != own {
		t.Errorf("web reads URL %q, want the person's %q", web2["URL"], own)
	}
	if want := "redis://:" + password + "@cache:6379/0"; api["URL"] != want {
		t.Errorf("api reads URL %q, want %q", api["URL"], want)
	}
}

// A release that takes web's URL as an input of the same name keeps the secret,
// rather than delete what it reads, and the upgrade applies (#570's recheck, N5).
func TestAURLSecretTheReleaseTakesAsAnInputStays(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	before := serviceEnv(t, u, "web")["URL"]
	next := strings.Replace(testEntry(2, "2.0", secondRelease, ""), "PASSWORD: ${PASSWORD}", "PASSWORD: ${PASSWORD}\n        URL: ${URL}", 1)
	next = strings.Replace(next, "    generate: 32\ncompose:", "    generate: 32\n  - name: URL\n    label: Database URL\n    type: INPUT_TYPE_SECRET\n    generate: 32\ncompose:", 1)
	u.release(next)
	up := u.upgrade()
	for _, r := range up.GetPlan().GetResources() {
		if r.GetResource().GetName() == "URL" && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_DELETE {
			t.Error("the upgrade deletes the secret URL, which the new release's input names")
		}
	}
	if got := apply(t, u.c, up); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the upgrade ended %v: %s", got.GetState(), got.GetError())
	}
	if got := serviceEnv(t, u, "web")["URL"]; got != before {
		t.Errorf("web reads URL %q, want %q, as it held", got, before)
	}
}

// A release that drops web's URL keeps the secret where a template of the person's
// reads it, rather than hold the upgrade up on a deletion (#570's recheck, N5).
func TestARetiredURLSecretAPersonsTemplateReadsStays(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	u.applied(propose(t, u.c, "Read the URL", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: u.env, Name: "MINE"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Template{Template: &resourcev1.ValueTemplate{
			Text: "${URL}&app=mine", Names: []*resourcev1.TemplateName{{Name: "URL", Secret: true}},
		}}}}}},
	}))
	u.release(testEntry(2, "2.0", secondRelease, ""))
	up := u.upgrade()
	if got := apply(t, u.c, up); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the upgrade ended %v: %s", got.GetState(), got.GetError())
	}
	if _, ok := serviceEnv(t, u, "web")["URL"]; ok {
		t.Error("web still reads URL after a release that drops it")
	}
}

// A change of the database need leaves a value with a secret inside that the need
// doesn't set, which the person pointed at their own cache, as they set it, and says
// so (#570's recheck, N3).
func TestANeedChangeKeepsAPersonsOwnValueWithASecretInside(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	entry := strings.Replace(blogEntry, "needs:\n", "  - name: CACHE_PASSWORD\n    label: Cache password\n    type: INPUT_TYPE_SECRET\n    generate: 32\nneeds:\n", 1)
	entry = strings.Replace(entry, "        DATABASE_URL: ${DATABASE_URL}\n", "        DATABASE_URL: ${DATABASE_URL}\n        CACHE_URL: redis://:${CACHE_PASSWORD}@cache:6379/0\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(entry), 0o600); err != nil {
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
	appEnv := func() map[string]string {
		out := map[string]string{}
		for _, kv := range e.Newest(docker.Project(env), "app").Config.Env {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		return out
	}
	if !strings.HasPrefix(appEnv()["CACHE_URL"], "redis://:") {
		t.Fatalf("after the install CACHE_URL %q", appEnv()["CACHE_URL"])
	}
	databaseURL := appEnv()["DATABASE_URL"]
	const mine = "redis://:mine@my-cache:6379/3"
	if got := apply(t, ch, propose(t, ch, "Use my own cache", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: env, Name: "CACHE_URL"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: mine}}},
	})); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the edit ended %v: %s", got.GetState(), got.GetError())
	}
	entryRes, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range entryRes.Msg.GetNeeds() {
		if o.GetService() == "db" && o.GetProject() != "blog" && o.GetWhy() == "" {
			choice = o.GetChoice()
		}
	}
	res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "blog", Need: "db", Choice: choice}))
	if err != nil {
		t.Fatal(err)
	}
	if n := noteOf(res.Msg.GetChange(), "upgrade_kept_own"); n == nil || !strings.Contains(n.GetMessage(), "CACHE_URL holds a value you set") {
		t.Errorf("the plan's note on the person's cache: %v", n)
	}
	if got := apply(t, ch, res.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the move ended %v: %s", got.GetState(), got.GetError())
	}
	if got := appEnv()["CACHE_URL"]; got != mine {
		t.Errorf("after a change of the database need, CACHE_URL %q, want the person's %q", got, mine)
	}
	// The need's own URL still moves to the new way.
	if got := appEnv()["DATABASE_URL"]; got == databaseURL {
		t.Errorf("after a change of the database need, DATABASE_URL is as it was: %q", got)
	}
}

// A person points the URL secret at a template of their own, as Connect a database
// writes one: an upgrade keeps it, and says so, as it keeps a value of their own
// (#570's recheck, N2).
func TestAPersonsOwnTemplateStaysThroughAnUpgrade(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	u.applied(propose(t, u.c, "Use my own database", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: u.env, Name: "URL"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Template{Template: &resourcev1.ValueTemplate{
			Text: "postgres://me:${PASSWORD}@elsewhere:5432/mine", Names: []*resourcev1.TemplateName{{Name: "PASSWORD", Secret: true, Encoding: resourcev1.Encoding_ENCODING_USERINFO}},
		}}}}}},
	}))
	password := serviceEnv(t, u, "web")["PASSWORD"]
	own := "postgres://me:" + password + "@elsewhere:5432/mine"
	if got := serviceEnv(t, u, "web")["URL"]; got != own {
		t.Fatalf("after the edit web reads URL %q", got)
	}
	u.release(insideEntry(2, "2.0", secondRelease, ""))
	up := u.upgrade()
	if n := noteOf(up, "upgrade_kept_own"); n == nil {
		t.Error("the plan says nothing of the person's URL")
	}
	u.applied(up)
	if got := serviceEnv(t, u, "web")["URL"]; got != own {
		t.Errorf("after an image-only upgrade web reads URL %q, want the person's %q", got, own)
	}
}

// A URL Dockwell wrote, the person never touched, still takes the release's new
// template, with no note: only a template other than the install's reads as theirs
// (#570's recheck, N2).
func TestADockwellTemplateTakesTheReleasesChange(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	password := serviceEnv(t, u, "web")["PASSWORD"]
	u.release(insideEntry(2, "2.0", secondRelease, "&sslmode=disable"))
	up := u.upgrade()
	if n := noteOf(up, "upgrade_kept_own"); n != nil {
		t.Errorf("the plan keeps Dockwell's own URL: %s", n.GetMessage())
	}
	u.applied(up)
	if got, want := serviceEnv(t, u, "web")["URL"], "postgres://u:"+password+"@db:5432/x?port=18100&sslmode=disable"; got != want {
		t.Errorf("web reads URL %q, want %q", got, want)
	}
}

// A release that gives another service an input named as the secret a person set
// web's URL in is refused, saying why and what to do, rather than move web to the
// release's URL and hand the person's to api. Once the person moves their value to a
// secret of another name, the upgrade keeps it with web (#570's check 2, M1).
func TestANewInputTakingAPersonsURLSecretIsRefused(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	const own = "postgres://me:own@elsewhere:5432/mine"
	setOwn(t, u, "URL")
	next := strings.Replace(insideEntry(2, "2.0", secondRelease, ""), "  services:\n    web:",
		"  services:\n    api:\n      image: "+secondRelease+"\n      environment:\n        URL: ${URL}\n    web:", 1)
	next = strings.Replace(next, "    generate: 32\ncompose:", "    generate: 32\n  - name: URL\n    label: API key\n    type: INPUT_TYPE_SECRET\n    generate: 32\ncompose:", 1)
	u.release(next)
	_, err := u.cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing", SkipBackup: true}))
	if err == nil || !strings.Contains(err.Error(), "web's URL holds a value you set in URL, which this release gives api as an input of its own, so api would read your value. Have web's URL use a secret of another name that holds your value, and delete URL, then upgrade") {
		t.Fatalf("the upgrade: %v", err)
	}
	// Done as it says, the value stays with web, and api takes a key of its own.
	setOwn(t, u, "MINE")
	svc := storedService(t, u, "web")
	svc.Secrets["URL"] = "MINE"
	u.applied(propose(t, u.c, "Read my own URL", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: "web"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
	}, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: u.env, Name: "URL"}, Delete: true}))
	u.applied(u.upgrade())
	web, api := serviceEnv(t, u, "web"), serviceEnv(t, u, "api")
	if web["URL"] != own {
		t.Errorf("web reads URL %q, want the person's %q", web["URL"], own)
	}
	if api["URL"] == own || len(api["URL"]) != 32 {
		t.Errorf("api reads URL %q, want a generated key", api["URL"])
	}
}

// storedService is a service of the install as the store holds it.
func storedService(t *testing.T, u *upgrading, name string) *resourcev1.Service {
	t.Helper()
	var spec proto.Message
	if err := u.f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), "service", u.env, name)
		if err != nil {
			return err
		}
		spec, err = u.f.keeper.Decode(t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return proto.CloneOf(spec.(*resourcev1.Service))
}

// Where the release gives the secret to the same service, for another variable, the
// refusal names that service, not another (#570's check 3, N2).
func TestATakenSecretOnTheSameServiceIsNamed(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	setOwn(t, u, "URL")
	next := strings.Replace(insideEntry(2, "2.0", secondRelease, ""), "PASSWORD: ${PASSWORD}\n", "PASSWORD: ${PASSWORD}\n        KEY: ${URL}\n", 1)
	next = strings.Replace(next, "    generate: 32\ncompose:", "    generate: 32\n  - name: URL\n    label: API key\n    type: INPUT_TYPE_SECRET\n    generate: 32\ncompose:", 1)
	u.release(next)
	_, err := u.cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing", SkipBackup: true}))
	const want = "web's URL holds a value you set in URL, which this release gives web as an input of its own too, so web would read your value in another variable. Have web's URL use a secret of another name that holds your value, and delete URL, then upgrade"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("the upgrade: %v", err)
	}
}
