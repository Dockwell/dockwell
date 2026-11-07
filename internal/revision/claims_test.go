// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// memIngress is an ingress in memory. It says what a test tells it to of what
// a step does to the ingress, and of DNS.
type memIngress struct {
	mu     sync.Mutex
	routes map[resource.Ref]*resourcev1.Route
	effect *resource.IngressEffect
	check  *resource.DomainCheck
	checks error
}

func (m *memIngress) Observe(_ context.Context, ref resource.Ref) (*resourcev1.Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes[ref], nil
}

func (m *memIngress) Check(context.Context, resource.Ref, []resource.Desired) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checks
}

func (m *memIngress) Judge(context.Context, resource.Ref, *resourcev1.Route, []resource.Desired) (*resource.IngressEffect, *resource.DomainCheck, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.effect, m.check, nil
}

func (m *memIngress) Apply(_ context.Context, ref resource.Ref, spec *resourcev1.Route, _ *resource.IngressEffect) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if spec == nil {
		delete(m.routes, ref)
		return nil
	}
	m.routes[ref] = spec
	return nil
}

func (*memIngress) Goes(context.Context, resource.Ref, []resource.Desired) *resource.RouteGoes {
	return nil
}

func (*memIngress) Returning(context.Context, resource.Ref, *resourcev1.Route, []resource.Desired) []string {
	return nil
}
func (*memIngress) Listening(context.Context, uint32) (string, bool) { return "", false }
func (*memIngress) Verify(context.Context, resource.Ref) error       { return nil }
func (*memIngress) Ready(context.Context) error                      { return nil }
func (m *memIngress) ReadyKey() any                                  { return m }

const panelDomain = "panel.example.com"

// setupRoutes is an engine with routes and services, on a fake engine and an
// ingress in memory on each server, with an environment staging beside
// production.
func setupRoutes(t *testing.T, b store.Backend, ids ...string) (e *Engine, prod, staging string, ingresses map[string]*memIngress) {
	t.Helper()
	db, _ := storetest.Open(t, b)
	if _, err := Setup(t.Context(), db, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	agents := onDocker{}
	ingresses = map[string]*memIngress{}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		for _, id := range ids {
			if err := tx.CreateServer(t.Context(), store.Server{ID: id, Name: id, CreatedAt: now, CertSerial: id, CertNotAfter: now}); err != nil {
				return err
			}
			engine := dockertest.New(t)
			engine.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
			client, kept := engine.Client(), &volume.Kept{}
			containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Deleted: kept}}
			ing := &memIngress{routes: map[resource.Ref]*resourcev1.Route{}}
			ingresses[id] = ing
			agents[id] = resource.Local{Drivers: resource.NewRegistry(&containers, &service.Driver{Containers: containers},
				&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: kept}}, &network.Driver{Client: client},
				&route.Driver{Ingress: ing, Reserved: panelDomain})}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&container.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{}, &route.Driver{Reserved: panelDomain})
	e = New(db, kinds, agents, secretstest.Keeper(db, kinds), slog.New(slog.DiscardHandler))
	prod = production(t, db)
	apply(t, e, propose(t, e, "Add staging", environment(tree(t, db).Projects[0].ID, "staging")))
	_, env, err := tree(t, db).Resolve("", "staging")
	if err != nil {
		t.Fatal(err)
	}
	staging = env.ID
	apply(t, e, propose(t, e, "Run web", webIn(prod, ids...)))
	apply(t, e, propose(t, e, "Run web in staging", webIn(staging, ids...)))
	return e, prod, staging, ingresses
}

func webIn(env string, servers ...string) Edit {
	return Edit{
		Ref:     resource.Ref{Kind: service.Kind, Scope: env, Name: "web"},
		Spec:    &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}},
		Servers: servers,
	}
}

