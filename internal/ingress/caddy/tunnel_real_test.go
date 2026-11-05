// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// waitAnswering waits for Caddy's plain HTTP port, once it has come back: its admin
// socket answers before its HTTP listener is up, so waitAdmin says too little.
func (r *realCaddy) waitAnswering(t *testing.T) {
	t.Helper()
	waitAdmin(t, r)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", r.http))
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing answers on port %d", r.http)
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
	c.waitAnswering(t)
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
	c.waitAnswering(t)
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
	path := serversPath + "/" + httpServerName + "/routes/1"
	edited, err := json.Marshal(map[string]any{
		"@id": "dockwell.env.www", "terminal": true,
		"match":  []map[string]any{{"host": []string{"app.example.net"}, "protocol": "https"}},
		"handle": []map[string]any{{"handler": "reverse_proxy", "upstreams": []map[string]any{{"dial": up}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.call(t.Context(), http.MethodPatch, "/config/apps/http/servers/"+httpServerName+"/routes/1", edited, nil); err != nil {
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
	if strings.Contains(string(b), `"X-Forwarded-Proto"`) {
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
	r.TrustedProxies, r.PlainForAnyone = []string{"203.0.113.0/24"}, true
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
	// What's there is this test's own server, which reads as this process.
	if step.ServerPort == nil || step.ServerPort.Port != uint32(port) || step.ServerPort.Listening == "" || step.ServerPort.Nothing {
		t.Errorf("the plan doesn't say what listens on port %d: %+v", port, step.ServerPort)
	}
	if step.Answers == nil || step.Answers.Reach != resource.ReachProxies || !slices.Equal(step.Answers.Proxies, []string{"127.0.0.1/32"}) {
		t.Errorf("the plan doesn't say who it answers: %+v", step.Answers)
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

// ownAddress is a non-loopback address of this machine, which a request to it comes
// from too, so it stands for a peer that isn't this server's loopback.
func ownAddress(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	t.Skip("this machine has no address but loopback")
	return ""
}

// plainGetAt is plainGet to one of this machine's addresses, which the request comes
// from too.
func (r *realCaddy) plainGetAt(t *testing.T, addr, host string, headers map[string]string) plainAnswer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://%s:%d/who", addr, r.http), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET http://%s/who as %s: %v", addr, host, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return plainAnswer{status: res.StatusCode, location: res.Header.Get("Location"), body: body}
}

// A route with no certificate doesn't answer the whole internet. On a server with a
// public address and no firewall, Host alone would otherwise go around a tunnel and
// whatever Access or WAF sits in it. So it answers its own proxies, by the peer on
// the connection and nothing it says, or with none, this server's own loopback. To
// anyone else it's what a name with no route is, so they can't tell it's there.
func TestAPlainRouteAnswersOnlyWhoItNamesOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	own := ownAddress(t)
	up := backend(t, "web")
	// Naming none, as with a tunnel on the server's own network: loopback alone.
	put(t, c, plainRoute("dockwell.env.none", "none.example.net", up))
	nobody := c.plainGet(t, "nope.example.net", "/who", nil)
	if nobody.status != http.StatusNotFound {
		t.Fatalf("a name with no route answers %d, so this proves nothing", nobody.status)
	}
	stranger := func(what, domain, addr string) {
		t.Helper()
		got := c.plainGetAt(t, addr, domain, nil)
		if got.status != nobody.status || !bytes.Equal(got.body, nobody.body) {
			t.Errorf("%s answered %d %q from %s, and a name with no route gets %d %q", what, got.status, got.body, addr, nobody.status, nobody.body)
		}
	}
	if got := c.plainGetAt(t, "127.0.0.1", "none.example.net", nil); got.status != http.StatusOK {
		t.Errorf("a route naming no proxy answers %d over loopback", got.status)
	}
	// localhost may reach ::1 first, and a tunnel dialling it is this server too.
	if got := c.plainGetAt(t, "[::1]", "none.example.net", nil); got.status != http.StatusOK {
		t.Errorf("a route naming no proxy answers %d over IPv6's loopback", got.status)
	}
	stranger("a route naming no proxy", "none.example.net", own)

	// A tunnel that's named: it answers that, and believes it.
	tunnel := plainRoute("dockwell.env.tunnel", "tunnel.example.net", up)
	tunnel.TrustedProxies = []string{"127.0.0.1/32"}
	put(t, c, tunnel)
	if seen := c.plainGetAt(t, "127.0.0.1", "tunnel.example.net", map[string]string{"X-Forwarded-For": "198.51.100.7"}).who(t); seen["real"] != "198.51.100.7" {
		t.Errorf("through its tunnel, the service saw %v", seen)
	}
	stranger("a route naming its tunnel", "tunnel.example.net", own)
	// Saying it's the proxy counts for nothing.
	got := c.plainGetAt(t, own, "tunnel.example.net", map[string]string{"X-Forwarded-For": "127.0.0.1", "X-Real-Ip": "127.0.0.1", "Forwarded": "for=127.0.0.1"})
	if got.status != nobody.status {
		t.Errorf("a stranger who says it's the proxy answered %d", got.status)
	}

	// An actual proxy on the network: that peer, not loopback.
	proxy := plainRoute("dockwell.env.proxy", "proxy.example.net", up)
	proxy.TrustedProxies = []string{own + "/32"}
	put(t, c, proxy)
	if seen := c.plainGetAt(t, own, "proxy.example.net", map[string]string{"X-Forwarded-For": "198.51.100.8", "X-Forwarded-Proto": "https"}).who(t); seen["real"] != "198.51.100.8" || seen["proto"] != "https" {
		t.Errorf("through its proxy, the service saw %v", seen)
	}
	stranger("a route naming a proxy elsewhere", "proxy.example.net", "127.0.0.1")

	// Anyone, as someone asked, with the peer as the client.
	open := plainRoute("dockwell.env.open", "open.example.net", up)
	open.PlainForAnyone = true
	put(t, c, open)
	for _, addr := range []string{"127.0.0.1", own} {
		if seen := c.plainGetAt(t, addr, "open.example.net", map[string]string{"X-Forwarded-For": "6.6.6.6"}).who(t); seen["real"] != addr {
			t.Errorf("a route for anyone, from %s, read %v", addr, seen)
		}
	}
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 4 {
		t.Fatalf("routes: %+v", routes)
	}
	for _, r := range routes {
		if r.Edited {
			t.Errorf("%s reads as edited by hand: %+v", r.ID, r)
		}
		if r.PlainForAnyone != (r.ID == "dockwell.env.open") {
			t.Errorf("%s reads %+v", r.ID, r)
		}
	}

	// What closes routes keeps to the same reach: a stranger still sees nothing there.
	if err := c.SetClosed(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if got := c.plainGetAt(t, "127.0.0.1", "none.example.net", nil); got.status != http.StatusServiceUnavailable {
		t.Errorf("closed, a route answers %d to what it answers", got.status)
	}
	stranger("a closed route", "none.example.net", own)
	if got := c.plainGetAt(t, own, "open.example.net", nil); got.status != http.StatusServiceUnavailable {
		t.Errorf("closed, a route for anyone answers %d", got.status)
	}
}

// HTTP-01 for a route with a certificate comes to the same port, from the CA, and a
// route's reach and the 404 after it are no part of that.
func TestHTTP01GoesPastAPlainRoutesReachOnRealCaddy(t *testing.T) {
	p := startPebble(t)
	bin := needCaddy(t)
	dir := shortDir(t)
	proc := &process{bin: bin, dir: dir}
	c := &Caddy{Dir: dir, Runtime: proc, ACMEDirectory: p.directory, ACMERoots: p.roots, HTTPPort: p.httpPort, HTTPSPort: p.tlsPort}
	c.issuerForTest = map[string]any{
		"module": "acme", "ca": p.directory,
		"challenges": map[string]any{
			"http":     map[string]any{"alternate_port": p.httpPort},
			"tls-alpn": map[string]any{"disabled": true},
		},
		"trusted_roots_pem_files": []string{filepath.Join(proc.Paths().Boot, acmeRootsName)},
	}
	t.Cleanup(func() { proc.stop(syscall.SIGKILL) })
	if err := c.Start(t.Context(), nil, true); err != nil {
		out, _ := os.ReadFile(filepath.Join(dir, "caddy.out"))
		t.Fatalf("%v\n%s", err, out)
	}
	r := &realCaddy{Caddy: c, proc: proc, http: c.HTTPPort, tls: c.HTTPSPort}
	up := backend(t, "web")
	// It names a proxy the CA isn't, so its reach would refuse the CA if that came
	// into it, and the 404 would swallow the challenge.
	named := plainRoute("dockwell.env.plain", "plain.example.net", up)
	named.TrustedProxies = []string{"203.0.113.0/24"}
	put(t, r, named)
	put(t, r, plainRoute("dockwell.env.local", "local.example.net", up))
	if res := r.plainGet(t, "nope.example.net", "/x", nil); res.status != http.StatusNotFound {
		t.Fatalf("a name with no route answers %d", res.status)
	}
	p.addRecord(t, "secure.example.net")
	put(t, r, aRoute("dockwell.env.secure", "secure.example.net", "", up))
	waitFor(t, c, "secure.example.net", ingress.CertIssued, 60*time.Second)
	noCertificateFor(t, r, "plain.example.net")
	noCertificateFor(t, r, "local.example.net")
}

// A route believes a proxy only if it named that proxy, whatever another route on
// the server names, though what Caddy trusts is per server.
func TestARouteBelievesOnlyTheProxyItNamedOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	up := backend(t, "web")
	mine := plainRoute("dockwell.envA.mine", "mine.example.net", up)
	mine.TrustedProxies, mine.PlainForAnyone = []string{"203.0.113.0/24"}, true // the only proxy this one named
	put(t, c, mine)
	theirs := plainRoute("dockwell.envB.theirs", "theirs.example.net", up)
	theirs.TrustedProxies = []string{"127.0.0.1/32"} // another project's, and the peer here
	put(t, c, theirs)
	forged := map[string]string{"X-Forwarded-For": "6.6.6.6", "X-Forwarded-Proto": "https", "X-Forwarded-Host": "evil.example.net"}
	seen := c.plainGet(t, "mine.example.net", "/who", forged).who(t)
	if seen["real"] != "127.0.0.1" || seen["xff"] != "127.0.0.1" {
		t.Errorf("another project's proxy made this route believe it: %v", seen)
	}
	if seen["proto"] != "http" {
		t.Errorf("X-Forwarded-Proto is %q, from a proxy this route never named", seen["proto"])
	}
	// And the one that named it does.
	seen = c.plainGet(t, "theirs.example.net", "/who", forged).who(t)
	if seen["real"] != "6.6.6.6" || seen["proto"] != "https" {
		t.Errorf("the route that named it read %v", seen)
	}
	// Over TLS as well, where a route's reach isn't limited.
	secure := aRoute("dockwell.envA.secure", "secure.example.net", "", up)
	secure.TrustedProxies = []string{"203.0.113.0/24"}
	put(t, c, secure)
	_ = c.whoAt(t, "secure.example.net", "/who")
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://secure.example.net/who", http.NoBody)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Forwarded-Proto", "http")
	res, err := c.client("secure.example.net").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := who(t, res); got["real"] != "127.0.0.1" || got["proto"] != "https" {
		t.Errorf("over TLS, from a peer it didn't name, the service saw %v", got)
	}
}

// Caddy owns what says how a request came, through a trusted proxy too. A visitor's
// X-Forwarded-Host passes through a proxy as anything else does, and an app builds a
// password reset link from it. X-Forwarded-Proto is https from a proxy this route
// named, and nothing else is taken from it.
func TestAVisitorsForwardedHeadersStopAtCaddyOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.Header.Clone() }))
	t.Cleanup(srv.Close)
	r := plainRoute("dockwell.env.www", "app.example.net", strings.TrimPrefix(srv.URL, "http://"))
	r.TrustedProxies = []string{"127.0.0.1/32"}
	put(t, c, r)
	c.plainGet(t, "app.example.net", "/who", map[string]string{
		"X-Forwarded-For": "6.6.6.6, 198.51.100.7", "X-Forwarded-Proto": "https", // as the edge sends them
		"X-Forwarded-Host": "evil.example.net", "X-Forwarded-Port": "1", "X-Forwarded-Server": "evil", "X-Original-Host": "evil.example.net", // as the visitor wrote them
		"X-Forwarded-Ssl": "on", "X-Forwarded-Scheme": "https", "Front-End-Https": "on", "X-Url-Scheme": "https", "X-Host": "evil.example.net",
	})
	if got := seen.Get("X-Forwarded-Host"); got != "app.example.net" {
		t.Errorf("X-Forwarded-Host is %q, want the route's own domain", got)
	}
	if got := seen.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto from the proxy it named is %q", got)
	}
	for _, h := range claimsOfTheRequest {
		if got := seen.Get(h); got != "" {
			t.Errorf("a visitor's %s reached the service: %q", h, got)
		}
	}
	for _, proto := range []string{"http", "ftp", "https, http", "javascript:alert(1)", ""} {
		c.plainGet(t, "app.example.net", "/who", map[string]string{"X-Forwarded-Proto": proto})
		if got := seen.Get("X-Forwarded-Proto"); got != "http" {
			t.Errorf("X-Forwarded-Proto %q reads %q, want what the request came over", proto, got)
		}
	}
	// Through a route that names none, on the same server, the same.
	other := plainRoute("dockwell.env.other", "other.example.net", strings.TrimPrefix(srv.URL, "http://"))
	put(t, c, other)
	c.plainGet(t, "other.example.net", "/who", map[string]string{"X-Forwarded-Host": "evil.example.net", "X-Forwarded-Ssl": "on", "X-Forwarded-Proto": "https"})
	if got := seen.Get("X-Forwarded-Host"); got != "other.example.net" {
		t.Errorf("beside it, X-Forwarded-Host is %q", got)
	}
	if got := seen.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("beside it, X-Forwarded-Proto is %q", got)
	}
	if got := seen.Get("X-Forwarded-Ssl"); got != "" {
		t.Errorf("beside it, X-Forwarded-Ssl is %q", got)
	}
}

// Cloudflare's ranges are shared by all its customers, and a Worker's cross-zone
// request comes from one, with what its author wrote in X-Forwarded-For and
// 2a06:98c0:3600::103 after it. So from them the address is CF-Connecting-IP, as
// Cloudflare's docs say an origin should read, and X-Forwarded-For is never walked.
// The peer stands in for the edge, as a test can't come from Cloudflare's addresses.
func TestCloudflaresRangesAreReadFromItsOwnHeaderOnRealCaddy(t *testing.T) {
	const workerAddress = "2a06:98c0:3600::103"
	worker := map[string]string{"X-Forwarded-For": "8.8.8.8, " + workerAddress, "Cf-Connecting-Ip": workerAddress, "X-Forwarded-Proto": "https"}
	c := startPlain(t)
	up := backend(t, "web")
	edge := plainRoute("dockwell.env.edge", "edge.example.net", up)
	edge.EdgeProxies = []string{"127.0.0.0/8"}
	put(t, c, edge)
	seen := c.plainGet(t, "edge.example.net", "/who", worker).who(t)
	if seen["real"] != workerAddress || seen["xff"] != workerAddress {
		t.Errorf("a Worker's request read %v, want Cloudflare's own word for it, %s", seen, workerAddress)
	}
	if seen["proto"] != "https" {
		t.Errorf("from the edge, X-Forwarded-Proto is %q", seen["proto"])
	}
	// With no such header, it's the peer, and still not what X-Forwarded-For says.
	if seen := c.plainGet(t, "edge.example.net", "/who", map[string]string{"X-Forwarded-For": "8.8.8.8"}).who(t); seen["real"] != "127.0.0.1" {
		t.Errorf("with no CF-Connecting-IP, the service saw %v", seen)
	}
	// The ranges of the preset aren't what the server trusts, so a route that names
	// something else doesn't walk past them either: that's the same Worker, through a
	// tunnel beside a route that names the preset.
	tunnel := plainRoute("dockwell.env.tunnel", "tunnel.example.net", up)
	tunnel.TrustedProxies = []string{"127.0.0.1/32"}
	put(t, c, tunnel)
	far := plainRoute("dockwell.env.far", "far.example.net", up)
	far.EdgeProxies = []string{"2a06:98c0::/29"}
	put(t, c, far)
	seen = c.plainGet(t, "tunnel.example.net", "/who", worker).who(t)
	if seen["real"] != workerAddress {
		t.Errorf("through a tunnel beside the preset, a Worker's request read %v: its author's %s was walked to", seen, "8.8.8.8")
	}
	cfg := configOf(t, c)
	if strings.Contains(cfg, `"ranges":["127.0.0.1/32","2a06`) || strings.Contains(cfg, `"source":"static","ranges":["127.0.0.0/8"`) {
		t.Errorf("a server trusts a preset's ranges: %s", cfg)
	}
	// And nobody but a peer in the ranges has the header read: not a route's named
	// proxy, not anyone on a route that names the preset elsewhere.
	if seen := c.plainGet(t, "tunnel.example.net", "/who", map[string]string{"Cf-Connecting-Ip": "6.6.6.6"}).who(t); seen["real"] != "127.0.0.1" {
		t.Errorf("a named proxy's CF-Connecting-IP was read: %v", seen)
	}
	away := plainRoute("dockwell.env.away", "away.example.net", up)
	away.EdgeProxies, away.PlainForAnyone = []string{"2a06:98c0::/29"}, true
	put(t, c, away)
	if seen := c.plainGet(t, "away.example.net", "/who", map[string]string{"Cf-Connecting-Ip": "6.6.6.6", "X-Forwarded-Proto": "https"}).who(t); seen["real"] != "127.0.0.1" || seen["proto"] != "http" {
		t.Errorf("from outside the preset's ranges, the service saw %v", seen)
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
}

// What decides who a route answers and what it believes is compared, not only read:
// a hand edit of either would otherwise read as nothing.
func TestHandEditsOfWhatARouteBelievesReadAsThemselvesOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	up := backend(t, "web")
	r := plainRoute("dockwell.env.www", "app.example.net", up)
	r.TrustedProxies = []string{"127.0.0.1/32"}
	put(t, c, r)
	path := "/config/apps/http/servers/" + httpServerName + "/routes/1"
	for name, edit := range map[string]func(route) route{
		"its reach": func(rt route) route {
			rt.Match[0].RemoteIP = &remoteIP{Ranges: []string{"0.0.0.0/0"}}
			return rt
		},
		"what it believes": func(rt route) route {
			rt.Handle[0].Routes[1].Match[0].RemoteIP = &remoteIP{Ranges: []string{"0.0.0.0/0"}}
			return rt
		},
		"an extra step": func(rt route) route {
			rt.Handle[0].Routes = append(rt.Handle[0].Routes, beliefRoute{Handle: []map[string]string{{"handler": "vars", clientVar: "6.6.6.6"}}})
			return rt
		},
		"its headers": func(rt route) route {
			rt.Handle[1].Headers = clientAddress()
			return rt
		},
		"the name it tells a service": func(rt route) route {
			rt.Handle[1].Headers = ownName(believingAddress(), "evil.example.org")
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
			if len(routes) != 1 || !routes[0].Edited {
				t.Errorf("it reads %+v", routes)
			}
			// And it gets no say in what the server trusts.
			if got := configOf(t, c); strings.Count(got, `"trusted_proxies"`) > 2 {
				t.Errorf("config: %s", got)
			}
		})
	}
}

