// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/autoupdate"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// newer is what a test says Dockwell's checks found, by service.
type newer struct {
	mu    sync.Mutex
	found map[string]services.Newer
	asked int
}

func (n *newer) Of(_ context.Context, id string) (services.Newer, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.asked++
	got, ok := n.found[id]
	return got, ok, nil
}

// benchOn is newBench on a server started already.
func benchOn(t *testing.T, f *fullServer) *bench {
	t.Helper()
	b := signedIn(t, f)
	return &bench{
		f: f, b: b, e: f.addDockerServer(t),
		c:  changeClient(b, b.origin),
		cc: dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()),
		s:  dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON()),
	}
}

// setAutoUpdate plans and applies giving a service automatic updates.
func (b *bench) setAutoUpdate(t *testing.T, svc *dockwellv1.ServiceInfo, a *resourcev1.AutoUpdate) {
	t.Helper()
	spec := svc.GetSpec()
	spec.AutoUpdate = a
	res, err := b.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Update " + svc.GetResource().GetName() + " on its own",
		Edits: []*dockwellv1.Edit{{
			Resource: svc.GetResource(), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}}, BaseRevision: svc.GetRevision(),
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	b.done(t, res.Msg.GetChange())
}

// Dockwell updates a service on its own only in its window, once a window, to what
// its level takes, as its own change with a backup first; and never a database's
// major release (ADR 0080).
func TestAnAutomaticUpdateRunsInItsWindowAsDockwell(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.ManualAlerts = true })
		b := benchOn(t, f)
		env := b.run(t, &dockwellv1.ImportComposeRequest{Compose: shopWithData})
		web := b.service(t, "shop", "web")
		b.setAutoUpdate(t, web, &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_PATCH, At: "03:00", Minutes: 60, TimeZone: "UTC"})
		if got := b.service(t, "shop", "web").GetSpec().GetAutoUpdate(); got.GetLevel() != resourcev1.AutoUpdate_LEVEL_PATCH || got.GetMinutes() != 60 {
			t.Fatalf("the service's automatic updates read %v", got)
		}

		found := &newer{found: map[string]services.Newer{web.GetId(): {Image: "ghcr.io/acme/shop:1.5", Kind: "patch"}}}
		now := time.Date(2026, 11, 14, 2, 59, 0, 0, time.UTC)
		r := &autoupdate.Runner{
			DB: f.store, Keeper: f.keeper, Planner: f.server.autoupdates.Planner, Engine: f.engine, Updates: found,
			Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now },
		}
		runs := func() string { return b.e.Newest(docker.Project(env), "web").Config.Image }

		if err := r.Tick(t.Context()); err != nil || runs() != "ghcr.io/acme/shop:1.4" || found.asked != 0 {
			t.Fatalf("before its window: %v, it runs %s, asked %d", err, runs(), found.asked)
		}
		now = now.Add(10 * time.Minute)
		if err := r.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if runs() != "ghcr.io/acme/shop:1.5" {
			t.Fatalf("in its window, it runs %s", runs())
		}
		list, err := b.c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{PageSize: 5}))
		if err != nil {
			t.Fatal(err)
		}
		c := list.Msg.GetChanges()[0]
		if c.GetProposedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL || c.GetProposedVia() != dockwellv1.Surface_SURFACE_AUTOMATIC ||
			c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || !strings.Contains(c.GetSummary(), "on its own") ||
			c.GetPlan().GetIntent().GetKind() != dockwellv1.IntentKind_INTENT_KIND_UPGRADE {
			t.Errorf("the automatic update reads %v by %v via %v, %v: %q", c.GetState(), c.GetProposedBy(), c.GetProposedVia(), c.GetPlan().GetIntent(), c.GetSummary())
		}
		full, err := b.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: c.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if noteOf(full.Msg.GetChange(), "backup_first") == nil {
			t.Errorf("an automatic update takes no backup first: %v", full.Msg.GetChange().GetPlan().GetNotes())
		}

		// Once a window, though its checks still say there's newer.
		b.e.Registry["ghcr.io/acme/shop:1.6"] = dockertest.Image{Size: 30 << 20, Config: docker.Config{Cmd: []string{"shop"}}}
		found.found[web.GetId()] = services.Newer{Image: "ghcr.io/acme/shop:1.6", Kind: "patch"}
		now = now.Add(10 * time.Minute)
		if err := r.Tick(t.Context()); err != nil || runs() != "ghcr.io/acme/shop:1.5" {
			t.Errorf("a second tick in one window: %v, it runs %s", err, runs())
		}
		// The next day's window takes it, and not what its level doesn't.
		found.found[web.GetId()] = services.Newer{Image: "ghcr.io/acme/shop:1.6", Kind: "minor"}
		now = now.Add(24 * time.Hour)
		if err := r.Tick(t.Context()); err != nil || runs() != "ghcr.io/acme/shop:1.5" {
			t.Errorf("a minor release at patch level: %v, it runs %s", err, runs())
		}
		found.found[web.GetId()] = services.Newer{Image: "ghcr.io/acme/shop:1.6", Kind: "patch"}
		now = now.Add(5 * time.Minute)
		if err := r.Tick(t.Context()); err != nil || runs() != "ghcr.io/acme/shop:1.6" {
			t.Errorf("the next window: %v, it runs %s", err, runs())
		}
	})
}

