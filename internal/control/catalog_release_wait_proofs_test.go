// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"log/slog"
	"strconv"
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
	"github.com/dockwell/dockwell/internal/resource/catalogsource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// waitStand is an install of notes-src from the source community, which follows a
// branch, with automatic updates of any kind in a window at 03:00 UTC.
type waitStand struct {
	f       *fullServer
	repo    *gitfetchtest.Repo
	cat     dockwellv1connect.CatalogServiceClient
	c       dockwellv1connect.ChangeServiceClient
	id      string
	commits []string
}

func newWaitStand(t *testing.T, backend store.Backend) *waitStand {
	repo := gitfetchtest.New(t)
	first := repo.Commit(map[string]string{"entries/notes-src.yaml": waitEntry("notes-src", 1, "1.1")})
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
	if web == nil {
		t.Fatal("no web service")
	}
	(&bench{f: f, b: b, e: e, c: c, s: s}).setAutoUpdate(t, web, &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_ANY, At: "03:00", TimeZone: "UTC"})
	return &waitStand{f: f, repo: repo, cat: cat, c: c, id: web.GetId(), commits: []string{first}}
}

func (r *waitStand) keeper() *serviceupdates.Keeper {
	return &serviceupdates.Keeper{DB: r.f.store, Secrets: r.f.keeper, Catalog: r.f.server.updates.Catalog}
}

func (r *waitStand) move(t *testing.T, files map[string]string) {
	t.Helper()
	r.commits = append(r.commits, r.repo.Commit(files))
	got, err := r.cat.CheckCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.CheckCatalogSourceRequest{Name: "community"}))
	if err != nil || got.Msg.GetChange() == nil {
		t.Fatalf("the source didn't move: %v, %v", got, err)
	}
}

func (r *waitStand) release(t *testing.T) uint32 {
	t.Helper()
	got, err := r.cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range got.Msg.GetInstalls() {
		if in.GetProject() == "notes-src" {
			return in.GetPin().GetRelease()
		}
	}
	t.Fatal("notes-src isn't installed")
	return 0
}

// record sets what the control plane recorded of notes-src's release on community,
// at the source's revision now, as if it had held it since then.
func (r *waitStand) record(t *testing.T, release uint32, since time.Time) {
	t.Helper()
	if err := r.f.store.Update(t.Context(), func(tx *store.Tx) error {
		list, err := tx.ListKind(t.Context(), string(catalogsource.Kind))
		if err != nil {
			return err
		}
		for _, src := range list {
			if src.Name == "community" {
				value := strconv.Itoa(int(release)) + " " + strconv.Itoa(src.Revision) + " " + since.UTC().Format(time.RFC3339Nano)
				return tx.PutSetting(t.Context(), "catalog.release."+src.ID+"/notes-src", []byte(value))
			}
		}
		t.Fatal("no source community")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (r *waitStand) tick(t *testing.T, at time.Time, updates services.Updates) {
	t.Helper()
	run := &autoupdate.Runner{
		DB: r.f.store, Keeper: r.f.keeper, Planner: r.f.server.autoupdates.Planner, Engine: r.f.engine,
		Updates: updates, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return at },
	}
	if err := run.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// windowAfter is the first 03:05 UTC after now, inside the install's window.
func windowAfter(now time.Time) time.Time {
	now = now.UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), 3, 5, 0, 0, time.UTC)
	if !at.After(now) {
		at = at.Add(24 * time.Hour)
	}
	return at
}

// An automatic update checks the wait on the release it plans, not the one it found:
// the keeper's read is kept for 30 s, and a source that moves in between moved to a
// release that waits (the release-wait review, H1).
func TestAnAutomaticUpdateWaitsOnTheReleaseItPlans(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		r := newWaitStand(t, backend)
		at := windowAfter(time.Now())
		r.move(t, map[string]string{"entries/notes-src.yaml": waitEntry("notes-src", 2, "1.2")})
		// Release 2 has been on the source for 4 days by the time the window opens.
		r.record(t, 2, at.Add(-4*24*time.Hour))
		k := r.keeper()
		cached := services.NewerFrom(k)
		got, ok, err := cached.Of(t.Context(), r.id)
		if err != nil || !ok || !got.Found.Equal(at.Add(-4*24*time.Hour)) {
			t.Fatalf("release 2's update: %+v, %v, %v", got, ok, err)
		}
		// The follower moves the source to release 3 a few seconds later.
		r.move(t, map[string]string{"entries/notes-src.yaml": waitEntry("notes-src", 3, "1.3")})
		fresh, _, _ := services.NewerFrom(r.keeper()).Of(t.Context(), r.id)
		if at.Sub(fresh.Found) >= autoupdate.CatalogWait {
			t.Fatalf("release 3 is found at %v, which isn't inside the wait at %v", fresh.Found, at)
		}
		// Control: a runner that reads afresh waits for release 3.
		r.tick(t, at, services.NewerFrom(r.keeper()))
		if got := r.release(t); got != 1 {
			t.Fatalf("control: a fresh read took release %d", got)
		}
		// The runner's read is the keeper's, within its 30 s.
		r.tick(t, at, cached)
		if got := r.release(t); got == 3 {
			t.Errorf("the automatic update took release 3, found %v, %v before the window, inside the 3 days: it checked release 2's time and planned whatever was newest", fresh.Found, at.Sub(fresh.Found).Round(time.Minute))
		}
	})
}

