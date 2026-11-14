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
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/pki"
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
	// check is what DNS says of every domain as a plan is made. Nil says nothing.
	check *resource.DomainCheck
}

func (m *routesInMemory) Observe(_ context.Context, ref resource.Ref) (*resourcev1.Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes[ref], nil
}

func (*routesInMemory) Check(context.Context, resource.Ref, []resource.Desired) error { return nil }
func (m *routesInMemory) Judge(_ context.Context, _ resource.Ref, spec *resourcev1.Route, _ []resource.Desired) (*resource.IngressEffect, *resource.DomainCheck, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.check == nil {
		return nil, nil, nil
	}
	c := *m.check
	c.Domain = spec.GetDomain()
	return nil, &c, nil
}

func (*routesInMemory) Goes(context.Context, resource.Ref, []resource.Desired) *resource.RouteGoes {
	return nil
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
func (*routesInMemory) Listening(context.Context, uint32) (string, bool) { return "", false }
func (*routesInMemory) Verify(context.Context, resource.Ref) error       { return nil }
func (*routesInMemory) Ready(context.Context) error                      { return nil }
func (m *routesInMemory) ReadyKey() any                                  { return m }

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
		if what == "a route to the server's port" && ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: "www"}] != nil {
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
	port := &resourcev1.Route{Domain: "app.example.net", HostPort: 8080, TrustedProxies: []string{"203.0.113.0/24"}}
	ch := propose(t, script, "Serve what's on the server", edit("www", port))
	refused("a route to the server's port", ch, "127.0.0.1:8080")
	if got := ch.GetPlan().GetResources()[0].GetHostAccess(); len(got) < 1 || got[0].GetKind() != dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PORT || got[0].GetPath() != "127.0.0.1:8080" || got[0].GetDetail() != "app.example.net" {
		t.Errorf("the plan names %v, want the port and where", got)
	}
	applied("a person's route to the server's port", person, ch)

	// What a person approved stays a token's to keep, and to narrow.
	kept := propose(t, script, "Believe less", edit("www", &resourcev1.Route{Domain: "app.example.net", HostPort: 8080}))
	if kept.GetPlan().GetHostAccess() {
		t.Errorf("a token's edit that keeps the port and believes less plans host access: %v", kept.GetPlan())
	}
	applied("a token's edit that keeps the port", script, kept)
	// Anything about where it answers is another thing to approve: another domain,
	// off TLS, and a path, which reaches more or less of what's there.
	refused("another domain", propose(t, script, "Another domain", edit("www", &resourcev1.Route{Domain: "anything.example.org", HostPort: 8080})), "anything.example.org")
	refused("another domain, off TLS", propose(t, script, "Another domain, plainly", edit("www", &resourcev1.Route{Domain: "anything.example.org", HostPort: 8080, Tls: "off"})), "over plain HTTP")
	refused("the same domain, off TLS", propose(t, script, "Plainly", edit("www", &resourcev1.Route{Domain: "app.example.net", HostPort: 8080, Tls: "off"})), "over plain HTTP")
	refused("under a path", propose(t, script, "Under a path", edit("www", &resourcev1.Route{Domain: "app.example.net", Path: "/api", HostPort: 8080})), "app.example.net/api")
	// Another port is another thing to reach.
	refused("another port", propose(t, script, "Another port", edit("www", &resourcev1.Route{Domain: "app.example.net", HostPort: 9090})), "127.0.0.1:9090")
	// As is the first port asked for on another route.
	refused("a new route to it", propose(t, script, "A second route", edit("db", &resourcev1.Route{Domain: "db.example.net", HostPort: 8080})), "127.0.0.1:8080")

	// Going round doesn't get there: a delete and its undo, or by way of a service.
	remove := propose(t, script, "Gone", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "www"}, Delete: true})
	applied("a token removing it", script, remove)
	undoGone, err := script.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: remove.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	refused("undoing that", undoGone.Msg.GetChange(), "127.0.0.1:8080")
	applied("a person puts it back", person, propose(t, person, "Back", edit("www", &resourcev1.Route{Domain: "app.example.net", HostPort: 8080})))
	applied("a token sends it to a service", script, propose(t, script, "To the service", edit("www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080})))
	refused("and back to the port", propose(t, script, "Back to the port", edit("www", &resourcev1.Route{Domain: "app.example.net", HostPort: 8080})), "127.0.0.1:8080")

	// Docker's API is no one's to route to.
	for who, c := range map[string]dockwellv1connect.ChangeServiceClient{"a token": script, "a person": person} {
		for _, p := range []uint32{2375, 2376} {
			_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Docker", Edits: []*dockwellv1.Edit{edit("docker", &resourcev1.Route{Domain: "docker.example.net", HostPort: p})}}))
			if err == nil || !strings.Contains(err.Error(), "Docker's API") {
				t.Errorf("%s planning a route to port %d = %v", who, p, err)
			}
		}
	}

	// A proxy to believe: any at all needs a person, a preset too.
	refused("a preset", propose(t, script, "Believe Cloudflare", edit("www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080, TrustedProxies: []string{"cloudflare"}})), "cloudflare")
	refused("a range", propose(t, script, "Believe a range", edit("www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080, TrustedProxies: []string{"203.0.113.0/24"}})), "203.0.113.0/24")
	two := &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080, TrustedProxies: []string{"203.0.113.0/24", "cloudflare"}}
	applied("a person's proxies", person, propose(t, person, "Believe both", edit("www", two)))

	// Trusting everyone is no one's to do: it isn't naming a proxy, so a person
	// can't approve it either.
	for _, everyone := range []string{"0.0.0.0/0", "::/0", "0.0.0.0/1", "128.0.0.0/1", "::ffff:0:0/96", "8.0.0.0/8", "2a00::/8"} {
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

	// Taking a live domain off HTTPS is a person's, as https:// stops answering. It's
	// said plainly, and needs no less for being done in two steps.
	off := propose(t, script, "Off HTTPS", edit("www", &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 8080, TrustedProxies: []string{"cloudflare"}, Tls: "off"}))
	refused("taking a live domain off HTTPS", off, "HTTPS on app.example.net, which stops")
	if !strings.Contains(hostAccessOf(off), "HOST_ACCESS_KIND_HTTPS_OFF app.example.net") {
		t.Errorf("the plan says %s", hostAccessOf(off))
	}
	secure := func(domain string) *resourcev1.Route {
		return &resourcev1.Route{Domain: domain, Service: "web", Port: 8080}
	}
	applied("a token's second domain", script, propose(t, script, "Second", edit("second", secure("second.example.net"))))
	swap := propose(t, script, "Swap it plain", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "second"}, Delete: true},
		edit("third", &resourcev1.Route{Domain: "second.example.net", Service: "web", Port: 8080, Tls: "off"}))
	refused("a delete and a plain route in one change", swap, "HTTPS on second.example.net, which stops")
	applied("a token deleting it", script, propose(t, script, "Gone", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "second"}, Delete: true}))
	refused("a plain route after a delete", propose(t, script, "Back plainly", edit("third", &resourcev1.Route{Domain: "second.example.net", Service: "web", Port: 8080, Tls: "off"})), "HTTPS on second.example.net, which stops")
	applied("a person's", person, off)

	// A new domain with no certificate says so, and needs no one: it answers only this
	// server, and a token can already deploy anything to this service.
	plain := propose(t, script, "Behind a tunnel", edit("fresh", &resourcev1.Route{Domain: "fresh.example.net", Service: "web", Port: 8080, Tls: "off"}))
	if plain.GetPlan().GetHostAccess() || plain.GetPlan().GetDestructive() {
		t.Errorf("a plain route needs a person: %v", plain.GetPlan())
	}
	var said, answers bool
	for _, server := range plain.GetPlan().GetServers() {
		for _, step := range server.GetSteps() {
			said = said || step.GetUnencrypted()
			answers = answers || step.GetAnswers().GetReach() == dockwellv1.RouteReach_ROUTE_REACH_THIS_SERVER
		}
	}
	if !said || !answers {
		t.Errorf("the plan doesn't say it's unencrypted, and who it answers: %v", plain.GetPlan())
	}
	applied("a token's plain route", script, plain)
	// Going on editing it is a token's too, as nothing about it gets more public.
	applied("a token editing a plain route", script, propose(t, script, "Under a path", edit("fresh", &resourcev1.Route{Domain: "fresh.example.net", Path: "/x", Service: "web", Port: 8080, Tls: "off"})))

	// Answering anyone goes around whatever is in front, so a person says so, for each
	// domain. A proxy to answer widens it too, and that's a proxy to believe, above.
	anyone := propose(t, script, "To anyone", edit("fresh", &resourcev1.Route{Domain: "fresh.example.net", Path: "/x", Service: "web", Port: 8080, Tls: "off", PlainForAnyone: true}))
	refused("a plain route for anyone", anyone, "for anyone who can reach this server")
	applied("a person's", person, anyone)
	refused("anyone on another domain", propose(t, script, "Elsewhere", edit("fresh", &resourcev1.Route{Domain: "else.example.net", Service: "web", Port: 8080, Tls: "off", PlainForAnyone: true})), "else.example.net")
	refused("a proxy for a plain route", propose(t, script, "Answer a proxy", edit("other", &resourcev1.Route{Domain: "else.example.net", Service: "web", Port: 8080, Tls: "off", TrustedProxies: []string{"203.0.113.9"}})), "203.0.113.9/32")
}

// What a plan needs a person for is part of its fingerprint, as that's the only thing
// that stops a plan made before a domain had a certificate applying after it. A token
// plans a plain route on a new name, which needs nobody. The name then has a
// certificate and loses it, so the same route now takes that domain off HTTPS, which
// needs a person, and the old plan says nothing of that.
func TestAPlanFromBeforeADomainsCertificateDoesntApplyAfter(t *testing.T) {
	storetest.Each(t, testAPlanFromBeforeACertificate)
}

func testAPlanFromBeforeACertificate(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	person := changeClient(b, b.origin)
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	first := importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", nil, false)
	if got := apply(t, person, first); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the service: %v", got)
	}
	env := first.GetPlan().GetResources()[0].GetResource().GetScope()
	ref := func(name string) *dockwellv1.ResourceRef {
		return &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: name}
	}
	on := func(name string, tls string) *dockwellv1.Edit {
		return &dockwellv1.Edit{Resource: ref(name), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "stale.example.net", Service: "web", Port: 80, Tls: tls}}}}
	}

	old := propose(t, script, "Plain, before anything", on("plain", "off"))
	if old.GetPlan().GetHostAccess() {
		t.Fatalf("a new plain name plans host access: %s", hostAccessOf(old))
	}
	if got := apply(t, script, propose(t, script, "Its first certificate", on("secure", ""))); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the certificate: %v", got)
	}
	if got := apply(t, script, propose(t, script, "And its going", &dockwellv1.Edit{Resource: ref("secure"), Delete: true})); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("its going: %v", got)
	}
	again := propose(t, script, "The same, now", on("plain", "off"))
	if !again.GetPlan().GetHostAccess() {
		t.Fatalf("the same route now plans no host access, so this proves nothing: %s", hostAccessOf(again))
	}

	_, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: old.GetId(), Fingerprint: old.GetPlan().GetFingerprint()}))
	if err == nil {
		t.Fatal("a token applied a plan from before the domain's certificate, which now takes it off HTTPS with no person")
	}
	t.Logf("refused: %v", err)
	if _, there := ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: "plain"}]; there {
		t.Error("the route was made")
	}
}

