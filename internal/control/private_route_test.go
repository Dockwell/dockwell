// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/signinprovider"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A private route through the API: a token may make one and keep out more, the
// server gets the password's hash alone, nothing shows it back, it's sealed at rest,
// and only a person opens it again (ADR 0087).
func TestAPrivateRouteThroughTheAPI(t *testing.T) { storetest.Each(t, testAPrivateRouteThroughTheAPI) }

func testAPrivateRouteThroughTheAPI(t *testing.T, backend store.Backend) {
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
	ref := resource.Ref{Kind: route.Kind, Scope: env, Name: "www"}
	edit := func(access *resourcev1.RouteAccess) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "www"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "app.example.net", Service: "web", Port: 80, Access: access}}},
		}
	}
	const password = "correct horse battery staple"
	shows := func(what string, m proto.Message) {
		t.Helper()
		b, err := protojson.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if s := string(b); strings.Contains(s, password) || strings.Contains(s, "$2a$") {
			t.Errorf("%s shows the password or its hash: %s", what, s)
		}
	}

	made := propose(t, script, "Private", edit(&resourcev1.RouteAccess{Allow: []string{"203.0.113.0/24"}, Users: []*resourcev1.RouteUser{{Name: "ci", Password: password}}}))
	if made.GetPlan().GetHostAccess() {
		t.Errorf("a new private route plans host access: %v", made.GetPlan())
	}
	shows("the plan", made)
	if got := apply(t, script, made); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("a token's private route: %v", got)
	}
	hash := ing.routes[ref].GetAccess().GetUsers()[0].GetPasswordHash()
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || ing.routes[ref].GetAccess().GetUsers()[0].GetPassword() != "" {
		t.Fatalf("the server got %v", ing.routes[ref].GetAccess())
	}
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(route.Kind), env, "www")
		if err != nil {
			return err
		}
		if !secrets.Sealed(r.Spec) {
			t.Error("a private route is stored in the clear")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	domains, err := dockwellv1connect.NewDomainServiceClient(b, b.origin).ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	shows("the list of domains", domains.Msg)
	if rs := domains.Msg.GetDomains()[0].GetRoutes(); len(rs) != 1 || strings.Join(rs[0].GetAllow(), ",") != "203.0.113.0/24" || strings.Join(rs[0].GetUsers(), ",") != "ci" {
		t.Errorf("the domain's route reads %v", rs)
	}

	// Keeping out more, naming the user with no password: a token's, and the hash stays.
	narrower := propose(t, script, "Narrower", edit(&resourcev1.RouteAccess{Allow: []string{"203.0.113.7"}, Users: []*resourcev1.RouteUser{{Name: "ci"}}}))
	if narrower.GetPlan().GetHostAccess() {
		t.Errorf("keeping out more plans host access: %v", narrower.GetPlan())
	}
	if got := apply(t, script, narrower); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("a token's narrower list: %v", got)
	}
	if got := ing.routes[ref].GetAccess().GetUsers()[0].GetPasswordHash(); got != hash {
		t.Error("naming the user with no password changed its hash")
	}

	// Letting more in is a person's.
	wider := propose(t, script, "Wider", edit(&resourcev1.RouteAccess{Allow: []string{"203.0.113.7", "198.51.100.0/24"}, Users: []*resourcev1.RouteUser{{Name: "ci"}}}))
	_, err = script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: wider.GetId(), Fingerprint: wider.GetPlan().GetFingerprint()}))
	if reasonOf(err) != authapi.ReasonNeedsSession || !strings.Contains(err.Error(), "lets in 198.51.100.0/24") {
		t.Errorf("a token letting more in = %v, want needs_session, saying what it lets in", err)
	}
	if got := wider.GetPlan().GetResources()[0].GetHostAccess(); len(got) != 1 || got[0].GetKind() != dockwellv1.HostAccessKind_HOST_ACCESS_KIND_ACCESS_OPENED || got[0].GetPath() != "app.example.net" {
		t.Errorf("the plan names %v", got)
	}
	w := watch(t, person, wider.GetId())
	if _, err := person.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: wider.GetId(), Fingerprint: wider.GetPlan().GetFingerprint()})); err != nil {
		t.Fatalf("a person letting more in: %v", err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("a person letting more in: %v", got)
	}
}

