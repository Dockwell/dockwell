// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProvider is an OpenID Connect provider over TLS: its discovery document, its
// keys, and a token endpoint that answers each code with what id says.
type fakeProvider struct {
	srv      *httptest.Server
	signer   signer
	keyReads atomic.Int32
	id       func(form url.Values) map[string]any
}

func newProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{signer: newSigner(t, "ES256", "k1")}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := p.srv.URL + "/"
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": base, "authorization_endpoint": base + "authorize", "token_endpoint": base + "token", "jwks_uri": base + "keys",
				"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
			})
		case "/keys":
			p.keyReads.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{p.signer.jwk}})
		case "/token":
			_ = r.ParseForm()
			if user, secret, ok := r.BasicAuth(); !ok || user != client || secret != "s3cret" || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
				return
			}
			claims := p.id(r.Form)
			claims["iss"] = base
			_ = json.NewEncoder(w).Encode(map[string]string{"id_token": token(p.signer, claims), "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// stand is a Server for one route at the fake provider, letting in example.com.
func stand(t *testing.T, p *fakeProvider) (*Server, *Route) {
	t.Helper()
	route := &Route{
		ID: "rte_admin", Domain: "app.example.net", Path: "/admin", Issuer: p.srv.URL + "/", ClientID: client, ClientSecret: "s3cret",
		EmailDomains: []string{"example.com"}, Groups: []string{"ops"}, Key: aKey(7),
	}
	return &Server{
		Routes: func(id string) (Route, bool) { return *route, id == route.ID },
		Client: p.srv.Client(),
		Now:    func() time.Time { return now },
	}, route
}

// ask is forward_auth for the route, as Caddy sends it, with cookies.
func ask(s *Server, method, uri, accept string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/route/rte_admin", http.NoBody)
	r.Header.Set("X-Forwarded-Method", method)
	r.Header.Set("X-Forwarded-Uri", uri)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

const html = "text/html,application/xhtml+xml"

func cookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no cookie %s in %v", name, w.Result().Header["Set-Cookie"])
	return nil
}

// signIn goes through a sign-in to its callback, with what the provider says.
func signIn(t *testing.T, s *Server, route *Route, p *fakeProvider, claims map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := ask(s, http.MethodGet, "/admin/settings?tab=1", html)
	if w.Code != http.StatusFound {
		t.Fatalf("a navigation answers %d", w.Code)
	}
	to, _ := url.Parse(w.Header().Get("Location"))
	_, flowName := Names(route.ID)
	flow := cookie(t, w, flowName)
	p.id = func(url.Values) map[string]any {
		c := goodClaims()
		c["nonce"] = to.Query().Get("nonce")
		for k, v := range claims {
			c[k] = v
		}
		return c
	}
	return ask(s, http.MethodGet, "/admin"+Prefix+"callback?code=abc&state="+url.QueryEscape(to.Query().Get("state")), html, flow)
}

// A navigation with no sign-in goes to the provider, with PKCE (S256), a state and a
// nonce, and a flow cookie that only this host may set; anything else gets 401.
func TestANavigationGoesToTheProvider(t *testing.T) {
	p := newProvider(t)
	s, route := stand(t, p)
	w := ask(s, http.MethodGet, "/admin/settings", html)
	to, err := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusFound || err != nil || !strings.HasPrefix(to.String(), p.srv.URL+"/authorize?") {
		t.Fatalf("a navigation answers %d, %s", w.Code, w.Header().Get("Location"))
	}
	q := to.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" ||
		q.Get("client_id") != client || q.Get("redirect_uri") != "https://app.example.net/admin"+Prefix+"callback" || q.Get("scope") != "openid email profile" {
		t.Errorf("it sends %v", q)
	}
	_, flowName := Names(route.ID)
	if c := cookie(t, w, flowName); !c.Secure || !c.HttpOnly || c.Path != "/" || c.SameSite != http.SameSiteLaxMode || !strings.HasPrefix(c.Name, "__Host-") {
		t.Errorf("its flow cookie is %+v", c)
	}
	for _, tc := range []struct{ method, accept string }{{http.MethodPost, html}, {http.MethodGet, "application/json"}, {http.MethodGet, ""}} {
		if w := ask(s, tc.method, "/admin/api", tc.accept); w.Code != http.StatusUnauthorized || w.Header().Get("Location") != "" {
			t.Errorf("%s with %q answers %d, %q", tc.method, tc.accept, w.Code, w.Header().Get("Location"))
		}
	}
}

// A sign-in that checks out sets the session cookie, goes back to where it began,
// and lets the person in, with who they are for the app, until they're taken off
// the route, the route signs everyone out, or they sign out.
func TestASignInLetsSomeoneIn(t *testing.T) {
	p := newProvider(t)
	s, route := stand(t, p)
	w := signIn(t, s, route, p, nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/admin/settings?tab=1" {
		t.Fatalf("the callback answers %d, %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	sessionName, _ := Names(route.ID)
	session := cookie(t, w, sessionName)
	in := ask(s, http.MethodPost, "/admin/api", "", session)
	if in.Code != http.StatusOK || in.Header().Get(UserHeader) != "mira@example.com" || in.Header().Get(SubjectHeader) != "248289761001" {
		t.Errorf("signed in, it answers %d, %v", in.Code, in.Header())
	}
	// Taken off: example.com no longer, and ops not among their groups.
	route.EmailDomains, route.Groups = []string{"example.org"}, nil
	if w := ask(s, http.MethodGet, "/admin", html, session); w.Code != http.StatusFound {
		t.Errorf("taken off, they're let in: %d", w.Code)
	}
	route.EmailDomains = []string{"example.com"}
	route.Generation++
	if w := ask(s, http.MethodPost, "/admin/api", "", session); w.Code != http.StatusUnauthorized {
		t.Errorf("signed out by a new generation, they're let in: %d", w.Code)
	}
	route.Generation--
	// Signing out goes to the route's own path, whatever a link says, as anyone can
	// make one (#598's review, M1).
	for _, next := range []string{"%2Fadmin%2Fx", "%2F%09%2Fevil.example%2Fx", "https%3A%2F%2Fevil.example%2F"} {
		out := ask(s, http.MethodGet, "/admin"+Prefix+"out?next="+next, html, session)
		if out.Code != http.StatusFound || out.Header().Get("Location") != "/admin" || cookie(t, out, sessionName).MaxAge >= 0 {
			t.Errorf("signing out with next=%s answers %d, %q, %v", next, out.Code, out.Header().Get("Location"), out.Result().Cookies())
		}
	}
}

// Someone the route doesn't name, or with an email the provider hasn't verified, is
// told they aren't let in, with a way to sign in as someone else; a callback whose
// state isn't the flow's, or with no flow, says the sign-in didn't finish.
func TestASignInTheRouteRefusesSaysSo(t *testing.T) {
	p := newProvider(t)
	s, route := stand(t, p)
	w := signIn(t, s, route, p, map[string]any{"email": "eve@elsewhere.example", "groups": []string{"guests"}})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "eve@elsewhere.example isn&#39;t let in to app.example.net/admin") ||
		!strings.Contains(w.Body.String(), "Sign in as someone else") {
		t.Errorf("someone it doesn't name gets %d: %s", w.Code, w.Body)
	}
	if w := signIn(t, s, route, p, map[string]any{"email_verified": false, "groups": nil}); w.Code != http.StatusForbidden {
		t.Errorf("an unverified email gets %d", w.Code)
	}
	if w := signIn(t, s, route, p, map[string]any{"email": "eve@elsewhere.example", "email_verified": false}); w.Code != http.StatusFound {
		t.Errorf("someone in a group the route names gets %d", w.Code)
	}
	if w := ask(s, http.MethodGet, "/admin"+Prefix+"callback?code=abc&state=forged", html); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "didn&#39;t finish") {
		t.Errorf("a callback with no flow gets %d: %s", w.Code, w.Body)
	}
	// A real flow, with a state that isn't its own, as a forged callback would carry,
	// and a provider that would answer it with a good token for that flow.
	begun := ask(s, http.MethodGet, "/admin", html)
	_, flowName := Names(route.ID)
	to, _ := url.Parse(begun.Header().Get("Location"))
	p.id = func(url.Values) map[string]any {
		c := goodClaims()
		c["nonce"] = to.Query().Get("nonce")
		return c
	}
	if w := ask(s, http.MethodGet, "/admin"+Prefix+"callback?code=abc&state=forged", html, cookie(t, begun, flowName)); w.Code != http.StatusBadRequest {
		t.Errorf("a callback whose state isn't its flow's gets %d", w.Code)
	}
	if h := w.Header(); h.Get("Cache-Control") != "no-store" || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("an end page's headers are %v", h)
	}
	route.ClientSecret = "wrong"
	if w := signIn(t, s, route, p, nil); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "refused Dockwell&#39;s client") {
		t.Errorf("a wrong secret gets %d: %s", w.Code, w.Body)
	}
}

