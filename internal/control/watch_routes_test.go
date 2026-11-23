// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/watchroutes"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

const watchStack = `services:
  web:
    image: nginx:1.27
`

// watchStand is an install with an app, web, routes served in memory, and a way to
// read its checks.
type watchStand struct {
	t   *testing.T
	f   *fullServer
	b   *browser
	ch  dockwellv1connect.ChangeServiceClient
	env string
	ing *routesInMemory
}

func newWatchStand(t *testing.T, backend store.Backend) *watchStand {
	t.Helper()
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	pullsAnything(f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} }))
	ch := changeClient(b, b.origin)
	first := importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), watchStack, nil, false)
	if got := apply(t, ch, first); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the stack: %v", got)
	}
	return &watchStand{t: t, f: f, b: b, ch: ch, env: first.GetPlan().GetResources()[0].GetResource().GetScope(), ing: ing}
}

func (s *watchStand) route(name string, r *resourcev1.Route) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: s.env, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: r}},
	}
}

func (s *watchStand) gone(name string) *dockwellv1.Edit {
	return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: s.env, Name: name}, Delete: true}
}

// checks are the install's checks, by name.
func (s *watchStand) checks() map[string]*dockwellv1.AlertCheckInfo {
	s.t.Helper()
	res, err := dockwellv1connect.NewAlertServiceClient(s.b, s.b.origin).ListAlertChecks(s.t.Context(), connect.NewRequest(&dockwellv1.ListAlertChecksRequest{}))
	if err != nil {
		s.t.Fatal(err)
	}
	out := map[string]*dockwellv1.AlertCheckInfo{}
	for _, c := range res.Msg.GetChecks() {
		out[c.GetName()] = c
	}
	return out
}

// routeID is a route's ID, and its spec, as the store has them.
func (s *watchStand) routeOf(name string) (string, *resourcev1.Route) {
	s.t.Helper()
	var id string
	var rt *resourcev1.Route
	if err := s.f.store.View(s.t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(s.t.Context(), string(route.Kind), s.env, name)
		if err != nil {
			return err
		}
		spec, err := s.f.keeper.Decode(s.t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		id, rt = r.ID, spec.(*resourcev1.Route)
		return err
	}); err != nil {
		s.t.Fatal(err)
	}
	return id, rt
}

func web(domain string) *resourcev1.Route {
	return &resourcev1.Route{Domain: domain, Service: "web", Port: 80}
}

// A route that serves a whole domain gets an uptime check of it in its own change,
// marked as the route's, on the defaults, with a note; removing the route removes it
// in the same change (#835).
func TestARoutedDomainIsWatchedAndTheCheckGoesWithIt(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		made := propose(t, s.ch, "A domain", s.route("www", web("shop.example.com")))
		if !slices.ContainsFunc(made.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool {
			return n.GetReason() == watchroutes.NoteWatches && n.GetMessage() == "Checks shop.example.com every minute"
		}) {
			t.Errorf("the plan doesn't say it watches the domain: %v", made.GetPlan().GetNotes())
		}
		apply(t, s.ch, made)
		id, _ := s.routeOf("www")
		c := s.checks()["shop-example-com"]
		if c.GetSpec().GetAddress() != "https://shop.example.com/" || c.GetSpec().GetForRoute() != id || c.GetSpec().GetEvery() != "1m" || c.GetSpec().GetDownAfter() != 2 {
			t.Fatalf("the route's check: %v", c)
		}
		apply(t, s.ch, propose(t, s.ch, "No domain", s.gone("www")))
		if _, ok := s.checks()["shop-example-com"]; ok {
			t.Error("the check stayed after its route went")
		}
	})
}

// A route that says not to watch, a redirect, a path, and a domain someone already
// watches get no check from Dockwell; saying not to watch later removes it, and
// undoing the route's change takes both back.
func TestWhatARouteDoesntGetWatched(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		quiet := web("quiet.example.com")
		quiet.Unwatched = true
		redirect := &resourcev1.Route{Domain: "old.example.com", Redirect: &resourcev1.RouteRedirect{To: "https://shop.example.com"}}
		api := web("api.example.com")
		api.Path = "/api"
		watched := propose(t, s.ch, "Mine first", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "mine"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: &resourcev1.AlertCheck{Address: "https://blog.example.com/health"}}},
		})
		apply(t, s.ch, watched)
		apply(t, s.ch, propose(t, s.ch, "Routes", s.route("quiet", quiet), s.route("old", redirect), s.route("api", api), s.route("blog", web("blog.example.com"))))
		if got := s.checks(); len(got) != 1 || got["mine"] == nil {
			t.Fatalf("checks after routes that aren't watched: %v", got)
		}

		// Watched, then not: the route's flag removes its check, with no server step.
		apply(t, s.ch, propose(t, s.ch, "A domain", s.route("www", web("shop.example.com"))))
		if s.checks()["shop-example-com"] == nil {
			t.Fatal("no check for shop.example.com")
		}
		off := web("shop.example.com")
		off.Unwatched = true
		unwatched := propose(t, s.ch, "Don't watch it", s.route("www", off))
		apply(t, s.ch, unwatched)
		if s.checks()["shop-example-com"] != nil {
			t.Error("don't watch kept the check")
		}

		// Undoing a route's change takes its check back with it, and nothing else.
		made := apply(t, s.ch, propose(t, s.ch, "Another domain", s.route("shop", web("store.example.com"))))
		if s.checks()["store-example-com"] == nil {
			t.Fatal("no check for store.example.com")
		}
		undo, err := s.ch.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: made.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, s.ch, undo.Msg.GetChange())
		if got := s.checks(); got["store-example-com"] != nil || got["mine"] == nil {
			t.Errorf("checks after the undo: %v", got)
		}
	})
}

