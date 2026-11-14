// SPDX-License-Identifier: AGPL-3.0-only

package signinproviders

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/netguard"
)

// issuerAt is a provider over TLS at an httptest server, whose discovery document
// is what edit leaves of a good one, and a Providers that trusts its certificate.
func issuerAt(t *testing.T, path string, edit func(doc map[string]any)) (*Providers, string) {
	t.Helper()
	var issuer string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != strings.TrimSuffix(path, "/")+"/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		doc := map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "authorize",
			"token_endpoint":                        issuer + "token",
			"jwks_uri":                              issuer + "keys",
			"response_types_supported":              []string{"code", "id_token"},
			"code_challenge_methods_supported":      []string{"plain", "S256"},
			"id_token_signing_alg_values_supported": []string{"RS256", "ES256", "HS256"},
		}
		if edit != nil {
			edit(doc)
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	issuer = srv.URL + path
	return &Providers{Client: srv.Client()}, issuer
}

// An issuer Dockwell can sign people in with is found, with the algorithms it
// signs with that Dockwell takes, strongest first, and never an HMAC; its issuer
// is what the document says, which is what a provider saves.
func TestAnIssuerDockwellCanUseIsFound(t *testing.T) {
	p, issuer := issuerAt(t, "/application/o/dockwell/", nil)
	got, err := p.Check(t.Context(), issuer)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetState() != dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_FOUND || got.GetIssuer() != issuer ||
		strings.Join(got.GetAlgorithms(), " ") != "ES256 RS256" || got.GetPublic() {
		t.Errorf("it reads %v", got)
	}
	// Typed without its trailing "/", it's found as the document says it.
	if got, err := p.Check(t.Context(), strings.TrimSuffix(issuer, "/")); err != nil || got.GetIssuer() != issuer || got.GetState() != dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_FOUND {
		t.Errorf("without its trailing slash it reads %v, %v", got, err)
	}
	// A document that leaves PKCE out may still do it, as Entra's does.
	p, issuer = issuerAt(t, "/", func(doc map[string]any) { delete(doc, "code_challenge_methods_supported") })
	if got, err := p.Check(t.Context(), issuer); err != nil || got.GetState() != dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_FOUND {
		t.Errorf("with no PKCE methods listed it reads %v, %v", got, err)
	}
}

// Each thing in a discovery document that Dockwell can't sign people in with is
// refused on its own, saying what to change at the provider.
func TestAnIssuerDockwellCantUseSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(doc map[string]any)
		want string
	}{
		{"another issuer", func(doc map[string]any) { doc["issuer"] = "https://elsewhere.example.com/" }, "names another issuer, https://elsewhere.example.com/"},
		{"a plain HTTP issuer", func(doc map[string]any) {
			doc["issuer"] = strings.Replace(doc["issuer"].(string), "https:", "http:", 1)
		}, "names another issuer"},
		{"no token endpoint", func(doc map[string]any) { delete(doc, "token_endpoint") }, "has no token endpoint"},
		{"keys over plain HTTP", func(doc map[string]any) { doc["jwks_uri"] = "http://keys.example.com/" }, "keys (jwks_uri) isn't HTTPS"},
		{"no code flow", func(doc map[string]any) { doc["response_types_supported"] = []string{"id_token"} }, "authorization code flow"},
		{"PKCE without S256", func(doc map[string]any) { doc["code_challenge_methods_supported"] = []string{"plain"} }, "PKCE with S256"},
		{"HMAC alone", func(doc map[string]any) { doc["id_token_signing_alg_values_supported"] = []string{"HS256", "none"} }, "signs ID tokens with HS256, none"},
		{"no issuer at all", func(doc map[string]any) { delete(doc, "issuer") }, "isn't a discovery document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, issuer := issuerAt(t, "/", tc.edit)
			got, err := p.Check(t.Context(), issuer)
			if err != nil {
				t.Fatal(err)
			}
			if got.GetState() != dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_UNUSABLE || !strings.Contains(got.GetDetail(), tc.want) {
				t.Errorf("it reads %v, want %q", got, tc.want)
			}
		})
	}
	p, issuer := issuerAt(t, "/application/o/dockwell/", nil)
	got, err := p.Check(t.Context(), strings.Replace(issuer, "dockwell", "another", 1))
	if err != nil || got.GetState() != dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_UNUSABLE || !strings.Contains(got.GetDetail(), "answered 404 for its discovery document, at /application/o/another/.well-known/openid-configuration") {
		t.Errorf("a wrong path reads %v, %v", got, err)
	}
}

// An issuer that can't be reached says so, and why, in a few words; an address
// that isn't an issuer's is refused before anything is read; and netguard's own
// client, which a real control plane uses, never reaches this server's own
// addresses, where a test's provider is.
func TestAnIssuerThatCantBeReadSaysWhy(t *testing.T) {
	p, issuer := issuerAt(t, "/", nil)
	srv := issuer
	p.Client.(*http.Client).Transport.(*http.Transport).CloseIdleConnections()
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	gone := closed.URL
	closed.Close()
	if got, err := p.Check(t.Context(), gone); err != nil || got.GetState() != dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_UNREACHABLE || !strings.Contains(got.GetDetail(), "Dockwell can't read") {
		t.Errorf("a closed issuer reads %v, %v", got, err)
	}
	if _, err := p.Check(t.Context(), "http://auth.example.com"); err == nil || !strings.Contains(err.Error(), "isn't HTTPS") {
		t.Errorf("a plain HTTP issuer reads %v", err)
	}
	guarded := &Providers{Client: netguard.Client(netguard.Options{Private: true})}
	got, err := guarded.Check(context.Background(), srv)
	if err != nil || got.GetState() != dockwellv1.SigninProviderState_SIGNIN_PROVIDER_STATE_UNREACHABLE ||
		!strings.Contains(got.GetDetail(), "Dockwell won't read") || !strings.Contains(got.GetDetail(), "this server's own address") {
		t.Errorf("an issuer on this server's own address reads %v, %v", got, err)
	}
}