// hostAccessOf lists what a change's plan says a person approves.
func hostAccessOf(ch *dockwellv1.Change) string {
	var says []string
	for _, r := range ch.GetPlan().GetResources() {
		for _, a := range r.GetHostAccess() {
			says = append(says, a.GetKind().String()+" "+a.GetPath())
		}
	}
	return strings.Join(says, ", ")
}

// A route moved to a server no person approved anything on is another approval, as
// what's on a port there is another thing.
func TestAPortOnAnotherServerNeedsAPerson(t *testing.T) { storetest.Each(t, testAPortOnAnotherServer) }

func testAPortOnAnotherServer(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	person := changeClient(b, b.origin)
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	first := importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", nil, false)
	if got := apply(t, person, first); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the service: %v", got)
	}
	env := first.GetPlan().GetResources()[0].GetResource().GetScope()
	edit := func(name string, r *resourcev1.Route, servers ...string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: r}},
			Servers:  servers,
		}
	}
	there := &resourcev1.Route{Domain: "internal.example.net", HostPort: 8080}
	applyConfirmed(t, person, propose(t, person, "Serve what's on the server", edit("www", there)))
	other := f.addRouteServer(t, "hz-2", &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}})
	res, err := script.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Move", Edits: []*dockwellv1.Edit{edit("www", there, other)}}))
	if err != nil {
		t.Fatalf("moving: %v", err)
	}
	if !res.Msg.GetChange().GetPlan().GetHostAccess() {
		t.Errorf("a token moved a route to another server's port with no person: %v", res.Msg.GetChange().GetPlan())
	}
}