func routeIn(env, name, domain, path string, servers ...string) Edit {
	return Edit{
		Ref:     resource.Ref{Kind: route.Kind, Scope: env, Name: name},
		Spec:    &resourcev1.Route{Domain: domain, Path: path, Service: "web", Port: 8080},
		Servers: servers,
	}
}

func TestOneDomainAndPathOneRoute(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, staging, _ := setupRoutes(t, b, "srv_A")
		apply(t, e, propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", "")))

		refuseProposal(t, e, "the same domain and path in the same environment", ErrInvalid,
			"app.example.com is already served by route www in default's production, so route again can't claim it",
			routeIn(prod, "again", "app.example.com", ""))
		refuseProposal(t, e, "the same domain and path, written as /", ErrInvalid,
			"app.example.com is already served by route www",
			routeIn(prod, "again", "app.example.com", "/"))
		refuseProposal(t, e, "the same domain and path in another environment", ErrInvalid,
			"app.example.com",
			routeIn(staging, "again", "app.example.com", ""))
		refuseProposal(t, e, "two routes at once in one plan", ErrInvalid,
			"this change has route a and route b both claim new.example.com/api",
			routeIn(prod, "a", "new.example.com", "/api"), routeIn(prod, "b", "new.example.com", "/api"))

		// Another path on the domain is another claim.
		apply(t, e, propose(t, e, "Add api", routeIn(prod, "api", "app.example.com", "/api")))
		// Editing a route to what another holds is a claim too.
		refuseProposal(t, e, "editing a route onto another's", ErrInvalid,
			"app.example.com/api is already served by route api",
			routeIn(prod, "www", "app.example.com", "/api"))
		// A route may keep what it holds, and its neighbours may swap in one
		// change, as what the change deletes doesn't count.
		apply(t, e, propose(t, e, "Keep www", routeIn(prod, "www", "app.example.com", "")))
		apply(t, e, propose(t, e, "Swap", Edit{Ref: resource.Ref{Kind: route.Kind, Scope: prod, Name: "www"}}, routeIn(prod, "home", "app.example.com", "")))
	})
}

// Every project has a production, so a refusal names the project with the
// environment, or two projects' read the same (#112).
func TestARefusalSaysWhichProjectsEnvironment(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A")
		db := e.db
		apply(t, e, propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", "")))
		apply(t, e, propose(t, e, "Add shop", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: tree(t, db).Organization.ID, Name: "shop"}, Spec: &resourcev1.Project{}}))
		_, shop, err := tree(t, db).Resolve("shop", "")
		if err != nil {
			t.Fatal(err)
		}
		if shop.Name != "production" {
			t.Fatalf("shop's environment is %s, want production", shop.Name)
		}
		apply(t, e, propose(t, e, "Run web in shop", webIn(shop.ID, "srv_A")))
		refuseProposal(t, e, "a domain another project's production holds", ErrInvalid,
			"app.example.com is already served by route www in default's production, so route www can't claim it",
			routeIn(shop.ID, "www", "app.example.com", "", "srv_A"))
		refuseProposal(t, e, "a path under another project's domain", ErrInvalid,
			"the domain app.example.com belongs to default's production, where route www serves it, so route login in shop's production can't serve a path under it",
			routeIn(shop.ID, "login", "app.example.com", "/login", "srv_A"))
	})
}

// A token that may only touch staging mustn't be able to claim /login on
// production's domain, which is more of a takeover than the whole name.
func TestADomainLivesInOneEnvironment(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, staging, _ := setupRoutes(t, b, "srv_A")
		apply(t, e, propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", "")))
		refuseProposal(t, e, "a path under another environment's domain", ErrInvalid,
			"the domain app.example.com belongs to default's production, where route www serves it, so route login in default's staging can't serve a path under it",
			routeIn(staging, "login", "app.example.com", "/login"))
		refuseProposal(t, e, "one change splitting a domain between environments", ErrInvalid,
			"the domain new.example.com belongs to",
			routeIn(prod, "a", "new.example.com", "/a"), routeIn(staging, "b", "new.example.com", "/b"))
	})
}