// A route that signs people in takes what its server needs of its provider from the
// provider as an admin saved it, whatever the edit says, and its server gets it,
// secret and all; the plan never shows the secret. A provider that isn't there, or
// that the route's project may not use, is refused by name (ADR 0087).
func TestARouteSignsInWithItsProviderAsSaved(t *testing.T) {
	storetest.Each(t, testARouteSignsInWithItsProviderAsSaved)
}

func testARouteSignsInWithItsProviderAsSaved(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	c := changeClient(b, b.origin)
	first := importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", nil, false)
	if got := apply(t, c, first); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the service: %v", got)
	}
	env := first.GetPlan().GetResources()[0].GetResource().GetScope()
	ref := resource.Ref{Kind: route.Kind, Scope: env, Name: "admin"}
	signsIn := func(provider string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "admin"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "admin.example.net", Service: "web", Port: 80, Access: &resourcev1.RouteAccess{
				SignIn: &resourcev1.RouteSignIn{
					Provider: provider, Emails: []string{"mira@example.com"},
					// What a client says of the provider is replaced by what's saved.
					Filled: &resourcev1.RouteSignInProvider{Issuer: "https://elsewhere.example/", ClientId: "theirs", AnyAccount: true},
				},
			}}}},
		}
	}
	refused := func(edit *dockwellv1.Edit, want string) {
		t.Helper()
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Sign in", Edits: []*dockwellv1.Edit{edit}}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("it says %v, want %q", err, want)
		}
	}
	refused(signsIn("ours"), `there's no sign-in provider by that name`)

	saved := propose(t, c, "Our provider", providerEdit("ours", &resourcev1.SigninProvider{
		DisplayName: "Our provider", Issuer: "https://auth.example.com/", ClientId: "dockwell", ClientSecret: providerSecret,
		Scopes: []string{"groups"}, Projects: []string{"prj_someone_elses"},
	}))
	apply(t, c, saved)
	refused(signsIn("ours"), "may not use the sign-in provider ours")

	opened := propose(t, c, "Every project", providerEdit("ours", &resourcev1.SigninProvider{
		DisplayName: "Our provider", Issuer: "https://auth.example.com/", ClientId: "dockwell", Scopes: []string{"groups"},
	}))
	apply(t, c, opened)
	made := propose(t, c, "Sign in to admin", signsIn("ours"))
	if b, _ := protojson.Marshal(made); strings.Contains(string(b), providerSecret) {
		t.Error("the plan shows the provider's client secret")
	}
	if got := apply(t, c, made); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the route: %v", got)
	}
	filled := ing.routes[ref].GetAccess().GetSignIn().GetFilled()
	if filled.GetIssuer() != "https://auth.example.com/" || filled.GetClientId() != "dockwell" || filled.GetClientSecret() != providerSecret ||
		filled.GetAnyAccount() || strings.Join(filled.GetScopes(), ",") != "groups" || filled.GetRevision() == 0 {
		t.Errorf("the server got the provider as %v", filled)
	}
	// The domains list says how the route signs in, with nothing of the provider's own,
	// so the panel can keep it as it edits the domain.
	domains, err := dockwellv1connect.NewDomainServiceClient(b, b.origin).ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := protojson.Marshal(domains.Msg); strings.Contains(string(b), providerSecret) || strings.Contains(string(b), "auth.example.com") {
		t.Errorf("the domains list shows the provider's own: %s", b)
	}
	var listed *resourcev1.RouteSignIn
	for _, d := range domains.Msg.GetDomains() {
		for _, r := range d.GetRoutes() {
			if r.GetName() == "admin" {
				listed = r.GetSignIn()
			}
		}
	}
	if listed.GetProvider() != "ours" || strings.Join(listed.GetEmails(), ",") != "mira@example.com" || listed.GetFilled() != nil {
		t.Errorf("the domains list says the route signs in as %v", listed)
	}

	// At a provider anyone has an account at, anyone at a mailbox domain is anyone at
	// all; at one that's the team's alone, it's who the team let have an account
	// there (#598's final check, L3).
	apply(t, c, propose(t, c, "Google", providerEdit("google", &resourcev1.SigninProvider{Issuer: "https://accounts.google.com", ClientId: "dockwell"})))
	atMailbox := func(provider string) *dockwellv1.Edit {
		e := signsIn(provider)
		e.GetSpec().GetRoute().GetAccess().GetSignIn().EmailDomains = []string{"Gmail.com"}
		return e
	}
	refused(atMailbox("google"), "lets in anyone at Gmail.com, where anyone can make an address")
	propose(t, c, "Anyone at a mailbox, at our own provider", atMailbox("ours"))
}