// A database's major release is never taken on its own, at any level.
func TestAnAutomaticUpdateNeverMovesADatabasesMajor(t *testing.T) {
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.ManualAlerts = true })
	b := benchOn(t, f)
	b.e.Registry["postgres:16.4"] = dockertest.Image{Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=16"}}}
	env := b.run(t, &dockwellv1.ImportComposeRequest{Compose: `name: shop
services:
  db:
    image: postgres:16.4
    environment:
      POSTGRES_PASSWORD: example
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata: {}
`})
	db := b.service(t, "shop", "db")
	b.setAutoUpdate(t, db, &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_ANY, At: "03:00", TimeZone: "UTC"})
	found := &newer{found: map[string]services.Newer{db.GetId(): {Image: "postgres:17.2", Kind: "major"}}}
	pulls := b.e.Pulls
	r := &autoupdate.Runner{
		DB: f.store, Keeper: f.keeper, Planner: f.server.autoupdates.Planner, Engine: f.engine, Updates: found,
		Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return time.Date(2026, 11, 14, 3, 5, 0, 0, time.UTC) },
	}
	if err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := b.e.Newest(docker.Project(env), "db").Config.Image; got != "postgres:16.4" || b.e.Pulls != pulls {
		t.Errorf("a database's major release was taken on its own: it runs %s", got)
	}
	// Nor another release series that its version's numbers call minor, as MySQL 8.0
	// to 8.4 is, which moves its data by a dump.
	b.e.Registry["mysql:8.0.36"] = dockertest.Image{Volumes: []string{"/var/lib/mysql"}, Config: docker.Config{Cmd: []string{"mysqld"}, Env: []string{"MYSQL_MAJOR=8.0"}}}
	b.run(t, &dockwellv1.ImportComposeRequest{Compose: `name: books
services:
  ledger:
    image: mysql:8.0.36
    environment:
      MYSQL_ROOT_PASSWORD: example
    volumes:
      - data:/var/lib/mysql
volumes:
  data: {}
`})
	ledger := b.service(t, "books", "ledger")
	b.setAutoUpdate(t, ledger, &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_ANY, At: "03:00", TimeZone: "UTC"})
	r.Updates = &newer{found: map[string]services.Newer{ledger.GetId(): {Image: "mysql:8.4.3", Kind: "minor"}}}
	planned := &countingPlanner{Planner: r.Planner}
	r.Planner = planned
	if err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if planned.n != 0 {
		t.Errorf("MySQL 8.0's move to 8.4 was planned on its own, %d times", planned.n)
	}
}

// countingPlanner counts the updates a runner plans.
type countingPlanner struct {
	autoupdate.Planner
	n int
}

func (c *countingPlanner) UpgradeService(ctx context.Context, req *connect.Request[dockwellv1.UpgradeServiceRequest]) (*connect.Response[dockwellv1.UpgradeServiceResponse], error) {
	c.n++
	return c.Planner.UpgradeService(ctx, req)
}

