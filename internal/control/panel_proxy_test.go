// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/control/settings"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// tunnelDomain is the panel's domain behind the tunnel in these tests.
const tunnelDomain = "panel.example.com"

// fakeTunnel ends TLS for tunnelDomain and sends what it gets to the panel's
// loopback listener, as cloudflared on the server does: the Host as the browser
// sent it, X-Forwarded-Proto https, and the visitor's address added to
// X-Forwarded-For. edit changes what it sends, as a tunnel set up otherwise
// would. paths are the paths it was asked for.
type fakeTunnel struct {
	addr  string
	roots *x509.CertPool
	mu    sync.Mutex
	paths []string
}

func startTunnel(t *testing.T, panel string, edit func(*httputil.ProxyRequest)) *fakeTunnel {
	t.Helper()
	target, err := url.Parse("http://" + panel)
	if err != nil {
		t.Fatal(err)
	}
	cert, roots := selfSigned(t, tunnelDomain)
	tun := &fakeTunnel{roots: roots}
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		tun.mu.Lock()
		tun.paths = append(tun.paths, r.In.URL.Path)
		tun.mu.Unlock()
		r.SetURL(target)
		r.Out.Host = r.In.Host
		r.Out.Header.Set("X-Forwarded-For", "203.0.113.9")
		r.Out.Header.Set("X-Forwarded-Proto", "https")
		if edit != nil {
			edit(r)
		}
	}}
	srv := httptest.NewUnstartedServer(proxy)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	tun.addr = srv.Listener.Addr().String()
	return tun
}

// answering is a TLS server for tunnelDomain that answers every request with
// code, as a site that isn't the panel does.
func answering(t *testing.T, code int) *fakeTunnel {
	t.Helper()
	return answeringWith(t, code, nil)
}

// answeringCloudflare answers as Cloudflare's edge does, with a CF-RAY, as for a
// tunnel that isn't connected.
func answeringCloudflare(t *testing.T, code int) *fakeTunnel {
	t.Helper()
	return answeringWith(t, code, http.Header{"Cf-Ray": {"8f2a1b3c4d5e6f70-AMS"}, "Server": {"cloudflare"}})
}

func answeringWith(t *testing.T, code int, headers http.Header) *fakeTunnel {
	t.Helper()
	cert, roots := selfSigned(t, tunnelDomain)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header()[k] = v
		}
		w.WriteHeader(code)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &fakeTunnel{addr: srv.Listener.Addr().String(), roots: roots}
}

// client reaches the tunnel whatever the URL's host, trusting its certificate.
func (f *fakeTunnel) client() *http.Client {
	var d net.Dialer
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return d.DialContext(ctx, network, f.addr)
			},
			TLSClientConfig: &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// browser is a browser on https://panel.example.com, which reaches the panel
// through the tunnel.
func (f *fakeTunnel) browser(t *testing.T) *browser {
	t.Helper()
	return newBrowser(t, "https://"+tunnelDomain, f.addr, f.roots)
}

// switchClient asks whichever tunnel a test points it at.
type switchClient struct {
	mu sync.Mutex
	to settings.Doer
}

func (s *switchClient) Do(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	to := s.to
	s.mu.Unlock()
	return to.Do(r)
}

func (s *switchClient) point(to settings.Doer) {
	s.mu.Lock()
	s.to = to
	s.mu.Unlock()
}

// startBehindTunnel is a control plane whose domain a tunnel serves, and the
// tunnel, cloudflared's stand-in.
func startBehindTunnel(t *testing.T, backend store.Backend) (*fullServer, *fakeTunnel) {
	t.Helper()
	f := startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) {
		c.PanelURL = "https://" + tunnelDomain
		c.PanelDomain = PanelDomain{From: dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_SETTING, BehindProxy: tunnelDomain}
	})
	return f, startTunnel(t, f.api, nil)
}

// Behind a tunnel that ends TLS, the panel is its domain over HTTPS: a passkey is
// made and signs in for the domain's RP ID, on its origin, with the __Host-
// cookie and the visitor's address, through the proxy in front. No certificate
// or public port of Dockwell's is involved (ADR 0063's amendment).
func TestThePanelBehindATunnelIsItsDomainOverHTTPS(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testThePanelBehindATunnel)
}

