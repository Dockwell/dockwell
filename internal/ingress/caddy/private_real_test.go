// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/ingress"
)

// answer is what a private route said: its status, its headers, and what its
// upstream saw, when it got that far.
type answer struct {
	status int
	header http.Header
	seen   map[string]string
}

// privateDomain is the private route's domain in these tests.
const privateDomain = "app.example.net"

// askFrom asks privateDomain over plain HTTP from one of this machine's addresses,
// which the request comes from too, with headers and a password if given.
func (r *realCaddy) askFrom(t *testing.T, addr string, headers map[string]string, user, password string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://%s/who", net.JoinHostPort(addr, strconv.Itoa(r.http))), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = privateDomain
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(addr)}}
	res, err := (&http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: dialer.DialContext}}).Do(req)
	if err != nil {
		t.Fatalf("GET http://%s/who from %s: %v", privateDomain, addr, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := answer{status: res.StatusCode, header: res.Header}
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out.seen); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
	}
	return out
}

// hashOf is a password's bcrypt hash at the cost a route keeps.
func hashOf(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

// started is how many times Caddy has been started.
func (p *process) started() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

// hasIPv6 skips a test on a machine with no IPv6 loopback.
func hasIPv6(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("this machine has no IPv6 loopback")
	}
	_ = ln.Close()
}

// readsAs reads the one route back, as itself.
func readsAs(t *testing.T, c *realCaddy, want ingress.Route) {
	t.Helper()
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(routes, func(r ingress.Route) bool { return r.ID == want.ID })
	if i < 0 {
		t.Fatalf("%s isn't there: %+v", want.ID, routes)
	}
	got := routes[i]
	if got.Edited || !slices.Equal(got.Allow, want.Allow) || !slices.Equal(got.Users, want.Users) {
		t.Errorf("it reads back %+v, want its list %v and users %v", got, want.Allow, want.Users)
	}
}

// A list lets in who's on it, by the peer on the connection, on IPv4 and IPv6, and
// anyone else gets 403 with nothing else said. What a visitor writes in
// X-Forwarded-For counts for nothing. A change of the list is a reload of the route,
// with Caddy and the app as they were (ADR 0087).
func TestAListKeepsOutWhoIsntOnItOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	own := ownAddress(t)
	r := plainRoute("dockwell.env.www", "app.example.net", backend(t, "web"))
	r.PlainForAnyone, r.Allow = true, []string{own + "/32"}
	put(t, c, r)
	readsAs(t, c, r)
	if a := c.askFrom(t, "127.0.0.1", nil, "", ""); a.status != http.StatusForbidden {
		t.Errorf("off the list, it answers %d", a.status)
	}
	forged := map[string]string{"X-Forwarded-For": own, "X-Real-Ip": own, "Forwarded": "for=" + own}
	if a := c.askFrom(t, "127.0.0.1", forged, "", ""); a.status != http.StatusForbidden {
		t.Errorf("off the list, saying it's on it, it answers %d", a.status)
	}
	if a := c.askFrom(t, own, nil, "", ""); a.status != http.StatusOK || a.seen["real"] != own {
		t.Errorf("on the list, it answers %d, and the app saw %v", a.status, a.seen)
	}
	runs := c.proc.started()

	hasIPv6(t)
	if a := c.askFrom(t, "::1", nil, "", ""); a.status != http.StatusForbidden {
		t.Errorf("off the list over IPv6, it answers %d", a.status)
	}
	r.Allow = []string{"::1/128", own + "/32"}
	put(t, c, r)
	readsAs(t, c, r)
	if a := c.askFrom(t, "::1", nil, "", ""); a.status != http.StatusOK {
		t.Errorf("on the list over IPv6, it answers %d", a.status)
	}
	if a := c.askFrom(t, "127.0.0.1", nil, "", ""); a.status != http.StatusForbidden {
		t.Errorf("IPv6's loopback let in IPv4's: %d", a.status)
	}
	if c.proc.started() != runs {
		t.Error("changing the list restarted Caddy")
	}
}