// A provider that rotated its keys is read again for a key it hasn't seen, once,
// and the sign-in goes through.
func TestRotatedKeysAreReadAgainOnce(t *testing.T) {
	p := newProvider(t)
	s, route := stand(t, p)
	if w := signIn(t, s, route, p, nil); w.Code != http.StatusFound {
		t.Fatalf("the first sign-in answers %d: %s", w.Code, w.Body)
	}
	p.signer = newSigner(t, "ES256", "k2")
	s.Now = func() time.Time { return now.Add(2 * time.Minute) }
	if w := signIn(t, s, route, p, nil); w.Code != http.StatusFound {
		t.Errorf("after the provider rotated its keys it answers %d: %s", w.Code, w.Body)
	}
	if got := p.keyReads.Load(); got != 2 {
		t.Errorf("the keys were read %d times, want twice", got)
	}
}

// Where to come back to is a path on this origin, never another site; a route
// exchanges 30 codes a minute and 4 at once at most.
func TestWhereToComeBackIsThisOriginAndExchangesAreBounded(t *testing.T) {
	r := Route{Path: "/admin"}
	for in, want := range map[string]string{
		"/admin/x?y=1": "/admin/x?y=1", "/admin/a%20b": "/admin/a%20b", "": "/admin",
		"//evil.example": "/admin", "https://evil.example/": "/admin", "/\\evil": "/admin",
		// What a browser drops or reads as a slash, as written and once decoded, as a
		// sign-out's next once was (#598's review, M1).
		"/\t/evil.example/x": "/admin", "/\v/evil.example/x": "/admin", "/\x7f/evil.example": "/admin",
		"/%09/evil.example/x": "/admin", "/%0B/evil.example/x": "/admin", "/%5C/evil.example": "/admin", "/%5c%5cevil": "/admin",
		"/%2F/evil.example": "/admin", "/%zz": "/admin", "javascript:alert(1)": "/admin",
	} {
		if got := r.back(in); got != want {
			t.Errorf("back(%q) = %q, want %q", in, got, want)
		}
	}
	s := &Server{Now: func() time.Time { return now }}
	var held []func()
	n := 0
	visitor := func() string { n++; return fmt.Sprintf("198.51.100.%d", n) }
	state := func() string { n++; return fmt.Sprintf("state-%d", n) }
	for range exchangesAtOnce {
		release, ok := s.exchange("r", visitor(), state())
		if !ok {
			t.Fatal("an exchange within the limit is refused")
		}
		held = append(held, release)
	}
	if _, ok := s.exchange("r", visitor(), state()); ok {
		t.Error("a fifth exchange at once goes ahead")
	}
	for _, release := range held {
		release()
	}
	for range exchangesAMinute - exchangesAtOnce {
		release, ok := s.exchange("r", visitor(), state())
		if !ok {
			t.Fatal("an exchange within a minute's limit is refused")
		}
		release()
	}
	if _, ok := s.exchange("r", visitor(), state()); ok {
		t.Error("a 31st exchange in a minute goes ahead")
	}
}