func testThePanelBehindATunnel(t *testing.T, backend store.Backend) {
	srv, tun := startBehindTunnel(t, backend)
	b := tun.browser(t)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	res := b.claim(t, link.Token)
	cookie := res.Header().Get("Set-Cookie")
	for _, want := range []string{auth.SecureCookieName + "=", "Secure", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(cookie, want) {
			t.Errorf("session cookie %q lacks %q", cookie, want)
		}
	}
	if !strings.Contains(res.Header().Get("Strict-Transport-Security"), "max-age=") {
		t.Error("responses through the tunnel don't set HSTS")
	}
	state, err := b.authClient().GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if err != nil || state.Msg.GetRpId() != tunnelDomain || state.Msg.GetUser() == nil {
		t.Errorf("GetAuthState through the tunnel = %v, %v, want the domain as the RP ID, and the person signed in", state, err)
	}
	// The passkey made through the tunnel signs in through it again.
	if _, err := b.authClient().SignOut(t.Context(), connect.NewRequest(&dockwellv1.SignOutRequest{})); err != nil {
		t.Fatal(err)
	}
	b.signIn(t)
	sessions, err := b.authClient().ListSessions(t.Context(), connect.NewRequest(&dockwellv1.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	current := 0
	for _, s := range sessions.Msg.GetSessions() {
		if s.GetCurrent() {
			current++
			if !strings.Contains(s.GetAddress(), "203.0.113.9") {
				t.Errorf("the session's address is %q, want the visitor's, from what the tunnel wrote", s.GetAddress())
			}
		}
	}
	if current != 1 {
		t.Errorf("%d sessions are this one", current)
	}

	// Settings say a tunnel serves it, with nothing to wait for.
	got, err := dockwellv1connect.NewSettingsServiceClient(b, b.origin, connect.WithProtoJSON()).GetSettings(t.Context(), connect.NewRequest(&dockwellv1.GetSettingsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Msg.GetSettings().GetPanel(); p.GetServedBy() != dockwellv1.PanelServedBy_PANEL_SERVED_BY_PROXY || p.GetUrl() != "https://"+tunnelDomain ||
		!p.GetCertified() || p.GetDomain() != tunnelDomain || p.GetCertificateAuthority() != "" {
		t.Errorf("the panel reads %v, want served by a proxy at https://%s", p, tunnelDomain)
	}

	// The panel's checks run as they do on HTTPS.
	post := func(host, origin string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+tunnelDomain+dockwellv1connect.AuthServiceListSessionsProcedure, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		if host != "" {
			req.Host = host
		}
		req.Header.Set("Origin", origin)
		res, err := b.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode
	}
	if code := post("", "https://"+tunnelDomain); code != http.StatusOK {
		t.Errorf("the panel's own origin = %d", code)
	}
	if code := post("", "https://app.example.com"); code != http.StatusForbidden {
		t.Errorf("an app's origin through the tunnel = %d, want 403", code)
	}
	if code := post("", "http://"+tunnelDomain); code != http.StatusForbidden {
		t.Errorf("the plain-HTTP origin through the tunnel = %d, want 403", code)
	}
	if code := post("other.example.com", "https://"+tunnelDomain); code != http.StatusMisdirectedRequest {
		t.Errorf("another Host through the tunnel = %d, want 421", code)
	}

	// Loopback stays loopback: localhost signs in as it did.
	_, port, _ := net.SplitHostPort(srv.api)
	local := newBrowser(t, "http://localhost:"+port, srv.api, nil)
	state, err = local.authClient().GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if err != nil || state.Msg.GetRpId() != "localhost" {
		t.Errorf("GetAuthState on loopback = %v, %v, want RP ID localhost", state, err)
	}
}

// Only what comes over loopback with the domain as its Host and X-Forwarded-Proto
// https reads as the domain's. A plain request for the domain, or one that says
// it came over plain HTTP, is another host's.
func TestOnlyATunnelsRequestReadsAsTheDomains(t *testing.T) {
	t.Parallel()
	srv, _ := startBehindTunnel(t, store.SQLite)
	ask := func(host string, headers map[string]string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+srv.api+dockwellv1connect.AuthServiceGetAuthStateProcedure, strings.NewReader("{}"))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://"+tunnelDomain)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode
	}
	for _, tt := range []struct {
		name    string
		host    string
		headers map[string]string
		want    int
	}{
		{"the tunnel's", tunnelDomain, map[string]string{"X-Forwarded-Proto": "https"}, http.StatusOK},
		{"with a port", tunnelDomain + ":443", map[string]string{"X-Forwarded-Proto": "HTTPS"}, http.StatusOK},
		{"no X-Forwarded-Proto", tunnelDomain, nil, http.StatusMisdirectedRequest},
		{"over plain HTTP", tunnelDomain, map[string]string{"X-Forwarded-Proto": "http"}, http.StatusMisdirectedRequest},
		{"another host", "other.example.com", map[string]string{"X-Forwarded-Proto": "https"}, http.StatusMisdirectedRequest},
		{"in early data", tunnelDomain, map[string]string{"X-Forwarded-Proto": "https", "Early-Data": "1"}, http.StatusTooEarly},
	} {
		if got := ask(tt.host, tt.headers); got != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, got, tt.want)
		}
	}
}

// ViaProxy believes only loopback: from anywhere else, as through the ingress's
// hand-over, which already marked the request, it leaves it as it came.
func TestViaProxyBelievesOnlyLoopback(t *testing.T) {
	t.Parallel()
	var seen *http.Request
	h := authapi.ViaProxy(tunnelDomain, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r }))
	for _, tt := range []struct {
		remote string
		marked bool
	}{
		{"127.0.0.1:50000", true},
		{"[::1]:50000", true},
		{"192.0.2.10:50000", false},
		{"[2001:db8::9]:50000", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.Host, r.RemoteAddr = tunnelDomain, tt.remote
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-For", "198.51.100.4, 203.0.113.9")
		h.ServeHTTP(httptest.NewRecorder(), r)
		if marked := seen.TLS != nil; marked != tt.marked {
			t.Errorf("from %s: marked %v, want %v", tt.remote, marked, tt.marked)
		}
		if tt.marked && seen.RemoteAddr != "203.0.113.9:0" {
			t.Errorf("from %s: the visitor is %q, want the last X-Forwarded-For", tt.remote, seen.RemoteAddr)
		}
	}
	if authapi.ViaProxy("", nil) != nil {
		t.Error("with no domain, ViaProxy wraps anything")
	}
}