// Two changes planned before either applied: the second finds out as it
// commits, as nothing else stops them both.
func TestAClaimIsCheckedAgainAsItCommits(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, staging, _ := setupRoutes(t, b, "srv_A")
		first := propose(t, e, "One", routeIn(prod, "www", "app.example.com", ""))
		second := propose(t, e, "Two", routeIn(staging, "www", "app.example.com", ""))
		apply(t, e, first)
		err := e.Apply(t.Context(), second.Change)
		if err == nil || !strings.Contains(err.Error(), "already served") {
			t.Errorf("the second claim as it commits = %v, want it refused", err)
		}
	})
}

// A write token can delete production's route and claim its domain from
// elsewhere, in one change or two, so that needs a person, for the undo window.
func TestTakingADomainFromAnotherEnvironmentNeedsAPerson(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, staging, _ := setupRoutes(t, b, "srv_A")
		www := resource.Ref{Kind: route.Kind, Scope: prod, Name: "www"}
		// In the same environment there is nothing to take.
		first := propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", ""))
		if first.HostAccess() {
			t.Errorf("a new domain needs a person: %+v", first.Resources)
		}
		apply(t, e, first)

		// Moving it in one change.
		move := propose(t, e, "Move it", Edit{Ref: www}, routeIn(staging, "www", "app.example.com", ""))
		if !move.HostAccess() {
			t.Errorf("moving a domain to another environment in one change needs no person: %+v", move.Resources)
		}
		if err := e.Discard(t.Context(), move.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}

		// Or in two.
		apply(t, e, propose(t, e, "Delete it", Edit{Ref: www}))
		claim := propose(t, e, "Claim it", routeIn(staging, "www", "app.example.com", ""))
		if !claim.HostAccess() {
			t.Fatalf("claiming a domain another environment held needs no person: %+v", claim.Resources)
		}
		got := claim.Resources[0].HostAccess
		if len(got) != 1 || got[0].Kind != resource.AccessDomain || got[0].Path != "app.example.com" || got[0].Detail != "production" {
			t.Errorf("the plan says %+v, want the domain and who held it", got)
		}
		if steps := claim.Steps(); len(steps) != 1 || len(steps[0].HostAccess) != 1 {
			t.Errorf("the step says %+v, want it to show too", steps)
		}
		words := resource.AccessWords(got[0])
		if !strings.Contains(words, "app.example.com") || !strings.Contains(words, "production") {
			t.Errorf("the words are %q", words)
		}
		if err := e.Discard(t.Context(), claim.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}

		// Once an environment holds it, more of it asks no one, though another
		// held it within the window.
		held := propose(t, e, "Claim it for a moment", routeIn(staging, "www", "app.example.com", ""))
		apply(t, e, held)
		more := propose(t, e, "Add a path", routeIn(staging, "api", "app.example.com", "/api"))
		if more.HostAccess() {
			t.Errorf("a path on a domain its environment holds needs a person: %+v", more.Resources)
		}
		if err := e.Discard(t.Context(), more.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}
		apply(t, e, propose(t, e, "Let it go", Edit{Ref: resource.Ref{Kind: route.Kind, Scope: staging, Name: "www"}}))

		// Its own environment can have it back, though staging held it too,
		// with a person asked.
		back := propose(t, e, "Have it back", routeIn(prod, "www", "app.example.com", ""))
		if !back.HostAccess() {
			t.Errorf("taking it back from staging needs no person: %+v", back.Resources)
		}
		apply(t, e, back)
		apply(t, e, propose(t, e, "Delete it", Edit{Ref: www}))

		// A week on, it is anyone's: a site down that long has been noticed.
		e.now = func() time.Time { return time.Now().Add(UndoWindow + time.Hour) }
		late := propose(t, e, "Claim it late", routeIn(staging, "www", "app.example.com", ""))
		if late.HostAccess() {
			t.Errorf("a domain nobody held for a week needs a person: %+v", late.Resources)
		}
	})
}

