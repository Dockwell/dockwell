// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/autoupdate"
	"github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/control/serviceupdates"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// waitEntry is a source's entry of an app at a release and version, whose image
// moves with its version.
func waitEntry(id string, release int, version string) string {
	image := fmt.Sprintf("nginx:%s@sha256:%s", version, strings.Repeat("a", 64))
	return strings.Replace(testEntry(release, version, image, ""), "id: thing", "id: "+id, 1)
}

// A catalog release a source moved to is taken by an install's automatic updates only
// once the source has held it for 3 days. A source that moves the entry again inside
// them starts them again, and one that moves another entry leaves them as they are.
// A release that withdraws the version the install runs, by pinning a lower one, is a
// move down: offered to new installs only, never taken on its own, and refused to a
// person (ADR 0080, for ADR 0100).
func TestACatalogReleaseWaitsOnItsSourceAndNeverMovesDown(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testACatalogReleaseWaitsOnItsSourceAndNeverMovesDown)
}

func testACatalogReleaseWaitsOnItsSourceAndNeverMovesDown(t *testing.T, backend store.Backend) {
	repo := gitfetchtest.New(t)
	repo.Commit(map[string]string{"entries/notes-src.yaml": waitEntry("notes-src", 1, "1.1")})
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "community", Repository: repo.URL, Path: "entries", Follow: "main"}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, res.Msg.GetChange())
	apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "notes-src"}))
	web := func() *dockwellv1.ServiceInfo {
		t.Helper()
		list, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, svc := range list.Msg.GetServices() {
			if svc.GetResource().GetName() == "web" {
				return svc
			}
		}
		t.Fatal("no web service")
		return nil
	}
	bench := &bench{f: f, b: b, e: e, c: c, s: s}
	bench.setAutoUpdate(t, web(), &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_ANY, At: "03:00", TimeZone: "UTC"})
	id := web().GetId()
	// What the catalog says now, from a keeper of its own, as the server's keeps what
	// it read for half a minute.
	updates := func() services.Updates {
		return services.NewerFrom(&serviceupdates.Keeper{DB: f.store, Secrets: f.keeper, Catalog: f.server.updates.Catalog})
	}
	offered := func() services.Newer {
		t.Helper()
		got, ok, err := updates().Of(t.Context(), id)
		if err != nil || !ok || !got.Catalog {
			t.Fatalf("the install's update: %+v, %v, %v", got, ok, err)
		}
		return got
	}
	move := func(files map[string]string) time.Time {
		t.Helper()
		before := time.Now()
		repo.Commit(files)
		got, err := cat.CheckCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.CheckCatalogSourceRequest{Name: "community"}))
		if err != nil || got.Msg.GetChange() == nil {
			t.Fatalf("the source didn't move: %v, %v", got, err)
		}
		return before
	}
	release := func() uint32 {
		t.Helper()
		got, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range got.Msg.GetInstalls() {
			if in.GetPin().GetEntry() == "notes-src" && in.GetProject() == "notes-src" {
				return in.GetPin().GetRelease()
			}
		}
		t.Fatal("notes-src isn't installed")
		return 0
	}
	// tick runs the automatic updates at 03:05 UTC on the day after `after` and days,
	// in the install's window, with what the catalog says now.
	tick := func(after time.Time, days int) {
		t.Helper()
		day := after.UTC().Add(time.Duration(days) * 24 * time.Hour)
		at := time.Date(day.Year(), day.Month(), day.Day(), 3, 5, 0, 0, time.UTC)
		r := &autoupdate.Runner{
			DB: f.store, Keeper: f.keeper, Planner: f.server.autoupdates.Planner, Engine: f.engine,
			Updates: updates(), Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return at },
		}
		if err := r.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	// The source moves to release 2: its update is found from when it moved.
	first := move(map[string]string{"entries/notes-src.yaml": waitEntry("notes-src", 2, "1.2")})
	moved := offered().Found
	if moved.Before(first.Add(-time.Second)) || moved.After(time.Now()) {
		t.Errorf("release 2 is found at %v, want when the source moved, from %v", moved, first)
	}
	// It moves again inside the 3 days, to release 3: the time starts again.
	second := move(map[string]string{"entries/notes-src.yaml": waitEntry("notes-src", 3, "1.3")})
	again := offered().Found
	if !again.After(moved) || again.Before(second.Add(-time.Second)) {
		t.Errorf("release 3 is found at %v, after release 2's %v; want from its own move, from %v", again, moved, second)
	}
	// Another entry moves: this one's time stands.
	move(map[string]string{"entries/wiki-src.yaml": waitEntry("wiki-src", 1, "1.0")})
	if got := offered().Found; !got.Equal(again) {
		t.Errorf("a move of another entry made release 3 found at %v, want %v", got, again)
	}

	// 2 days on, it waits; past 3, it's taken on its own.
	tick(again, 2)
	if got := release(); got != 1 {
		t.Fatalf("2 days after its source moved, the install took release %d on its own", got)
	}
	tick(again, 4)
	if got := release(); got != 3 {
		t.Fatalf("4 days after its source moved, the install is on release %d, want 3", got)
	}

	// The catalog withdraws 1.3 by pinning 1.2 again, as release 4: a move down for
	// the install on 1.3, which nothing offers, takes or plans.
	move(map[string]string{"entries/notes-src.yaml": waitEntry("notes-src", 4, "1.2")})
	if got, ok, err := updates().Of(t.Context(), id); err != nil || ok {
		t.Errorf("the withdrawal is offered as an update: %+v, %v", got, err)
	}
	tick(time.Now(), 10)
	if got := release(); got != 3 {
		t.Errorf("the install moved down to release %d on its own", got)
	}
	// Its install says its catalog withdrew the version it runs, and what to.
	installs, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range installs.Msg.GetInstalls() {
		if w := in.GetWithdrawn(); in.GetProject() == "notes-src" && (w.GetVersion() != "1.2" || w.GetRelease() != 4 || in.GetUpgrade() != nil) {
			t.Errorf("the install on the withdrawn 1.3 says withdrawn %v, upgrade %v; want 1.2 release 4, and no upgrade", w, in.GetUpgrade())
		}
	}
	_, err = cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "notes-src"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "moves_down" || !strings.Contains(err.Error(), "below the 1.3 it runs") {
		t.Errorf("a person's upgrade down to 1.2: %v", err)
	}
	// Nor does it take a new build alone, which would build 1.2's entry.
	_, err = s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: id, Rebuild: true}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "moves_down" || !strings.Contains(err.Error(), "takes no new build alone") {
		t.Errorf("a new build alone on the withdrawn 1.3: %v", err)
	}
	// A new install takes the release as it is.
	apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "notes-src", Project: "notes-new"}))
	list, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range list.Msg.GetInstalls() {
		if in.GetProject() == "notes-new" && (in.GetPin().GetVersion() != "1.2" || in.GetPin().GetRelease() != 4) {
			t.Errorf("a new install is on %s release %d, want 1.2 release 4", in.GetPin().GetVersion(), in.GetPin().GetRelease())
		}
	}
}