// Users are asked for a password, with the domain as the realm; the right one gets
// in, and the app never sees it. With a list too, someone off it gets 403 before any
// password is checked.
func TestPasswordsOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	own := ownAddress(t)
	const password = "correct horse battery staple"
	r := plainRoute("dockwell.env.www", "app.example.net", backend(t, "web"))
	r.PlainForAnyone, r.Users = true, []ingress.User{{Name: "ci", Hash: hashOf(t, password)}, {Name: "ops", Hash: hashOf(t, "another long password")}}
	put(t, c, r)
	readsAs(t, c, r)
	a := c.askFrom(t, own, nil, "", "")
	if a.status != http.StatusUnauthorized || a.header.Get("Www-Authenticate") != `Basic realm="app.example.net"` {
		t.Errorf("with no password, it answers %d, %q", a.status, a.header.Get("Www-Authenticate"))
	}
	if a := c.askFrom(t, own, nil, "ci", "wrong password!"); a.status != http.StatusUnauthorized {
		t.Errorf("with a wrong password, it answers %d", a.status)
	}
	if a := c.askFrom(t, own, nil, "ops", password); a.status != http.StatusUnauthorized {
		t.Errorf("with another user's password, it answers %d", a.status)
	}
	a = c.askFrom(t, own, nil, "ci", password)
	if a.status != http.StatusOK {
		t.Fatalf("with the password, it answers %d", a.status)
	}
	if a.seen["auth"] != "" {
		t.Errorf("the app saw the password: %q", a.seen["auth"])
	}

	r.Allow = []string{own + "/32"}
	put(t, c, r)
	readsAs(t, c, r)
	if a := c.askFrom(t, "127.0.0.1", nil, "ci", password); a.status != http.StatusForbidden {
		t.Errorf("off the list with the password, it answers %d", a.status)
	}
	if a := c.askFrom(t, own, nil, "", ""); a.status != http.StatusUnauthorized {
		t.Errorf("on the list with no password, it answers %d", a.status)
	}
	if a := c.askFrom(t, own, nil, "ci", password); a.status != http.StatusOK {
		t.Errorf("on the list with the password, it answers %d", a.status)
	}
}

// Behind a proxy the route names, the list holds the client that proxy says it
// passed on; from anyone else, the peer, whatever they say.
func TestAListBehindANamedProxyOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	own := ownAddress(t)
	r := plainRoute("dockwell.env.tunnel", "app.example.net", backend(t, "web"))
	r.TrustedProxies, r.PlainForAnyone, r.Allow = []string{"127.0.0.1/32"}, true, []string{"198.51.100.7/32"}
	put(t, c, r)
	readsAs(t, c, r)
	via := func(xff string) map[string]string { return map[string]string{"X-Forwarded-For": xff} }
	if a := c.askFrom(t, "127.0.0.1", via("6.6.6.6, 198.51.100.7"), "", ""); a.status != http.StatusOK || a.seen["real"] != "198.51.100.7" {
		t.Errorf("through the tunnel, from an address on the list, it answers %d, and the app saw %v", a.status, a.seen)
	}
	if a := c.askFrom(t, "127.0.0.1", via("198.51.100.7, 6.6.6.6"), "", ""); a.status != http.StatusForbidden {
		t.Errorf("through the tunnel, from an address off the list that wrote one on it first, it answers %d", a.status)
	}
	if a := c.askFrom(t, "127.0.0.1", nil, "", ""); a.status != http.StatusForbidden {
		t.Errorf("the tunnel itself, off the list, gets %d", a.status)
	}
	if a := c.askFrom(t, own, via("198.51.100.7"), "", ""); a.status != http.StatusForbidden {
		t.Errorf("a peer it doesn't name, saying it passes on one on the list, gets %d", a.status)
	}
	r.Allow = []string{"198.51.100.7/32", own + "/32"}
	put(t, c, r)
	if a := c.askFrom(t, own, nil, "", ""); a.status != http.StatusOK {
		t.Errorf("a peer it doesn't name, itself on the list, gets %d", a.status)
	}
}

// Over HTTPS, as with no proxy in front, the list holds the peer, and a change to it
// applies in the same reload as any route's.
func TestAListOverHTTPSOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	own := ownAddress(t)
	r := aRoute("dockwell.env.secure", "secure.example.net", "", backend(t, "web"))
	r.Allow = []string{"127.0.0.1/32"}
	put(t, c, r)
	_ = c.whoAt(t, "secure.example.net", "/who")
	r.Allow = []string{own + "/32"}
	put(t, c, r)
	res, err := c.client("secure.example.net").Get("https://secure.example.net/who")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("off the list over HTTPS, it answers %d", res.StatusCode)
	}
	readsAs(t, c, r)
}

