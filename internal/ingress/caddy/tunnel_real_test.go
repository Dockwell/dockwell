// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	routekind "github.com/dockwell/dockwell/internal/resource/route"
)

// plainRoute is a route that takes no certificate.
func plainRoute(id, domain string, dials ...string) ingress.Route {
	return ingress.Route{ID: id, Domain: domain, Upstreams: dials, Plain: true}
}

// startPlain is Caddy started for routes that take no certificate.
func startPlain(t *testing.T) *realCaddy {
	t.Helper()
	bin := needCaddy(t)
	dir := shortDir(t)
	proc := &process{bin: bin, dir: dir}
	c := &Caddy{Dir: dir, Runtime: proc, HTTPPort: freePort(t), HTTPSPort: freePort(t), issuerForTest: map[string]any{"module": "internal"}}
	t.Cleanup(func() { proc.stop(syscall.SIGKILL) })
	if err := c.Start(t.Context(), nil, true); err != nil {
		out, _ := os.ReadFile(filepath.Join(dir, "caddy.out"))
		t.Fatalf("%v\n%s", err, out)
	}
	return &realCaddy{Caddy: c, proc: proc, http: c.HTTPPort, tls: c.HTTPSPort}
}

// plainAnswer is what came back over plain HTTP.
type plainAnswer struct {
	status   int
	location string
	body     []byte
}

// who reads what the upstream saw.
func (a plainAnswer) who(t *testing.T) map[string]string {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal(a.body, &out); err != nil {
		t.Fatalf("%d: %q: %v", a.status, a.body, err)
	}
	return out
}

// plainGet asks over plain HTTP, following no redirect, as Host says.
func (r *realCaddy) plainGet(t *testing.T, host, path string, headers map[string]string) plainAnswer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", r.http, path), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET http://%s%s: %v", host, path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("GET http://%s%s: %v", host, path, err)
	}
	return plainAnswer{status: res.StatusCode, location: res.Header.Get("Location"), body: body}
}

// bound reports whether something holds a port, on TCP or UDP.
func bound(t *testing.T, port int) (tcp, udp bool) {
	t.Helper()
	if l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", fmt.Sprintf(":%d", port)); err == nil {
		_ = l.Close()
	} else {
		tcp = true
	}
	if u, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp", fmt.Sprintf(":%d", port)); err == nil {
		_ = u.Close()
	} else {
		udp = true
	}
	return tcp, udp
}

// noCertificateFor says nothing was ever asked of a certificate authority for a
// domain: no certificate on disk, and no word of it in what Caddy logs of issuing.
func noCertificateFor(t *testing.T, c *realCaddy, domain string) {
	t.Helper()
	found, _ := filepath.Glob(filepath.Join(c.Dir, "data", "caddy", "certificates", "*", domain))
	if len(found) > 0 {
		t.Errorf("there's a certificate for %s: %v", domain, found)
	}
	log, err := os.ReadFile(filepath.Join(c.Dir, "data", tlsLogName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if strings.Contains(string(log), domain) {
		t.Errorf("Caddy worked on a certificate for %s: %s", domain, log)
	}
	out, _ := os.ReadFile(filepath.Join(c.Dir, "caddy.out"))
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, domain) && strings.Contains(line, "obtain") {
			t.Errorf("Caddy tried for a certificate for %s: %s", domain, line)
		}
	}
}