// A route edited off a domain gave it up as a deletion does.
func TestEditingARouteOffADomainGivesItUp(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, staging, _ := setupRoutes(t, b, "srv_A")
		apply(t, e, propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", "")))
		apply(t, e, propose(t, e, "Move www", routeIn(prod, "www", "other.example.com", "")))
		claim := propose(t, e, "Claim it", routeIn(staging, "www", "app.example.com", ""))
		if !claim.HostAccess() {
			t.Errorf("claiming what a route was edited off needs no person: %+v", claim.Resources)
		}
	})
}

// A route adds nothing to what its environment already holds.
func TestPointingARouteAtAnotherServiceNeedsNoPerson(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A")
		apply(t, e, propose(t, e, "Add api", Edit{
			Ref:  resource.Ref{Kind: service.Kind, Scope: prod, Name: "api"},
			Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}},
		}))
		apply(t, e, propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", "")))
		edit := routeIn(prod, "www", "app.example.com", "")
		edit.Spec.(*resourcev1.Route).Service = "api"
		p := propose(t, e, "Point it elsewhere", edit)
		if p.HostAccess() {
			t.Errorf("pointing a route at another service in its environment needs a person: %+v", p.Resources)
		}
		apply(t, e, p)
	})
}

func TestARouteReachesAServiceInItsOwnEnvironmentAlone(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, staging, _ := setupRoutes(t, b, "srv_A")
		apply(t, e, propose(t, e, "Add api in staging", Edit{
			Ref:  resource.Ref{Kind: service.Kind, Scope: staging, Name: "api"},
			Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}},
		}))
		other := routeIn(prod, "www", "app.example.com", "")
		other.Spec.(*resourcev1.Route).Service = "api"
		refuseProposal(t, e, "a route to another environment's service", ErrInvalid,
			"route www serves service api, which isn't in its environment", other)
		missing := routeIn(prod, "www", "app.example.com", "")
		missing.Spec.(*resourcev1.Route).Service = "nope"
		refuseProposal(t, e, "a route to no service", ErrInvalid, "isn't in its environment", missing)

		apply(t, e, propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", "")))
		refuseProposal(t, e, "deleting a service a route serves", ErrInvalid,
			"service web can't go while route www serves it",
			del(service.Kind, prod, "web"))
		// A route that dials a port on the server serves its service all the same.
		onPort := routeIn(prod, "onport", "port.example.com", "")
		onPort.Spec.(*resourcev1.Route).Port, onPort.Spec.(*resourcev1.Route).HostPort = 0, 8080
		apply(t, e, propose(t, e, "Add onport", onPort))
		refuseProposal(t, e, "deleting a service a route to a port on the server serves", ErrInvalid,
			"service web can't go while route",
			del(service.Kind, prod, "web"), del(route.Kind, prod, "www"))
		apply(t, e, propose(t, e, "Remove onport", del(route.Kind, prod, "onport")))
		// Together, the route goes first.
		gone := propose(t, e, "Retire web", del(service.Kind, prod, "web"), del(route.Kind, prod, "www"))
		if got := stepKinds(gone); got[0] != "delete route" || got[1] != "delete service" {
			t.Errorf("steps %v, want the route to go first", got)
		}
	})
}

func TestARouteMakesItsServiceFirstInAChange(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A")
		edit := routeIn(prod, "api", "api.example.com", "")
		edit.Spec.(*resourcev1.Route).Service = "api"
		p := propose(t, e, "Run api, with its route", edit, Edit{
			Ref:  resource.Ref{Kind: service.Kind, Scope: prod, Name: "api"},
			Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}},
		})
		if got := stepKinds(p); got[0] != "create service" || got[1] != "create route" {
			t.Errorf("steps %v, want the service first", got)
		}
		apply(t, e, p)
	})
}

