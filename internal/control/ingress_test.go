// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/control/authapi"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// proxiedPanel is the panel with its proxied listener, on a unix socket, and
// proxy is what a browser sees through Caddy: its URLs are the domain's, over
// HTTPS, and what reaches the socket is what Caddy writes.
type proxiedPanel struct {
	*fullServer
	socket string
}

func startProxied(t *testing.T, backend store.Backend) *proxiedPanel {
	t.Helper()
	cert, _ := selfSigned(t, "panel.acme.dev")
	dir, err := os.MkdirTemp("", "dwp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "panel.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	f := startFullWith(t, backend, &https.Static{Name: "panel.acme.dev", Cert: cert}, slog.New(slog.DiscardHandler), ln, logapi.Options{})
	return &proxiedPanel{fullServer: f, socket: socket}
}

// caddyLike is what Caddy does to a request on its way to the panel's socket:
// it writes X-Forwarded-For itself, and leaves Host, Origin and Sec-Fetch-Site.
type caddyLike struct {
	socket, client string
	transport      *http.Transport
}

func (c *caddyLike) RoundTrip(r *http.Request) (*http.Response, error) {
	out := r.Clone(r.Context())
	out.URL.Scheme = "http"
	out.Header.Set("X-Forwarded-For", c.client)
	out.Header.Set("X-Forwarded-Proto", "https")
	return c.transport.RoundTrip(out)
}

func (p *proxiedPanel) browser(t *testing.T, client string) *browser {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	var d net.Dialer
	rt := &caddyLike{socket: p.socket, client: client, transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx, "unix", p.socket) },
	}}
	t.Cleanup(rt.transport.CloseIdleConnections)
	return &browser{origin: "https://panel.acme.dev", client: &http.Client{Jar: jar, Transport: rt}, auth: &authtest.Authenticator{}}
}

// What the panel gets over its socket is HTTPS for its domain: the same RP ID,
// the same cookie, and the client's real address. So moving behind Caddy
// changes nothing about signing in.
func TestThePanelBehindItsProxyIsItsDomainOverHTTPS(t *testing.T) {
	storetest.Each(t, testThePanelBehindItsProxy)
}