// A route that takes no certificate is served over plain HTTP, with no redirect to
// HTTPS, as that would loop behind a tunnel. With no route needing HTTPS, nothing
// binds its port, over TCP or UDP. A name with no route gets a 404, not an empty
// 200 that reads as a site that works.
func TestAPlainRouteServesHTTPAloneOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	if tcp, udp := bound(t, c.tls); tcp || udp {
		t.Errorf("port %d is held, TCP %v and UDP %v, with no route needing HTTPS", c.tls, tcp, udp)
	}
	if tcp, _ := bound(t, c.http); !tcp {
		t.Errorf("nothing holds the plain HTTP port %d", c.http)
	}
	put(t, c, plainRoute("dockwell.env.www", "app.example.net", backend(t, "web")))
	res := c.plainGet(t, "app.example.net", "/who", nil)
	if res.status != http.StatusOK {
		t.Fatalf("http://app.example.net/who answers %d, %s, want it served", res.status, res.location)
	}
	seen := res.who(t)
	if seen["name"] != "web" || seen["proto"] != "http" {
		t.Errorf("the service saw %v", seen)
	}
	if tcp, udp := bound(t, c.tls); tcp || udp {
		t.Errorf("port %d is held, TCP %v and UDP %v, after a route that takes no certificate", c.tls, tcp, udp)
	}
	res = c.plainGet(t, "nope.example.net", "/who", nil)
	if res.status != http.StatusNotFound {
		t.Errorf("a name with no route answers %d, want 404", res.status)
	}
	if !strings.Contains(string(res.body), "no site at this address") {
		t.Errorf("a name with no route reads %q", res.body)
	}
	noCertificateFor(t, c, "app.example.net")
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || !routes[0].Plain || routes[0].Edited || routes[0].Domain != "app.example.net" {
		t.Errorf("it reads back as %+v", routes)
	}
	// A stop for good and a resume keep all that, as Docker's restart does.
	c.proc.stop(syscall.SIGKILL)
	if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	waitAdmin(t, c)
	if res := c.plainGet(t, "app.example.net", "/who", nil); res.status != http.StatusOK {
		t.Errorf("after a restart it answers %d", res.status)
	}
	if tcp, udp := bound(t, c.tls); tcp || udp {
		t.Errorf("port %d is held after a restart", c.tls)
	}
}

// Beside a route with a certificate, each is served its own way: HTTPS gets bound
// when something needs it, each domain's plain HTTP does what it should, and TLS
// answers for nothing that takes no certificate. One config change moves a route
// from one to the other, and the last with a certificate going frees the port.
func TestPlainAndSecureRoutesKeepToThemselvesOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	up := backend(t, "web")
	put(t, c, plainRoute("dockwell.env.plain", "plain.example.net", up))
	put(t, c, aRoute("dockwell.env.secure", "secure.example.net", "", up))
	if seen := c.whoAt(t, "secure.example.net", "/who"); seen["name"] != "web" || seen["proto"] != "https" {
		t.Errorf("over HTTPS the service saw %v", seen)
	}
	if tcp, _ := bound(t, c.tls); !tcp {
		t.Error("nothing holds the HTTPS port with a route that takes a certificate")
	}
	res := c.plainGet(t, "secure.example.net", "/x?y=1", nil)
	if res.status != http.StatusPermanentRedirect || !strings.HasPrefix(res.location, "https://secure.example.net") {
		t.Errorf("http://secure.example.net answers %d, %q, want a redirect", res.status, res.location)
	}
	if res := c.plainGet(t, "plain.example.net", "/who", nil); res.status != http.StatusOK {
		t.Errorf("http://plain.example.net answers %d, %q, want it served", res.status, res.location)
	}
	if res := c.plainGet(t, "nope.example.net", "/who", nil); res.status != http.StatusNotFound {
		t.Errorf("a name with no route answers %d", res.status)
	}
	if res, err := c.client("plain.example.net").Get("https://plain.example.net/who"); err == nil {
		_ = res.Body.Close()
		t.Errorf("a domain that takes no certificate answered over TLS: %d", res.StatusCode)
	}
	noCertificateFor(t, c, "plain.example.net")

	// In one reload, either way.
	put(t, c, plainRoute("dockwell.env.secure", "secure.example.net", up))
	if res := c.plainGet(t, "secure.example.net", "/who", nil); res.status != http.StatusOK {
		t.Errorf("a route turned plain answers %d", res.status)
	}
	if tcp, udp := bound(t, c.tls); tcp || udp {
		t.Errorf("port %d is held, TCP %v and UDP %v, with nothing needing it", c.tls, tcp, udp)
	}
	put(t, c, aRoute("dockwell.env.plain", "plain.example.net", "", up))
	if seen := c.whoAt(t, "plain.example.net", "/who"); seen["proto"] != "https" {
		t.Errorf("a route given a certificate reads %v", seen)
	}
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes %+v", routes)
	}
	for _, r := range routes {
		if r.Edited || r.Plain != (r.ID == "dockwell.env.secure") {
			t.Errorf("%+v", r)
		}
	}
}