// What a service reaches isn't its route's: serving traffic to a service that
// mounts a server path doesn't need the person the mount needed.
func TestARouteDoesntTakeOnWhatItsServiceReaches(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A")
		reaching := Edit{
			Ref: resource.Ref{Kind: service.Kind, Scope: prod, Name: "web"},
			Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
				Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/app"}, Target: "/app"}},
			}}},
		}
		if p := propose(t, e, "Mount /srv/app", reaching); !p.HostAccess() {
			t.Fatalf("a mount needs no person: %+v", p.Resources)
		} else {
			apply(t, e, p)
		}
		p := propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", ""))
		if p.HostAccess() {
			t.Errorf("a route to a service with a mount needs a person: %+v", p.Resources)
		}
	})
}

func TestThePanelsDomainCantBeClaimedByAnyone(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A")
		refuseProposal(t, e, "the panel's domain", ErrInvalid, "the panel's own domain", routeIn(prod, "www", panelDomain, ""))
		refuseProposal(t, e, "a path on the panel's domain", ErrInvalid, "the panel's own domain", routeIn(prod, "www", panelDomain, "/x"))
		refuseProposal(t, e, "a name under the panel's domain", ErrInvalid, "under the panel's own domain", routeIn(prod, "www", "x."+panelDomain, ""))
	})
}

func TestARouteRunsOnOneServer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A", "srv_B")
		refuseProposal(t, e, "a route on two servers", ErrInvalid,
			"route www can run on one server at a time", routeIn(prod, "www", "app.example.com", "", "srv_A", "srv_B"))
		refuseProposal(t, e, "a route with no server named, of two", ErrInvalid,
			"say which", routeIn(prod, "www", "app.example.com", ""))
		p := propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", "", "srv_A"))
		apply(t, e, p)
		// A move is one server at a time: the new one's step, then the old's.
		move := propose(t, e, "Move www", routeIn(prod, "www", "app.example.com", "", "srv_B"))
		if len(move.Servers) != 2 {
			t.Fatalf("the move's servers = %+v", move.Servers)
		}
		apply(t, e, move)
	})
}

// DNS may change between a plan and applying it, as people set it after: that
// mustn't stale a plan. What the plan says of the ingress must.
func TestAPlansFingerprintCoversTheIngressAndNotDNS(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, ingresses := setupRoutes(t, b, "srv_A")
		ing := ingresses["srv_A"]
		ing.check = &resource.DomainCheck{Domain: "app.example.com", State: resource.DomainNoRecord}
		ing.effect = &resource.IngressEffect{Change: resource.IngressStarts, Proxy: "Caddy", Ports: []string{"80/tcp"}}
		p := propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", ""))
		steps := p.Steps()
		if len(steps) != 1 || steps[0].Ingress == nil || steps[0].Domain == nil {
			t.Fatalf("the step says %+v", steps)
		}
		if steps[0].Destroys || p.Destructive() {
			t.Errorf("starting the ingress reads as destructive: %+v", steps[0])
		}

		ing.mu.Lock()
		ing.check = &resource.DomainCheck{Domain: "app.example.com", State: resource.DomainHere}
		ing.mu.Unlock()
		if err := e.Apply(t.Context(), p.Change); err != nil {
			t.Errorf("DNS changing stale a plan: %v", err)
		}

		ing.mu.Lock()
		ing.effect = nil
		ing.mu.Unlock()
		next := propose(t, e, "Add api", routeIn(prod, "api", "app.example.com", "/api"))
		ing.mu.Lock()
		ing.effect = &resource.IngressEffect{Change: resource.IngressStarts, Proxy: "Caddy", Ports: []string{"80/tcp"}}
		ing.mu.Unlock()
		err := e.Apply(t.Context(), next.Change)
		if err == nil || !strings.Contains(err.Error(), "changed since") {
			t.Errorf("the ingress's part changing since the plan = %v, want the plan out of date", err)
		}
	})
}

