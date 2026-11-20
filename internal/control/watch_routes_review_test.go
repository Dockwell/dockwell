// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/watchroutes"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// stored are the install's checks as the store has them, by name, as a developer
// can't list them.
func (s *watchStand) stored() map[string]*resourcev1.AlertCheck {
	s.t.Helper()
	out := map[string]*resourcev1.AlertCheck{}
	if err := s.f.store.View(s.t.Context(), func(tx *store.Tx) error {
		list, err := tx.ListKind(s.t.Context(), string(alert.CheckKind))
		if err != nil {
			return err
		}
		for i := range list {
			r := &list[i]
			spec, err := s.f.keeper.Decode(s.t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			out[r.Name] = spec.(*resourcev1.AlertCheck)
		}
		return nil
	}); err != nil {
		s.t.Fatal(err)
	}
	return out
}

func (s *watchStand) refuseNext() {
	s.ing.mu.Lock()
	s.ing.refuseNext = true
	s.ing.mu.Unlock()
}

func checkEdit(name string, c *resourcev1.AlertCheck) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: c}},
	}
}

// pauseAfterHook has the next proposal stop once the watch hook has read, until the
// test has done what it does meanwhile: it hands over the hook's edits, and waits.
func (s *watchStand) pauseAfterHook() (seen <-chan []revision.Edit, goOn chan<- struct{}) {
	watch := &watchroutes.Watcher{Keeper: s.f.keeper}
	var armed atomic.Bool
	armed.Store(true)
	saw, next := make(chan []revision.Edit, 1), make(chan struct{})
	s.f.engine.SetEditHook(revision.ChainEditHooks(watch.Hook, func(_ context.Context, _ *store.Tx, edits []revision.Edit) ([]revision.Edit, []revision.Note, error) {
		if armed.CompareAndSwap(true, false) {
			saw <- slices.Clone(edits)
			<-next
		}
		return edits, nil, nil
	}))
	return saw, next
}

// Review H1: what the hook adds is made from what it read. A check an admin makes
// at the name the hook chose, between the hook's read and the plan, isn't taken
// over: the route's plan is refused. A check an admin makes theirs meanwhile isn't
// deleted with the route: the route's plan is stale.
func TestARouteCheckTakesOverNothingChangedAsItsPlanIsMade(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		seen, goOn := s.pauseAfterHook()
		errs := make(chan error, 1)
		go func() {
			_, err := s.ch.ProposeChange(context.WithoutCancel(t.Context()), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
				Summary: "A domain", Edits: []*dockwellv1.Edit{s.route("www", web("shop.example.com"))},
			}))
			errs <- err
		}()
		edits := <-seen
		i := slices.IndexFunc(edits, func(ed revision.Edit) bool { return ed.Ref.Kind == alert.CheckKind && ed.MustCreate && ed.MadeForRoute })
		if i < 0 {
			close(goOn)
			t.Fatalf("the hook made no check that must be new: %v", edits)
		}
		name := edits[i].Ref.Name
		apply(t, s.ch, propose(t, s.ch, "Mine", checkEdit(name, &resourcev1.AlertCheck{Address: "https://shop.example.com/health"})))
		close(goOn)
		if err := <-errs; reasonOf(err) != "already_exists" {
			t.Errorf("the route's plan with %s taken meanwhile = %v, want it refused", name, err)
		}
		if got := s.stored()[name]; got.GetAddress() != "https://shop.example.com/health" || got.GetForRoute() != "" {
			t.Errorf("the admin's check became %v", got)
		}

		// The route made with its check, then its deletion planned while an admin
		// makes the check theirs.
		apply(t, s.ch, propose(t, s.ch, "Mine goes", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: name}, Delete: true}))
		s.f.engine.SetEditHook((&watchroutes.Watcher{Keeper: s.f.keeper}).Hook)
		apply(t, s.ch, propose(t, s.ch, "A domain", s.route("www", web("shop.example.com"))))
		made := s.stored()[name]
		if made.GetForRoute() == "" {
			t.Fatalf("the route's check: %v", made)
		}
		seen, goOn = s.pauseAfterHook()
		go func() {
			_, err := s.ch.ProposeChange(context.WithoutCancel(t.Context()), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
				Summary: "No domain", Edits: []*dockwellv1.Edit{s.gone("www")},
			}))
			errs <- err
		}()
		edits = <-seen
		if !slices.ContainsFunc(edits, func(ed revision.Edit) bool { return ed.Ref.Name == name && ed.Spec == nil && ed.Base > 0 }) {
			close(goOn)
			t.Fatalf("the hook's deletion isn't made from what it read: %v", edits)
		}
		tuned := &resourcev1.AlertCheck{Address: made.GetAddress(), Every: "5m", ForRoute: made.GetForRoute()}
		apply(t, s.ch, propose(t, s.ch, "Every 5 min", checkEdit(name, tuned)))
		close(goOn)
		if err := <-errs; reasonOf(err) != "plan_stale" {
			t.Errorf("the route's deletion with its check changed meanwhile = %v, want it stale", err)
		}
		if got := s.stored()[name]; got.GetEvery() != "5m" || got.GetForRoute() != "" {
			t.Errorf("the admin's check became %v", got)
		}
	})
}