// An admin's default for new apps' automatic updates goes to an app a change
// makes, in the install's time zone, and not to one made before, nor one whose
// change says its own (ADR 0080).
func TestNewAppsTakeTheInstallsAutomaticUpdates(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		b.run(t, &dockwellv1.ImportComposeRequest{Compose: shopFile})
		settings := dockwellv1connect.NewSettingsServiceClient(b.b, b.b.origin, connect.WithProtoJSON())
		if _, err := settings.SetTimeZone(t.Context(), connect.NewRequest(&dockwellv1.SetTimeZoneRequest{TimeZone: "Europe/Amsterdam"})); err != nil {
			t.Fatal(err)
		}
		res, err := settings.SetAutoUpdateDefault(t.Context(), connect.NewRequest(&dockwellv1.SetAutoUpdateDefaultRequest{
			AutoUpdate: &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_MINOR},
		}))
		if err != nil {
			t.Fatal(err)
		}
		got := res.Msg.GetSettings().GetAutoUpdate()
		if got.GetLevel() != resourcev1.AutoUpdate_LEVEL_MINOR || got.GetAt() != "03:00" || got.GetMinutes() != 120 || got.GetTimeZone() != "Europe/Amsterdam" {
			t.Errorf("the default reads %v", got)
		}
		if _, err := settings.SetAutoUpdateDefault(t.Context(), connect.NewRequest(&dockwellv1.SetAutoUpdateDefaultRequest{
			AutoUpdate: &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_PATCH, At: "25:00"},
		})); reasonOf(err) != "invalid" {
			t.Errorf("a default that can't open = %v", err)
		}

		b.run(t, &dockwellv1.ImportComposeRequest{Compose: "name: blog\nservices:\n  web:\n    image: ghcr.io/acme/shop:1.4\n"})
		if a := b.service(t, "blog", "web").GetSpec().GetAutoUpdate(); a.GetLevel() != resourcev1.AutoUpdate_LEVEL_MINOR || a.GetTimeZone() != "Europe/Amsterdam" {
			t.Errorf("a new app's automatic updates read %v", a)
		}
		// One made before keeps its own, imported again or not.
		b.run(t, &dockwellv1.ImportComposeRequest{Compose: strings.Replace(shopFile, "1.4", "1.5", 1)})
		if a := b.service(t, "shop", "web").GetSpec().GetAutoUpdate(); a != nil {
			t.Errorf("an app made before took the default: %v", a)
		}

		// Off again: new apps take none.
		if _, err := settings.SetAutoUpdateDefault(t.Context(), connect.NewRequest(&dockwellv1.SetAutoUpdateDefaultRequest{})); err != nil {
			t.Fatal(err)
		}
		b.run(t, &dockwellv1.ImportComposeRequest{Compose: "name: docs\nservices:\n  web:\n    image: ghcr.io/acme/shop:1.4\n"})
		if a := b.service(t, "docs", "web").GetSpec().GetAutoUpdate(); a != nil {
			t.Errorf("with the default off, a new app's automatic updates read %v", a)
		}
	})
}

// imagesSay is what a test says each image declares, by reference.
type imagesSay map[string]*dockwellv1.ImageConfig

func (i imagesSay) Config(_ context.Context, _, ref string) (*dockwellv1.ImageConfig, error) {
	return i[ref], nil
}

// A moving tag's new image is taken at the level its versions say it moves by:
// the same release rebuilt at patch; 5.2 to 5.3 at minor and not at patch; and
// one whose images say no version only at any, as latest can move an app from
// 5.x to 6.0 (ADR 0080).
func TestAMovingTagIsTakenByWhatItsVersionsSay(t *testing.T) {
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.ManualAlerts = true })
	b := benchOn(t, f)
	name, web := runLatest(t, b)
	now := time.Date(2026, 11, 14, 3, 5, 0, 0, time.UTC)
	found := &newer{found: map[string]services.Newer{web.GetId(): {Image: latest, Kind: "digest"}}}
	said := imagesSay{}
	r := &autoupdate.Runner{
		DB: f.store, Keeper: f.keeper, Planner: f.server.autoupdates.Planner, Engine: f.engine, Updates: found, Images: said,
		Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now },
	}
	level := func(l resourcev1.AutoUpdate_Level) {
		t.Helper()
		b.setAutoUpdate(t, b.service(t, "shop", "web"), &resourcev1.AutoUpdate{Level: l, At: "03:00", TimeZone: "UTC"})
	}
	runs := func() string { return b.e.Serving(name).Image }
	// The next window, with the tag moved to an image of its own, and what the image
	// that runs and the new one say of their versions, if anything.
	moves := 0
	window := func(was, is string) string {
		t.Helper()
		moves++
		ran := runs()
		moveLatest(b.e, "sha256:"+strings.Repeat(string(rune('c'+moves)), 64))
		clear(said)
		if was != "" {
			for _, s := range b.revisions(t, web.GetId()) {
				for _, d := range s.GetDeployments() {
					if d.GetDigest() != "" {
						said["ghcr.io/acme/shop@"+d.GetDigest()] = &dockwellv1.ImageConfig{Version: was}
					}
				}
			}
			said[latest] = &dockwellv1.ImageConfig{Version: is}
		}
		now = now.Add(24 * time.Hour)
		if err := r.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		return map[bool]string{true: "kept", false: "taken"}[runs() == ran]
	}
	level(resourcev1.AutoUpdate_LEVEL_PATCH)
	if got := window("5.2", "5.3"); got != "kept" {
		t.Errorf("latest moving 5.2 to 5.3 at patch: %s", got)
	}
	level(resourcev1.AutoUpdate_LEVEL_MINOR)
	if got := window("5.2", "5.3"); got != "taken" {
		t.Errorf("latest moving 5.2 to 5.3 at minor: %s", got)
	}
	if got := window("", ""); got != "kept" {
		t.Errorf("latest with no version at minor: %s", got)
	}
	level(resourcev1.AutoUpdate_LEVEL_ANY)
	if got := window("", ""); got != "taken" {
		t.Errorf("latest with no version at any: %s", got)
	}
	level(resourcev1.AutoUpdate_LEVEL_PATCH)
	if got := window("5.3", "5.3"); got != "taken" {
		t.Errorf("the same release rebuilt at patch: %s", got)
	}
}