// The way in is checked by asking the domain for a token through the tunnel,
// before the domain is saved and after, and each way it can go wrong says the fix
// (ADR 0063's amendment).
func TestAReachCheckFindsThePanelThroughItsTunnel(t *testing.T) {
	t.Parallel()
	var to switchClient
	// No domain yet: the check is for one being typed.
	srv := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) {
		c.PanelListen = "127.0.0.1:7600"
		// The stand-ins listen on loopback, which the check would count as inside.
		c.PanelDomain = PanelDomain{ReachClient: &to, ReachInside: func(netip.Addr) bool { return false }}
	})
	b := signedIn(t, srv)
	c := dockwellv1connect.NewSettingsServiceClient(b, b.origin, connect.WithProtoJSON())
	check := func() *dockwellv1.CheckPanelReachResponse {
		t.Helper()
		res, err := c.CheckPanelReach(t.Context(), connect.NewRequest(&dockwellv1.CheckPanelReachRequest{Domain: " Panel.Example.com. "}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}

	tun := startTunnel(t, srv.api, nil)
	to.point(tun.client())
	if got := check(); !got.GetReached() || got.GetProblem() != "" || got.GetTarget() != "http://127.0.0.1:7600" {
		t.Errorf("through the tunnel: %v, want reached", got)
	}
	// The token answers once.
	tun.mu.Lock()
	asked := tun.paths[len(tun.paths)-1]
	tun.mu.Unlock()
	if !strings.HasPrefix(asked, settings.ReachPath) {
		t.Fatalf("the check asked for %q", asked)
	}
	again, err := tun.client().Get("https://" + tunnelDomain + asked)
	if err != nil {
		t.Fatal(err)
	}
	_ = again.Body.Close()
	if again.StatusCode != http.StatusNotFound {
		t.Errorf("a token asked for again: %d, want 404", again.StatusCode)
	}

	for _, tt := range []struct {
		name string
		via  settings.Doer
		want string
	}{
		{
			"a tunnel that sends its own Host", startTunnel(t, srv.api, func(r *httputil.ProxyRequest) { r.Out.Host = "localhost:7600" }).client(),
			"with localhost:7600 as its host, not panel.example.com, so passkeys can't work there",
		},
		{
			"a proxy that doesn't say HTTPS", startTunnel(t, srv.api, func(r *httputil.ProxyRequest) { r.Out.Header.Del("X-Forwarded-Proto") }).client(),
			"without X-Forwarded-Proto https",
		},
		{
			"a proxy that doesn't say who's visiting", startTunnel(t, srv.api, func(r *httputil.ProxyRequest) { r.Out.Header.Del("X-Forwarded-For") }).client(),
			"without X-Forwarded-For, so every visitor reads as one address",
		},
		{"a tunnel that isn't connected", answeringCloudflare(t, 530).client(), "Cloudflare answered 530 for panel.example.com: its tunnel isn't connected"},
		{"Cloudflare's proxy, not a tunnel", answeringCloudflare(t, 522).client(), "Cloudflare answered 522 for panel.example.com: its proxy connects to this server's public address, not a tunnel"},
		{"Cloudflare's proxy with no certificate here", answeringCloudflare(t, 525).client(), "choose Dockwell serves it and set Cloudflare's SSL mode to Full (strict)"},
		{"Cloudflare's proxy that gave up waiting", answeringCloudflare(t, 524).client(), "Cloudflare answered 524 for panel.example.com: its proxy connects"},
		{"Cloudflare's proxy with an empty answer", answeringCloudflare(t, 520).client(), "Cloudflare answered 520 for panel.example.com"},
		// A 52x without Cloudflare's mark is whatever answered's own (N4).
		{"a 522 that isn't Cloudflare's", answering(t, 522).client(), "something else answered 522 for panel.example.com"},
		{"a tunnel pointing at nothing", answering(t, http.StatusBadGateway).client(), "it can't reach what it points at. Point it at http://127.0.0.1:7600"},
		{"something else", answering(t, http.StatusNotFound).client(), "something else answered 404 for panel.example.com, not this panel"},
		{"another Dockwell", startTunnel(t, startFull(t, store.SQLite, nil).api, nil).client(), "another Dockwell answered for panel.example.com"},
		{"nothing listening", &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, "127.0.0.1:1") // as the CA a test reaches is: nothing listens there
		}}}, "nothing answered HTTPS where panel.example.com leads. Start the tunnel or proxy"},
		{"no record", &http.Client{Transport: &http.Transport{DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}}}, "there's no DNS record for panel.example.com yet"},
	} {
		to.point(tt.via)
		if got := check(); got.GetReached() || !strings.Contains(got.GetProblem(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.name, got, tt.want)
		}
	}

	// A name that leads inside is answered reached or not, and nothing of what
	// answers there (#734's review, L4).
	inside := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) {
		c.PanelListen = "127.0.0.1:7600"
		// Whatever the name resolves to, the address connected to decides (N3): this
		// stand-in, on loopback, is inside.
		c.PanelDomain = PanelDomain{ReachClient: answering(t, http.StatusNotFound).client()}
	})
	ib := signedIn(t, inside)
	got, err := dockwellv1connect.NewSettingsServiceClient(ib, ib.origin, connect.WithProtoJSON()).CheckPanelReach(t.Context(),
		connect.NewRequest(&dockwellv1.CheckPanelReachRequest{Domain: "inside.example.com"}))
	if err != nil || got.Msg.GetReached() || strings.Contains(got.Msg.GetProblem(), "404") ||
		got.Msg.GetProblem() != "Dockwell couldn't reach this panel through inside.example.com. Point the tunnel or proxy at http://127.0.0.1:7600" {
		t.Errorf("a name that leads inside: %v, %v", got, err)
	}

	// An address isn't a domain the panel can take.
	res, err := c.CheckPanelReach(t.Context(), connect.NewRequest(&dockwellv1.CheckPanelReachRequest{Domain: "192.0.2.10"}))
	if err != nil || res.Msg.GetReached() || !strings.Contains(res.Msg.GetProblem(), "is an IP address") {
		t.Errorf("an address: %v, %v", res, err)
	}
}

