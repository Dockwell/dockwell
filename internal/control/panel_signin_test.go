// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/oidc/oidctest"
	"github.com/dockwell/dockwell/internal/store"
)

// The panel on its own domain signs someone in with a provider, from end to end:
// an admin turns the provider on for Dockwell in a change, the owner links their
// account with their passkey, and after signing out, signing in with the provider
// goes there and comes back through the panel's way back, signed in. The way back
// answers a page that goes on to the panel, with the session cookie, and clears the
// flow's cookie; a second use of it signs no one in (ADR 0095).
func TestThePanelSignsInWithAProvider(t *testing.T) {
	cert, roots := selfSigned(t, "panel.example.com")
	idp := oidctest.NewProvider(t)
	srv := startFullConfig(t, store.SQLite, &https.Static{Name: "panel.example.com", Cert: cert}, slog.New(slog.DiscardHandler), nil, logapi.Options{},
		func(c *Config) { c.SigninClient = idp.Srv.Client() })
	_, port, _ := net.SplitHostPort(srv.https)
	origin := "https://panel.example.com:" + port
	b := newBrowser(t, origin, srv.https, roots)
	link, err := srv.auth.CreateClaimLink(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	b.claim(t, link.Token)
	c := changeClient(b, b.origin)
	made := apply(t, c, propose(t, c, "Ours", providerEdit("ours", &resourcev1.SigninProvider{
		DisplayName: "Ours", Issuer: idp.Issuer(), ClientId: oidctest.Client, ClientSecret: oidctest.Secret, Panel: true,
	})))
	id := made.GetPlan().GetResources()[0].GetId()
	a := b.authClient()
	state, err := a.GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if err != nil || len(state.Msg.GetProviders()) != 1 || state.Msg.GetProviders()[0].GetDisplayName() != "Ours" {
		t.Fatalf("the auth state offers %v, %v", state.Msg.GetProviders(), err)
	}

	// back goes to the provider at target, signs in there as Mira's account, and comes
	// back to the panel's way back, which answers what back returns.
	back := func(target string) (*http.Response, string) {
		t.Helper()
		to, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		q := to.Query()
		if q.Get("redirect_uri") != origin+auth.ProviderCallbackPath {
			t.Fatalf("the provider is told to come back to %q", q.Get("redirect_uri"))
		}
		now := time.Now()
		idp.ID = func(url.Values) map[string]any {
			claims := oidctest.GoodClaims()
			claims["nonce"], claims["iat"], claims["exp"] = q.Get("nonce"), now.Unix(), now.Add(time.Hour).Unix()
			return claims
		}
		way := origin + auth.ProviderCallbackPath + "?" + url.Values{"code": {"abc"}, "state": {q.Get("state")}}.Encode()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, way, http.NoBody)
		// A top-level navigation from the provider's site, as a browser sends it.
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		res, err := b.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		page, _ := io.ReadAll(res.Body)
		return res, string(page)
	}

	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_LINK_PROVIDER, id)
	begun, err := a.BeginLinkProvider(t.Context(), connect.NewRequest(&dockwellv1.BeginLinkProviderRequest{Provider: id, Proof: proof}))
	if err != nil {
		t.Fatal(err)
	}
	if res, page := back(begun.Msg.GetUrl()); res.StatusCode != http.StatusOK || !strings.Contains(page, "Linked your account at Ours") {
		t.Fatalf("linking comes back %d: %s", res.StatusCode, page)
	}
	links, err := a.ListProviderLinks(t.Context(), connect.NewRequest(&dockwellv1.ListProviderLinksRequest{}))
	if err != nil || len(links.Msg.GetLinks()) != 1 || links.Msg.GetLinks()[0].GetState() != dockwellv1.ProviderLinkState_PROVIDER_LINK_STATE_WORKS {
		t.Fatalf("the links are %v, %v", links.Msg.GetLinks(), err)
	}

	if _, err := a.SignOut(t.Context(), connect.NewRequest(&dockwellv1.SignOutRequest{})); err != nil {
		t.Fatal(err)
	}
	started, err := a.BeginProviderSignIn(t.Context(), connect.NewRequest(&dockwellv1.BeginProviderSignInRequest{Provider: id}))
	if err != nil {
		t.Fatal(err)
	}
	if set := started.Header().Get("Set-Cookie"); !strings.Contains(set, auth.ProviderFlowCookie+"=") || !strings.Contains(set, "SameSite=Lax") || !strings.Contains(set, "Secure") {
		t.Errorf("the flow's cookie is %q", set)
	}
	res, page := back(started.Msg.GetUrl())
	if res.StatusCode != http.StatusOK || !strings.Contains(page, `http-equiv="refresh" content="0;url=/"`) {
		t.Fatalf("signing in comes back %d: %s", res.StatusCode, page)
	}
	cookies := strings.Join(res.Header.Values("Set-Cookie"), " | ")
	if !strings.Contains(cookies, auth.SecureCookieName+"=") || !strings.Contains(cookies, auth.ProviderFlowCookie+"=; Path=/; Max-Age=0") {
		t.Errorf("the way back sets %q", cookies)
	}
	state, err = a.GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if err != nil || state.Msg.GetUser() == nil || state.Msg.GetSignInMethod() != dockwellv1.SignInMethod_SIGN_IN_METHOD_PROVIDER {
		t.Fatalf("after the way back, the auth state is %v, %v", state.Msg, err)
	}

	// The flow is used once: its way back again signs no one in.
	again, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, res.Request.URL.String(), http.NoBody)
	b.client.Jar.SetCookies(res.Request.URL, []*http.Cookie{{Name: auth.ProviderFlowCookie, Value: "used"}})
	replay, err := b.client.Do(again)
	if err != nil {
		t.Fatal(err)
	}
	_ = replay.Body.Close()
	if replay.StatusCode != http.StatusBadRequest {
		t.Errorf("a way back again answers %d", replay.StatusCode)
	}
}