// A tag of two places, as 16.4, is a release rebuilt where its images say so, and
// taken at patch then; where they say nothing it could be 1.27 moving to 1.28, so
// only any takes it (ADR 0080).
func TestATagOfTwoPlacesIsARebuildWhereItsImagesSaySo(t *testing.T) {
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.ManualAlerts = true })
	b := benchOn(t, f)
	const tag = "ghcr.io/acme/shop:16.4"
	b.e.Registry[tag] = dockertest.Image{Digest: digestA, Config: docker.Config{Cmd: []string{"shop"}}}
	env := b.run(t, &dockwellv1.ImportComposeRequest{Compose: "name: shop\nservices:\n  web:\n    image: " + tag + "\n"})
	web := b.service(t, "shop", "web")
	b.setAutoUpdate(t, web, &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_PATCH, At: "03:00", TimeZone: "UTC"})
	said := imagesSay{}
	now := time.Date(2026, 11, 14, 3, 5, 0, 0, time.UTC)
	r := &autoupdate.Runner{
		DB: f.store, Keeper: f.keeper, Planner: f.server.autoupdates.Planner, Engine: f.engine, Images: said,
		Updates: &newer{found: map[string]services.Newer{web.GetId(): {Image: tag, Kind: "digest"}}},
		Logger:  slog.New(slog.DiscardHandler), Now: func() time.Time { return now },
	}
	runs := func() string { return b.e.Newest(docker.Project(env), "web").Image }

	// Rebuilt, saying nothing: kept at patch.
	b.e.Registry[tag] = dockertest.Image{Digest: digestB, Config: docker.Config{Cmd: []string{"shop"}}}
	ran := runs()
	if err := r.Tick(t.Context()); err != nil || runs() != ran {
		t.Errorf("16.4 rebuilt with no version at patch: %v, replaced %t", err, runs() != ran)
	}
	// The next window, its images saying 16.4 both: taken.
	said["ghcr.io/acme/shop@"+digestA] = &dockwellv1.ImageConfig{Version: "16.4"}
	said[tag] = &dockwellv1.ImageConfig{Version: "16.4"}
	now = now.Add(24 * time.Hour)
	if err := r.Tick(t.Context()); err != nil || runs() == ran {
		t.Errorf("16.4 rebuilt, saying 16.4, at patch: %v, replaced %t", err, runs() != ran)
	}
}

// A catalog install's newer release is taken on its own as its install's upgrade,
// with no image of its own (ADR 0080).
func TestAnAutomaticUpdateOfACatalogInstallUpgradesIt(t *testing.T) {
	u := startUpgrading(t, store.SQLite, testEntry(1, "1.0", firstRelease, ""))
	u.release(testEntry(2, "2.0", secondRelease, ""))
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
		Updates: &newer{found: map[string]services.Newer{web.GetId(): {Image: secondRelease, Kind: "major", Catalog: true}}},
		Logger:  slog.New(slog.DiscardHandler), Now: func() time.Time { return time.Date(2026, 11, 14, 3, 5, 0, 0, time.UTC) },
	}
	// 1.0 to 2.0 is a major release, which minor doesn't take.
	if err := r.Tick(t.Context()); err != nil || u.web().Config.Image != firstRelease {
		t.Fatalf("a major catalog release at minor: %v, web runs %s", err, u.web().Config.Image)
	}
	r.Updates = &newer{found: map[string]services.Newer{web.GetId(): {Image: secondRelease, Kind: "minor", Catalog: true}}}
	r.Now = func() time.Time { return time.Date(2026, 11, 15, 3, 5, 0, 0, time.UTC) }
	if err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := u.web().Config.Image; got != secondRelease {
		t.Errorf("a catalog install's minor release on its own: web runs %s", got)
	}
}