// A sign-in begun is exchanged once, and one visitor's made-up callbacks spend that
// visitor's share alone, so they can't stop everyone else signing in to the route
// (#598's review, L3).
func TestOneVisitorCantSpendARoutesSignIns(t *testing.T) {
	s := &Server{Now: func() time.Time { return now }}
	release, ok := s.exchange("r", "198.51.100.7", "st-1")
	if !ok {
		t.Fatal("a first exchange is refused")
	}
	release()
	if _, ok := s.exchange("r", "198.51.100.8", "st-1"); ok {
		t.Error("a sign-in's state is exchanged twice")
	}
	for i := range exchangesAVisitor - 1 {
		release, ok := s.exchange("r", "198.51.100.7", fmt.Sprintf("st-%d", i+2))
		if !ok {
			t.Fatalf("exchange %d of one visitor's share is refused", i+2)
		}
		release()
	}
	if _, ok := s.exchange("r", "198.51.100.7", "st-more"); ok {
		t.Error("one visitor goes past their share")
	}
	if release, ok := s.exchange("r", "203.0.113.9", "st-other"); !ok {
		t.Error("another visitor is refused for the first one's callbacks")
	} else {
		release()
	}
}

// An end page draws with the tokens design/system/tokens.json has, in both themes.
func TestEndPagesDrawWithTheTokens(t *testing.T) {
	raw, err := os.ReadFile("../../../design/system/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	var tokens struct {
		Color struct {
			Tokens []struct {
				Name  string          `json:"name"`
				Value json.RawMessage `json:"value"`
			} `json:"tokens"`
		} `json:"color"`
	}
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatal(err)
	}
	found := map[string][2]string{}
	for _, tk := range tokens.Color.Tokens {
		var v struct{ Light, Dark string }
		if json.Unmarshal(tk.Value, &v) == nil {
			found[tk.Name] = [2]string{v.Light, v.Dark}
		}
	}
	for name, want := range brand {
		if found[name] != want {
			t.Errorf("--%s is %v here and %v in tokens.json", name, want, found[name])
		}
	}
	css := style()
	if !strings.Contains(css, "prefers-color-scheme:dark") || !strings.Contains(css, `"Host Grotesk"`) {
		t.Errorf("the page's style is %s", css)
	}
}