// The owner moves the panel between Dockwell serving its domain and a tunnel, on
// the same domain, with a passkey: a restart, on the activity log, with nothing
// else to change. Clearing the domain clears how it was served.
func TestTheOwnerPutsThePanelBehindATunnel(t *testing.T) {
	t.Parallel()
	f, restarts, _ := panelDomainStand(t)
	b := signedIn(t, f)
	c := dockwellv1connect.NewSettingsServiceClient(b, b.origin, connect.WithProtoJSON())
	set := func(domain string, behind bool) (*dockwellv1.SetPanelDomainResponse, error) {
		res, err := c.SetPanelDomain(t.Context(), connect.NewRequest(&dockwellv1.SetPanelDomainRequest{
			Domain: domain, BehindProxy: behind,
			Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_PANEL_DOMAIN, settings.PanelDomainTarget(cmpOr(domain, tunnelDomain), behind && domain != "")),
		}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	held := func() settings.PanelSet {
		t.Helper()
		s, err := settings.PanelSetting(t.Context(), f.store)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// A passkey for the domain served by Dockwell doesn't confirm it behind a proxy.
	if _, err := c.SetPanelDomain(t.Context(), connect.NewRequest(&dockwellv1.SetPanelDomainRequest{
		Domain: tunnelDomain, BehindProxy: true,
		Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_PANEL_DOMAIN, tunnelDomain),
	})); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("behind a proxy with a passkey for the domain alone: %v, want needs_step_up", err)
	}
	if got, err := set(tunnelDomain, true); err != nil || !got.GetRestarting() {
		t.Fatalf("setting it behind a tunnel: %v, %v", got, err)
	}
	if s := held(); s != (settings.PanelSet{Domain: tunnelDomain, BehindProxy: true}) {
		t.Errorf("the settings hold %+v", s)
	}
	if got, err := set(tunnelDomain, true); err != nil || got.GetRestarting() {
		t.Errorf("the same again: %v, %v; want nothing to do", got, err)
	}
	// Dockwell serving it instead, on the same domain, isn't a move.
	if got, err := set(tunnelDomain, false); err != nil || !got.GetRestarting() {
		t.Fatalf("serving it from Dockwell: %v, %v", got, err)
	}
	if s := held(); s != (settings.PanelSet{Domain: tunnelDomain}) {
		t.Errorf("the settings hold %+v", s)
	}
	if _, err := set(tunnelDomain, true); err != nil {
		t.Fatal(err)
	}
	if was, err := settings.ClearPanelDomain(t.Context(), f.store, "cli:root@web-1", time.Now()); err != nil || was != tunnelDomain {
		t.Errorf("clearing it locally: %q, %v", was, err)
	}
	if s := held(); s != (settings.PanelSet{}) {
		t.Errorf("after clearing it, the settings hold %+v", s)
	}
	waitFor(t, "the restart", func() bool { return restarts.Load() >= 1 })

	events := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SETTINGS},
	})
	var said []string
	for _, e := range events {
		s := e.GetSetting()
		said = append(said, s.GetPrevious()+tunnelWord(s.GetPreviousBehindProxy())+">"+s.GetValue()+tunnelWord(s.GetBehindProxy()))
	}
	want := "panel.example.com+tunnel>, panel.example.com>panel.example.com+tunnel, panel.example.com+tunnel>panel.example.com, >panel.example.com+tunnel"
	if got := strings.Join(said, ", "); got != want {
		t.Errorf("the timeline has %v, want %v", got, want)
	}
}

func tunnelWord(behind bool) string {
	if behind {
		return "+tunnel"
	}
	return ""
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// A domain whose records lead to Cloudflare is said to, so the panel offers the
// tunnel first.
func TestADomainOnCloudflareIsSaidToBe(t *testing.T) {
	t.Parallel()
	f, _, _ := panelDomainStand(t)
	b := signedIn(t, f)
	c := dockwellv1connect.NewSettingsServiceClient(b, b.origin, connect.WithProtoJSON())
	for domain, want := range map[string]bool{"tunnel.example.com": true, tunnelDomain: false} {
		res, err := c.CheckPanelDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckPanelDomainRequest{Domain: domain}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Msg.GetCloudflare() != want {
			t.Errorf("%s reads %v, want cloudflare %v", domain, res.Msg, want)
		}
	}
}