// Review H2: a change that fails on its server is rolled back with its route's
// check: a route made loses the check it got, a route deleted gets its check back,
// and a rename gets the old domain's check back in place of the new one's.
func TestARollbackPutsARoutesCheckBack(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		rolledBack := func(what string, ch *dockwellv1.Change) {
			t.Helper()
			failed := apply(t, s.ch, ch)
			if failed.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || failed.GetRolledBackBy() == "" {
				t.Fatalf("%s with its route refused = %v, want it failed and rolled back", what, failed)
			}
			if rb := waitWatch(t, watch(t, s.ch, failed.GetRolledBackBy())).last(); rb.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
				t.Fatalf("%s's rollback = %v: %s", what, rb.GetState(), rb.GetError())
			}
		}

		s.refuseNext()
		rolledBack("a new route", propose(t, s.ch, "A domain", s.route("shop", web("shop.example.com"))))
		if got := s.stored(); len(got) != 0 {
			t.Errorf("after a new route's rollback the checks are %v, want none", got)
		}

		apply(t, s.ch, propose(t, s.ch, "A domain", s.route("shop", web("shop.example.com"))))
		id, _ := s.routeOf("shop")
		s.refuseNext()
		rolledBack("a deleted route", propose(t, s.ch, "No domain", s.gone("shop")))
		if got := s.stored()["shop-example-com"]; got.GetForRoute() != id || got.GetAddress() != "https://shop.example.com/" {
			t.Errorf("after a deleted route's rollback its check is %v, want it back", got)
		}

		s.refuseNext()
		rolledBack("a rename", propose(t, s.ch, "Another domain", s.route("shop", web("store.example.com"))))
		got := s.stored()
		if got["shop-example-com"].GetForRoute() != id || got["store-example-com"] != nil {
			t.Errorf("after a rename's rollback the checks are %v, want the old domain's alone", got)
		}
	})
}

// Review M1, M3 and M9: a developer renames a watched domain and undoes it, the
// check following each time; the route's TLS turned off moves its check to HTTP;
// and a developer's own extra check for the route still takes an admin.
func TestADeveloperRenamesAWatchedDomain(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		protectEnv(t, s.ch, s.f, s.env, resourcev1.Protection_PROTECTION_OPEN)
		apply(t, s.ch, propose(t, s.ch, "A domain", s.route("www", web("shop.example.com"))))
		id, _ := s.routeOf("www")
		hold := aMember(t, s.f)
		hold(store.Grant{ProjectID: envProject(t, s.f, s.env), Role: store.RoleDeveloper})

		renamed := apply(t, s.ch, propose(t, s.ch, "Another domain", s.route("www", web("store.example.com"))))
		if renamed.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("a developer's rename = %v: %s", renamed.GetState(), renamed.GetError())
		}
		if got := s.stored(); got["shop-example-com"] != nil || got["store-example-com"].GetForRoute() != id {
			t.Fatalf("after the rename the checks are %v", got)
		}
		undo, err := s.ch.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: renamed.GetId()}))
		if err != nil {
			t.Fatalf("a developer's undo of the rename: %v", err)
		}
		if got := apply(t, s.ch, undo.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the undo = %v: %s", got.GetState(), got.GetError())
		}
		if got := s.stored(); got["store-example-com"] != nil || got["shop-example-com"].GetForRoute() != id {
			t.Fatalf("after the undo the checks are %v", got)
		}

		// TLS ends before the server now: the same check asks over HTTP.
		plain := web("shop.example.com")
		plain.Tls = ingress.TLSOff
		// Taking HTTPS away is confirmed with the plan's fingerprint.
		applyConfirmed(t, s.ch, propose(t, s.ch, "TLS in front", s.route("www", plain)))
		if got := s.stored()["shop-example-com"]; got.GetAddress() != "http://shop.example.com/" || got.GetForRoute() != id {
			t.Errorf("after TLS went off the route's check is %v", got)
		}

		// A second check of the route's, made by the developer beside an edit of it,
		// isn't the route's own: it takes an admin.
		again := web("shop.example.com")
		again.Tls, again.Port = ingress.TLSOff, 8080
		_, err = s.ch.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Twice", Edits: []*dockwellv1.Edit{
			s.route("www", again),
			checkEdit("shop-twice", &resourcev1.AlertCheck{Address: "http://shop.example.com/", ForRoute: id}),
		}}))
		if err == nil {
			t.Error("a developer made a second check for their route")
		}
	})
}