// The panel reads the last of X-Forwarded-For, which is the peer, whatever a route
// on the server trusts. Its address can't be forged through someone else's proxy.
func TestThePanelReadsThePeerBesideARouteThatTrustsItOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", PanelSocket(c.Dir))
	if err != nil {
		t.Fatal(err)
	}
	var seen http.Header
	protos := new(http.Protocols)
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Protocols: protos, Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.Header.Clone() })}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	trusted := aRoute("dockwell.env.tunnel", "tunnel.example.net", "", backend(t, "web"))
	trusted.TrustedProxies = []string{"127.0.0.1/32"}
	put(t, c, trusted)
	if err := c.SetPanel(t.Context(), &ingress.PanelSite{Domain: "panel.example.net"}); err != nil {
		t.Fatal(err)
	}
	var res *http.Response
	for range 100 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://panel.example.net/who", http.NoBody)
		req.Header.Set("X-Forwarded-For", "6.6.6.6")
		req.Header.Set("X-Real-Ip", "6.6.6.7")
		if res, err = c.client("panel.example.net").Do(req); err == nil {
			_ = res.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	list := strings.Split(seen.Get("X-Forwarded-For"), ",")
	if got := strings.TrimSpace(list[len(list)-1]); got != "127.0.0.1" {
		t.Errorf("the panel would read %q, want the peer", got)
	}
	if got := seen.Get("X-Real-Ip"); got != "127.0.0.1" {
		t.Errorf("the panel's X-Real-IP is %q", got)
	}
}