// dbEntry is a catalog entry whose app keeps its data in PostgreSQL of image.
func dbEntry(release int, version, image string) string {
	return fmt.Sprintf(`id: thing
name: Thing
summary: Something to upgrade
category: storage
version: %q
release: %d
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
inputs:
  - name: PASSWORD
    label: Password
    type: INPUT_TYPE_SECRET
    generate: 32
compose: |
  services:
    db:
      image: %s
      environment:
        POSTGRES_PASSWORD: ${PASSWORD}
      volumes:
        - pgdata:/var/lib/postgresql/data
  volumes:
    pgdata: {}
`, version, release, image)
}

// M1: a catalog release that moves its install's database to another release, as
// one that takes postgres from 16 to 17 does, is never applied on its own, whatever
// its kind: its plan, which moves the data, is left for a person.
func TestAnAutomaticUpdateNeverMovesADatabaseInACatalogRelease(t *testing.T) {
	u := &upgrading{t: t, dir: t.TempDir()}
	t.Setenv(catalog.DirEnv, u.dir)
	u.release(dbEntry(1, "1.0", "postgres:16.4@sha256:"+digest1))
	f := startFull(t, store.SQLite, nil)
	u.b, u.f = signedIn(t, f), f
	u.e = f.addDockerServer(t)
	u.e.Find = func(ref string) (dockertest.Image, bool) {
		switch {
		case strings.HasPrefix(ref, "postgres:16"):
			return pgOf("16"), true
		case strings.HasPrefix(ref, "postgres:17"):
			return pgOf("17"), true
		}
		return dockertest.Image{}, true
	}
	u.c, u.cat = changeClient(u.b, u.b.origin), catalogClient(u.b)
	apply(t, u.c, installApp(t, u.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "thing"}))
	for _, r := range mustList(t, u.b).GetResources() {
		u.env = r.GetId()
	}
	u.release(dbEntry(2, "1.1", "postgres:17.2@sha256:"+digest2))
	s := dockwellv1connect.NewServiceServiceClient(u.b, u.b.origin, connect.WithProtoJSON())
	list, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var db *dockwellv1.ServiceInfo
	for _, svc := range list.Msg.GetServices() {
		if svc.GetResource().GetName() == "db" {
			db = svc
		}
	}
	if db == nil {
		t.Fatalf("no db among %v", list.Msg.GetServices())
	}
	b := &bench{f: u.f, b: u.b, e: u.e, c: u.c, s: s}
	b.setAutoUpdate(t, db, &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_ANY, At: "03:00", TimeZone: "UTC"})
	r := &autoupdate.Runner{
		DB: u.f.store, Keeper: u.f.keeper, Planner: u.f.server.autoupdates.Planner, Engine: u.f.engine,
		Updates: &newer{found: map[string]services.Newer{db.GetId(): {Kind: "release", Catalog: true}}},
		Logger:  slog.New(slog.DiscardHandler), Now: func() time.Time { return time.Date(2026, 11, 14, 3, 5, 0, 0, time.UTC) },
	}
	if err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := u.e.Newest(docker.Project(db.GetResource().GetScope()), "db").Config.Image; !strings.HasPrefix(got, "postgres:16") {
		t.Errorf("a catalog release moved the database on its own: it runs %s", got)
	}
	planned, err := u.c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	left := false
	for _, listed := range planned.Msg.GetChanges() {
		if listed.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
			continue
		}
		got, err := u.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: listed.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		for _, sv := range got.Msg.GetChange().GetPlan().GetServers() {
			left = left || slices.ContainsFunc(sv.GetSteps(), func(st *dockwellv1.PlanStep) bool { return st.GetDatabaseMove() != nil })
		}
	}
	if !left {
		t.Error("the release's move isn't left planned for a person")
	}
}