// addRouteServer is a second server that runs routes.
func (f *fullServer) addRouteServer(t *testing.T, name string, ing *routesInMemory) string {
	t.Helper()
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, name)
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		drivers: resource.NewRegistry(&route.Driver{Ingress: ing}),
	}
	s.connect(t, f)
	return joined.Server.ID
}

// A token can publish a loopback port that a person approved a route to, while it's
// free, and that route's traffic then goes to its container. docs/security/ingress.md
// says so, and this is what it says: a published port isn't host access, as it never
// was. A person approves a port on the understanding that something there answers.
func TestATokenCanPublishALoopbackPort(t *testing.T) { storetest.Each(t, testATokenCanPublish) }

func testATokenCanPublish(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addServerWith(t, func() []resource.Type { return nil })
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := bearer{token: token, rt: b.client.Transport}
	ch := importCompose(t, dockwellv1connect.NewComposeServiceClient(script, b.origin, connect.WithProtoJSON()), "services:\n  fake:\n    image: ghcr.io/acme/shop:1.4\n    ports: [\"127.0.0.1:8080:8080\"]\n", nil, false)
	if ch.GetPlan().GetHostAccess() {
		t.Errorf("this has changed, so docs/security/ingress.md should too: %v", ch.GetPlan())
	}
}