// What keeps a visitor out is Dockwell's: a hand edit of the list, a user or a hash
// reads as what it now holds, and of the realm or a step as edited by hand. Either is
// drift from the spec, so a revert writes it back.
func TestHandEditsOfWhoGetsInReadAsThemselvesOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	own := ownAddress(t)
	r := plainRoute("dockwell.env.www", "app.example.net", backend(t, "web"))
	r.TrustedProxies, r.PlainForAnyone = []string{"127.0.0.1/32"}, true
	r.Allow, r.Users = []string{own + "/32"}, []ingress.User{{Name: "ci", Hash: hashOf(t, "correct horse battery staple")}}
	put(t, c, r)
	readsAs(t, c, r)
	path := "/config/apps/http/servers/" + httpServerName + "/routes/1"
	for name, edit := range map[string]func(route) route{
		"the list": func(rt route) route {
			rt.Handle[1].Routes[0].Match[0].Not[0].ClientIP.Ranges = []string{"0.0.0.0/0"}
			return rt
		},
		"a step taken off the gate": func(rt route) route {
			rt.Handle[1].Routes = rt.Handle[1].Routes[:1]
			return rt
		},
		"the gate taken off": func(rt route) route {
			rt.Handle = slices.Delete(rt.Handle, 1, 2)
			return rt
		},
		"the check of names taken off": func(rt route) route {
			rt.Handle = slices.Delete(rt.Handle, 2, 3)
			return rt
		},
		"a name the check lets by": func(rt route) route {
			check := &rt.Handle[2].Routes[0].Match[0]
			check.Not = append(check.Not, matcher{HeaderRegexp: map[string]pattern{"Authorization": {Pattern: basicNamePattern("eve")}}})
			return rt
		},
		"a hash": func(rt route) route {
			rt.Handle[3].Providers.HTTPBasic.Accounts[0].Password = hashOf(t, "another long password")
			return rt
		},
		"a user": func(rt route) route {
			b := rt.Handle[3].Providers.HTTPBasic
			b.Accounts = append(b.Accounts, account{Username: "eve", Password: hashOf(t, "another long password")})
			return rt
		},
		"the realm": func(rt route) route {
			rt.Handle[3].Providers.HTTPBasic.Realm = "evil.example.org"
			return rt
		},
		"the password let through": func(rt route) route {
			rt.Handle[4].Headers = guarded(ownName(believingAddress(), "app.example.net"), ingress.Route{Allow: r.Allow})
			return rt
		},
		"a shared cache let keep it": func(rt route) route {
			rt.Handle[4].Headers.Response = nil
			return rt
		},
	} {
		t.Run(name, func(t *testing.T) {
			put(t, c, r)
			b, err := json.Marshal(edit(routeJSON(r)))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.call(t.Context(), http.MethodPatch, path, b, nil); err != nil {
				t.Fatal(err)
			}
			routes, err := c.Routes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(routes) != 1 {
				t.Fatalf("it reads %+v", routes)
			}
			if got := routes[0]; !got.Edited && slices.Equal(got.Allow, r.Allow) && slices.Equal(got.Users, r.Users) {
				t.Errorf("it reads as what Dockwell wrote: %+v", got)
			}
		})
	}
}

// A private route gets its certificate over each challenge: Caddy answers HTTP-01
// before any route, and TLS-ALPN-01 in the handshake, so neither meets the list or
// the password, and a visitor off the list still gets 403 over it.
func TestACertificateComesForAPrivateRouteOnRealCaddy(t *testing.T) {
	for _, challenge := range []string{"http", "tls-alpn"} {
		t.Run(challenge, func(t *testing.T) {
			p := startPebble(t)
			c := startCaddyWithCA(t, p, &ingress.PanelSite{Domain: "panel.example.net"}, challenge)
			p.addRecord(t, "app.example.net")
			r := aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web"))
			r.Allow, r.Users = []string{"198.51.100.7/32"}, []ingress.User{{Name: "ci", Hash: hashOf(t, "correct horse battery staple")}}
			put(t, c, r)
			st := waitFor(t, c.Caddy, "app.example.net", ingress.CertIssued, 45*time.Second)
			if st.Problem != ingress.ProblemNone {
				t.Errorf("the certificate reads %+v", st)
			}
			res := c.waitServing(t, "app.example.net", "/who")
			_ = res.Body.Close()
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("off the list, with its certificate, it answers %d", res.StatusCode)
			}
		})
	}
}