// A second list of what's on plain HTTP: what closes routes covers these, and the
// rest still redirect; a Caddy that's down has them taken out of its last config.
func TestPlainRoutesCloseAndGoOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	up := backend(t, "web")
	put(t, c, plainRoute("dockwell.env.plain", "plain.example.net", up))
	put(t, c, aRoute("dockwell.env.secure", "secure.example.net", "", up))
	_ = c.whoAt(t, "secure.example.net", "/who")
	if err := c.SetClosed(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if res := c.plainGet(t, "plain.example.net", "/who", nil); res.status != http.StatusServiceUnavailable {
		t.Errorf("a closed plain route answers %d, want 503", res.status)
	}
	if res := c.plainGet(t, "secure.example.net", "/who", nil); res.status != http.StatusPermanentRedirect {
		t.Errorf("closed, a domain with a certificate's plain HTTP answers %d, want its redirect", res.status)
	}
	if res := c.plainGet(t, "nope.example.net", "/who", nil); res.status != http.StatusNotFound {
		t.Errorf("closed, a name with no route answers %d, want 404", res.status)
	}
	// A route changed while they are closed stays closed.
	put(t, c, plainRoute("dockwell.env.other", "other.example.net", up))
	if res := c.plainGet(t, "other.example.net", "/who", nil); res.status != http.StatusServiceUnavailable {
		t.Errorf("a route added while closed answers %d, want 503", res.status)
	}
	if err := c.SetClosed(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if res := c.plainGet(t, "plain.example.net", "/who", nil); res.status != http.StatusOK {
		t.Errorf("open again, it answers %d", res.status)
	}
	// With Caddy down, a plain route still comes out of the last config.
	c.proc.stop(syscall.SIGKILL)
	if routes, err := c.Routes(t.Context()); err != nil || len(routes) != 3 {
		t.Fatalf("with Caddy down, routes read %+v, %v", routes, err)
	}
	if err := c.DeleteRoute(t.Context(), "dockwell.env.plain"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetClosed(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	waitAdmin(t, c)
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(routes, func(r ingress.Route) bool { return r.ID == "dockwell.env.plain" }) {
		t.Errorf("a plain route removed with Caddy down came back: %+v", routes)
	}
	if res := c.plainGet(t, "plain.example.net", "/who", nil); res.status != http.StatusNotFound {
		t.Errorf("after coming back it answers %d, want 404", res.status)
	}
	if res := c.plainGet(t, "other.example.net", "/who", nil); res.status != http.StatusServiceUnavailable {
		t.Errorf("closed with Caddy down, it answers %d, want 503", res.status)
	}
}

// A plain route's hand edit reads as one, and so does a matcher Dockwell doesn't
// write.
func TestHandEditsOfAPlainRouteReadAsThemselvesOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	up := backend(t, "web")
	put(t, c, plainRoute("dockwell.env.www", "app.example.net", up))
	path := serversPath + "/" + httpServerName + "/routes/0"
	edited, err := json.Marshal(map[string]any{
		"@id": "dockwell.env.www", "terminal": true,
		"match":  []map[string]any{{"host": []string{"app.example.net"}, "protocol": "https"}},
		"handle": []map[string]any{{"handler": "reverse_proxy", "upstreams": []map[string]any{{"dial": up}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.call(t.Context(), http.MethodPatch, "/config/apps/http/servers/"+httpServerName+"/routes/0", edited, nil); err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || !routes[0].Edited {
		t.Errorf("a matcher changed by hand reads %+v", routes)
	}
}

// What a service reads of its client. With no proxy named, nothing a client sends
// counts. With one named, a request from it is believed, read from the right, so
// what the client added itself stays out of it; a request from anywhere else isn't
// believed, whichever route it reaches; and a route that names none, on the same
// server, believes nothing, though Caddy decides what it trusts per server.
func TestTrustedProxiesOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	up := backend(t, "web")
	put(t, c, plainRoute("dockwell.env.none", "none.example.net", up))
	forged := map[string]string{"X-Forwarded-For": "6.6.6.6", "X-Forwarded-Proto": "https", "X-Forwarded-Host": "evil.example.net"}
	seen := c.plainGet(t, "none.example.net", "/who", forged).who(t)
	if seen["xff"] != "127.0.0.1" || seen["real"] != "127.0.0.1" || seen["proto"] != "http" {
		t.Errorf("with no proxy named, the service saw %v", seen)
	}

	trusted := plainRoute("dockwell.env.tunnel", "tunnel.example.net", up)
	trusted.TrustedProxies = []string{"127.0.0.1/32"}
	put(t, c, trusted)
	other := plainRoute("dockwell.env.other", "other.example.net", up)
	other.TrustedProxies = []string{"203.0.113.0/24"}
	put(t, c, other)

	// As Cloudflare sends it: what the client said, then who connected to it.
	chain := map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.7", "X-Forwarded-Proto": "https", "X-Forwarded-Host": "tunnel.example.net"}
	seen = c.plainGet(t, "tunnel.example.net", "/who", chain).who(t)
	if seen["real"] != "198.51.100.7" || seen["xff"] != "198.51.100.7" {
		t.Errorf("through a proxy it trusts, the service saw %v, want the address the proxy found, alone", seen)
	}
	if seen["proto"] != "https" {
		t.Errorf("through a proxy it trusts, the service reads X-Forwarded-Proto %q, want the proxy's https, or an app redirects for ever", seen["proto"])
	}
	// Past a second proxy it trusts, to the first address it doesn't.
	chain["X-Forwarded-For"] = "6.6.6.6, 198.51.100.7, 127.0.0.1"
	if seen = c.plainGet(t, "tunnel.example.net", "/who", chain).who(t); seen["real"] != "198.51.100.7" {
		t.Errorf("past a proxy it trusts, the service saw %v", seen)
	}
	// With nothing to believe, the peer is the client.
	if seen = c.plainGet(t, "tunnel.example.net", "/who", nil).who(t); seen["real"] != "127.0.0.1" || seen["xff"] != "127.0.0.1" {
		t.Errorf("with no header, the service saw %v", seen)
	}
	chain["X-Forwarded-For"] = "not an address"
	if seen = c.plainGet(t, "tunnel.example.net", "/who", chain).who(t); seen["real"] != "127.0.0.1" {
		t.Errorf("with a header that says nothing, the service saw %v", seen)
	}

	// A route that names none, on a server that now trusts this peer.
	seen = c.plainGet(t, "none.example.net", "/who", forged).who(t)
	if seen["xff"] != "127.0.0.1" || seen["real"] != "127.0.0.1" || seen["proto"] != "http" {
		t.Errorf("a route that names no proxy believed another route's: the service saw %v", seen)
	}
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Edited {
			t.Errorf("%s reads as edited by hand: %+v", r.ID, r)
		}
	}

	// HTTPS is a server too, as behind a proxy that goes on to this one over TLS.
	secure := aRoute("dockwell.env.secure", "secure.example.net", "", up)
	secure.TrustedProxies = []string{"127.0.0.1/32"}
	put(t, c, secure)
	_ = c.whoAt(t, "secure.example.net", "/who")
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://secure.example.net/who", http.NoBody)
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.8")
	res, err := c.client("secure.example.net").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if seen := who(t, res); seen["real"] != "198.51.100.8" || seen["xff"] != "198.51.100.8" {
		t.Errorf("over HTTPS through a proxy it trusts, the service saw %v", seen)
	}

	// With none left, the servers trust nothing, and the headers are as before.
	for _, id := range []string{"dockwell.env.tunnel", "dockwell.env.other", "dockwell.env.secure"} {
		if err := c.DeleteRoute(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	b, _, err := c.call(t.Context(), http.MethodGet, serversPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "trusted_proxies") {
		t.Errorf("with no route naming a proxy, a server still trusts one: %s", b)
	}
	if strings.Contains(string(b), "X-Forwarded-Proto") {
		t.Errorf("with no route naming a proxy, a route still overwrites what Caddy writes: %s", b)
	}
	seen = c.plainGet(t, "none.example.net", "/who", forged).who(t)
	if seen["xff"] != "127.0.0.1" || seen["proto"] != "http" {
		t.Errorf("with none left, the service saw %v", seen)
	}
}

// A proxy that isn't the peer is believed by no one: the header says what it says,
// and the address comes from the connection.
func TestAProxyNamedElsewhereIsntBelievedOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	r := plainRoute("dockwell.env.www", "app.example.net", backend(t, "web"))
	r.TrustedProxies = []string{"203.0.113.0/24"}
	put(t, c, r)
	seen := c.plainGet(t, "app.example.net", "/who", map[string]string{"X-Forwarded-For": "6.6.6.6", "X-Forwarded-Proto": "https"}).who(t)
	if seen["real"] != "127.0.0.1" || seen["xff"] != "127.0.0.1" || seen["proto"] != "http" {
		t.Errorf("from a peer it doesn't trust, the service saw %v", seen)
	}
}

// A WebSocket over plain HTTP stays open through a reload, as those over HTTPS do.
func TestAWebSocketOnAPlainRouteStaysThroughReloadsOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	up := backend(t, "web")
	put(t, c, plainRoute("dockwell.env.www", "app.example.net", up))
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", c.http))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: app.example.net\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: x\r\nSec-WebSocket-Version: 13\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	read := bufio.NewReader(conn)
	line, err := read.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		t.Fatalf("upgrade: %q, %v", line, err)
	}
	for {
		l, err := read.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == "\r\n" {
			break
		}
	}
	// The route it is on, another that gives HTTPS a port, and that port freed.
	put(t, c, plainRoute("dockwell.env.www", "app.example.net", up, up))
	put(t, c, aRoute("dockwell.env.secure", "secure.example.net", "", up))
	if err := c.DeleteRoute(t.Context(), "dockwell.env.secure"); err != nil {
		t.Fatal(err)
	}
	// The backend's Hijack copies what comes back, to nowhere: a closed connection
	// is what this can tell.
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := read.ReadByte(); err == nil || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("a WebSocket through reloads: %v, want it still open", err)
	}
}

// The whole of a route behind a tunnel, from its plan to a request: to a process
// on the server, with no certificate, naming the tunnel as its proxy, on port 80
// alone, with no network and no drift. A route that wants a certificate takes what
// it needs, and the last route frees everything.
func TestARouteBehindATunnelOnRealCaddy(t *testing.T) {
	s := newWholeStack(t, nil)
	onHost := backend(t, "on-the-server")
	_, portWords, err := net.SplitHostPort(onHost)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portWords)
	if err != nil {
		t.Fatal(err)
	}
	spec := &resourcev1.Route{Domain: "app.example.net", HostPort: uint32(port), Tls: ingress.TLSOff, TrustedProxies: []string{"127.0.0.1"}}
	step := s.change(t, "www", spec)
	if step.Ingress == nil || step.Ingress.Change != resource.IngressStarts || !slices.Equal(step.Ingress.Ports, []string{fmt.Sprintf("%d/tcp", s.http)}) {
		t.Errorf("the first step's effect is %+v", step.Ingress)
	}
	if !step.Unencrypted {
		t.Error("the plan doesn't say traffic reaches this server unencrypted")
	}
	if step.Domain != nil {
		t.Errorf("the plan says what DNS says, of a domain that points at a tunnel: %+v", step.Domain)
	}
	r := &realCaddy{Caddy: s.caddy, proc: s.proc, http: s.http, tls: s.tls}
	seen := r.plainGet(t, "app.example.net", "/who", map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.7", "X-Forwarded-Proto": "https"}).who(t)
	if seen["name"] != "on-the-server" || seen["real"] != "198.51.100.7" || seen["proto"] != "https" {
		t.Errorf("the process on the server saw %v", seen)
	}
	if tcp, udp := bound(t, s.tls); tcp || udp {
		t.Errorf("port %d is held, with no route needing HTTPS", s.tls)
	}
	if s.engine.Network(docker.IngressNetwork) != nil {
		t.Error("a route to a process on the server made the ingress network")
	}
	noCertificateFor(t, r, "app.example.net")
	if err := s.mgr.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.engine.Network(docker.IngressNetwork) != nil {
		t.Error("a reconcile made the ingress network")
	}
	// Reading it back as what it said is no drift: Validate wrote 127.0.0.1 as its
	// range, and that's what Apply compared. The same spec plans nothing.
	again := proto.Clone(spec).(*resourcev1.Route)
	steps, err := s.agent.Plan(t.Context(), []resource.Desired{{Ref: resource.Ref{Kind: routekind.Kind, Scope: s.env, Name: "www"}, Spec: again}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != resource.ActionNone {
		t.Errorf("a route read back plans %+v, want nothing", steps[0])
	}

	// A route that wants a certificate takes the rest, with this one left alone.
	s.container("web")
	addStep := s.change(t, "secure", &resourcev1.Route{Domain: "secure.example.net", Service: "web", Port: 8080})
	if addStep.Ingress == nil || addStep.Ingress.Change != resource.IngressTakesPorts {
		t.Errorf("the first route with a certificate says %+v", addStep.Ingress)
	}
	if tcp, _ := bound(t, s.tls); !tcp {
		t.Errorf("nothing holds port %d after a route that takes a certificate", s.tls)
	}
	if addStep.Unencrypted {
		t.Error("the plan of a route with a certificate says traffic is unencrypted")
	}
	delStep := s.change(t, "secure", nil)
	if delStep.Ingress == nil || delStep.Ingress.Change != resource.IngressFreesPorts {
		t.Errorf("the last route with a certificate going says %+v", delStep.Ingress)
	}
	if tcp, udp := bound(t, s.tls); tcp || udp {
		t.Errorf("port %d is held after the last route with a certificate went", s.tls)
	}
	last := s.change(t, "www", nil)
	if last.Ingress == nil || last.Ingress.Change != resource.IngressStops {
		t.Errorf("the last route says %+v", last.Ingress)
	}
	if tcp, _ := bound(t, s.http); tcp {
		t.Errorf("port %d is held after the last route", s.http)
	}
}

// The panel's rules on plain HTTP, and its redirect, stay beside plain routes, and
// each goes without the other. Moving the panel's site in and out is one reload.
func TestThePanelsRulesStayBesidePlainRoutesOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	up := backend(t, "web")
	put(t, c, plainRoute("dockwell.env.www", "app.example.net", up))
	site := &ingress.PanelSite{Domain: "panel.example.net"}
	if err := c.SetPanel(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	if tcp, _ := bound(t, c.tls); !tcp {
		t.Error("nothing holds the HTTPS port, with the panel's site to serve")
	}
	if res := c.plainGet(t, "app.example.net", "/who", nil); res.status != http.StatusOK {
		t.Errorf("beside the panel's rules, a plain route answers %d", res.status)
	}
	if res := c.plainGet(t, "panel.example.net", "/x", map[string]string{"Authorization": "Bearer dwt_x"}); res.status != http.StatusBadRequest {
		t.Errorf("a token over plain HTTP to the panel's domain gets %d, want 400", res.status)
	}
	res := c.plainGet(t, "panel.example.net", "/x", nil)
	if res.status != http.StatusPermanentRedirect || !strings.HasPrefix(res.location, "https://panel.example.net") {
		t.Errorf("the panel's domain over plain HTTP gets %d, %q, want a redirect", res.status, res.location)
	}
	if err := c.SetPanel(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if res := c.plainGet(t, "app.example.net", "/who", nil); res.status != http.StatusOK {
		t.Errorf("after the panel's rules went, a plain route answers %d", res.status)
	}
	b, _, err := c.call(t.Context(), http.MethodGet, serversPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), panelPlainPrefix) || strings.Contains(string(b), panelRouteID+"\"") {
		t.Errorf("the panel's rules stayed: %s", b)
	}
	if tcp, _ := bound(t, c.tls); tcp {
		t.Error("the HTTPS port stayed held, with nothing to serve there")
	}
}

// The panel's key goes with the last route, from Caddy's last config, the agent's
// copy and what a crash left of either, so no later Caddy can read it.
func TestNoKeyStaysBehindOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	site := &ingress.PanelSite{Domain: "panel.example.net", Cert: selfSignedChain(t, "panel.example.net", "the panel's own CA")}
	if err := c.SetPanel(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	if err := c.PutRoute(t.Context(), aRoute("dockwell.env.www", "app.example.net", "", "127.0.0.1:1")); err != nil {
		t.Fatal(err)
	}
	// What older builds and crashes leave.
	for _, left := range []string{
		filepath.Join(c.configDir(), "caddy", "autosave.json.last"),
		filepath.Join(c.configDir(), "caddy", "autosave.json.damaged"),
		filepath.Join(c.configDir(), "caddy", ".autosave.json.1234.tmp"),
		filepath.Join(c.Dir, ".last-good.json.1234.tmp"),
	} {
		if err := os.WriteFile(left, []byte("-----BEGIN PRIVATE KEY-----"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Certificates are Caddy's own, for its own routes, and stay: only the panel's
	// key must go, and only these hold that.
	var left []string
	err := filepath.WalkDir(c.Dir, func(p string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case !d.Type().IsRegular() || strings.HasPrefix(p, filepath.Join(c.Dir, "data")):
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "PRIVATE KEY") {
			left = append(left, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("with Caddy gone and the panel on its ports, a key is still in %v", left)
	}
}

// A temp file a crash left goes at the next write, as what it holds may be what
// the file no longer does.
func TestAWriteClearsWhatACrashLeft(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, ".copy.json.999.tmp")
	if err := os.WriteFile(left, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, ".other.json.999.tmp")
	if err := os.WriteFile(other, []byte("not this file's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeIn(dir, "copy.json", []byte("new"), 0o600, agentOwns); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("what a crash left is still there: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another file's was taken: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "copy.json")); err != nil || string(b) != "new" {
		t.Errorf("the file reads %q, %v", b, err)
	}
	// A FIFO at the directory can't hold it up: it isn't a directory.
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		root, err := os.OpenRoot(dir)
		if err != nil {
			done <- err
			return
		}
		defer root.Close()
		done <- removeTemps(root, "pipe/x")
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was read as a directory")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO held up removing what a crash left")
	}
}
