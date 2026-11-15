// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/resource/signinprovider"
	"github.com/dockwell/dockwell/internal/store"
)

// The made-up client secret the tests save a provider with.
const providerSecret = "s3cret-client-secret-for-tests"

func providerEdit(name string, p *resourcev1.SigninProvider) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(signinprovider.Kind), Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_SigninProvider{SigninProvider: p}},
	}
}

// fakeIssuer is a provider's discovery document over TLS, and the client that
// trusts it, for the control plane to read in place of netguard's, which never
// reaches this machine's own addresses.
func fakeIssuer(t *testing.T) (issuer string, client *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "authorize", "token_endpoint": issuer + "token", "jwks_uri": issuer + "keys",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
			"id_token_signing_alg_values_supported": []string{"ES256"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/", srv.Client()
}

// An admin checks an issuer live and saves a provider as a change, with a plan. Its
// secret is in no plan, change or answer, and an update that leaves it out keeps
// it. Below an admin, a provider their projects may use reads as its name alone,
// and checking or saving one is an admin's (ADR 0087).
func TestASigninProviderIsAnAdminsAndItsSecretStaysPut(t *testing.T) {
	t.Parallel()
	issuer, client := fakeIssuer(t)
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.SigninClient = client })
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	dc := dockwellv1connect.NewDomainServiceClient(b, b.origin, connect.WithProtoJSON())

	checked, err := dc.CheckSigninProvider(t.Context(), connect.NewRequest(&dockwellv1.CheckSigninProviderRequest{Issuer: strings.TrimSuffix(issuer, "/")}))
	if err != nil || checked.Msg.GetState() != dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_FOUND || checked.Msg.GetIssuer() != issuer {
		t.Fatalf("checking the issuer = %v, %v", checked, err)
	}
	ch := propose(t, c, "Sign in with our provider", providerEdit("ours", &resourcev1.SigninProvider{
		DisplayName: "Our provider", Issuer: checked.Msg.GetIssuer(), ClientId: "dockwell", ClientSecret: providerSecret, Scopes: []string{"groups"},
	}))
	done := apply(t, c, ch)
	for what, msg := range map[string]proto.Message{"the plan": ch, "the change": done} {
		if b, _ := protojson.Marshal(msg); strings.Contains(string(b), providerSecret) {
			t.Errorf("%s holds the client secret", what)
		}
	}
	listed, err := dc.ListSigninProviders(t.Context(), connect.NewRequest(&dockwellv1.ListSigninProvidersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if p := listed.Msg.GetProviders(); len(p) != 1 || p[0].GetName() != "ours" || p[0].GetDisplayName() != "Our provider" || p[0].GetIssuer() != issuer ||
		!p[0].GetHasSecret() || strings.Join(p[0].GetScopes(), " ") != "groups" {
		t.Errorf("an admin lists %v", p)
	}
	if b, _ := protojson.Marshal(listed.Msg); strings.Contains(string(b), providerSecret) {
		t.Error("the list holds the client secret")
	}
	// An update without the secret keeps it, as the form asks for it once.
	apply(t, c, propose(t, c, "Ask for groups too", providerEdit("ours", &resourcev1.SigninProvider{
		DisplayName: "Our provider", Issuer: issuer, ClientId: "dockwell", Scopes: []string{"groups", "offline_access"},
	})))
	if listed, err = dc.ListSigninProviders(t.Context(), connect.NewRequest(&dockwellv1.ListSigninProvidersRequest{})); err != nil || !listed.Msg.GetProviders()[0].GetHasSecret() {
		t.Errorf("an update without the secret lists %v, %v", listed, err)
	}

	// Below an admin: names alone, and nothing to check or save.
	hold := aMember(t, f)
	hold(onInstall(store.RoleDeveloper))
	trimmed, err := dc.ListSigninProviders(t.Context(), connect.NewRequest(&dockwellv1.ListSigninProvidersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if p := trimmed.Msg.GetProviders(); len(p) != 1 || p[0].GetName() != "ours" || p[0].GetDisplayName() != "Our provider" ||
		p[0].GetIssuer() != "" || p[0].GetClientId() != "" || p[0].GetHasSecret() || len(p[0].GetScopes()) != 0 {
		t.Errorf("a developer lists %v", p)
	}
	_, err = dc.CheckSigninProvider(t.Context(), connect.NewRequest(&dockwellv1.CheckSigninProviderRequest{Issuer: issuer}))
	needsRole(t, "a developer checking an issuer", err)
	if _, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Another", Edits: []*dockwellv1.Edit{
		providerEdit("mine", &resourcev1.SigninProvider{Issuer: issuer, ClientId: "mine"}),
	}})); err == nil {
		t.Error("a developer planned a sign-in provider")
	}
}

