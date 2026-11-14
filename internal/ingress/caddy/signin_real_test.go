// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/signin"
	"github.com/dockwell/dockwell/internal/ingress"
)

// oidc is a provider, as far as signing in needs one: discovery, its keys, an
// authorization endpoint that signs in mira@example.com at once, and a token
// endpoint that gives an ID token with the nonce the sign-in asked for.
type oidc struct {
	srv *httptest.Server
	key *ecdsa.PrivateKey

	mu     sync.Mutex
	nonces map[string]string
}

const signInClient = "dockwell"

func newOIDC(t *testing.T) *oidc {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := &oidc{key: key, nonces: map[string]string{}}
	b64 := base64.RawURLEncoding.EncodeToString
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := p.srv.URL + "/"
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": base, "authorization_endpoint": base + "authorize", "token_endpoint": base + "token", "jwks_uri": base + "keys",
				"token_endpoint_auth_methods_supported": []string{"client_secret_basic"}, "code_challenge_methods_supported": []string{"S256"},
			})
		case "/keys":
			x, y := make([]byte, 32), make([]byte, 32)
			p.key.X.FillBytes(x)
			p.key.Y.FillBytes(y)
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "EC", "crv": "P-256", "kid": "k1", "x": b64(x), "y": b64(y)}}})
		case "/authorize":
			q := r.URL.Query()
			code := fmt.Sprintf("code-%d", time.Now().UnixNano())
			p.mu.Lock()
			p.nonces[code] = q.Get("nonce")
			p.mu.Unlock()
			back, _ := url.Parse(q.Get("redirect_uri"))
			back.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
			http.Redirect(w, r, back.String(), http.StatusFound)
		case "/token":
			_ = r.ParseForm()
			p.mu.Lock()
			nonce, ok := p.nonces[r.Form.Get("code")]
			delete(p.nonces, r.Form.Get("code"))
			p.mu.Unlock()
			if user, secret, basic := r.BasicAuth(); !ok || !basic || user != signInClient || secret != "s3cret" || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			now := time.Now()
			head, _ := json.Marshal(map[string]any{"alg": "ES256", "kid": "k1", "typ": "JWT"})
			claims, _ := json.Marshal(map[string]any{
				"iss": base, "aud": signInClient, "sub": "248289761001", "email": "mira@example.com", "email_verified": true,
				"nonce": nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			})
			signed := b64(head) + "." + b64(claims)
			sum := sha256.Sum256([]byte(signed))
			rr, ss, err := ecdsa.Sign(rand.Reader, p.key, sum[:])
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			sig := make([]byte, 64)
			rr.FillBytes(sig[:32])
			ss.FillBytes(sig[32:])
			_ = json.NewEncoder(w).Encode(map[string]string{"id_token": signed + "." + b64(sig), "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// seenApp is an app that says what reached it of who signed in, and its cookies.
func seenApp(t *testing.T) (dial string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An app that would sign its visitors in as someone it chose, with its own cookie beside.
		if r.URL.Query().Has("set") {
			w.Header().Add("Set-Cookie", "__Host-dw_abcdef012345=chosen; Path=/; Secure; HttpOnly")
			w.Header().Add("Set-Cookie", "__HOST-dw_flow_abcdef012345=chosen; Path=/; Secure")
			w.Header().Add("Set-Cookie", "theme=dark; Path=/")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"user": r.Header.Get(signin.UserHeader), "subject": r.Header.Get(signin.SubjectHeader),
			"cookie": strings.Join(r.Header.Values("Cookie"), " | "), "path": r.URL.Path,
		})
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// browser is a browser that reaches domain through Caddy, and anything else where
// it is, with cookies, and stops at each redirect, so a test can see each.
func (r *realCaddy) browser(t *testing.T, domain string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				if host, _, _ := net.SplitHostPort(addr); host == domain {
					return d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", r.tls))
				}
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

// visited is what a visit got: its status, where it sends on, and, from the app,
// what reached it.
type visited struct {
	status int
	to     *url.URL
	seen   map[string]string
	set    []string
}

// visit sends a request as a browser does, a navigation unless method says otherwise.
func visit(t *testing.T, b *http.Client, method, target string, header map[string]string) visited {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := b.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer res.Body.Close()
	out := visited{status: res.StatusCode, set: res.Header.Values("Set-Cookie")}
	out.to, _ = res.Location()
	if res.StatusCode == http.StatusOK {
		_ = json.NewDecoder(res.Body).Decode(&out.seen)
	}
	return out
}

// Signing in goes through the route's agent, as Caddy asks it before each request:
// a navigation goes to the provider, a script gets 401, the callback leaves a
// session cookie, and the app then gets who signed in, over any a visitor wrote,
// without Dockwell's cookies. An agent that doesn't answer is 502, never the app
// (ADR 0087).
func TestSigningInGoesThroughTheAgentOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	p := newOIDC(t)
	keep := &signin.Keep{Dir: t.TempDir()}
	const domain = "app.example.net"
	route := ingress.Route{ID: "dockwell.shop.admin", Domain: domain, Upstreams: []string{seenApp(t)}, SignIn: c.SignInDial()}
	spec := &resourcev1.RouteSignIn{
		Provider: "ours", EmailDomains: []string{"example.com"},
		Filled: &resourcev1.RouteSignInProvider{Issuer: p.srv.URL + "/", ClientId: signInClient, ClientSecret: "s3cret"},
	}
	if err := keep.Put(route.ID, route.Domain, route.Path, spec); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", SignInSocket(c.Dir))
	if err != nil {
		t.Fatal(err)
	}
	// What Caddy tells the agent of who the visitor is, as the agent last heard it.
	var told atomic.Value
	answer := &signin.Server{Routes: keep.Route, Client: p.srv.Client()}
	// As the agent serves it, with signin.Serve, over the connections Caddy keeps.
	go func() {
		_ = signin.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			told.Store(r.Header.Get(signin.ClientHeader))
			answer.ServeHTTP(w, r)
		}), 10*time.Second, slog.New(slog.DiscardHandler))
	}()
	t.Cleanup(func() { _ = ln.Close() })
	put(t, c, route)
	if got, err := c.Routes(t.Context()); err != nil || len(got) != 1 || got[0].SignIn != route.SignIn || got[0].Edited {
		t.Fatalf("the route reads back as %+v, %v", got, err)
	}

	b := c.browser(t, domain)
	home := "https://" + domain + "/"
	for range 100 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, home, http.NoBody)
		req.Header.Set("Accept", "text/html")
		var first *http.Response
		if first, err = b.Do(req); err == nil {
			_ = first.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("%s never answered: %v", domain, err)
	}

	got := visit(t, b, http.MethodGet, home, nil)
	toProvider := got.to
	if got.status != http.StatusFound || toProvider == nil || !strings.HasPrefix(toProvider.String(), p.srv.URL+"/authorize?") {
		t.Fatalf("a navigation gets %d to %v, want a redirect to the provider", got.status, toProvider)
	}
	// Who the visitor is comes from Caddy, whatever they write (#598's review, L3).
	if signInClientHeader != signin.ClientHeader {
		t.Fatalf("Caddy writes %s, and the agent reads %s", signInClientHeader, signin.ClientHeader)
	}
	if got = visit(t, b, http.MethodPost, home, map[string]string{signin.ClientHeader: "203.0.113.9", "X-Forwarded-For": "203.0.113.9"}); got.status != http.StatusUnauthorized {
		t.Errorf("a POST gets %d, want 401", got.status)
	}
	if said, _ := told.Load().(string); said != "127.0.0.1" {
		t.Errorf("the agent is told the visitor is %q, want the peer, 127.0.0.1", said)
	}
	// The stranger check, asking as a stranger navigates, reads where they're sent.
	provider, _ := url.Parse(p.srv.URL)
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := ingress.AskStrangers(t.Context(), c.Caddy, routes)[domain]; len(got) != 1 ||
		got[0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_SENT_TO_SIGN_IN || got[0].GetSaid() != provider.Hostname() {
		t.Errorf("the stranger check reads %v, want sent to sign in at %s", got, provider.Hostname())
	}

	got = visit(t, b, http.MethodGet, toProvider.String(), nil)
	callback := got.to
	if got.status != http.StatusFound || callback == nil || callback.Host != domain {
		t.Fatalf("the provider sends back %d to %v", got.status, callback)
	}
	if got = visit(t, b, http.MethodGet, callback.String(), nil); got.status != http.StatusFound || got.to == nil || got.to.Path != "/" {
		t.Fatalf("the callback answers %d to %v, want a redirect back to /", got.status, got.to)
	}

	got = visit(t, b, http.MethodGet, home, map[string]string{signin.UserHeader: "eve@elsewhere.example", "Cookie": "theme=dark"})
	if got.status != http.StatusOK {
		t.Fatalf("signed in, it answers %d", got.status)
	}
	seen := got.seen
	if seen["user"] != "mira@example.com" || seen["subject"] != "248289761001" {
		t.Errorf("the app is told it's %q (%q), want mira@example.com", seen["user"], seen["subject"])
	}
	if strings.Contains(seen["cookie"], "__Host-dw_") || !strings.Contains(seen["cookie"], "theme=dark") {
		t.Errorf("the app gets the cookies %q, want its own alone", seen["cookie"])
	}

	// With users too, Basic credentials are checked against them, and what a visitor
	// says of who signed in doesn't reach the app that way either; anything else
	// goes to sign in.
	hash, err := bcrypt.GenerateFromPassword([]byte("a-long-ci-password"), 10)
	if err != nil {
		t.Fatal(err)
	}
	both := ingress.Route{ID: "dockwell.shop.api", Domain: domain, Path: "/api", Upstreams: route.Upstreams, SignIn: c.SignInDial(), Users: []ingress.User{{Name: "ci", Hash: string(hash)}}}
	if err := keep.Put(both.ID, both.Domain, both.Path, spec); err != nil {
		t.Fatal(err)
	}
	put(t, c, both)
	if got, err := c.Routes(t.Context()); err != nil || len(got) != 2 || got[0].Edited || got[1].Edited {
		t.Fatalf("the routes read back as %+v, %v", got, err)
	}
	api := "https://" + domain + "/api/who"
	script := &http.Client{Transport: b.Transport, Timeout: 30 * time.Second, CheckRedirect: b.CheckRedirect}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, api, http.NoBody)
	req.SetBasicAuth("ci", "a-long-ci-password")
	req.Header.Set(signin.UserHeader, "eve@elsewhere.example")
	res, err := script.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("a script with the password gets %v, %v", res, err)
	}
	if seen := who(t, res); seen["user"] != "" {
		t.Errorf("with a password, the app is told it's %q, which the visitor wrote", seen["user"])
	}
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, api, http.NoBody)
	req.SetBasicAuth("ci", "not-the-password")
	wrong, err := script.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = wrong.Body.Close()
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong password gets %d, want 401", wrong.StatusCode)
	}
	fresh := c.browser(t, domain)
	if got = visit(t, fresh, http.MethodGet, api, nil); got.status != http.StatusFound {
		t.Errorf("a navigation without a password gets %d, want a redirect to sign in", got.status)
	}

	// A public route beside it on the same host gets the browser's cookies for that
	// host, Dockwell's session among them, and passes on its app's own alone
	// (#598's review, M4).
	put(t, c, ingress.Route{ID: "dockwell.shop.pub", Domain: domain, Path: "/pub", Upstreams: route.Upstreams})
	if got = visit(t, b, http.MethodGet, "https://"+domain+"/pub/who", map[string]string{"Cookie": "theme=dark"}); got.status != http.StatusOK ||
		strings.Contains(got.seen["cookie"], "__Host-dw_") || !strings.Contains(got.seen["cookie"], "theme=dark") {
		t.Errorf("a public route beside it passes on %d, %q, want the app's own cookies alone", got.status, got.seen["cookie"])
	}
	// Nor may its app set them, in any case of the prefix; its own cookie stays
	// (#598's review, New 3).
	got = visit(t, b, http.MethodGet, "https://"+domain+"/pub/who?set=1", nil)
	if joined := strings.Join(got.set, " | "); strings.Contains(strings.ToLower(joined), "__host-dw_") || !strings.Contains(joined, "theme=dark") {
		t.Errorf("the public route's app sets %q, want its own cookie alone", joined)
	}

	// The agent gone: its socket closed, and the connections Caddy kept with it.
	_ = ln.Close()
	_ = os.Remove(SignInSocket(c.Dir))
	if got = visit(t, b, http.MethodGet, home, nil); got.status != http.StatusBadGateway {
		t.Errorf("with the agent down, it answers %d, want 502", got.status)
	}
}