// A route's round trip, as for every kind: made, read back unchanged, undone.
func TestARouteUndoes(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, ingresses := setupRoutes(t, b, "srv_A")
		ing := ingresses["srv_A"]
		ref := resource.Ref{Kind: route.Kind, Scope: prod, Name: "www"}
		p := propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", "/api"))
		apply(t, e, p)
		if !proto.Equal(ing.routes[ref], &resourcev1.Route{Domain: "app.example.com", Path: "/api", Service: "web", Port: 8080}) {
			t.Errorf("the server has %v", ing.routes[ref])
		}
		drift, err := e.Drift(t.Context(), "srv_A")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range drift {
			if d.Ref == ref {
				t.Errorf("a route just made reads as drift: %+v", d)
			}
		}
		apply(t, e, undo(t, e, p))
		if ing.routes[ref] != nil {
			t.Errorf("after the undo the server has %v", ing.routes[ref])
		}
	})
}

// ADR 0016 says a token can't cut off access. A first route on the control server
// moves the panel's domain off the certificate it has, and only a person may do
// that. A person can see it coming: the plan says so, and the fingerprint covers
// it.
func TestMovingThePanelNeedsAPerson(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, ings := setupRoutes(t, b, "srv_A")
		ings["srv_A"].effect = &resource.IngressEffect{Change: resource.IngressStarts, Proxy: "Caddy", Ports: []string{"80/tcp", "443/tcp", "443/udp"}, PanelDomain: panelDomain}
		p := propose(t, e, "First route", routeIn(prod, "www", "app.example.com", ""))
		if !p.HostAccess() {
			t.Fatalf("a write token may apply a plan that moves the panel's domain behind Caddy: %+v", p.Resources)
		}
		access := p.Steps()[0].HostAccess
		if len(access) != 1 || access[0].Kind != resource.AccessPanelDomain || access[0].Path != panelDomain || access[0].Detail != "Caddy" {
			t.Errorf("the step reaches %+v", access)
		}
		if err := e.Discard(t.Context(), p.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}

		// Nothing about the panel moves: nothing to approve.
		ings["srv_A"].effect = &resource.IngressEffect{Change: resource.IngressStarts, Proxy: "Caddy", Ports: []string{"80/tcp", "443/tcp", "443/udp"}}
		p = propose(t, e, "First route", routeIn(prod, "www", "app.example.com", ""))
		if p.HostAccess() {
			t.Errorf("a first route away from the panel asks for a person: %+v", p.Resources)
		}
		apply(t, e, p)

		// The panel taking its ports back cuts nothing off, and removing a route
		// is something a token can do.
		ings["srv_A"].effect = &resource.IngressEffect{Change: resource.IngressStops, Proxy: "Caddy", Ports: []string{"80/tcp"}, PanelDomain: panelDomain}
		p = propose(t, e, "Last route", Edit{Ref: resource.Ref{Kind: route.Kind, Scope: prod, Name: "www"}})
		if p.HostAccess() {
			t.Errorf("the last route's removal asks for a person: %+v", p.Resources)
		}
	})
}

func plainRouteIn(env, name, domain, path string) Edit {
	return Edit{
		Ref:  resource.Ref{Kind: route.Kind, Scope: env, Name: name},
		Spec: &resourcev1.Route{Domain: domain, Path: path, Service: "web", Port: 8080, Tls: "off"},
	}
}