// A provider whose projects leave a project out isn't that project's routes' to
// use, and its list doesn't show it.
func TestASigninProviderIsItsProjectsAlone(t *testing.T) {
	t.Parallel()
	issuer, client := fakeIssuer(t)
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.SigninClient = client })
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	dc := dockwellv1connect.NewDomainServiceClient(b, b.origin, connect.WithProtoJSON())
	project := func(name string) *dockwellv1.Edit {
		return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "project", Name: name}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}}}
	}
	made := apply(t, c, propose(t, c, "Projects", project("shop"), project("blog")))
	ids := map[string]string{}
	for _, r := range made.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "project" {
			ids[r.GetResource().GetName()] = r.GetId()
		}
	}
	apply(t, c, propose(t, c, "Shop's provider", providerEdit("shop-auth", &resourcev1.SigninProvider{Issuer: issuer, ClientId: "shop", Projects: []string{ids["shop"]}})))
	for name, want := range map[string]int{"shop": 1, "blog": 0} {
		listed, err := dc.ListSigninProviders(t.Context(), connect.NewRequest(&dockwellv1.ListSigninProvidersRequest{Project: ids[name]}))
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Msg.GetProviders()) != want {
			t.Errorf("%s may use %v", name, listed.Msg.GetProviders())
		}
	}
}

// A provider's edit that stops it signing people in to Dockwell says, in its plan,
// how many people sign in with it: deleting it, turning it off for the panel, or
// moving it to another issuer. Another edit says nothing (ADR 0095).
func TestAProvidersEditSaysWhoSignsInWithIt(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	ours := &resourcev1.SigninProvider{DisplayName: "Ours", Issuer: "https://auth.example.com/", ClientId: "dockwell", Panel: true}
	made := apply(t, c, propose(t, c, "Ours", providerEdit("ours", ours)))
	id := made.GetPlan().GetResources()[0].GetId()
	err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		users, err := tx.ListUsers(t.Context())
		if err != nil || len(users) == 0 {
			return fmt.Errorf("no owner: %w", err)
		}
		return tx.CreateProviderLink(t.Context(), store.ProviderLink{
			ID: "plk_mira", UserID: users[0].ID, ProviderID: id, Issuer: "https://auth.example.com/", Subject: "248289761001",
			Account: "mira@example.com", CreatedAt: time.Now(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	says := func(edit *dockwellv1.Edit) string {
		t.Helper()
		plan := propose(t, c, "Edit ours", edit).GetPlan()
		for _, n := range plan.GetNotes() {
			if n.GetReason() == "provider_links_stop" {
				return strings.Join(n.GetNames(), ",") + ": " + n.GetMessage()
			}
		}
		return ""
	}
	edited := func(edit func(p *resourcev1.SigninProvider)) *dockwellv1.Edit {
		p := proto.Clone(ours).(*resourcev1.SigninProvider)
		edit(p)
		return providerEdit("ours", p)
	}
	deleted := &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(signinprovider.Kind), Name: "ours"}, Delete: true}
	for name, edit := range map[string]*dockwellv1.Edit{
		"deleting it":         deleted,
		"turning it off":      edited(func(p *resourcev1.SigninProvider) { p.Panel = false }),
		"moving its issuer":   edited(func(p *resourcev1.SigninProvider) { p.Issuer = "https://moved.example.com/" }),
		"another client name": edited(func(p *resourcev1.SigninProvider) { p.DisplayName = "Ours, renamed" }),
	} {
		got := says(edit)
		if want := name != "another client name"; want != strings.HasPrefix(got, "1: 1 person signs in to Dockwell with ours") {
			t.Errorf("%s says %q", name, got)
		}
	}
}