func testThePanelBehindItsProxy(t *testing.T, backend store.Backend) {
	srv := startProxied(t, backend)
	b := srv.browser(t, "2001:db8::9")
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	res := b.claim(t, link.Token)
	cookie := res.Header().Get("Set-Cookie")
	for _, want := range []string{auth.SecureCookieName + "=", "Secure", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(cookie, want) {
			t.Errorf("session cookie %q lacks %q", cookie, want)
		}
	}
	if !strings.Contains(res.Header().Get("Strict-Transport-Security"), "max-age=") {
		t.Error("responses through the proxy don't set HSTS")
	}
	state, err := b.authClient().GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if err != nil || state.Msg.GetRpId() != "panel.acme.dev" || state.Msg.GetUser() == nil {
		t.Errorf("GetAuthState through the proxy = %v, %v, want the domain as the RP ID, and the person signed in", state, err)
	}
	// A passkey made here works here, whatever serves the domain.
	if _, err := b.authClient().SignOut(t.Context(), connect.NewRequest(&dockwellv1.SignOutRequest{})); err != nil {
		t.Fatal(err)
	}
	b.signIn(t)
	sessions, err := b.authClient().ListSessions(t.Context(), connect.NewRequest(&dockwellv1.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sessions.Msg.GetSessions() {
		if s.GetCurrent() {
			found = true
			if !strings.Contains(s.GetAddress(), "2001:db8::9") {
				t.Errorf("the session's address is %q, want the client's, from what the proxy wrote", s.GetAddress())
			}
		}
	}
	if !found {
		t.Error("no session is this one")
	}

	// The panel's checks run as they do on HTTPS.
	post := func(host, origin string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://panel.acme.dev"+dockwellv1connect.AuthServiceListSessionsProcedure, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := b.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode
	}
	if code := post("", "https://panel.acme.dev"); code != http.StatusOK {
		t.Errorf("the panel's own origin = %d", code)
	}
	if code := post("", "https://app.acme.dev"); code != http.StatusForbidden {
		t.Errorf("an app's origin through the proxy = %d, want 403", code)
	}
	if code := post("", "http://panel.acme.dev"); code != http.StatusForbidden {
		t.Errorf("the plain-HTTP origin through the proxy = %d, want 403", code)
	}
	if code := post("other.acme.dev", "https://panel.acme.dev"); code != http.StatusMisdirectedRequest {
		t.Errorf("another Host through the proxy = %d, want 421", code)
	}
}

// Anything on the server can reach loopback, and so can a container on the host
// network. If a header could say "this was HTTPS", any of them could say it.
// Only the listener decides, so none of these get anywhere.
func TestNothingOnLoopbackCanPretendToBeHTTPS(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		srv := startProxied(t, backend)
		_, port, _ := net.SplitHostPort(srv.api)
		link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
		client := &http.Client{}

		spoof := func(method, host, path, body string) (int, string) {
			t.Helper()
			req, _ := http.NewRequestWithContext(t.Context(), method, "http://"+srv.api+path, strings.NewReader(body))
			req.Host = host
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
			req.Header.Set("X-Forwarded-Host", "panel.acme.dev")
			req.Header.Set("X-Real-IP", "203.0.113.9")
			req.Header.Set("Forwarded", "for=203.0.113.9;proto=https;host=panel.acme.dev")
			req.Header.Set("Origin", "https://panel.acme.dev")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			return res.StatusCode, res.Header.Get(authapi.ReasonKey)
		}

		// The panel's domain, from loopback: refused, whatever the headers say.
		code, why := spoof(http.MethodPost, "panel.acme.dev", dockwellv1connect.AuthServiceGetAuthStateProcedure, "{}")
		if code != http.StatusMisdirectedRequest {
			t.Errorf("the panel's Host on loopback, with forwarded headers = %d, want 421", code)
		}
		if why != authapi.ReasonMisdirected {
			t.Errorf("reason %q, want %q", why, authapi.ReasonMisdirected)
		}

		// On localhost it is plain HTTP on localhost, whatever the headers say,
		// and those Origin and Sec-Fetch-Site don't make it the panel's.
		if code, _ := spoof(http.MethodPost, "localhost:"+port, dockwellv1connect.AuthServiceGetAuthStateProcedure, "{}"); code != http.StatusForbidden {
			t.Errorf("localhost with the domain's Origin = %d, want 403", code)
		}

		// A real claim from localhost, with those headers on, still gets the
		// plain cookie and localhost's RP ID.
		b := newBrowser(t, "http://localhost:"+port, srv.api, nil)
		b.client.Transport = addHeaders{b.client.Transport, map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.9"}}
		c := b.claim(t, link.Token)
		cookie := c.Header().Get("Set-Cookie")
		if !strings.HasPrefix(cookie, auth.LoopbackCookieName+"=") || strings.Contains(cookie, "Secure") {
			t.Errorf("a claim on loopback with forwarded headers got %q, want the plain cookie", cookie)
		}
		if h := c.Header().Get("Strict-Transport-Security"); h != "" {
			t.Errorf("a response on loopback set HSTS: %q", h)
		}
		state, err := b.authClient().GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
		if err != nil || state.Msg.GetRpId() != "localhost" {
			t.Errorf("GetAuthState on loopback = %v, %v, want RP ID localhost", state, err)
		}
		sessions, err := b.authClient().ListSessions(t.Context(), connect.NewRequest(&dockwellv1.ListSessionsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sessions.Msg.GetSessions() {
			if strings.Contains(s.GetAddress(), "203.0.113.9") {
				t.Errorf("the session on loopback took its address from a header: %q", s.GetAddress())
			}
		}
	})
}

// addHeaders sets headers on every request.
type addHeaders struct {
	next    http.RoundTripper
	headers map[string]string
}

func (a addHeaders) RoundTrip(r *http.Request) (*http.Response, error) {
	out := r.Clone(r.Context())
	for k, v := range a.headers {
		out.Header.Set(k, v)
	}
	return a.next.RoundTrip(out)
}

// With no X-Forwarded-For, the address can't pass for loopback: nothing the
// proxy sends is loopback's to claim.
func TestAMissingForwardedAddressIsntLoopback(t *testing.T) {
	seen := ""
	h := authapi.ViaIngress(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr }))
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(nil, req)
	if host, _, err := net.SplitHostPort(seen); err != nil || host == "127.0.0.1" || host == "::1" {
		t.Errorf("RemoteAddr = %q", seen)
	}
}

// The last entry is Caddy's: it overwrites what a client sends, and if a proxy
// in front ever added to it, the end is what Caddy saw.
func TestTheForwardedAddressIsTheLastEntry(t *testing.T) {
	seen := ""
	h := authapi.ViaIngress(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr }))
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	h.ServeHTTP(nil, req)
	if seen != "203.0.113.9:0" {
		t.Errorf("RemoteAddr = %q", seen)
	}
}