// An end page escapes what it says, as a domain or a provider's words can hold
// anything, and its one link stays on this site, as written without a template
// (#622).
func TestEndPagesEscapeWhatTheySayAndLinkOnSite(t *testing.T) {
	for _, href := range []string{"https://elsewhere.example/", "//elsewhere.example/", "/\\elsewhere.example/", "javascript:alert(1)", ""} {
		w := httptest.NewRecorder()
		end{Status: http.StatusBadRequest, Title: `<script>"a"</script>`, Said: "it's <b>", Action: "Try & again", Href: href}.write(w)
		body := w.Body.String()
		if strings.Contains(body, "<script>") || strings.Contains(body, "<b>") {
			t.Errorf("a page says what it was given unescaped: %s", body)
		}
		for _, want := range []string{"&lt;script&gt;&#34;a&#34;&lt;/script&gt;", "it&#39;s &lt;b&gt;", "Try &amp; again", `href="/"`} {
			if !strings.Contains(body, want) {
				t.Errorf("with a link to %q, the page lacks %s: %s", href, want, body)
			}
		}
	}
	w := httptest.NewRecorder()
	end{Status: http.StatusForbidden, Title: "t", Action: "a", Href: "/admin/?a=1&b=2"}.write(w)
	if !strings.Contains(w.Body.String(), `href="/admin/?a=1&amp;b=2"`) {
		t.Errorf("a link on this site doesn't stay as it was: %s", w.Body.String())
	}
}
