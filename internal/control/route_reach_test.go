// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// routesInMemory is an ingress that does what it's told, as this is about what the
// control plane lets through.
type routesInMemory struct {
	mu     sync.Mutex
	routes map[resource.Ref]*resourcev1.Route
}

func (m *routesInMemory) Observe(_ context.Context, ref resource.Ref) (*resourcev1.Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes[ref], nil
}

func (*routesInMemory) Check(context.Context, resource.Ref, []resource.Desired) error { return nil }
func (*routesInMemory) Judge(context.Context, resource.Ref, *resourcev1.Route, []resource.Desired) (*resource.IngressEffect, *resource.DomainCheck, error) {
	return nil, nil, nil
}

func (*routesInMemory) Returning(context.Context, resource.Ref, *resourcev1.Route, []resource.Desired) []string {
	return nil
}

func (m *routesInMemory) Apply(_ context.Context, ref resource.Ref, spec *resourcev1.Route, _ *resource.IngressEffect) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if spec == nil {
		delete(m.routes, ref)
		return nil
	}
	m.routes[ref] = spec
	return nil
}
func (*routesInMemory) Verify(context.Context, resource.Ref) error { return nil }
func (*routesInMemory) Ready(context.Context) error                { return nil }
func (m *routesInMemory) ReadyKey() any                            { return m }

// What a route can reach and believe is a person's to approve, whatever a token
// asks: the server's own ports reach past containers, and a proxy it believes can
// say any client is anyone. So each is asked of a real token through the API, and
// the server's own refusal is what counts.
func TestARoutesReachNeedsAPerson(t *testing.T) { storetest.Each(t, testARoutesReach) }

func testARoutesReach(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	person := changeClient(b, b.origin)
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)

	// A service gives this its environment, and a route to send to.
	first := importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", nil, false)
	if got := apply(t, person, first); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the service: %v", got)
	}
	env := first.GetPlan().GetResources()[0].GetResource().GetScope()
	edit := func(name string, r *resourcev1.Route) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: r}},
		}
	}
	// refused has a token ask to apply what it planned, and the server say no.
	refused := func(what string, ch *dockwellv1.Change, says string) {
		t.Helper()
		if !ch.GetPlan().GetHostAccess() {
			t.Errorf("%s plans no host access: %v", what, ch.GetPlan())
		}
		_, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}))
		if reasonOf(err) != authapi.ReasonNeedsSession || !strings.Contains(err.Error(), says) {
			t.Errorf("a token applying %s = %v, want needs_session, saying %q", what, err, says)
		}
		if ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: "www"}] != nil && what == "a route to the server's port" {
			t.Errorf("a token's refused change reached the server")
		}
	}
	applied := func(what string, c dockwellv1connect.ChangeServiceClient, ch *dockwellv1.Change) {
		t.Helper()
		w := watch(t, c, ch.GetId())
		if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("%s: %v", what, got)
		}
	}

	// A port on the server itself: a token plans it, and only a person applies it.
	port := &resourcev1.Route{Domain: "app.example.net", HostPort: 8080}
	ch := propose(t, script, "Serve what's on the server", edit("www", port))
	refused("a route to the server's port", ch, "127.0.0.1:8080")
	if got := ch.GetPlan().GetResources()[0].GetHostAccess(); len(got) != 1 || got[0].GetKind() != dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PORT || got[0].GetPath() != "127.0.0.1:8080" {
		t.Errorf("the plan names %v, want the port", got)
	}
	applied("a person's route to the server's port", person, ch)

	// The same port again needs no one: the token already could reach what it had.
	applied("a token's edit that keeps the port", script, propose(t, script, "Under a path", edit("www", &resourcev1.Route{Domain: "app.example.net", Path: "/api", HostPort: 8080})))
	// Another port is another thing to reach.
	refused("another port", propose(t, script, "Another port", edit("www", &resourcev1.Route{Domain: "app.example.net", Path: "/api", HostPort: 9090})), "127.0.0.1:9090")
	// As is the first port asked for on another route.
	refused("a new route to it", propose(t, script, "A second route", edit("db", &resourcev1.Route{Domain: "db.example.net", HostPort: 8080})), "127.0.0.1:8080")

	// A proxy to believe: any at all needs a person, a preset too.
	back := &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080}
	applied("a person sends it back to the service", person, propose(t, person, "Back", edit("www", back)))
	refused("a preset", propose(t, script, "Believe Cloudflare", edit("www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080, TrustedProxies: []string{"cloudflare"}})), "cloudflare")
	refused("a range", propose(t, script, "Believe a range", edit("www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080, TrustedProxies: []string{"203.0.113.0/24"}})), "203.0.113.0/24")
	two := &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080, TrustedProxies: []string{"203.0.113.0/24", "cloudflare"}}
	applied("a person's proxies", person, propose(t, person, "Believe both", edit("www", two)))

	// Trusting everyone is no one's to do: it isn't naming a proxy, so a person
	// can't approve it either.
	for _, everyone := range []string{"0.0.0.0/0", "::/0", "0.0.0.0/1", "128.0.0.0/1", "::ffff:0:0/96"} {
		for who, c := range map[string]dockwellv1connect.ChangeServiceClient{"a token": script, "a person": person} {
			_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Everyone", Edits: []*dockwellv1.Edit{
				edit("www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080, TrustedProxies: []string{everyone}}),
			}}))
			if err == nil {
				t.Errorf("%s planned a route trusting %s", who, everyone)
			}
		}
	}

	// Believing less is a token's to do, and the undo of that isn't.
	narrow := propose(t, script, "Believe less", edit("www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080, TrustedProxies: []string{"cloudflare"}}))
	if narrow.GetPlan().GetHostAccess() {
		t.Errorf("believing less plans host access: %v", narrow.GetPlan())
	}
	applied("a token believing less", script, narrow)
	undo, err := script.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: narrow.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	refused("undoing that", undo.Msg.GetChange(), "203.0.113.0/24")

	// Serving plain HTTP says so and needs no one: a token can already deploy
	// anything to this service.
	plain := propose(t, script, "Behind a tunnel", edit("www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080, TrustedProxies: []string{"cloudflare"}, Tls: "off"}))
	if plain.GetPlan().GetHostAccess() || plain.GetPlan().GetDestructive() {
		t.Errorf("a plain route needs a person: %v", plain.GetPlan())
	}
	var said bool
	for _, server := range plain.GetPlan().GetServers() {
		for _, step := range server.GetSteps() {
			said = said || step.GetUnencrypted()
		}
	}
	if !said {
		t.Errorf("the plan doesn't say traffic reaches the server unencrypted: %v", plain.GetPlan())
	}
	applied("a token's plain route", script, plain)
}