// A source that leaves a release and comes back to it, while no install is offered
// it, holds it from its move back (the release-wait review, M1).
func TestASourceThatComesBackToAReleaseHoldsItFromItsReturn(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		r := newWaitStand(t, backend)
		r.move(t, map[string]string{"entries/notes-src.yaml": waitEntry("notes-src", 2, "1.2")})
		if _, _, err := services.NewerFrom(r.keeper()).Of(t.Context(), r.id); err != nil {
			t.Fatal(err)
		}
		// As if the follower moved it 2 days ago.
		old := time.Now().Add(-2 * 24 * time.Hour)
		r.record(t, 2, old)
		// A person moves the source back to the commit before, pinned.
		res, err := r.cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "community", Repository: r.repo.URL, Path: "entries", Commit: r.commits[0]}))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, r.c, res.Msg.GetChange())
		if got, ok, _ := services.NewerFrom(r.keeper()).Of(t.Context(), r.id); ok && got.Catalog {
			t.Fatalf("an install on release 1 with its source on release 1 is offered %+v", got)
		}
		// The branch moves on with another entry; the person follows it again.
		r.repo.Commit(map[string]string{"entries/wiki-src.yaml": waitEntry("wiki-src", 1, "1.0")})
		before := time.Now()
		res, err = r.cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "community", Repository: r.repo.URL, Path: "entries", Follow: "main"}))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, r.c, res.Msg.GetChange())
		got, ok, err := services.NewerFrom(r.keeper()).Of(t.Context(), r.id)
		if err != nil || !ok {
			t.Fatalf("release 2's update: %+v, %v, %v", got, ok, err)
		}
		if got.Found.Before(before.Add(-time.Second)) {
			t.Errorf("release 2 is found at %v, before the source moved back to it at %v: the wait counts the 2 days it wasn't there", got.Found, before)
		}
		// Unattended: 1.5 days after the source came back to release 2, it's taken.
		r.tick(t, windowAfter(before.Add(36*time.Hour)), services.NewerFrom(r.keeper()))
		if got := r.release(t); got == 2 {
			t.Errorf("the automatic update took release 2 within 3 days of the source's move back to it")
		}
	})
}

// A source removed and added again under the same name, repository and folder holds
// a release from its own adding, not the removed one's time (the release-wait
// review, M1).
func TestASourceAddedAgainHoldsAReleaseFromItsOwnAdding(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		r := newWaitStand(t, backend)
		r.move(t, map[string]string{"entries/notes-src.yaml": waitEntry("notes-src", 2, "1.2")})
		if _, _, err := services.NewerFrom(r.keeper()).Of(t.Context(), r.id); err != nil {
			t.Fatal(err)
		}
		r.record(t, 2, time.Now().Add(-10*24*time.Hour))
		res, err := r.cat.RemoveCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.RemoveCatalogSourceRequest{Name: "community"}))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, r.c, res.Msg.GetChange())
		before := time.Now()
		set, err := r.cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "community", Repository: r.repo.URL, Path: "entries", Follow: "main"}))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, r.c, set.Msg.GetChange())
		got, ok, err := services.NewerFrom(r.keeper()).Of(t.Context(), r.id)
		if err != nil || !ok {
			t.Fatalf("release 2's update: %+v, %v, %v", got, ok, err)
		}
		if got.Found.Before(before.Add(-time.Second)) {
			t.Errorf("a source added just now holds release 2 since %v, the removed source's time", got.Found)
		}
	})
}