// A route with no name to serve doesn't bind HTTPS on a server that's otherwise
// plain, or beside another proxy on 443 that reload would fail. What closes the
// rest is one, and #93's route that says something of every response is another.
func TestARouteThatNamesNoHostDoesntBindHTTPSOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	put(t, c, plainRoute("dockwell.env.www", "app.example.net", backend(t, "web")))
	noName, err := json.Marshal(map[string]any{
		"@id":    "something-else",
		"handle": []map[string]any{{"handler": "headers", "response": map[string]any{"set": map[string][]string{"Connection": {"close"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.mutate(t.Context(), func(routes []json.RawMessage) ([]json.RawMessage, error) { return append(routes, noName), nil }); err != nil {
		t.Fatal(err)
	}
	if tcp, udp := bound(t, c.tls); tcp || udp {
		t.Errorf("a route with no name made HTTPS bind: TCP %v, UDP %v", tcp, udp)
	}
	if err := c.SetClosed(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if tcp, udp := bound(t, c.tls); tcp || udp {
		t.Errorf("closing routes made HTTPS bind: TCP %v, UDP %v", tcp, udp)
	}
}

// Port 80 alone works beside a real holder of 443, on TCP and UDP, through a restart
// and a reload, and a route that then wants a certificate fails only itself.
func TestAPlainServerRunsBesideAHolderOfHTTPSOnRealCaddy(t *testing.T) {
	bin := needCaddy(t)
	dir := shortDir(t)
	proc := &process{bin: bin, dir: dir}
	c := &Caddy{Dir: dir, Runtime: proc, HTTPPort: freePort(t), HTTPSPort: freePort(t), issuerForTest: map[string]any{"module": "internal"}}
	t.Cleanup(func() { proc.stop(syscall.SIGKILL) })
	tl, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", fmt.Sprintf(":%d", c.HTTPSPort))
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	ul, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp", fmt.Sprintf(":%d", c.HTTPSPort))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	if err := c.Start(t.Context(), nil, true); err != nil {
		out, _ := os.ReadFile(filepath.Join(dir, "caddy.out"))
		t.Fatalf("start beside a holder of %d: %v\n%s", c.HTTPSPort, err, out)
	}
	r := &realCaddy{Caddy: c, proc: proc, http: c.HTTPPort, tls: c.HTTPSPort}
	up := backend(t, "web")
	put(t, r, plainRoute("dockwell.env.www", "app.example.net", up))
	if res := r.plainGet(t, "app.example.net", "/who", nil); res.status != http.StatusOK {
		t.Errorf("it answers %d", res.status)
	}
	proc.stop(syscall.SIGKILL)
	if err := proc.Run(t.Context(), filepath.Join(proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	r.waitAnswering(t)
	if res := r.plainGet(t, "app.example.net", "/who", nil); res.status != http.StatusOK {
		t.Errorf("after a restart it answers %d", res.status)
	}
	before := configOf(t, r)
	if err := c.PutRoute(t.Context(), aRoute("dockwell.env.secure", "secure.example.net", "", up)); err == nil {
		t.Error("a route that wants a certificate went in, with port 443 held")
	}
	if res := r.plainGet(t, "app.example.net", "/who", nil); res.status != http.StatusOK {
		t.Errorf("after that failed, the plain route answers %d", res.status)
	}
	if got := configOf(t, r); got != before {
		t.Errorf("the failed reload changed the config:\n%s\nwas\n%s", got, before)
	}
	put(t, r, plainRoute("dockwell.env.b", "b.example.net", up))
	if err := c.DeleteRoute(t.Context(), "dockwell.env.www"); err != nil {
		t.Fatalf("removing a route beside the holder: %v", err)
	}
}

func configOf(t *testing.T, c *realCaddy) string {
	t.Helper()
	b, _, err := c.call(t.Context(), http.MethodGet, serversPath, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Nothing but the panel's own key has to go: Caddy's own certificates stay, for a
// route that comes back, and this says what that means for the panel's domain.
func TestThePanelsOwnKeyGoesAndCaddysStaysOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	site := &ingress.PanelSite{Domain: "panel.example.net", Cert: selfSignedChain(t, "panel.example.net", "the panel's own CA")}
	if err := c.SetPanel(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", "127.0.0.1:1"))
	_ = c.waitServing(t, "panel.example.net", "/").Body.Close()
	_ = c.waitServing(t, "app.example.net", "/").Body.Close()
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	panelKey := strings.TrimSpace(string(site.Cert.Key))
	escaped, err := json.Marshal(panelKey)
	if err != nil {
		t.Fatal(err)
	}
	var caddysOwn bool
	err = filepath.WalkDir(c.Dir, func(p string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case !d.Type().IsRegular():
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(c.Dir, p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), panelKey) || strings.Contains(string(b), strings.Trim(string(escaped), `"`)) {
			t.Errorf("the panel's own key is in %s", rel)
		}
		if strings.Contains(string(b), "PRIVATE KEY") {
			if !strings.HasPrefix(rel, "data"+string(filepath.Separator)) {
				t.Errorf("a key outside Caddy's data: %s", rel)
			}
			caddysOwn = caddysOwn || strings.HasSuffix(rel, filepath.Join("panel.example.net", "panel.example.net.key"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !caddysOwn {
		t.Error("Caddy's own key for the panel's domain went, and docs/security/ingress.md says it stays until it expires")
	}
}

// namesSeen is an upstream that keeps what it was told of its own name.
type namesSeen struct {
	mu              sync.Mutex
	host, forwarded string
}

func (n *namesSeen) get() (host, forwarded string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.host, n.forwarded
}

func startNames(t *testing.T) (seen *namesSeen, dial string) {
	t.Helper()
	seen = &namesSeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		seen.host, seen.forwarded = r.Host, r.Header.Get("X-Forwarded-Host")
	}))
	t.Cleanup(srv.Close)
	return seen, strings.TrimPrefix(srv.URL, "http://")
}

// Caddy matches a route on the name before the last colon of Host and lets the rest
// through, so a visitor's own name reaches a route inside Host itself. Frameworks
// take the last of a list, and build a reset link from it. So a service reads the
// route's own domain in both headers, on every kind of route, whatever was sent.
func TestAServiceReadsItsRoutesOwnNameOnRealCaddy(t *testing.T) {
	spellings := func(domain string) []string {
		return []string{
			domain, strings.ToUpper(domain), domain + ":80",
			domain + ":1,evil.example.org", domain + ":evil.example.org", domain + ":1;evil.example.org", domain + ":1,evil.example.org,", domain + ":$(id)",
		}
	}
	askPlain := func(c *realCaddy) func(t *testing.T, host string) int {
		return func(t *testing.T, host string) int {
			t.Helper()
			return c.plainGet(t, host, "/", map[string]string{"X-Forwarded-Host": "evil.example.org"}).status
		}
	}
	askTLS := func(c *realCaddy, domain string) func(t *testing.T, host string) int {
		return func(t *testing.T, host string) int {
			t.Helper()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+domain+"/password/reset", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = host
			req.Header.Set("X-Forwarded-Host", "evil.example.org")
			res, err := c.client(domain).Do(req)
			if err != nil {
				t.Fatalf("%s as %q: %v", domain, host, err)
			}
			defer res.Body.Close()
			return res.StatusCode
		}
	}
	// What's written differs with what the server trusts, so each kind goes both ways.
	for _, trusts := range []bool{false, true} {
		t.Run(fmt.Sprintf("a server that trusts a proxy: %v", trusts), func(t *testing.T) {
			c := startPlain(t)
			seen, up := startNames(t)
			anyone := plainRoute("dockwell.env.anyone", "anyone.example.net", up)
			anyone.PlainForAnyone = true
			put(t, c, anyone)
			put(t, c, plainRoute("dockwell.env.none", "none.example.net", up))
			put(t, c, aRoute("dockwell.env.secure", "secure.example.net", "", up))
			_ = c.waitServing(t, "secure.example.net", "/").Body.Close()
			kinds := map[string]func(t *testing.T, host string) int{
				"anyone.example.net": askPlain(c),
				"none.example.net":   askPlain(c),
				"secure.example.net": askTLS(c, "secure.example.net"),
			}
			if trusts {
				named := plainRoute("dockwell.env.named", "named.example.net", up)
				named.TrustedProxies = []string{"127.0.0.1/32"}
				put(t, c, named)
				edge := plainRoute("dockwell.env.edge", "edge.example.net", up)
				edge.EdgeProxies = []string{"127.0.0.1/32"}
				put(t, c, edge)
				kinds["named.example.net"] = askPlain(c)
				kinds["edge.example.net"] = askPlain(c)
			}
			for domain, ask := range kinds {
				for _, host := range spellings(domain) {
					if status := ask(t, host); status != http.StatusOK {
						t.Errorf("%q answered %d", host, status)
						continue
					}
					if gotHost, forwarded := seen.get(); gotHost != domain || forwarded != domain {
						t.Errorf("Host %q reached the service as Host %q and X-Forwarded-Host %q, want %s in both", host, gotHost, forwarded, domain)
					}
				}
			}
		})
	}
}

// A route written before a service read its route's own name reads as Dockwell's, so
// an upgrade isn't drift, and the next change on the server writes the name in, for
// a route that believes a proxy too, whose steps are left as they are.
func TestAnOlderRouteGetsItsOwnNameAtTheNextChangeOnRealCaddy(t *testing.T) {
	for name, tc := range map[string]struct {
		older   func() *headers
		trusts  bool
		handles int
	}{
		"naming no proxy":   {older: clientAddress, handles: 1},
		"older still":       {older: olderClientAddress, handles: 1},
		"beside a proxy":    {older: distrustingAddress, handles: 1},
		"believing a proxy": {older: believingAddress, trusts: true, handles: 2},
		// What Caddy ran before Client-Ip and its like were stripped.
		"before Client-Ip":                 {older: func() *headers { return strippingOnly(clientAddress(), strippedSecond) }, handles: 1},
		"before Client-Ip, beside a proxy": {older: func() *headers { return strippingOnly(distrustingAddress(), strippedSecond) }, handles: 1},
		"before Client-Ip, believing a proxy": {
			older: func() *headers { return strippingOnly(believingAddress(), strippedSecond) }, trusts: true, handles: 2,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := startPlain(t)
			seen, up := startNames(t)
			r := plainRoute("dockwell.env.www", "app.example.net", up)
			if tc.trusts {
				r.TrustedProxies = []string{"127.0.0.1/32"}
			}
			put(t, c, r)
			written := routeJSON(r)
			written.Handle[tc.handles-1].Headers = tc.older()
			b, err := json.Marshal(written)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.call(t.Context(), http.MethodPatch, "/config/apps/http/servers/"+httpServerName+"/routes/1", b, nil); err != nil {
				t.Fatal(err)
			}
			routes, err := c.Routes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(routes) != 1 || routes[0].Edited {
				t.Fatalf("an older route reads %+v", routes)
			}
			put(t, c, plainRoute("dockwell.env.other", "other.example.net", up))
			if status := c.plainGet(t, "app.example.net:1,evil.example.org", "/", nil).status; status != http.StatusOK {
				t.Fatalf("it answered %d", status)
			}
			if host, forwarded := seen.get(); host != "app.example.net" || forwarded != "app.example.net" {
				t.Errorf("after the next change, Host is %q and X-Forwarded-Host %q", host, forwarded)
			}
			if routes, err = c.Routes(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, got := range routes {
				if got.Edited {
					t.Errorf("%s reads as edited by hand", got.ID)
				}
			}
		})
	}
}

// A known limit, which docs/security/ingress.md states, and this holds to what it
// says. Caddy has the walk along X-Forwarded-For per server, so it skips an address
// any route on the server names. Anyone whose outgoing address is in such a range
// writes the address a service reads, and so does a customer of a shared range on a
// route that names it, whose compute comes from that range. If this starts failing,
// the limit may have gone: say so there too.
func TestAnotherRoutesRangeIsWalkedPastOnRealCaddy(t *testing.T) {
	t.Run("named on another route", func(t *testing.T) {
		c := startPlain(t)
		up := backend(t, "web")
		tunnel := plainRoute("dockwell.envA.tunnel", "tunnel.example.net", up)
		tunnel.TrustedProxies = []string{"127.0.0.1/32"}
		put(t, c, tunnel)
		// Without it, the visitor is read for who they are.
		seen := c.plainGet(t, "tunnel.example.net", "/who", map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.9"}).who(t)
		if seen["real"] != "198.51.100.9" {
			t.Fatalf("with no other route naming that range, a visitor reads %v", seen)
		}
		lb := plainRoute("dockwell.envB.lb", "lb.example.net", up)
		lb.TrustedProxies = []string{"198.51.100.0/24"}
		put(t, c, lb)
		seen = c.plainGet(t, "tunnel.example.net", "/who", map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.9"}).who(t)
		if seen["real"] != "6.6.6.6" || seen["xff"] != "6.6.6.6" {
			t.Errorf("the limit has moved: a visitor in the other route's range reads %v, and the threat model says 6.6.6.6", seen)
		}
		// What they get is that address alone.
		if seen["proto"] != "http" {
			t.Errorf("it changed X-Forwarded-Proto: %v", seen)
		}
		// What names no proxy isn't affected, and nor is the preset, which reads no walk.
		put(t, c, plainRoute("dockwell.envC.none", "none.example.net", up))
		if seen := c.plainGet(t, "none.example.net", "/who", map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.9"}).who(t); seen["real"] != "127.0.0.1" {
			t.Errorf("a route naming no proxy reads %v", seen)
		}
		edge := plainRoute("dockwell.envD.edge", "edge.example.net", up)
		edge.EdgeProxies = []string{"127.0.0.1/32"}
		put(t, c, edge)
		if seen := c.plainGet(t, "edge.example.net", "/who", map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.9", "Cf-Connecting-Ip": "203.0.113.7"}).who(t); seen["real"] != "203.0.113.7" {
			t.Errorf("a route naming the preset reads %v", seen)
		}
	})
	t.Run("a shared range on the route itself", func(t *testing.T) {
		c := startPlain(t)
		shared := plainRoute("dockwell.env.shared", "shared.example.net", backend(t, "web"))
		shared.TrustedProxies = []string{"127.0.0.1/32", "203.0.113.0/24"} // a CDN's edge, and its compute
		put(t, c, shared)
		if seen := c.plainGet(t, "shared.example.net", "/who", map[string]string{"X-Forwarded-For": "6.6.6.6, 203.0.113.9"}).who(t); seen["real"] != "6.6.6.6" {
			t.Errorf("the limit has moved: a customer of a range the route names reads %v, and the threat model says 6.6.6.6", seen)
		}
	})
}

// Anyone with a Worker on Cloudflare can write CF-Connecting-IP, so it reaches a
// service only if it has the shape of an address. Otherwise the client is the peer.
func TestWhatIsntAnAddressStopsAtCaddyOnRealCaddy(t *testing.T) {
	c := startPlain(t)
	edge := plainRoute("dockwell.env.edge", "edge.example.net", backend(t, "web"))
	edge.EdgeProxies = []string{"127.0.0.1/32"}
	put(t, c, edge)
	for _, value := range []string{"203.0.113.9", "240.0.0.1", "2001:db8::1", "2606:4700:4700::1111", "1:2:3:4:5:6:7:8", "::1", "::", "fe80::", "2001:db8::"} {
		seen := c.plainGet(t, "edge.example.net", "/who", map[string]string{"Cf-Connecting-Ip": value}).who(t)
		if seen["real"] != value || seen["xff"] != value {
			t.Errorf("an address, %q, read %v", value, seen)
		}
	}
	for _, value := range []string{
		"6.6.6.6, 7.7.7.7", "6.6.6.6,7.7.7.7", "", " ", "<script>alert(1)</script>", "' OR 1=1 --", "{http.request.host}", "localhost", "1.2.3", "1.2.3.4.5",
		":::", "::::", "abcde::1", "2001:db8::1%eth0", "6.6.6.6 7.7.7.7", "::ffff:6.6.6.6, 7.7.7.7",
		"{http.request.remote.host}", "{http.request.header.Host}", "6.6.6.6{http.vars.none}",
	} {
		seen := c.plainGet(t, "edge.example.net", "/who", map[string]string{"Cf-Connecting-Ip": value}).who(t)
		if seen["real"] != "127.0.0.1" || seen["xff"] != "127.0.0.1" {
			t.Errorf("%q isn't an address, and a service read %v", value, seen)
		}
	}
	// Sent twice, it's a list, whatever each one looks like.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/who", c.http), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "edge.example.net"
	req.Header["Cf-Connecting-Ip"] = []string{"6.6.6.6", "7.7.7.7"}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if seen := (plainAnswer{status: res.StatusCode, body: body}).who(t); seen["real"] != "127.0.0.1" {
		t.Errorf("the header sent twice read %v", seen)
	}
}

// A real address that didn't have the shape would quietly read as Cloudflare's own,
// so every address netip can write has it. An IPv4 address mapped to IPv6 is the one
// exception, as Cloudflare sends those as IPv4.
func TestEveryAddressHasTheShape(t *testing.T) {
	shape := regexp.MustCompile(addressShape)
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200000 {
		var b [16]byte
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		// A run of groups of zeros, anywhere, of any length, is what compresses.
		for i := 2 * rng.IntN(9); i < 2*rng.IntN(9); i++ {
			b[i] = 0
		}
		for _, a := range []netip.Addr{netip.AddrFrom16(b), netip.AddrFrom4([4]byte(b[:4]))} {
			if !a.Is4In6() && !shape.MatchString(a.String()) {
				t.Fatalf("%s is an address, and doesn't have the shape", a)
			}
		}
	}
	for _, a := range []string{"::", "::1", "1::", "abcd::", "1:2:3:4:5:6:7:8", "1:2:3:4:5:6:7::", "::2:3:4:5:6:7:8"} {
		if !shape.MatchString(a) {
			t.Errorf("%s is an address, and doesn't have the shape", a)
		}
	}
}

// A route is written with its own name, not left for the next change to put right
// (withTrust), which skips what reads as edited by hand.
func TestARouteIsWrittenWithItsOwnName(t *testing.T) {
	plain := ingress.Route{ID: "dockwell.env.www", Domain: "app.example.net", Upstreams: []string{"127.0.0.1:9"}}
	named := plain
	named.TrustedProxies = []string{"127.0.0.1/32"}
	for _, r := range []ingress.Route{plain, named} {
		handle := routeJSON(r).Handle
		set := handle[len(handle)-1].Headers.Request.Set
		if !slices.Equal(set["Host"], []string{r.Domain}) || !slices.Equal(set["X-Forwarded-Host"], []string{r.Domain}) {
			t.Errorf("%+v is written with %v", r, set)
		}
	}
}