// What a stranger gets, as the server asks its own Caddy: 403 off a list, 401 for a
// password, through a tunnel here as an address no list names, over HTTPS and plain
// HTTP, and "can't tell" where the server itself is on the list. A private route's
// other spellings keep a stranger out too, a private root's below a public path.
func TestTheStrangerCheckOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	up := backend(t, "web")
	listed := aRoute("dockwell.env.listed", "listed.example.net", "", up)
	listed.Allow = []string{"203.0.113.0/24"}
	password := plainRoute("dockwell.env.password", "password.example.net", up)
	password.PlainForAnyone, password.Users = true, []ingress.User{{Name: "ci", Hash: hashOf(t, "correct horse battery staple")}}
	tunnel := plainRoute("dockwell.env.tunnel", "tunnel.example.net", up)
	tunnel.TrustedProxies, tunnel.Allow = []string{"127.0.0.1/32"}, []string{"203.0.113.0/24"}
	open := plainRoute("dockwell.env.open", "open.example.net", up)
	open.TrustedProxies, open.Allow = []string{"127.0.0.1/32"}, []string{"192.0.2.0/24"}
	// A private path under a public root, asked by its odd spellings too.
	shop := aRoute("dockwell.env.shop", "shop.example.net", "", up)
	admin := aRoute("dockwell.env.admin", "shop.example.net", "/admin", up)
	admin.Allow = []string{"203.0.113.0/24"}
	// A private root under a public path, asked from below it, as /api/.;/../ would
	// reach the root's app through /api (#606's final check of H2).
	root := aRoute("dockwell.env.root", "root.example.net", "", up)
	root.Allow = []string{"203.0.113.0/24"}
	api := aRoute("dockwell.env.api", "root.example.net", "/api", up)
	for _, r := range []ingress.Route{listed, password, tunnel, open, shop, admin, root, api} {
		put(t, c, r)
	}
	// Its certificate is Caddy's own, and comes in a moment.
	_ = c.waitServing(t, "listed.example.net", "/who").Body.Close()
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := ingress.AskStrangers(t.Context(), c.Caddy, routes)
	for domain, want := range map[string]dockwellv1.StrangerOutcome{
		"listed.example.net":   dockwellv1.StrangerOutcome_STRANGER_OUTCOME_KEPT_OUT,
		"password.example.net": dockwellv1.StrangerOutcome_STRANGER_OUTCOME_ASKED_FOR_PASSWORD,
		"tunnel.example.net":   dockwellv1.StrangerOutcome_STRANGER_OUTCOME_KEPT_OUT,
		"open.example.net":     dockwellv1.StrangerOutcome_STRANGER_OUTCOME_CANT_TELL,
		"shop.example.net":     dockwellv1.StrangerOutcome_STRANGER_OUTCOME_KEPT_OUT,
		"root.example.net":     dockwellv1.StrangerOutcome_STRANGER_OUTCOME_KEPT_OUT,
	} {
		if len(got[domain]) != 1 || got[domain][0].GetOutcome() != want {
			t.Errorf("%s reads %v, want %v", domain, got[domain], want)
		}
	}
	// A change reads at once, as Watch asks again after one.
	tunnel.Allow = nil
	tunnel.Users = []ingress.User{{Name: "ci", Hash: hashOf(t, "correct horse battery staple")}}
	put(t, c, tunnel)
	if routes, err = c.Routes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := ingress.AskStrangers(t.Context(), c.Caddy, routes)["tunnel.example.net"]; len(got) != 1 || got[0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_ASKED_FOR_PASSWORD {
		t.Errorf("through the tunnel with a password, it reads %v", got)
	}
}