// A request that says it came in early data can be replayed, and is asked to wait
// for the handshake: this listener marks what it serves as having had one.
func TestViaIngressRefusesEarlyData(t *testing.T) {
	served := false
	h := authapi.ViaIngress(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	r := httptest.NewRequest(http.MethodPost, "/dockwell.v1.ChangeService/ApplyChange", http.NoBody)
	r.Header.Set("Early-Data", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooEarly || served {
		t.Errorf("a request in early data got %d, served %v, want 425", w.Code, served)
	}
	r = httptest.NewRequest(http.MethodPost, "/dockwell.v1.ChangeService/ApplyChange", http.NoBody)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !served {
		t.Error("a request after the handshake wasn't served")
	}
}

// Only one process can hold the public ports: the panel gives them up when the
// proxy starts, and takes them back when it stops.
func TestThePublicPortsGoAndComeBack(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		cert, roots := selfSigned(t, "panel.acme.dev")
		srv := startFull(t, backend, &https.Static{Name: "panel.acme.dev", Cert: cert})
		ports := srv.server.PublicPorts(srv.https, srv.http)

		answers := func(addr string, useTLS bool) bool {
			t.Helper()
			var d net.Dialer
			conn, err := d.DialContext(t.Context(), "tcp", addr)
			if err != nil {
				return false
			}
			defer conn.Close()
			if !useTLS {
				return true
			}
			c := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "panel.acme.dev", MinVersion: tls.VersionTLS12})
			return c.HandshakeContext(t.Context()) == nil
		}
		waitUntil := func(what string, ok func() bool) {
			t.Helper()
			deadline := time.Now().Add(10 * time.Second)
			for !ok() {
				if time.Now().After(deadline) {
					t.Fatalf("%s didn't happen", what)
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		waitUntil("the HTTPS listener answering", func() bool { return answers(srv.https, true) })
		// Settings say who serves the panel's domain now, as it changes, without a restart.
		person := signedIn(t, srv)
		settings := dockwellv1connect.NewSettingsServiceClient(person, person.origin, connect.WithProtoJSON())
		servedBy := func() *dockwellv1.PanelServing {
			t.Helper()
			res, err := settings.GetSettings(t.Context(), connect.NewRequest(&dockwellv1.GetSettingsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg.GetSettings().GetPanel()
		}
		if p := servedBy(); p.GetServedBy() != dockwellv1.PanelServedBy_PANEL_SERVED_BY_DOCKWELL || p.GetUrl() != "https://panel.acme.dev" ||
			p.GetHttpsListen() != "[::]:443" || p.GetCertificateAuthority() != "Let's Encrypt" || p.GetDomain() != "panel.acme.dev" {
			t.Errorf("before any route, the panel is served as %v, want by Dockwell", p)
		}

		if err := ports.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
		if answers(srv.https, false) || answers(srv.http, false) {
			t.Error("the ports still answer once released")
		}
		if p := servedBy(); p.GetServedBy() != dockwellv1.PanelServedBy_PANEL_SERVED_BY_INGRESS || p.GetHttpsListen() != "" || p.GetUrl() != "https://panel.acme.dev" {
			t.Errorf("with the ports given to the ingress, the panel is served as %v, want by the ingress", p)
		}
		// Something else can bind them now, as the proxy will.
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", srv.https)
		if err != nil {
			t.Fatalf("the HTTPS port isn't free once released: %v", err)
		}
		_ = l.Close()
		// The loopback listener doesn't depend on any of this.
		res, err := http.Get("http://" + srv.api + "/healthz")
		if err != nil {
			t.Fatalf("the tunnel's listener went with the public ports: %v", err)
		}
		_ = res.Body.Close()
		if err := ports.Release(t.Context()); err != nil {
			t.Errorf("releasing twice: %v", err)
		}

		if err := ports.Retake(t.Context()); err != nil {
			t.Fatal(err)
		}
		waitUntil("the HTTPS listener answering again", func() bool { return answers(srv.https, true) })
		if p := servedBy(); p.GetServedBy() != dockwellv1.PanelServedBy_PANEL_SERVED_BY_DOCKWELL {
			t.Errorf("with the ports taken back, the panel is served as %v, want by Dockwell", p)
		}
		if err := ports.Retake(t.Context()); err != nil {
			t.Errorf("taking back twice: %v", err)
		}

		// One that can't bind says so, and leaves the panel as it was.
		if err := ports.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
		held, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", srv.https)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		if err := ports.Retake(t.Context()); err == nil {
			t.Error("taking back a port something else holds worked")
		}
	})
}