// A person's change to a check Dockwell made makes it theirs, so it stays when the
// route goes; deleting one says not to watch its route, so it doesn't come back; and
// a route from before checks were made for routes keeps what it had.
func TestAPersonsHandOnACheckIsKept(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		apply(t, s.ch, propose(t, s.ch, "Domains", s.route("www", web("shop.example.com")), s.route("blog", web("blog.example.com"))))
		c := s.checks()["shop-example-com"]
		tuned := &resourcev1.AlertCheck{Address: c.GetSpec().GetAddress(), Every: "5m", ForRoute: c.GetSpec().GetForRoute()}
		apply(t, s.ch, propose(t, s.ch, "Every 5 min", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "shop-example-com"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: tuned}},
		}))
		if got := s.checks()["shop-example-com"].GetSpec(); got.GetForRoute() != "" || got.GetEvery() != "5m" {
			t.Fatalf("a person's change kept the route's mark: %v", got)
		}
		apply(t, s.ch, propose(t, s.ch, "No domain", s.gone("www")))
		if s.checks()["shop-example-com"] == nil {
			t.Error("a check a person changed went with the route")
		}

		// Deleting the blog's check says not to watch the blog's route.
		apply(t, s.ch, propose(t, s.ch, "Stop watching the blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "blog-example-com"}, Delete: true,
		}))
		if _, rt := s.routeOf("blog"); !rt.GetUnwatched() {
			t.Error("deleting the route's check left its route watched")
		}
		moved := web("blog.example.com")
		moved.Port = 8080
		moved.Unwatched = true
		apply(t, s.ch, propose(t, s.ch, "Another port", s.route("blog", moved)))
		if s.checks()["blog-example-com"] != nil {
			t.Error("the blog's check came back")
		}

		// Watching the blog again makes its check again.
		again := web("blog.example.com")
		again.Port = 8080
		apply(t, s.ch, propose(t, s.ch, "Watch the blog", s.route("blog", again)))
		if s.checks()["blog-example-com"].GetSpec().GetForRoute() == "" {
			t.Error("watching the blog again made no check of its route's")
		}

		// A route that serves its domain with no check of Dockwell's, as one from before
		// routes got checks, keeps none as it changes: those are offered instead. The
		// shop's check is the person's now; once deleted, the route is watched and
		// checkless.
		apply(t, s.ch, propose(t, s.ch, "Domain", s.route("www", web("shop.example.com"))))
		apply(t, s.ch, propose(t, s.ch, "Drop the check", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "shop-example-com"}, Delete: true,
		}))
		if _, rt := s.routeOf("www"); rt.GetUnwatched() {
			t.Fatal("deleting a person's check said not to watch the route")
		}
		ported := web("shop.example.com")
		ported.Port = 8080
		plan := propose(t, s.ch, "Another port", s.route("www", ported))
		if slices.ContainsFunc(plan.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
			return r.GetResource().GetKind() == string(alert.CheckKind)
		}) {
			t.Error("a route that served its domain before got a check as it changed")
		}
	})
}

// A developer adding a route to their project gets its check, though checks are an
// admin's: the route's edit takes the role. A check that isn't Dockwell's own for
// that route still needs an admin (#835).
func TestADevelopersRouteGetsItsCheck(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newWatchStand(t, backend)
		// Open, as a developer's change in a protected environment waits for an admin.
		protectEnv(t, s.ch, s.f, s.env, resourcev1.Protection_PROTECTION_OPEN)
		project := envProject(t, s.f, s.env)
		hold := aMember(t, s.f)
		hold(store.Grant{ProjectID: project, Role: store.RoleDeveloper})
		apply(t, s.ch, propose(t, s.ch, "A domain", s.route("www", web("shop.example.com"))))
		// A developer can't list checks, so they're read as stored.
		var made bool
		if err := s.f.store.View(t.Context(), func(tx *store.Tx) error {
			list, err := tx.ListKind(t.Context(), string(alert.CheckKind))
			made = len(list) == 1 && list[0].Name == "shop-example-com"
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !made {
			t.Fatal("a developer's route got no check")
		}
		id, _ := s.routeOf("www")
		_, err := s.ch.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Inside", Edits: []*dockwellv1.Edit{
			s.route("api", web("api.example.com")),
			{
				Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "api-inside"},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: &resourcev1.AlertCheck{Address: "https://api.example.com/", InsideNetwork: true, ForRoute: id}}},
			},
		}}))
		if err == nil {
			t.Error("a developer made a check of their own, inside the network")
		}
	})
}