// A provider's edit reaches every route that signs in with it, in the same change and
// its plan: a rotated secret and another client go to each route's server at once, the
// client marked as opening the route. What would leave a route letting in someone the
// provider no longer does, or with nothing to sign in with, is refused, naming the
// route: taking off every account where a route names no one, leaving out the route's
// project, deleting the provider (ADR 0087).
func TestAProvidersEditReachesItsRoutes(t *testing.T) {
	storetest.Each(t, testAProvidersEditReachesItsRoutes)
}

func testAProvidersEditReachesItsRoutes(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	c := changeClient(b, b.origin)
	first := importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", nil, false)
	if got := apply(t, c, first); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the service: %v", got)
	}
	env := first.GetPlan().GetResources()[0].GetResource().GetScope()
	provider := func(secret, client string, anyAccount bool, projects ...string) *dockwellv1.Edit {
		return providerEdit("ours", &resourcev1.SigninProvider{
			DisplayName: "Our provider", Issuer: "https://auth.example.com/", ClientId: client, ClientSecret: secret, AnyAccount: anyAccount, Projects: projects,
		})
	}
	routeEdit := func(name string, emails ...string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: name},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: name + ".example.net", Service: "web", Port: 80, Access: &resourcev1.RouteAccess{
				SignIn: &resourcev1.RouteSignIn{Provider: "ours", Emails: emails},
			}}}},
		}
	}
	apply(t, c, propose(t, c, "Our provider", provider(providerSecret, "dockwell", true)))
	apply(t, c, propose(t, c, "Sign in", routeEdit("admin", "mira@example.com"), routeEdit("team")))
	admin, team := resource.Ref{Kind: route.Kind, Scope: env, Name: "admin"}, resource.Ref{Kind: route.Kind, Scope: env, Name: "team"}
	if !ing.routes[team].GetAccess().GetSignIn().GetFilled().GetAnyAccount() {
		t.Fatalf("the route naming no one got %v", ing.routes[team].GetAccess().GetSignIn())
	}

	planned := func(ch *dockwellv1.Change) map[string]*dockwellv1.PlannedResource {
		out := map[string]*dockwellv1.PlannedResource{}
		for _, r := range ch.GetPlan().GetResources() {
			out[r.GetResource().GetName()] = r
		}
		return out
	}
	const rotated = "a-rotated-client-secret-for-tests"
	ch := propose(t, c, "Rotate the secret", provider(rotated, "dockwell", true))
	if got := planned(ch); got["admin"] == nil || got["team"] == nil {
		t.Errorf("rotating the secret plans %v, want both routes in it", slices.Collect(maps.Keys(got)))
	}
	if b, _ := protojson.Marshal(ch); strings.Contains(string(b), rotated) {
		t.Error("the plan shows the rotated secret")
	}
	apply(t, c, ch)
	for _, ref := range []resource.Ref{admin, team} {
		if got := ing.routes[ref].GetAccess().GetSignIn().GetFilled().GetClientSecret(); got != rotated {
			t.Errorf("%s's server signs in with the secret %q, want the rotated one", ref.Name, got)
		}
	}

	ch = propose(t, c, "Another client", provider(rotated, "dockwell-2", true))
	if got := planned(ch)["admin"].GetHostAccess(); len(got) != 1 || got[0].GetKind() != dockwellv1.HostAccessKind_HOST_ACCESS_KIND_ACCESS_OPENED || got[0].GetDetail() != "client:dockwell-2" {
		t.Errorf("another client reads as %v on the route, want it opening the route", got)
	}
	w := watchWithin(t, c, ch.GetId(), 20*time.Second)
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}); err != nil {
		t.Fatalf("applying another client, confirmed: %v", err)
	}
	waitWatchWithin(t, w, 20*time.Second)
	if got := ing.routes[admin].GetAccess().GetSignIn().GetFilled().GetClientId(); got != "dockwell-2" {
		t.Errorf("the route's server signs in as %q", got)
	}

	refused := func(what, want string, edit *dockwellv1.Edit) {
		t.Helper()
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: what, Edits: []*dockwellv1.Edit{edit}}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s says %v, want %q", what, err, want)
		}
	}
	refused("taking off every account", "team", provider(rotated, "dockwell-2", false))
	refused("leaving out the route's project", "may not use the sign-in provider ours", provider(rotated, "dockwell-2", true, "prj_someone_elses"))
	refused("deleting the provider", "which admin.example.net signs people in with", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(signinprovider.Kind), Name: "ours"}, Delete: true})
}