// A domain is served one way or the other. Its plain HTTP would otherwise answer for
// some paths and redirect for others, and its certificate have no one state. The
// engine sees every route and the whole of a change, as a plan made while its server
// was offline never reaches the agent's check, and a step of a plan that changes them
// together can't be told from one that changes one.
func TestADomainsRoutesAgreeOnACertificate(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A")
		apply(t, e, propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", "")))
		refuseProposal(t, e, "a path taking none beside a domain that does", ErrInvalid,
			"the domain app.example.com would be served two ways: route www takes a certificate, and route api takes none",
			plainRouteIn(prod, "api", "app.example.com", "/api"))
		refuseProposal(t, e, "two ways within a change", ErrInvalid,
			"the domain new.example.com would be served two ways",
			routeIn(prod, "a", "new.example.com", "/a"), plainRouteIn(prod, "b", "new.example.com", "/b"))
		apply(t, e, propose(t, e, "A path too", routeIn(prod, "api", "app.example.com", "/api")))
		refuseProposal(t, e, "one of two turned plain", ErrInvalid, "would be served two ways",
			plainRouteIn(prod, "api", "app.example.com", "/api"))
		// Both in one change is what to do, and a plain domain stays that way.
		apply(t, e, propose(t, e, "Both", plainRouteIn(prod, "www", "app.example.com", ""), plainRouteIn(prod, "api", "app.example.com", "/api")))
		refuseProposal(t, e, "a path taking one beside a domain that takes none", ErrInvalid, "would be served two ways",
			routeIn(prod, "more", "app.example.com", "/more"))
		apply(t, e, propose(t, e, "Another", plainRouteIn(prod, "more", "app.example.com", "/more")))
	})
}

// What it takes to go off HTTPS: an edit in place, a delete and a new route in one
// change or two, each counts, and a domain that never had a certificate doesn't.
func TestTakingADomainOffHTTPSNeedsAPerson(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, staging, _ := setupRoutes(t, b, "srv_A")
		off := func(p *Plan) bool {
			for i := range p.Resources {
				for _, a := range p.Resources[i].HostAccess {
					if a.Kind == resource.AccessHTTPSOff && a.Path == "app.example.com" {
						return true
					}
				}
			}
			return false
		}
		ref := resource.Ref{Kind: route.Kind, Scope: prod, Name: "www"}
		apply(t, e, propose(t, e, "Add www", routeIn(prod, "www", "app.example.com", "")))
		if p := propose(t, e, "Off", plainRouteIn(prod, "www", "app.example.com", "")); !off(p) || !p.HostAccess() {
			t.Errorf("an edit off HTTPS plans %+v", p.Resources)
		}
		if p := propose(t, e, "Swap", Edit{Ref: ref}, plainRouteIn(prod, "home", "app.example.com", "")); !off(p) {
			t.Errorf("a delete and a new route in one change plans %+v", p.Resources)
		}
		apply(t, e, propose(t, e, "Gone", Edit{Ref: ref}))
		if p := propose(t, e, "Back, plain", plainRouteIn(prod, "www", "app.example.com", "")); !off(p) {
			t.Errorf("a new route after a delete plans %+v", p.Resources)
		}
		if p := propose(t, e, "Back, elsewhere", plainRouteIn(staging, "www", "app.example.com", "")); !off(p) {
			t.Errorf("a new route in another environment plans %+v", p.Resources)
		}
		// A domain that has only been plain is a token's to serve, and to go on editing.
		first := propose(t, e, "New", plainRouteIn(prod, "new", "new.example.com", ""))
		if first.HostAccess() {
			t.Errorf("a new plain domain plans %+v", first.Resources)
		}
		apply(t, e, first)
		if p := propose(t, e, "Edit", plainRouteIn(prod, "new", "new.example.com", "/x")); p.HostAccess() {
			t.Errorf("an edit of a plain route plans %+v", p.Resources)
		}
		// Once a person has said so, editing it is a token's again.
		applyPlan := propose(t, e, "Back", plainRouteIn(prod, "www", "app.example.com", ""))
		apply(t, e, applyPlan)
		if p := propose(t, e, "Under a path", plainRouteIn(prod, "www", "app.example.com", "/y")); off(p) {
			t.Errorf("an edit of what's plain already plans %+v", p.Resources)
		}
	})
}