// A private route's other spellings reach neither a public route nor anything an app
// behind could read as the private path: Caddy's matchers see a path decoded and
// cleaned, while Tomcat takes ";x" off before it resolves "..", IIS reads "\\" as "/",
// and Windows takes trailing dots and spaces off. So on a domain with a private route
// and another, a target no browser writes for a site's own links gets 400, as the
// client wrote it, below a public path to a private root too. A domain with one
// route, or none private, is as it was. A hand edit of the spellings a private path
// catches reads as edited (#606's review and its final check, H2).
func TestAPrivatePathsOtherSpellingsReachItOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	own := ownAddress(t)
	up := backend(t, "web")
	plainFor := func(id, domain, path string, allow ...string) ingress.Route {
		r := plainRoute(id, domain, up)
		r.Path, r.PlainForAnyone, r.Allow = path, true, allow
		put(t, c, r)
		return r
	}
	plainFor("dockwell.env.shop", privateDomain, "")
	admin := plainFor("dockwell.env.admin", privateDomain, "/admin", "198.51.100.7/32")
	readsAs(t, c, admin)
	plainFor("dockwell.env.root", "root.example.net", "", "198.51.100.7/32")
	plainFor("dockwell.env.api", "root.example.net", "/api")
	plainFor("dockwell.env.alone", "alone.example.net", "", "198.51.100.7/32")
	plainFor("dockwell.env.other", "other.example.net", "")
	plainFor("dockwell.env.other-api", "other.example.net", "/api")
	// Each as a visitor writes it on the request line, with nothing cleaned or escaped again.
	status := func(host, target string) int {
		t.Helper()
		conn, err := net.Dial("tcp", net.JoinHostPort(own, strconv.Itoa(c.http)))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target, host); err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	for _, p := range []string{"/admin", "/admin/x", "/ADMIN", "//admin"} {
		if got := status(privateDomain, p); got != http.StatusForbidden {
			t.Errorf("%s reaches %d, want the private path's 403", p, got)
		}
	}
	// Each a server behind may read as /admin, and no browser writes: 400 for the
	// domain, or the private path's 403, never the public route.
	for _, p := range []string{
		"/admin;x=1", "/admin%3bx", "/admin%3B", "/admin%00", "/admin%20", "/admin%09", "/admin%5cx", "/admin.", "/admin..",
		"/admin%2e", "/Admin;jsessionid=1", "/x/..;/admin", "/;x/admin", "/.;/admin", "/admin;x/html", "/admin/x/..;/html",
		"/%5cadmin", "/x%5c..%5cadmin", "/admin%253b", "/admin%c2%a0", "/admin%e3%80%80", "/admin%7f", "/admin::$DATA", "/x/%2e%2e/admin",
		// What Caddy cleans to a public path, and Tomcat reads after taking ";x" off.
		"/x/;/../admin", "/x/.;/../admin", "/x/.;x/../admin", "/x/;x/../admin", "/x/y/z/..;/../../admin", "/x/%3b/../admin",
		"/x/%2e%3b/../admin", "/x/;/%2e%2e/admin", "/admin%5cx/..", "/x/%5c/../admin", "/x/admin/../../admin",
		"http://" + privateDomain + "/x/;/../admin", "http://" + privateDomain + "/x/..;/admin",
		// The lows: a trailing dot or space in a segment, overlong UTF-8, look-alikes, "#" and "?" encoded, what no one sees.
		"/admin./x", "/admin%20/x", "/x%c0%af..%c0%afadmin", "/%c0%afadmin", "/%ef%bc%8fadmin", "/x%ef%bc%bc..%ef%bc%bcadmin",
		"/admin%23", "/admin%3f", "/admin%e2%80%8b",
	} {
		if got := status(privateDomain, p); got != http.StatusBadRequest && got != http.StatusForbidden {
			t.Errorf("%s reaches %d, want 400 or the private path's 403", p, got)
		}
	}
	// The public route answers these, whatever its app says of them.
	for _, p := range []string{
		"/", "/adminx", "/admin.php", "/admin2", "/administrator", "/who", "/.well-known/security.txt", "/a.b/c", "/..x",
		"/caf%C3%A9", "/x?q=a;b&r=%2f..%2f", "/~user/a(1)",
	} {
		if got := status(privateDomain, p); got == http.StatusForbidden || got == http.StatusBadRequest {
			t.Errorf("%s answers %d, want the public route", p, got)
		}
	}
	// Below a public path to a private root: an app reads these as the root's.
	for _, p := range []string{"/api/..;/secret", "/api/.;/../secret", "/api/x%5c..%5c..%5csecret", "/api/;/../secret", "/api/%2e%2e/secret", "/api/x/..%c0%afsecret"} {
		if got := status("root.example.net", p); got != http.StatusBadRequest && got != http.StatusForbidden {
			t.Errorf("%s on a private root reaches %d, want 400 or its 403", p, got)
		}
	}
	if got := status("root.example.net", "/api/x"); got == http.StatusForbidden || got == http.StatusBadRequest {
		t.Errorf("the public path below a private root answers %d", got)
	}
	if got := status("root.example.net", "/secret"); got != http.StatusForbidden {
		t.Errorf("a private root answers %d", got)
	}
	// A domain whose one route is private has nothing to fall to, and one with none private is as it was.
	if got := status("alone.example.net", "/x;y"); got != http.StatusForbidden {
		t.Errorf("a domain with one private route answers ;y with %d, want its 403", got)
	}
	for _, p := range []string{"/who;jsessionid=1", "/x/..;/admin", "/%5cadmin", "/admin%253b", "/admin%c2%a0", "/admin::$DATA", "/api/..;/x", "/a%20b", "/.well-known/acme-challenge/x"} {
		if got := status("other.example.net", p); got == http.StatusBadRequest {
			t.Errorf("a domain with no private route refuses %s", p)
		}
	}

	rt := routeJSON(admin)
	if len(rt.Match) != 2 {
		t.Fatalf("a private path matches %+v", rt.Match)
	}
	rt.Match[1].PathRegexp.Pattern = `^/admin$`
	b, err := json.Marshal(rt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.call(t.Context(), http.MethodPatch, "/id/"+admin.ID, b, nil); err != nil {
		t.Fatal(err)
	}
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(routes, func(r ingress.Route) bool { return r.ID == admin.ID }); i < 0 || !routes[i].Edited {
		t.Errorf("a hand edit of the spellings reads %+v", routes)
	}
}