// A plan to sign in to a route, made before its provider changed, is stale once the
// provider's edit applies; and a provider's edit, planned before a route started
// signing in with it, is stale once that route's change applies. Either way round,
// the plan applied last would otherwise put back what the provider took away, as
// every account let in (#598's review, New 1).
func TestAPlanMadeBeforeItsProviderChangedIsStale(t *testing.T) {
	storetest.Each(t, testAPlanMadeBeforeItsProviderChangedIsStale)
}

func testAPlanMadeBeforeItsProviderChangedIsStale(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	c := changeClient(b, b.origin)
	first := importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", nil, false)
	apply(t, c, first)
	env := first.GetPlan().GetResources()[0].GetResource().GetScope()
	provider := func(name string, anyAccount bool) *dockwellv1.Edit {
		return providerEdit(name, &resourcev1.SigninProvider{DisplayName: name, Issuer: "https://auth.example.com/", ClientId: "dockwell", ClientSecret: providerSecret, AnyAccount: anyAccount})
	}
	signsIn := func(name, with string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: name},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: name + ".example.net", Service: "web", Port: 80, Access: &resourcev1.RouteAccess{
				SignIn: &resourcev1.RouteSignIn{Provider: with},
			}}}},
		}
	}
	stale := func(what string, ch *dockwellv1.Change, name string) {
		t.Helper()
		_, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}))
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "Propose the change again") {
			t.Errorf("%s applies with %v, want it stale, naming %s", what, err, name)
		}
	}

	// The route's plan first, then the provider's edit.
	apply(t, c, propose(t, c, "Ours", provider("ours", true)))
	team := propose(t, c, "Sign in to team", signsIn("team", "ours"))
	apply(t, c, propose(t, c, "Not every account", provider("ours", false)))
	stale("a route planned before its provider took off every account", team, "team")
	if got := ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: "team"}]; got != nil {
		t.Errorf("the server got the route anyway: %v", got.GetAccess().GetSignIn().GetFilled())
	}

	// The provider's edit first, then the route's.
	apply(t, c, propose(t, c, "Theirs", provider("theirs", true)))
	off := propose(t, c, "Not every account there", provider("theirs", false))
	apply(t, c, propose(t, c, "Sign in to crew", signsIn("crew", "theirs")))
	stale("a provider's edit planned before a route signed in with it", off, "crew")
	if got := ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: "crew"}].GetAccess().GetSignIn().GetFilled(); !got.GetAnyAccount() {
		t.Errorf("crew's server got %v, as the stale edit left it", got)
	}
}