// Review M4 and M9: a route that says not to be watched says so through Domains, so
// the offer leaves it out; a person's check of it keeps the route's mark only as its
// one check of exactly its address, and taking one on purpose turns watching back on
// in the same plan.
func TestWatchingARouteAgainOnPurpose(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		quiet := web("quiet.example.com")
		quiet.Unwatched = true
		apply(t, s.ch, propose(t, s.ch, "Quiet", s.route("quiet", quiet)))
		domains, err := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON()).ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(domains.Msg.GetDomains(), func(d *dockwellv1.Domain) bool { return d.GetName() == "quiet.example.com" })
		if i < 0 || !domains.Msg.GetDomains()[i].GetRoutes()[0].GetUnwatched() {
			t.Fatalf("Domains doesn't say quiet.example.com isn't watched: %v", domains.Msg.GetDomains())
		}
		id, _ := s.routeOf("quiet")

		// Not its address exactly: the check is the person's.
		apply(t, s.ch, propose(t, s.ch, "Close", checkEdit("close", &resourcev1.AlertCheck{Address: "https://quiet.example.com", ForRoute: id})))
		if got := s.stored()["close"]; got.GetForRoute() != "" {
			t.Errorf("a check of another address kept the route's mark: %v", got)
		}
		if _, rt := s.routeOf("quiet"); !rt.GetUnwatched() {
			t.Error("a check of another address turned watching back on")
		}

		// Its address exactly: the route's, and watched again in the same plan.
		on := propose(t, s.ch, "Watch quiet", checkEdit("quiet-example-com", &resourcev1.AlertCheck{Address: "https://quiet.example.com/", ForRoute: id}))
		if !slices.ContainsFunc(on.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool { return r.GetResource().GetName() == "quiet" }) {
			t.Errorf("watching quiet again doesn't change its route in the plan: %v", on.GetPlan().GetResources())
		}
		apply(t, s.ch, on)
		if got := s.stored()["quiet-example-com"]; got.GetForRoute() != id {
			t.Errorf("the check of the route's address: %v", got)
		}
		if _, rt := s.routeOf("quiet"); rt.GetUnwatched() {
			t.Error("taking the route's check on purpose left the route not watched")
		}

		// A second one isn't the route's.
		apply(t, s.ch, propose(t, s.ch, "Twice", checkEdit("quiet-twice", &resourcev1.AlertCheck{Address: "https://quiet.example.com/", ForRoute: id})))
		if got := s.stored()["quiet-twice"]; got.GetForRoute() != "" {
			t.Errorf("a second check kept the route's mark: %v", got)
		}
	})
}

// Review M5: a project deleted with what's in it takes its routes' checks in the
// same change, and undoing that brings them back with the routes.
func TestAProjectGoesWithItsRoutesChecks(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		apply(t, s.ch, propose(t, s.ch, "Domains", s.route("www", web("shop.example.com")), s.route("blog", web("blog.example.com"))))
		var project string
		if err := s.f.store.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResourceByID(t.Context(), envProject(t, s.f, s.env))
			project = r.Name
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// The organization keeps a project, so a spare one comes first.
		apply(t, s.ch, propose(t, s.ch, "A spare project", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "spare"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Spare"}}},
		}))
		gone := propose(t, s.ch, "Delete the project", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: project}, Delete: true, WithContents: true,
		})
		applyConfirmed(t, s.ch, gone)
		if got := s.stored(); len(got) != 0 {
			t.Fatalf("after the project went its routes' checks are %v", got)
		}
		undo, err := s.ch.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: gone.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		applyConfirmed(t, s.ch, undo.Msg.GetChange())
		got := s.stored()
		if got["shop-example-com"].GetForRoute() == "" || got["blog-example-com"].GetForRoute() == "" {
			t.Errorf("after the undo the checks are %v, want both routes' back", got)
		}
	})
}