// A name that isn't one of the route's users is answered 401 before any hash is
// checked: Caddy checks a name it doesn't know against a fake hash at cost 14, which
// a flood of made-up names would spend a core on. A user's name still gets its own
// check, and the right password gets in, with a browser asked as before (#606's
// review, M1). A private route's answers aren't for a shared cache (L2).
func TestOnlyAUsersNameCostsAHashOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	own := ownAddress(t)
	const password = "correct horse battery staple"
	r := plainRoute("dockwell.env.www", privateDomain, backend(t, "web"))
	r.PlainForAnyone, r.Users = true, []ingress.User{{Name: "ci", Hash: hashOf(t, password)}, {Name: "ops.team_1", Hash: hashOf(t, password)}}
	put(t, c, r)
	readsAs(t, c, r)
	a := c.askFrom(t, own, nil, "", "")
	if a.status != http.StatusUnauthorized || a.header.Get("Www-Authenticate") != `Basic realm="app.example.net"` {
		t.Errorf("with no password, it answers %d, %q", a.status, a.header.Get("Www-Authenticate"))
	}
	// Five made-up names, each of which would cost Caddy's own check about a second,
	// each with a password of its own, as Caddy keeps what it checked.
	start := time.Now()
	for _, name := range []string{"eve", "admin", "root", "c", "cii"} {
		if a := c.askFrom(t, own, nil, name, password+" for "+name); a.status != http.StatusUnauthorized || a.header.Get("Www-Authenticate") == "" {
			t.Errorf("the name %s answers %d, %q", name, a.status, a.header.Get("Www-Authenticate"))
		}
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("five names that aren't users took %s, as if each cost a hash", took)
	}
	for _, name := range []string{"ci", "ops.team_1"} {
		if a := c.askFrom(t, own, nil, name, "a wrong long password"); a.status != http.StatusUnauthorized {
			t.Errorf("%s with a wrong password answers %d", name, a.status)
		}
		a := c.askFrom(t, own, nil, name, password)
		if a.status != http.StatusOK {
			t.Errorf("%s with the password answers %d", name, a.status)
		}
		if cc := a.header.Values("Cache-Control"); !slices.Contains(cc, "private") {
			t.Errorf("a signed-in answer says Cache-Control %q, want private", cc)
		}
	}
}
