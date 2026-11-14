// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

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