// Review M8 and L3: a route of path / serves its whole domain, so it's watched; and
// a check that can't be read doesn't stop a route's change, which says so.
func TestASlashIsTheWholeDomainAndAnUnreadableCheckBlocksNothing(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		apply(t, s.ch, propose(t, s.ch, "Broken soon", checkEdit("broken", &resourcev1.AlertCheck{Address: "https://other.example.com/"})))
		if backend == store.Postgres {
			storetest.Exec(t, s.f.db, `DROP TRIGGER revisions_never_change ON revisions`)
			storetest.Exec(t, s.f.db, `UPDATE revisions SET spec = decode('0a0a', 'hex') WHERE resource_id IN (SELECT id FROM resources WHERE kind = 'alert.check' AND name = 'broken')`)
		} else {
			storetest.Exec(t, s.f.db, `DROP TRIGGER revisions_never_change`)
			storetest.Exec(t, s.f.db, `UPDATE revisions SET spec = x'0a0a' WHERE resource_id IN (SELECT id FROM resources WHERE kind = 'alert.check' AND name = 'broken')`)
		}
		slash := web("shop.example.com")
		slash.Path = "/"
		res, err := s.ch.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "A domain", Edits: []*dockwellv1.Edit{s.route("www", slash)}}))
		if err != nil {
			t.Fatalf("a route's change with a check that can't be read: %v", err)
		}
		notes := res.Msg.GetChange().GetPlan().GetNotes()
		i := slices.IndexFunc(notes, func(n *dockwellv1.PlanNote) bool { return n.GetReason() == watchroutes.NoteUnreadable })
		if i < 0 {
			t.Errorf("the plan doesn't say a check can't be read: %v", notes)
		} else if n := notes[i]; len(n.GetNames()) > 0 || n.GetResource().GetName() != "" || strings.Contains(n.GetMessage(), "broken") {
			// Whoever plans a route may not read checks: the server log names it (the re-check's N6).
			t.Errorf("the plan's note names the check it can't read: %v", n)
		}
		apply(t, s.ch, res.Msg.GetChange())
		id, _ := s.routeOf("www")
		var made bool
		if err := s.f.store.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResource(t.Context(), string(alert.CheckKind), "", "shop-example-com")
			if err != nil {
				org, oerr := tx.Organization(t.Context())
				if oerr != nil {
					return oerr
				}
				r, err = tx.GetResource(t.Context(), string(alert.CheckKind), org.ID, "shop-example-com")
			}
			if err != nil {
				return err
			}
			spec, err := s.f.keeper.Decode(t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			made = err == nil && spec.(*resourcev1.AlertCheck).GetForRoute() == id
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !made {
			t.Error("a route of path / got no check")
		}
	})
}

// Re-check N1: a project developer deletes an open environment that has a watched
// route, and its check goes with it, authorized by the route the deletion takes. A
// protected one can be planned, and applying it takes the environment's role.
func TestADeveloperDeletesAnEnvironmentWithAWatchedRoute(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		project := envProject(t, s.f, s.env)
		compose := dockwellv1connect.NewComposeServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		// An environment with web in it and a watched route, as an admin makes them.
		withRoute := func(name, domain string, p resourcev1.Protection) {
			t.Helper()
			apply(t, s.ch, propose(t, s.ch, "Add "+name, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: project, Name: name},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
			}))
			res, err := compose.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: scope.DefaultProject, Environment: name, Compose: watchStack}))
			if err != nil {
				t.Fatalf("ImportCompose into %s: %v", name, err)
			}
			apply(t, s.ch, res.Msg.GetChange())
			env := res.Msg.GetChange().GetPlan().GetResources()[0].GetResource().GetScope()
			apply(t, s.ch, propose(t, s.ch, "A domain in "+name, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "www"},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: web(domain)}},
			}))
			protectEnv(t, s.ch, s.f, env, p)
		}
		withRoute("staging", "staging.example.com", resourcev1.Protection_PROTECTION_OPEN)
		withRoute("prod", "prod.example.com", resourcev1.Protection_PROTECTION_PROTECTED)
		if got := s.stored(); got["staging-example-com"].GetForRoute() == "" || got["prod-example-com"].GetForRoute() == "" {
			t.Fatalf("the environments' routes' checks: %v", got)
		}
		hold := aMember(t, s.f)
		hold(store.Grant{ProjectID: project, Role: store.RoleDeveloper})
		gone := func(name string) *dockwellv1.Edit {
			return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: project, Name: name}, Delete: true, WithContents: true}
		}

		applyConfirmed(t, s.ch, propose(t, s.ch, "Delete staging", gone("staging")))
		if got := s.stored(); got["staging-example-com"] != nil || got["prod-example-com"] == nil {
			t.Fatalf("after staging went the checks are %v", got)
		}

		planned := propose(t, s.ch, "Delete prod", gone("prod"))
		_, err := s.ch.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: planned.GetId(), Fingerprint: planned.GetPlan().GetFingerprint()}))
		if reasonOf(err) != "needs_role" {
			t.Errorf("a developer applying prod's deletion = %v, want it waiting for the environment's role", err)
		}
		if s.stored()["prod-example-com"] == nil {
			t.Error("prod's check went though its deletion wasn't applied")
		}
	})
}
