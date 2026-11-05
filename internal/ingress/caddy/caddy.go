// SPDX-License-Identifier: AGPL-3.0-only

// Package caddy runs a server's routes on Caddy, as an ingress.Proxy: a
// container on the server's own network, pinned by digest, managed through its
// admin API on a unix socket with JSON config. See
// docs/adr/0024-the-caddy-ingress-driver.md.
package caddy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/ingress"
)

// Name is the proxy's name, for people.
const Name = "Caddy"

// serverName is the HTTP server in Caddy's config that holds the routes that
// take a certificate, and httpServerName the one on the plain HTTP port alone,
// which holds those that don't and keeps the panel's rules. Caddy applies no
// automatic HTTPS to a server on that port alone, so what is on it asks for no
// certificate and redirects nowhere, and what isn't can't be reached over it.
const (
	serverName     = "dockwell"
	httpServerName = "dockwell_http"
	panelRouteID   = "dockwell.panel"
	closedRouteID  = "dockwell.closed"
	// panelPlainPrefix starts the IDs of the panel's rules on plain HTTP.
	panelPlainPrefix = panelRouteID + ".plain."
	// derivedPrefix starts the IDs of what the plain HTTP server gets from the
	// rest of the config, so it is made anew with every change.
	derivedPrefix = "dockwell.plain."
	plainClosedID = derivedPrefix + "closed"
	plainNoneID   = derivedPrefix + "none"
	// secureNoneID is the same for the HTTPS server, and is made anew with it.
	secureNoneID = derivedPrefix + "secure-none"
	routesPath   = "/config/apps/http/servers/" + serverName + "/routes"
	// serversPath is every server, whose ETag covers them all, as one reload can
	// move a route from one to the other.
	serversPath = "/config/apps/http/servers"
	// startWait bounds waiting for Caddy to answer after it starts.
	startWait = 30 * time.Second
	// streamClose is how long a WebSocket, or anything else that upgraded,
	// lives on after a reload closes its config. Caddy's default closes
	// upgraded connections as the config reloads.
	streamClose = "24h"
	// stopGrace is how long what is in flight gets as Caddy stops, under the
	// 30 s Docker waits before it kills it (stopWait), so it ends cleanly. A
	// reload doesn't cut requests at this point, as they go on in the old
	// config until they end, which TestReloadsDropNothingOnRealCaddy holds.
	stopGrace = "25s"
	// tryDuration holds a request while its upstream refuses connections, as
	// a container's replacement starts, before it answers 502.
	tryDuration = "3s"
	tryInterval = "250ms"
	// adminWait bounds one call of the admin API. A Caddy that hangs, as under
	// docker pause, accepts a connection and never answers, and the agent holds
	// up every plan on this server while it waits. A reload of the most routes a
	// server may have takes a few seconds at worst, each naming the preset, as
	// measured on a busy machine (ADR 0030).
	adminWait = 15 * time.Second
)

// Paths are where the proxy finds its directories, on its side of the mounts.
// Boot is read-only to it, and the rest are its own.
type Paths struct {
	Data, Config, Run, Boot string
}

// adminListen is the admin API's address, on its socket. Caddy's default is TCP
// on the server's loopback, where any process on the server could load a config
// that serves every key it holds.
func adminListen(p Paths) string {
	return "unix/" + filepath.Join(p.Run, adminSocketName) + "|0600"
}

// AdminEnv tells Caddy the admin API's address in its environment, as well as in
// its config. A config that names no address, which an empty last config is,
// would otherwise put the admin API on Caddy's default.
func AdminEnv(p Paths) string { return "CADDY_ADMIN=" + adminListen(p) }

// Runtime is what runs the proxy process. Docker is the one the product uses;
// tests run the binary.
type Runtime interface {
	// Paths are the directories as the proxy sees them.
	Paths() Paths
	// Status says whether it exists and runs.
	Status(ctx context.Context) (ingress.Status, error)
	// Pull gets what it needs, if the server hasn't it.
	Pull(ctx context.Context) error
	// Run starts it, replacing any of an older version, and returns once it
	// is started. It reads config as its bootstrap.
	Run(ctx context.Context, config string) error
	// Abandon stops a proxy that failed to start, keeping what it left for
	// someone to read why.
	Abandon(ctx context.Context) error
	// Remove stops it gracefully and removes it.
	Remove(ctx context.Context) error
}

// Caddy is an ingress.Proxy on Caddy.
type Caddy struct {
	// Dir is the ingress's directory on the server. Its data, config and run
	// directories are the proxy's.
	Dir string
	// Runtime runs the process.
	Runtime Runtime
	// ACMEDirectory is the ACME CA's directory, and ACMERoots a PEM file of
	// roots to trust for it, on the server. Empty means Let's Encrypt.
	ACMEDirectory, ACMERoots string
	// HTTPPort and HTTPSPort are the public ports. Zero means 80 and 443.
	HTTPPort, HTTPSPort int

	// AdminWait bounds one call of the admin API. Zero means adminWait.
	AdminWait time.Duration

	// issuerForTest replaces the issuer in tests that need a certificate and
	// no certificate authority.
	issuerForTest map[string]any
	// procNetTCP is where the server's connections are read. Empty means
	// /proc/net/tcp.
	procNetTCP string

	once  sync.Once
	admin *http.Client

	logMu sync.Mutex
	log   logReader

	// good is the config last written to goodFile, so a config that hasn't
	// changed isn't written again each minute.
	goodMu sync.Mutex
	good   []byte
}

var _ ingress.Proxy = (*Caddy)(nil)

// Name is "Caddy".
func (*Caddy) Name() string { return Name }

func (c *Caddy) dataDir() string   { return filepath.Join(c.Dir, "data") }
func (c *Caddy) configDir() string { return filepath.Join(c.Dir, "config") }
func (c *Caddy) runDir() string    { return filepath.Join(c.Dir, "run") }
func (c *Caddy) bootDir() string   { return filepath.Join(c.Dir, bootDirName) }

// PanelSocket is where the panel listens for what Caddy sends it, on the
// server's side, under dir.
func PanelSocket(dir string) string { return filepath.Join(dir, "run", panelSocketName) }

const (
	adminSocketName = "admin.sock"
	panelSocketName = "panel.sock"
	// bootDirName is what Caddy reads as it starts, which is root's: its
	// container mounts it read-only (ADR 0024).
	bootDirName = "boot"
)

func (c *Caddy) client() *http.Client {
	c.once.Do(func() {
		socket := filepath.Join(c.runDir(), adminSocketName)
		c.admin = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
			MaxIdleConns: 1, IdleConnTimeout: 5 * time.Second,
		}}
	})
	return c.admin
}

// errDown is Caddy's admin API not answering, as it isn't running.
var errDown = errors.New("caddy isn't answering")

// call makes a request of Caddy's admin API, and returns its body and ETag.
func (c *Caddy) call(ctx context.Context, method, path string, body []byte, header http.Header) ([]byte, string, error) {
	wait := c.AdminWait
	if wait == 0 {
		wait = adminWait
	}
	bounded, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	req, err := http.NewRequestWithContext(bounded, method, "http://dockwell"+path, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", fmt.Errorf("%w: %w", errDown, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxConfigFile))
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", fmt.Errorf("%w: %w", errDown, err)
	}
	if res.StatusCode >= 300 {
		return nil, "", &adminError{Status: res.StatusCode, Message: errorOf(b)}
	}
	return b, res.Header.Get("Etag"), nil
}

// adminError is Caddy refusing a request, with its own reason.
type adminError struct {
	Status  int
	Message string
}

func (e *adminError) Error() string { return e.Message }

func errorOf(b []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(b))
}

// matcher, upstream and handler are the parts of a route's JSON that Dockwell
// writes. Each is built from typed values, never from strings put together,
// so what a route's spec holds can't become config.
type matcher struct {
	Host []string `json:"host,omitempty"`
	Path []string `json:"path,omitempty"`
	// Protocol is "http" on a route that takes no certificate, which is both what
	// marks it and a second lock: it can't answer over TLS.
	Protocol string `json:"protocol,omitempty"`
	// RemoteIP is who a route that takes no certificate answers, by the peer on the
	// connection, whatever it says of itself.
	RemoteIP *remoteIP `json:"remote_ip,omitempty"`
	// Header holds what a request must carry, in what a route believes of it.
	Header map[string][]string `json:"header,omitempty"`
	// VarsRegexp holds what a placeholder must look like, in the same. It looks at
	// what the placeholder gives, as header_regexp passes if any one line of a header
	// matches, and the placeholder joins them all.
	VarsRegexp map[string]pattern `json:"vars_regexp,omitempty"`
}

type remoteIP struct {
	Ranges []string `json:"ranges"`
}

type upstream struct {
	Dial string `json:"dial"`
}

type loadBalancing struct {
	TryDuration string `json:"try_duration,omitempty"`
	TryInterval string `json:"try_interval,omitempty"`
}

type transport struct {
	Protocol  string     `json:"protocol"`
	Versions  []string   `json:"versions,omitempty"`
	KeepAlive *keepAlive `json:"keep_alive,omitempty"`
}

// keepAlive is how long Caddy keeps a connection to an upstream it isn't using.
type keepAlive struct {
	IdleConnTimeout string `json:"idle_timeout,omitempty"`
}

// upstreamIdle is how long Caddy keeps a connection to an upstream with no request
// on it. A drain counts connections, so that is how long one takes to read as clear
// once nothing is on its way there. It costs an app a new connection after that
// long with no requests, over the server's own bridge.
const upstreamIdle = "2s"

// upstreamTransport is how a route reaches its upstream.
func upstreamTransport() *transport {
	return &transport{Protocol: "http", KeepAlive: &keepAlive{IdleConnTimeout: upstreamIdle}}
}

type handler struct {
	Handler          string         `json:"handler"`
	Upstreams        []upstream     `json:"upstreams,omitempty"`
	FlushInterval    int            `json:"flush_interval,omitempty"`
	StreamCloseDelay string         `json:"stream_close_delay,omitempty"`
	LoadBalancing    *loadBalancing `json:"load_balancing,omitempty"`
	Transport        *transport     `json:"transport,omitempty"`
	Headers          *headers       `json:"headers,omitempty"`
	// Routes are a subroute's: what a route believes of its request.
	Routes []beliefRoute `json:"routes,omitempty"`
}

// beliefRoute is one step of what a route believes. It sets request variables, and
// its route's reverse_proxy reads those, so which peer is believed is the route's
// own, though what Caddy trusts is per server.
type beliefRoute struct {
	Match  []matcher           `json:"match,omitempty"`
	Handle []map[string]string `json:"handle"`
}

// headers changes what a handler sends upstream.
type headers struct {
	Request *headerOps `json:"request,omitempty"`
}

type headerOps struct {
	Set    map[string][]string `json:"set,omitempty"`
	Delete []string            `json:"delete,omitempty"`
}

// forgeableHeaders say where a request came from, and only Caddy knows, so none
// of what a client sends in them reaches a service. Caddy already overwrites
// X-Forwarded-For, with no trusted_proxies. An app moved from another proxy
// often trusts one of these for per-address limits and allowlists, so a forged
// one is a way around them.
var forgeableHeaders = []string{
	"Forwarded", "Forwarded-For", "X-Client-Ip", "X-Cluster-Client-Ip", "X-Originating-Ip", "X-Remote-Ip", "X-Remote-Addr",
	"True-Client-Ip", "Cf-Connecting-Ip", "Cf-Connecting-Ipv6", "Fastly-Client-Ip", "Ali-Cdn-Real-Ip", "Proxy-Client-Ip", "Wl-Proxy-Client-Ip",
	"X-Envoy-External-Address", "X-Original-Forwarded-For",
}

// claimsOfTheRequest say how a request reached the server, or by what name, and
// frameworks read them to build links and decide what's secure. Nothing says only
// a proxy wrote them, so a route writes X-Forwarded-Host and X-Forwarded-Proto
// itself, and these don't reach a service at all.
var claimsOfTheRequest = []string{
	"X-Forwarded-Port", "X-Forwarded-Server", "X-Forwarded-Ssl", "X-Forwarded-Scheme", "X-Url-Scheme", "Front-End-Https", "X-Original-Host", "X-Host",
}

// Caddy's placeholders for where a request came from and how: the peer on the
// connection, the client Caddy found through a proxy it trusts, which is the peer
// unless that proxy said otherwise, what Cloudflare says connected to it, and what
// a route's own steps decided of these.
const (
	peerAddress    = "{http.request.remote.host}"
	resolvedClient = "{http.vars.client_ip}"
	edgeClient     = "{http.request.header.Cf-Connecting-Ip}"
	realScheme     = "{http.request.scheme}"
	requestHost    = "{http.request.hostport}"
	clientVar      = "dockwell_client"
	schemeVar      = "dockwell_scheme"
)

// clientAddress is what a route sends upstream of where a request came from, on a
// server that trusts no proxy: X-Real-IP is the client's real address, so an app
// moved from OpenResty keeps working, and what else a client might claim is gone.
// Caddy overwrites X-Forwarded-For and its like itself.
func clientAddress() *headers {
	return &headers{Request: &headerOps{
		Set:    map[string][]string{"X-Real-Ip": {peerAddress}},
		Delete: slices.Concat(forgeableHeaders, claimsOfTheRequest),
	}}
}

// olderClientAddress is clientAddress as a config from before claimsOfTheRequest
// has it, which reads as Dockwell's own, so an upgrade isn't drift.
func olderClientAddress() *headers {
	return &headers{Request: &headerOps{
		Set:    map[string][]string{"X-Real-Ip": {peerAddress}},
		Delete: forgeableHeaders,
	}}
}

// distrustingAddress is clientAddress on a server where another route trusts a
// proxy. Caddy decides what it trusts per server, so it would keep that proxy's
// forwarded headers for this route too, and this writes them over.
func distrustingAddress() *headers {
	return &headers{Request: &headerOps{
		Set: map[string][]string{
			"X-Real-Ip": {peerAddress}, "X-Forwarded-For": {peerAddress},
			"X-Forwarded-Proto": {realScheme}, "X-Forwarded-Host": {requestHost},
		},
		Delete: slices.Concat(forgeableHeaders, claimsOfTheRequest),
	}}
}

// believingAddress is what a route that names a proxy sends, from what its own
// steps decided (beliefOf). A service reads one address, not a list a client may
// have written the start of. A proxy passes on what a visitor writes in
// X-Forwarded-Host, and an app builds links from it, so ownName writes over that.
func believingAddress() *headers {
	client, scheme := "{http.vars."+clientVar+"}", "{http.vars."+schemeVar+"}"
	return &headers{Request: &headerOps{
		Set: map[string][]string{
			"X-Real-Ip": {client}, "X-Forwarded-For": {client},
			"X-Forwarded-Proto": {scheme}, "X-Forwarded-Host": {requestHost},
		},
		Delete: slices.Concat(forgeableHeaders, claimsOfTheRequest),
	}}
}

// ownName has a route tell a service its own domain, in Host and X-Forwarded-Host.
// Caddy matches a route on the name before the last colon of Host and lets the rest
// through, so Host: app.example.net:1,evil.example.org reaches the route, and
// frameworks take the last of a list, and build a reset link from it. A port a proxy
// in front keeps in Host goes with that.
func ownName(h *headers, domain string) *headers {
	h.Request.Set["Host"] = []string{domain}
	h.Request.Set["X-Forwarded-Host"] = []string{domain}
	return h
}

// addressShape is what CF-Connecting-IP must look like to reach a service as its
// client's address. Anyone with a Worker on Cloudflare can write that header, so a
// list, nothing at all, or text an app might print goes no further. It's the shape
// of an address, not a check of one: Caddy compiles this for every route that names
// the preset, and an exact pattern costs more than three times the memory there. So
// 999.1.1.1 gets through, and an app that parses it refuses it. Every address that
// netip writes matches, as a real one that didn't would read as Cloudflare's own.
const addressShape = `^(?:[0-9]{1,3}(?:\.[0-9]{1,3}){3}|[0-9A-Fa-f]{0,4}(?::{1,2}[0-9A-Fa-f]{1,4}){1,7}:{0,2}|[0-9A-Fa-f]{0,4}::)$`

// pattern is what Caddy's vars_regexp matcher takes for a placeholder.
type pattern struct {
	Pattern string `json:"pattern"`
}

// beliefOf is what a route that names a proxy believes of a request: the peer on
// the connection and the scheme it came over, unless the peer is one it named. Only
// that route's own list counts, though Caddy finds {http.vars.client_ip} by what any
// route on the server names.
//
//   - From a range it named, the client is the first address from the right of
//     X-Forwarded-For that no route on the server names.
//   - From Cloudflare's, the client is CF-Connecting-IP if it has the shape of an
//     address, and otherwise the peer. X-Forwarded-For isn't read: those ranges are
//     shared by all its customers, and a Worker's request comes from one with
//     whatever X-Forwarded-For its author wrote (ADR 0030).
//   - From either, https is taken at its word, and nothing else is: a visitor can't
//     put anything else in front of an app through it.
func beliefOf(named, edge []string) handler {
	steps := []beliefRoute{{Handle: []map[string]string{{"handler": "vars", clientVar: peerAddress, schemeVar: realScheme}}}}
	if len(named) > 0 {
		steps = append(steps, beliefRoute{
			Match:  []matcher{{RemoteIP: &remoteIP{Ranges: named}}},
			Handle: []map[string]string{{"handler": "vars", clientVar: resolvedClient}},
		})
	}
	if len(edge) > 0 {
		steps = append(steps, beliefRoute{
			Match:  []matcher{{RemoteIP: &remoteIP{Ranges: edge}, VarsRegexp: map[string]pattern{edgeClient: {Pattern: addressShape}}}},
			Handle: []map[string]string{{"handler": "vars", clientVar: edgeClient}},
		})
	}
	steps = append(steps, beliefRoute{
		Match:  []matcher{{RemoteIP: &remoteIP{Ranges: slices.Concat(named, edge)}, Header: map[string][]string{"X-Forwarded-Proto": {"https"}}}},
		Handle: []map[string]string{{"handler": "vars", schemeVar: "https"}},
	})
	return handler{Handler: "subroute", Routes: steps}
}

// readBelief finds what ranges a route's steps believe, or nothing for a route that
// has none. routeJSON writes it back to see they're what it would write.
func readBelief(h handler) (named, edge []string) {
	for _, step := range h.Routes {
		if len(step.Match) != 1 || step.Match[0].RemoteIP == nil || len(step.Handle) != 1 {
			continue
		}
		switch step.Handle[0][clientVar] {
		case resolvedClient:
			named = step.Match[0].RemoteIP.Ranges
		case edgeClient:
			edge = step.Match[0].RemoteIP.Ranges
		}
	}
	return named, edge
}

// thisServer is who a route with no certificate answers if it names no proxy, as a
// tunnel on the server's own network. A blanket rule for private addresses would let
// in a whole network, as servers often sit on one, so a bridge's range is named in
// trusted_proxies instead.
var thisServer = []string{"127.0.0.0/8", "::1/128"}

// reachOf is who a route with no certificate answers, by the peer on the connection,
// or nil for anyone. Anyone else falls through to what a name with no route gets, so
// it can't tell the route is there.
func reachOf(r ingress.Route) *remoteIP {
	switch {
	case r.PlainForAnyone:
		return nil
	case len(r.TrustedProxies)+len(r.EdgeProxies) > 0:
		return &remoteIP{Ranges: slices.Concat(r.TrustedProxies, r.EdgeProxies)}
	}
	return &remoteIP{Ranges: thisServer}
}

type route struct {
	ID       string    `json:"@id,omitempty"`
	Match    []matcher `json:"match,omitempty"`
	Handle   []handler `json:"handle,omitempty"`
	Terminal bool      `json:"terminal,omitempty"`
}

// routeJSON is a route as Dockwell writes it. A path covers itself and what is
// under it, and nothing that only starts the same: /api isn't /apis.
func routeJSON(r ingress.Route) route {
	m := matcher{Host: []string{r.Domain}}
	if r.Path != "" {
		m.Path = []string{r.Path, r.Path + "/*"}
	}
	if r.Plain {
		m.Protocol = plainProtocol
		m.RemoteIP = reachOf(r)
	}
	ups := make([]upstream, len(r.Upstreams))
	for i, u := range r.Upstreams {
		ups[i] = upstream{Dial: u}
	}
	h := handler{
		Handler: "reverse_proxy", Upstreams: ups, FlushInterval: -1, StreamCloseDelay: streamClose,
		LoadBalancing: &loadBalancing{TryDuration: tryDuration, TryInterval: tryInterval},
		Transport:     upstreamTransport(),
		Headers:       ownName(clientAddress(), r.Domain),
	}
	handle := []handler{h}
	if len(r.TrustedProxies)+len(r.EdgeProxies) > 0 {
		h.Headers = ownName(believingAddress(), r.Domain)
		handle = []handler{beliefOf(r.TrustedProxies, r.EdgeProxies), h}
	}
	return route{ID: r.ID, Match: []matcher{m}, Terminal: true, Handle: handle}
}

// plainProtocol is what Caddy's protocol matcher takes for a request not over TLS.
const plainProtocol = "http"

// panelRoute sends the panel's domain to its socket, over cleartext HTTP/2 so
// that gRPC reaches it whole. The socket is only as trusted as who can reach
// it, which is only Caddy and root (ADR 0024).
func panelRoute(p *ingress.PanelSite, socket string) route {
	return route{ID: panelRouteID, Match: []matcher{{Host: []string{p.Domain}}}, Terminal: true, Handle: []handler{{
		Handler: "reverse_proxy", Upstreams: []upstream{{Dial: "unix/" + socket}}, FlushInterval: -1, StreamCloseDelay: streamClose,
		Transport: &transport{Protocol: "http", Versions: []string{"h2c", "1.1"}},
		Headers:   clientAddress(),
	}}}
}

// plainHTTPRules keep the panel's rules on plain HTTP: a request for its
// domain with an Authorization header, or any method but GET and HEAD, is
// refused rather than redirected, as a client follows a redirect with its
// token still on (ADR 0016).
func plainHTTPRules(p *ingress.PanelSite) []map[string]any {
	refuse := func(id, msg string, match map[string]any) map[string]any {
		match["host"] = []string{p.Domain}
		return map[string]any{
			"@id": id, "terminal": true, "match": []map[string]any{match},
			"handle": []map[string]any{{
				"handler": "static_response", "status_code": 400, "body": msg,
				"headers": map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}},
			}},
		}
	}
	return []map[string]any{
		refuse("dockwell.panel.plain.token", fmt.Sprintf("Dockwell answers only over HTTPS, and this request sent an Authorization header over plain HTTP, "+
			"so anyone on the network may have seen it. If it carried an API token, revoke the token under Profile and security "+
			"and create a new one. Then send requests to https://%s instead.\n", p.Domain),
			map[string]any{"header": map[string][]string{"Authorization": {"*"}}}),
		refuse("dockwell.panel.plain.method", fmt.Sprintf("Dockwell answers only over HTTPS. Send this request to https://%s instead.\n", p.Domain),
			map[string]any{"not": []map[string]any{{"method": []string{"GET", "HEAD"}}}}),
	}
}

// bootstrap is the config Caddy starts with when it has no last config of its
// own. It names the admin socket, as AdminEnv does: Caddy's default is TCP on
// the server's loopback. plainOnly listens on the plain HTTP port alone, for
// routes that take no certificate, which a server with the panel's site has none
// of the use of: that needs HTTPS.
func (c *Caddy) bootstrap(panel *ingress.PanelSite, plainOnly bool) ([]byte, error) {
	paths := c.Runtime.Paths()
	httpPort, httpsPort := c.ports()
	routes := []route{}
	servers := map[string]any{}
	listen := []string{":" + strconv.Itoa(httpsPort)}
	switch {
	case panel != nil:
		routes = append(routes, panelRoute(panel, filepath.Join(paths.Run, panelSocketName)))
		servers[httpServerName] = map[string]any{"listen": []string{":" + strconv.Itoa(httpPort)}, "routes": plainHTTPRules(panel)}
	case plainOnly:
		listen = []string{}
		servers[httpServerName] = map[string]any{"listen": []string{":" + strconv.Itoa(httpPort)}, "routes": []route{}}
	}
	servers[serverName] = map[string]any{
		"listen": listen, "strict_sni_host": true, "routes": routes,
		// A certificate loaded for the panel's domain would otherwise stop Caddy
		// getting its own, and nothing renews one that was loaded.
		"automatic_https": map[string]any{"ignore_loaded_certificates": true},
		// Early data can be replayed by anyone who captured it, and its source
		// address isn't proven, so a request in it is neither safe to act on
		// nor safe to attribute. A client simply waits one round trip more.
		"allow_0rtt": false,
	}
	config := map[string]any{
		"admin": map[string]any{
			"listen": adminListen(paths),
			"config": map[string]any{"persist": true},
		},
		"logging": map[string]any{"logs": map[string]any{
			// What certificates are doing, for the agent, as Caddy has no API
			// that says.
			"dockwell_tls": map[string]any{
				"writer":  map[string]any{"output": "file", "filename": filepath.Join(paths.Data, tlsLogName), "roll_size_mb": 1, "roll_keep": 1},
				"level":   "INFO",
				"include": []string{"tls.obtain", "tls.renew"},
			},
		}},
		"apps": map[string]any{
			"http": map[string]any{
				"http_port": httpPort, "https_port": httpsPort, "grace_period": stopGrace, "servers": servers,
			},
		},
	}
	if issuer := c.issuer(paths); issuer != nil {
		config["apps"].(map[string]any)["tls"] = map[string]any{"automation": map[string]any{"policies": []any{map[string]any{"issuers": []any{issuer}}}}}
	}
	return json.MarshalIndent(config, "", "  ")
}

func (c *Caddy) ports() (httpPort, httpsPort int) {
	httpPort, httpsPort = c.HTTPPort, c.HTTPSPort
	if httpPort == 0 {
		httpPort = 80
	}
	if httpsPort == 0 {
		httpsPort = 443
	}
	return httpPort, httpsPort
}

// acmeRootsName is where the CA's roots are, in the boot directory.
const acmeRootsName = "acme-roots.pem"

// issuer is the certificates' issuer, when it isn't Caddy's default of Let's
// Encrypt, as for Pebble.
func (c *Caddy) issuer(paths Paths) map[string]any {
	if c.issuerForTest != nil {
		return c.issuerForTest
	}
	httpPort, httpsPort := c.ports()
	if c.ACMEDirectory == "" && httpPort == 80 && httpsPort == 443 {
		return nil
	}
	issuer := map[string]any{"module": "acme"}
	if c.ACMEDirectory != "" {
		issuer["ca"] = c.ACMEDirectory
	}
	if c.ACMERoots != "" {
		issuer["trusted_roots_pem_files"] = []string{filepath.Join(paths.Boot, acmeRootsName)}
	}
	if httpPort != 80 || httpsPort != 443 {
		issuer["challenges"] = map[string]any{
			"http":     map[string]any{"alternate_port": httpPort},
			"tls-alpn": map[string]any{"alternate_port": httpsPort},
		}
	}
	return issuer
}

// Status says whether Caddy exists and runs.
func (c *Caddy) Status(ctx context.Context) (ingress.Status, error) { return c.Runtime.Status(ctx) }

// Prepare writes what Caddy reads as it starts, and gets its image.
func (c *Caddy) Prepare(ctx context.Context, panel *ingress.PanelSite) error {
	if err := c.prepare(panel, false); err != nil {
		return err
	}
	return c.Runtime.Pull(ctx)
}

// Start runs Caddy, and returns once its admin API answers.
func (c *Caddy) Start(ctx context.Context, panel *ingress.PanelSite, plainOnly bool) error {
	if err := c.prepare(panel, plainOnly); err != nil {
		return err
	}
	// A damaged last config would start it with nothing, or not at all.
	if _, err := c.repairFile(); err != nil {
		return err
	}
	if err := c.Runtime.Run(ctx, filepath.Join(c.Runtime.Paths().Boot, bootstrapName)); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, startWait)
	defer cancel()
	for {
		_, _, err := c.call(wait, http.MethodGet, routesPath, nil, nil)
		if err == nil {
			c.keepRunning(wait)
			return nil
		}
		if !errors.Is(err, errDown) {
			if wait.Err() == nil {
				return fmt.Errorf("caddy started, but its admin API refused the agent: %w", err)
			}
		}
		st, serr := c.Runtime.Status(wait)
		if serr == nil && !st.Running {
			return c.failedToStart(ctx, "it exited")
		}
		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return c.failedToStart(ctx, "its admin API didn't answer within "+startWait.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c *Caddy) failedToStart(ctx context.Context, what string) error {
	_ = c.Runtime.Abandon(context.WithoutCancel(ctx))
	return fmt.Errorf("caddy didn't start: %s. See why with docker logs %s; another process on port 80 or 443 is the usual reason", what, ingress.ContainerName)
}

// bootstrapName is the config Caddy starts with when it has no last one.
const bootstrapName = "bootstrap.json"

// prepare makes Caddy's directories, and writes what it reads as it starts.
// That goes in the boot directory, which is root's and read-only to Caddy, so
// nothing Caddy does can change where a write of root's lands, or what it reads
// when it next starts.
func (c *Caddy) prepare(panel *ingress.PanelSite, plainOnly bool) error {
	if err := EnsureDirs(c.Dir); err != nil {
		return err
	}
	b, err := c.bootstrap(panel, plainOnly)
	if err != nil {
		return err
	}
	if err := writeIn(c.bootDir(), bootstrapName, b, 0o644, agentOwns); err != nil {
		return fmt.Errorf("can't write Caddy's bootstrap config: %w", err)
	}
	if c.ACMERoots != "" {
		roots, err := os.ReadFile(c.ACMERoots)
		if err != nil {
			return fmt.Errorf("can't read the ACME CA's roots from %s: %w", c.ACMERoots, err)
		}
		if err := writeIn(c.bootDir(), acmeRootsName, roots, 0o644, agentOwns); err != nil {
			return fmt.Errorf("can't write the ACME CA's roots: %w", err)
		}
	}
	return nil
}

// ProxyUID is the user Caddy runs as when the agent is root, and the agent's
// own otherwise, as in CI. Root on the host's network can reach abstract
// sockets on the server, so Caddy isn't root even in its container.
const ProxyUID = 65532

// proxyUser is the user Caddy runs as, and whether its files need giving to
// it.
func proxyUser() (uid, gid int, chown bool) {
	if os.Geteuid() == 0 {
		return ProxyUID, ProxyUID, true
	}
	return os.Geteuid(), os.Getegid(), false
}

// EnsureDirs makes the ingress's directories, owner-only, as Caddy's user. A
// panel makes them too, for its socket.
func EnsureDirs(dir string) error {
	uid, gid, chown := proxyUser()
	for _, d := range []string{dir, filepath.Join(dir, "data"), filepath.Join(dir, "config"), filepath.Join(dir, "run")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("can't create %s: %w", d, err)
		}
		if err := os.Chmod(d, 0o700); err != nil { //nolint:gosec // a path under the ingress's own directory
			return fmt.Errorf("can't keep %s owner-only: %w", d, err)
		}
		if chown && d != dir {
			if err := os.Chown(d, uid, gid); err != nil {
				return fmt.Errorf("can't give %s to Caddy's user: %w", d, err)
			}
		}
	}
	// What Caddy reads as it starts stays root's, and needs its directory open
	// to read, as Caddy isn't root. Its container mounts it read-only, and the
	// ingress's own directory keeps everyone else out on the server.
	boot := filepath.Join(dir, bootDirName)
	if err := os.MkdirAll(boot, 0o755); err != nil { //nolint:gosec // read by Caddy's user; nothing in it is secret
		return fmt.Errorf("can't create %s: %w", boot, err)
	}
	if err := os.Chmod(boot, 0o755); err != nil { //nolint:gosec // as above
		return fmt.Errorf("can't open %s to Caddy's user: %w", boot, err)
	}
	return nil
}

// OwnSocket gives the panel's socket to Caddy's user, and nobody else. Its
// directory is Caddy's, so what stands at the name by now may not be the socket:
// the owner goes on whatever is there, never through a link, and the mode stays
// in that directory.
func OwnSocket(path string) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("can't reach the panel's socket: %w", err)
	}
	defer root.Close()
	name := filepath.Base(path)
	uid, gid, chown := proxyUser()
	if chown {
		if err := root.Lchown(name, uid, gid); err != nil {
			return fmt.Errorf("can't give the panel's socket to Caddy's user: %w", err)
		}
	}
	if err := root.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("can't keep the panel's socket owner-only: %w", err)
	}
	return nil
}

// autosaveFile is Caddy's last config, for messages.
func (c *Caddy) autosaveFile() string { return filepath.Join(c.configDir(), autosaveRel) }

// Stop stops Caddy and frees its ports, keeping its certificates. Its last config
// goes, not left to resume, and the agent's copy too, and all they held of the
// panel's key: a server's next first route starts from the bootstrap, with what
// this build sets.
func (c *Caddy) Stop(ctx context.Context) error {
	if err := c.Runtime.Remove(ctx); err != nil {
		return err
	}
	if err := c.forgetLastConfig(); err != nil {
		return err
	}
	return c.forgetGood()
}

// readServers reads Caddy's servers with their ETag, or from its last config when
// it is down, whereupon the ETag is empty. up says it answered.
func (c *Caddy) readServers(ctx context.Context) (servers map[string]json.RawMessage, etag string, up bool, err error) {
	b, etag, err := c.call(ctx, http.MethodGet, serversPath, nil, nil)
	if err == nil {
		servers, err := decodeServers(b)
		return servers, etag, err == nil, err
	}
	if !errors.Is(err, errDown) {
		// A Caddy with no config at all says the path to the servers is invalid.
		var ae *adminError
		if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || strings.Contains(ae.Message, "invalid traversal path")) {
			return nil, etag, true, nil
		}
		return nil, "", false, err
	}
	saved, missing, err := c.lastConfig()
	if err != nil {
		return nil, "", false, fmt.Errorf("can't read Caddy's last config: %w", err)
	}
	if missing {
		return nil, "", false, nil
	}
	if damaged(saved) {
		return nil, "", false, fmt.Errorf("caddy isn't answering, and its last config in %s is damaged, as a full disk or a power cut can leave it. The agent puts back its own copy within a minute", c.autosaveFile())
	}
	var config struct {
		Apps struct {
			HTTP struct {
				Servers map[string]json.RawMessage `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(saved, &config); err != nil {
		return nil, "", false, fmt.Errorf("can't read Caddy's last config in %s: %w", c.autosaveFile(), err)
	}
	return config.Apps.HTTP.Servers, "", false, nil
}

func decodeServers(b []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(b)) == 0 || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil, nil
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(b, &servers); err != nil {
		return nil, fmt.Errorf("can't read Caddy's servers: %w", err)
	}
	return servers, nil
}

// routesIn are the routes of both Dockwell's servers, as one list, and what is
// left of the plain HTTP server's: routes that aren't Dockwell's, which stay
// where they are. What it derives from the rest isn't in either.
func routesIn(servers map[string]json.RawMessage) (all, others []json.RawMessage, err error) {
	read := func(name string) ([]json.RawMessage, error) {
		var server struct {
			Routes json.RawMessage `json:"routes"`
		}
		if len(servers[name]) == 0 {
			return nil, nil
		}
		if err := json.Unmarshal(servers[name], &server); err != nil {
			return nil, fmt.Errorf("can't read Caddy's server %s: %w", name, err)
		}
		return decodeRoutes(server.Routes)
	}
	if all, err = read(serverName); err != nil {
		return nil, nil, err
	}
	all = slices.DeleteFunc(all, func(raw json.RawMessage) bool {
		id := routeID(raw)
		return isGeneration(id) || id == secureNoneID
	})
	plain, err := read(httpServerName)
	if err != nil {
		return nil, nil, err
	}
	for _, raw := range plain {
		id := routeID(raw)
		switch {
		case strings.HasPrefix(id, derivedPrefix), isGeneration(id):
		case strings.HasPrefix(id, "dockwell."):
			all = append(all, raw)
		default:
			others = append(others, raw)
		}
	}
	return all, others, nil
}

// rawRoutes reads the routes of both Dockwell's servers, or from Caddy's last
// config when it is down. up says it answered.
func (c *Caddy) rawRoutes(ctx context.Context) (routes []json.RawMessage, up bool, err error) {
	servers, _, up, err := c.readServers(ctx)
	if err != nil {
		return nil, false, err
	}
	routes, _, err = routesIn(servers)
	if err != nil {
		return nil, false, err
	}
	return routes, up, nil
}

func decodeRoutes(b []byte) ([]json.RawMessage, error) {
	if len(bytes.TrimSpace(b)) == 0 || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil, nil
	}
	var routes []json.RawMessage
	if err := json.Unmarshal(b, &routes); err != nil {
		return nil, fmt.Errorf("can't read Caddy's routes: %w", err)
	}
	return routes, nil
}

// Routes returns the routes Dockwell gave Caddy, as it has them now, or as it
// last had them if it is down.
func (c *Caddy) Routes(ctx context.Context) ([]ingress.Route, error) {
	raw, _, err := c.rawRoutes(ctx)
	if err != nil {
		return nil, err
	}
	var out []ingress.Route
	for _, r := range raw {
		if read, ok := readRoute(r); ok {
			out = append(out, c.dialed(read))
		}
	}
	return out, nil
}

// upstreamsDir holds, in Caddy's run directory, what each route dials.
const upstreamsDir = "upstreams"

// maxUpstreamFile bounds what is read of one: an address and a port.
const maxUpstreamFile = 1 << 10

// throughFile makes a route that sends to one upstream dial through a file, which
// Caddy reads for each request. So moving it is writing the file: nothing reloads,
// and no connection to any site on the server is touched by it, whichever protocol
// it speaks. Caddy needs to be there for none of this, and starts with what the file
// says (ADR 0027). A route with several upstreams dials them as they are.
func (c *Caddy) throughFile(r ingress.Route) (ingress.Route, error) {
	if len(r.Upstreams) != 1 {
		return r, nil
	}
	name := filepath.Join(upstreamsDir, r.ID)
	if err := writeIn(c.runDir(), name, []byte(r.Upstreams[0]), 0o644, proxyOwns()); err != nil {
		return r, fmt.Errorf("can't write what the route %s dials: %w. Check that %s is writable", r.ID, err, c.runDir())
	}
	r.Upstreams = []string{dialFile(c.Runtime.Paths().Run, r.ID)}
	return r, nil
}

// dialFile is what a route that dials through a file says to dial.
func dialFile(run, id string) string {
	return "{file." + filepath.Join(run, upstreamsDir, id) + "}"
}

// dialed reads what a route dials through its file. That is Caddy's directory, so
// what isn't an address reads as a hand edit, as drift, and a revert writes it again.
func (c *Caddy) dialed(r ingress.Route) ingress.Route {
	for i, up := range r.Upstreams {
		if !strings.HasPrefix(up, "{file.") {
			continue
		}
		if up != dialFile(c.Runtime.Paths().Run, r.ID) {
			r.Edited = true
			continue
		}
		b, err := readIn(c.runDir(), filepath.Join(upstreamsDir, r.ID), maxUpstreamFile)
		address := strings.TrimSpace(string(b))
		if _, _, splitErr := net.SplitHostPort(address); err != nil || splitErr != nil {
			r.Edited, r.Upstreams = true, slices.Delete(slices.Clone(r.Upstreams), i, i+1)
			return r
		}
		r.Upstreams[i] = address
	}
	return r
}

// alreadySo reports routes that Caddy has as they are, as they would come out of
// its last config if it is down, so what changes only what a file says reloads
// nothing and needs nothing of Caddy.
func (c *Caddy) alreadySo(ctx context.Context, bodies map[string]json.RawMessage) bool {
	raw, _, err := c.rawRoutes(ctx)
	if err != nil {
		return false
	}
	// A server that trusts a proxy changes what its other routes do with a header
	// as it writes them (withTrust), so that isn't a difference to reload for.
	trusts := len(trustedRanges(raw)) > 0
	found := 0
	for _, r := range raw {
		if body, ok := bodies[routeID(r)]; ok {
			if !sameJSON(r, withTrust([]json.RawMessage{body}, trusts)[0]) {
				return false
			}
			found++
		}
	}
	return found == len(bodies)
}

// routeID reads a raw route's ID.
func routeID(raw json.RawMessage) string {
	var r struct {
		ID string `json:"@id"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.ID
}

// known keys of a route, a matcher and a handler, as Dockwell writes them. A
// key beyond these, or a second handler or matcher, reads as a hand edit. An
// older build's keys stay on the list, so an upgrade isn't drift.
var (
	knownRouteKeys   = []string{"@id", "match", "handle", "terminal"}
	knownMatchKeys   = []string{"host", "path", "protocol", "remote_ip"}
	knownHandlerKeys = []string{"handler", "upstreams", "flush_interval", "stream_close_delay", "load_balancing", "headers", "transport"}
)

// readRoute reads one of Dockwell's routes; ok is false for another's, which
// Dockwell leaves alone.
func readRoute(raw json.RawMessage) (ingress.Route, bool) {
	id := routeID(raw)
	if !strings.HasPrefix(id, "dockwell.") || strings.HasPrefix(id, panelRouteID) || strings.HasPrefix(id, derivedPrefix) || id == closedRouteID || isGeneration(id) {
		return ingress.Route{}, false
	}
	out := ingress.Route{ID: id}
	var r route
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Match) != 1 || len(r.Match[0].Host) != 1 || len(r.Handle) == 0 || len(r.Handle) > 2 || r.Handle[len(r.Handle)-1].Handler != "reverse_proxy" {
		out.Edited = true
		return out, true
	}
	proxy := r.Handle[len(r.Handle)-1]
	out.Domain = r.Match[0].Host[0]
	switch p := r.Match[0].Path; {
	case len(p) == 0:
	case len(p) == 2 && p[1] == p[0]+"/*":
		out.Path = p[0]
	default:
		out.Edited = true
	}
	switch r.Match[0].Protocol {
	case "":
	case plainProtocol:
		out.Plain = true
	default:
		out.Edited = true
	}
	for _, u := range proxy.Upstreams {
		out.Upstreams = append(out.Upstreams, u.Dial)
	}
	if len(r.Handle) == 2 {
		out.TrustedProxies, out.EdgeProxies = readBelief(r.Handle[0])
	}
	out.PlainForAnyone = out.Plain && r.Match[0].RemoteIP == nil
	out.Edited = out.Edited || !onlyKnownKeys(raw) || !headersAreOurs(raw, len(r.Handle) == 2, out.Domain) || !beliefAndReachAreOurs(r, out) || !transportIsOurs(raw)
	return out, true
}

// beliefAndReachAreOurs reports a route whose steps and whose reach are what
// Dockwell would write for what it read of them, as these decide who it answers and
// what a service believes, and a hand edit of them would otherwise read as nothing.
func beliefAndReachAreOurs(r route, read ingress.Route) bool {
	want := routeJSON(read)
	wantMatch, err1 := json.Marshal(want.Match[0].RemoteIP)
	haveMatch, err2 := json.Marshal(r.Match[0].RemoteIP)
	if err1 != nil || err2 != nil || !sameJSON(wantMatch, haveMatch) {
		return false
	}
	if len(r.Handle) != len(want.Handle) {
		return false
	}
	if len(r.Handle) == 1 {
		return true
	}
	wantSteps, err1 := json.Marshal(want.Handle[0])
	haveSteps, err2 := json.Marshal(r.Handle[0])
	return err1 == nil && err2 == nil && sameJSON(wantSteps, haveSteps)
}

// headersAreOurs reports a route that changes the headers it sends as Dockwell
// has it do, or, as an older config may, not at all. Its key alone isn't enough:
// a header added by hand is a hand edit, and would otherwise read as nothing. A
// route that trusts a proxy believes it, and one that doesn't never does. A shape
// from before ownName reads as ours, and the next change writes over it (withTrust).
func headersAreOurs(raw json.RawMessage, trusts bool, domain string) bool {
	var r struct {
		Handle []struct {
			Headers json.RawMessage `json:"headers"`
		} `json:"handle"`
	}
	if json.Unmarshal(raw, &r) != nil || len(r.Handle) == 0 {
		return false
	}
	have := r.Handle[len(r.Handle)-1].Headers
	if len(have) == 0 {
		return !trusts
	}
	shapes := []*headers{ownName(clientAddress(), domain), ownName(distrustingAddress(), domain), clientAddress(), olderClientAddress(), distrustingAddress()}
	if trusts {
		shapes = []*headers{ownName(believingAddress(), domain), believingAddress()}
	}
	return slices.ContainsFunc(shapes, func(h *headers) bool {
		ours, err := json.Marshal(h)
		return err == nil && sameJSON(have, ours)
	})
}

// transportIsOurs reports a route that reaches its upstream as Dockwell has it do,
// or, as an older config may, as Caddy does by default.
func transportIsOurs(raw json.RawMessage) bool {
	var r struct {
		Handle []struct {
			Transport json.RawMessage `json:"transport"`
		} `json:"handle"`
	}
	if json.Unmarshal(raw, &r) != nil || len(r.Handle) == 0 {
		return false
	}
	// A route that believes a proxy has a step ahead of the one that sends it on.
	transport := r.Handle[len(r.Handle)-1].Transport
	if len(transport) == 0 {
		return true
	}
	ours, err := json.Marshal(upstreamTransport())
	return err == nil && sameJSON(transport, ours)
}

func onlyKnownKeys(raw json.RawMessage) bool {
	var r struct {
		Match  []map[string]json.RawMessage `json:"match"`
		Handle []map[string]json.RawMessage `json:"handle"`
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || json.Unmarshal(raw, &r) != nil {
		return false
	}
	in := func(m map[string]json.RawMessage, keys []string) bool {
		for k := range m {
			if !slices.Contains(keys, k) {
				return false
			}
		}
		return true
	}
	return in(top, knownRouteKeys) && len(r.Match) == 1 && in(r.Match[0], knownMatchKeys) && len(r.Handle) > 0 && in(r.Handle[len(r.Handle)-1], knownHandlerKeys)
}

// mutate changes the routes as one reload, against the version it read, so a
// change made between the two isn't lost; it tries again if there was one.
// Routes that aren't Dockwell's stay, so a hand-added one isn't lost either.
func (c *Caddy) mutate(ctx context.Context, edit func([]json.RawMessage) ([]json.RawMessage, error)) error {
	for try := 0; ; try++ {
		servers, etag, up, err := c.readServers(ctx)
		if err != nil {
			return err
		}
		if !up {
			return fmt.Errorf("%w, so it can't change its routes. Start it with docker start %s; the agent starts it within a minute", errDown, ingress.ContainerName)
		}
		generation, err := c.newGeneration()
		if err != nil {
			return err
		}
		if err := c.editServers(servers, edit, generation); err != nil {
			c.forgetGeneration(generation)
			return err
		}
		body, err := json.Marshal(servers)
		if err != nil {
			c.forgetGeneration(generation)
			return err
		}
		header := http.Header{}
		if etag != "" {
			header.Set("If-Match", etag)
		}
		_, _, err = c.call(ctx, http.MethodPatch, serversPath, body, header)
		var ae *adminError
		if errors.As(err, &ae) && ae.Status == http.StatusPreconditionFailed && try < 3 {
			c.forgetGeneration(generation)
			continue
		}
		if err != nil {
			c.forgetGeneration(generation)
			return err
		}
		c.keepOnlyGeneration(generation)
		c.keepRunning(ctx)
		return nil
	}
}

// generationRouteID names the route that tells a config it is out of date, and
// plainGenerationRouteID the same route on the plain HTTP server, as Caddy refuses
// two of one ID in a config, across its servers.
const (
	generationRouteID      = "dockwell-generation"
	plainGenerationRouteID = generationRouteID + "-http"
)

// isGeneration reports one of those.
func isGeneration(id string) bool { return id == generationRouteID || id == plainGenerationRouteID }

// generationPrefix starts the name of the file each config looks for, in the
// proxy's run directory.
const generationPrefix = "generation-"

// newGeneration gives a config a file of its own to look for, and a first route
// that says Connection: close on every response once the file is gone.
//
// A reload makes the old config's connections leave: Go's HTTP/2 server sends
// each a GOAWAY as it shuts down. But it only tells those it has registered, and
// it registers one after its TLS handshake, so one mid-handshake as the reload
// comes is never told. It goes on with the old config for as long as its client
// keeps it busy, and Caddy's old config, with its context cancelled, answers 502
// at once on an upstream that has gone, with no retry. Measured under load, that
// is one connection in about 3 rollouts at 30 new connections a second. So the
// old config can't be told by Caddy, and tells itself: the file goes as the
// reload ends, and its next response sends the connection to the new config,
// while the old upstream is still there to answer it (ADR 0027).
func (c *Caddy) newGeneration() (string, error) {
	name := generationPrefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	dir := c.generationDir()
	if err := touchNew(filepath.Join(dir, name)); err != nil {
		return "", fmt.Errorf("can't write %s in %s, which Caddy reads to tell its old config from its new: %w. Check that directory is writable", name, dir, err)
	}
	return name, nil
}

// touchNew makes an empty file that wasn't there. Nothing in Caddy's directories
// is to be trusted, so this neither follows a link planted at the name, which
// would make the agent write where Caddy can't, nor takes a file already there.
func touchNew(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // it's empty, in a directory only Caddy's user can enter, and that user has to find it
	if err != nil {
		return err
	}
	return f.Close()
}

func generationRoute(id, run, name string) map[string]any {
	return map[string]any{
		"@id": id,
		"match": []map[string]any{{"not": []map[string]any{{
			"file": map[string]any{"root": run, "try_files": []string{"/" + name}},
		}}}},
		"handle": []map[string]any{{"handler": "headers", "response": map[string]any{"set": map[string][]string{"Connection": {"close"}}}}},
	}
}

// generationDir is the run directory on the server's side of the mounts.
func (c *Caddy) generationDir() string { return filepath.Join(c.Dir, "run") }

func (c *Caddy) forgetGeneration(name string) {
	_ = os.Remove(filepath.Join(c.generationDir(), name))
}

// keepOnlyGeneration removes every generation file but the one the config now
// loaded looks for: what was left behind goes too, as a crash can leave one, and
// the configs before it read as out of date from here on.
func (c *Caddy) keepOnlyGeneration(name string) {
	entries, err := os.ReadDir(c.generationDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), generationPrefix) && e.Name() != name {
			_ = os.Remove(filepath.Join(c.generationDir(), e.Name()))
		}
	}
}

// errNoServer is a config that lacks the server Dockwell's routes go on.
var errNoServer = errors.New("caddy isn't running Dockwell's config, so its routes can't change. The agent puts back its own copy of a config left empty within a minute. If this goes on, see what it runs with docker logs " + ingress.ContainerName)

// editServers applies an edit to the routes of Dockwell's servers, and puts each
// route on the one it belongs on: what takes no certificate on the plain HTTP
// server, and the rest on the other. What follows from the routes is set here too,
// so it can't fall out of step with them: whether HTTPS listens at all, what the
// plain HTTP server derives, and which proxies the servers trust. Everything else
// in them is left as it is.
func (c *Caddy) editServers(servers map[string]json.RawMessage, edit func([]json.RawMessage) ([]json.RawMessage, error), generation string) error {
	if len(servers[serverName]) == 0 {
		return errNoServer
	}
	all, others, err := routesIn(servers)
	if err != nil {
		return err
	}
	next, err := edit(all)
	if err != nil {
		return err
	}
	var secure []json.RawMessage
	plain := others
	for _, raw := range next {
		if onPlainServer(raw) {
			plain = append(plain, raw)
		} else {
			secure = append(secure, raw)
		}
	}
	trusted := trustedRanges(next)
	secure = withTrust(secure, len(trusted) > 0)
	if secure, err = withSecureNone(secure); err != nil {
		return err
	}
	plain = withTrust(plain, len(trusted) > 0)
	var tlsServer map[string]json.RawMessage
	if err := unmarshalIf(servers[serverName], &tlsServer); err != nil {
		return err
	}
	if err := c.setListen(tlsServer, secure); err != nil {
		return err
	}
	// The generation route goes on both, as a config on either can outlive a reload.
	// It names no host, so it doesn't make HTTPS listen.
	route, err := json.Marshal(generationRoute(generationRouteID, c.Runtime.Paths().Run, generation))
	if err != nil {
		return err
	}
	secure = append(secure, route)
	if err := setServerRoutes(tlsServer, secure, trusted); err != nil {
		return err
	}
	if servers[serverName], err = json.Marshal(tlsServer); err != nil {
		return err
	}
	if plain, err = withDerived(plain, secure); err != nil {
		return err
	}
	if len(plain) == 0 {
		delete(servers, httpServerName)
		return nil
	}
	if route, err = json.Marshal(generationRoute(plainGenerationRouteID, c.Runtime.Paths().Run, generation)); err != nil {
		return err
	}
	plain = append(plain, route)
	plainServer := map[string]json.RawMessage{}
	if err := unmarshalIf(servers[httpServerName], &plainServer); err != nil {
		return err
	}
	if len(plainServer["listen"]) == 0 {
		httpPort, _ := c.ports()
		if plainServer["listen"], err = json.Marshal([]string{":" + strconv.Itoa(httpPort)}); err != nil {
			return err
		}
	}
	if err := setServerRoutes(plainServer, plain, trusted); err != nil {
		return err
	}
	servers[httpServerName], err = json.Marshal(plainServer)
	return err
}

// onPlainServer reports a route that goes on the plain HTTP server: the panel's
// rules, and Dockwell's routes that take no certificate.
func onPlainServer(raw json.RawMessage) bool {
	id := routeID(raw)
	if strings.HasPrefix(id, panelPlainPrefix) {
		return true
	}
	read, ok := readRoute(raw)
	return ok && read.Plain
}

// trustedRanges are the ranges any of Dockwell's routes trusts, sorted, for its
// servers. A route edited by hand gets no say in what they trust.
func trustedRanges(routes []json.RawMessage) []string {
	var out []string
	for _, raw := range routes {
		if read, ok := readRoute(raw); ok && !read.Edited {
			out = append(out, read.TrustedProxies...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// withTrust has each of Dockwell's routes that trusts no proxy write over what
// Caddy keeps for a proxy, if its server trusts any. Caddy decides that per server,
// so what another route names would otherwise reach this one.
func withTrust(routes []json.RawMessage, serverTrusts bool) []json.RawMessage {
	for i, raw := range routes {
		read, ok := readRoute(raw)
		if !ok || read.Edited {
			continue
		}
		var r route
		if json.Unmarshal(raw, &r) != nil || len(r.Handle) == 0 {
			continue
		}
		// One with steps of its own decides for itself what it believes, whatever the
		// server trusts, and only needs its own name written.
		want := ownName(clientAddress(), read.Domain)
		switch {
		case len(r.Handle) == 2:
			want = ownName(believingAddress(), read.Domain)
		case serverTrusts:
			want = ownName(distrustingAddress(), read.Domain)
		}
		last := &r.Handle[len(r.Handle)-1]
		have, err1 := json.Marshal(last.Headers)
		ours, err2 := json.Marshal(want)
		if err1 != nil || err2 != nil || sameJSON(have, ours) {
			continue
		}
		last.Headers = want
		if b, err := json.Marshal(r); err == nil {
			routes[i] = b
		}
	}
	return routes
}

// setListen has the HTTPS server listen if it serves any name, and bind nothing if
// not, as a server behind a tunnel needs no port 443. A route with no host names
// nothing to serve, as with what closes the rest, or says something of every
// response. Caddy applies no automatic HTTPS to a server with no port but the plain
// HTTP one, so with none it asks for no certificate and makes no ACME account. A
// listen address changed by hand stays, while it listens at all.
func (c *Caddy) setListen(server map[string]json.RawMessage, routes []json.RawMessage) error {
	_, httpsPort := c.ports()
	serves := slices.ContainsFunc(routes, namesAHost)
	var listen []string
	if err := unmarshalIf(server["listen"], &listen); err != nil {
		return err
	}
	switch {
	case !serves:
		listen = []string{}
	case len(listen) == 0:
		listen = []string{":" + strconv.Itoa(httpsPort)}
	}
	var err error
	server["listen"], err = json.Marshal(listen)
	return err
}

// namesAHost reports a route that matches a name, under any of its matchers.
func namesAHost(raw json.RawMessage) bool {
	var r struct {
		Match []struct {
			Host []string `json:"host"`
		} `json:"match"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return false
	}
	return slices.ContainsFunc(r.Match, func(m struct {
		Host []string `json:"host"`
	},
	) bool {
		return len(m.Host) > 0
	})
}

// setServerRoutes puts routes on a server, in the order Caddy should try them, with
// the ranges it trusts. Strict reads X-Forwarded-For from the right: Caddy's default
// takes its leftmost address, which is whatever the client sent, through any proxy
// that appends to the header, as Cloudflare does.
func setServerRoutes(server map[string]json.RawMessage, routes []json.RawMessage, trusted []string) error {
	if routes == nil {
		routes = []json.RawMessage{} // "null" isn't a list of routes
	}
	sortRoutes(routes)
	var err error
	if server["routes"], err = json.Marshal(routes); err != nil {
		return err
	}
	if len(trusted) == 0 {
		delete(server, "trusted_proxies")
		delete(server, "trusted_proxies_strict")
		return nil
	}
	if server["trusted_proxies"], err = json.Marshal(map[string]any{"source": "static", "ranges": trusted}); err != nil {
		return err
	}
	server["trusted_proxies_strict"], err = json.Marshal(1)
	return err
}

// withDerived adds what the plain HTTP server's routes need, given those on the
// HTTPS server. What closes routes there says 503 for their domains alone, as the
// panel's domain and those with certificates still redirect. And a name with no
// route gets a 404, as Caddy would otherwise answer an empty 200, which reads as a
// site that works. It goes last, so what Caddy adds for a domain's redirect comes
// ahead of it.
func withDerived(plain, secure []json.RawMessage) ([]json.RawMessage, error) {
	// A route's own reach goes with what closes it: anyone it doesn't answer gets what
	// a name with no route gets, closed or not, so it can't tell the route is there.
	var reached []matcher
	for _, raw := range plain {
		read, ok := readRoute(raw)
		if !ok || !read.Plain {
			continue
		}
		m := matcher{Host: []string{read.Domain}, RemoteIP: reachOf(read)}
		if !slices.ContainsFunc(reached, func(o matcher) bool { return slices.Equal(o.Host, m.Host) && reflect.DeepEqual(o.RemoteIP, m.RemoteIP) }) {
			reached = append(reached, m)
		}
	}
	if len(reached) == 0 {
		return plain, nil
	}
	if slices.ContainsFunc(secure, func(raw json.RawMessage) bool { return routeID(raw) == closedRouteID }) {
		closed, err := closedRouteFor(reached)
		if err != nil {
			return nil, err
		}
		plain = append(plain, closed)
	}
	none, err := json.Marshal(map[string]any{
		"@id": plainNoneID, "terminal": true,
		"handle": noSite,
	})
	if err != nil {
		return nil, err
	}
	return append(plain, none), nil
}

// noSite answers for what no route covers, as Caddy would otherwise answer an empty
// 200, which reads as a site that works.
var noSite = []map[string]any{{
	"handler": "static_response", "status_code": http.StatusNotFound,
	"body":    "There's no site at this address.\n",
	"headers": map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}, "Cache-Control": {"no-store"}},
}}

// withSecureNone has a domain with a certificate answer a 404 for a path none of its
// routes covers, as with a route to /api alone. It's for those domains alone, so it
// says nothing of the panel's, or of what isn't Dockwell's.
func withSecureNone(secure []json.RawMessage) ([]json.RawMessage, error) {
	var domains []string
	for _, raw := range secure {
		if read, ok := readRoute(raw); ok && !read.Edited && !slices.Contains(domains, read.Domain) {
			domains = append(domains, read.Domain)
		}
	}
	if len(domains) == 0 {
		return secure, nil
	}
	slices.Sort(domains)
	none, err := json.Marshal(map[string]any{
		"@id": secureNoneID, "terminal": true,
		"match":  []matcher{{Host: domains}},
		"handle": noSite,
	})
	if err != nil {
		return nil, err
	}
	return append(secure, none), nil
}

// sortRoutes puts what is most specific first, as Caddy takes the first route
// that matches: the panel, routes that aren't Dockwell's, what closes the rest,
// then each domain's longest paths first, and last what answers for a name with no
// route. It is stable, so a config reads the same each time.
func sortRoutes(routes []json.RawMessage) {
	type key struct {
		rank         int
		domain, path string
	}
	keyOf := func(raw json.RawMessage) key {
		id := routeID(raw)
		switch {
		case isGeneration(id):
			return key{rank: -1}
		case strings.HasPrefix(id, panelRouteID):
			return key{rank: 0}
		case !strings.HasPrefix(id, "dockwell."):
			return key{rank: 1}
		case id == closedRouteID, id == plainClosedID:
			return key{rank: 2}
		case id == plainNoneID, id == secureNoneID:
			return key{rank: 4}
		}
		r, _ := readRoute(raw)
		return key{rank: 3, domain: r.Domain, path: r.Path}
	}
	type keyed struct {
		raw json.RawMessage
		key key
	}
	// Reading a route is the most of this, so each is read once.
	list := make([]keyed, len(routes))
	for i, raw := range routes {
		list[i] = keyed{raw, keyOf(raw)}
	}
	slices.SortStableFunc(list, func(a, b keyed) int {
		ka, kb := a.key, b.key
		switch {
		case ka.rank != kb.rank:
			return ka.rank - kb.rank
		case ka.rank != 3:
			return 0
		case ka.domain != kb.domain:
			return strings.Compare(ka.domain, kb.domain)
		case len(ka.path) != len(kb.path):
			return len(kb.path) - len(ka.path)
		}
		return strings.Compare(ka.path, kb.path)
	})
	for i := range list {
		routes[i] = list[i].raw
	}
}

// projectOf is the environment in a route's ID, ingress.RouteID.
func projectOf(id string) string {
	parts := strings.SplitN(id, ".", 3)
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

// replacedBy reports a route of Dockwell's that can't stand beside r, and is left
// from before: the engine lets one route hold a domain and path, and one
// environment hold a domain, across an install. So another route for r's domain
// and path, or for its domain from another environment, is what another control
// plane or an older database left. It would otherwise stay ahead of r and go on
// serving, while r's plan applied and verified.
func replacedBy(raw json.RawMessage, r ingress.Route) bool {
	old, ok := readRoute(raw)
	if !ok || old.ID == r.ID || old.Domain != r.Domain {
		return false
	}
	return old.Path == r.Path || projectOf(old.ID) != projectOf(r.ID)
}

// PutRoute adds a route, or changes the one with its ID, in one reload, and
// takes out any left from before that it can't stand beside.
func (c *Caddy) PutRoute(ctx context.Context, r ingress.Route) error {
	r, err := c.throughFile(r)
	if err != nil {
		return err
	}
	body, err := json.Marshal(routeJSON(r))
	if err != nil {
		return err
	}
	if c.alreadySo(ctx, map[string]json.RawMessage{r.ID: body}) {
		return nil
	}
	return c.mutate(ctx, func(routes []json.RawMessage) ([]json.RawMessage, error) {
		routes = slices.DeleteFunc(routes, func(raw json.RawMessage) bool { return replacedBy(raw, r) })
		for i := range routes {
			if routeID(routes[i]) == r.ID {
				routes[i] = body
				return routes, nil
			}
		}
		return append(routes, body), nil
	})
}

// PutRoutes changes several routes as one change. A rollout's changes only what
// each dials, which reloads nothing. Anything else is one reload, as reloads close
// together are what the next catches clients coming back from (newGeneration).
func (c *Caddy) PutRoutes(ctx context.Context, list []ingress.Route) error {
	bodies := make(map[string]json.RawMessage, len(list))
	order := make([]string, 0, len(list))
	for i := range list {
		r, err := c.throughFile(list[i])
		if err != nil {
			return err
		}
		body, err := json.Marshal(routeJSON(r))
		if err != nil {
			return err
		}
		bodies[r.ID] = body
		order = append(order, r.ID)
	}
	if c.alreadySo(ctx, bodies) {
		return nil
	}
	return c.mutate(ctx, func(routes []json.RawMessage) ([]json.RawMessage, error) {
		done := map[string]bool{}
		for i := range routes {
			if body, ok := bodies[routeID(routes[i])]; ok {
				routes[i] = body
				done[routeID(routes[i])] = true
			}
		}
		for _, id := range order {
			if !done[id] {
				routes = append(routes, bodies[id])
			}
		}
		return routes, nil
	})
}

// DeleteRoute removes a route, if Caddy has it. With Caddy down it comes out of
// the last config, which is what Caddy serves when it starts again, so a route
// can always be removed: starting Caddy for it may be just what another proxy on
// the ports stops. A Caddy that runs and doesn't answer, as under docker pause,
// would write its own config back over that, so that says what to do instead.
func (c *Caddy) DeleteRoute(ctx context.Context, id string) error {
	err := c.deleteRoute(ctx, id)
	if err == nil {
		// Nothing dials it now, and a route of its name writes it anew.
		_ = os.Remove(filepath.Join(c.runDir(), upstreamsDir, id))
	}
	return err
}

func (c *Caddy) deleteRoute(ctx context.Context, id string) error {
	err := c.mutate(ctx, func(routes []json.RawMessage) ([]json.RawMessage, error) {
		return slices.DeleteFunc(routes, func(raw json.RawMessage) bool { return routeID(raw) == id }), nil
	})
	if !errors.Is(err, errDown) {
		return err
	}
	st, serr := c.Runtime.Status(ctx)
	if serr != nil {
		return serr
	}
	if st.Running {
		return fmt.Errorf("caddy is running and isn't answering, so it can't remove the route: it would write its config back when it wakes. If it is paused, run docker unpause %s; otherwise run docker restart %s, and remove the route again", ingress.ContainerName, ingress.ContainerName)
	}
	return c.editLastConfig(func(routes []json.RawMessage) []json.RawMessage {
		return slices.DeleteFunc(routes, func(raw json.RawMessage) bool { return routeID(raw) == id })
	})
}

// closedRoute answers every request that reaches it with 503. It has no
// matcher, and comes ahead of Dockwell's routes, so what they dial is never
// dialled. Certificates come from the names in those routes, which stay. The
// routes on the plain HTTP server get theirs from this (closedRouteFor).
func closedRoute() ([]byte, error) { return json.Marshal(closedBody(closedRouteID, nil)) }

// closedRouteFor is closedRoute for what these match alone.
func closedRouteFor(match []matcher) ([]byte, error) {
	return json.Marshal(closedBody(plainClosedID, match))
}

func closedBody(id string, match []matcher) map[string]any {
	body := map[string]any{
		"@id": id, "terminal": true,
		"handle": []map[string]any{{
			"handler": "static_response", "status_code": http.StatusServiceUnavailable,
			"body":    "This site can't be reached right now. Try again in a little while.\n",
			"headers": map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}, "Cache-Control": {"no-store"}, "Retry-After": {"60"}},
		}},
	}
	if len(match) > 0 {
		body["match"] = match
	}
	return body
}

// SetClosed has every route answer 503, or takes that away, if it isn't so
// already. With Caddy down, it goes in the last config, which is what Caddy
// serves when it starts again.
func (c *Caddy) SetClosed(ctx context.Context, closed bool) error {
	raw, up, err := c.rawRoutes(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(raw, func(r json.RawMessage) bool { return routeID(r) == closedRouteID }) == closed {
		return nil
	}
	body, err := closedRoute()
	if err != nil {
		return err
	}
	edit := func(routes []json.RawMessage) []json.RawMessage {
		routes = slices.DeleteFunc(routes, func(r json.RawMessage) bool { return routeID(r) == closedRouteID })
		if closed {
			routes = append(routes, body)
		}
		return routes
	}
	if !up {
		return c.editLastConfig(edit)
	}
	return c.mutate(ctx, func(routes []json.RawMessage) ([]json.RawMessage, error) { return edit(routes), nil })
}

// SetPanel makes Caddy serve the panel's site, or stop, if it isn't already.
func (c *Caddy) SetPanel(ctx context.Context, panel *ingress.PanelSite) error {
	raw, up, err := c.rawRoutes(ctx)
	if err != nil || !up {
		return err
	}
	if err := c.setPanelCert(ctx, panel); err != nil {
		return err
	}
	i := slices.IndexFunc(raw, func(r json.RawMessage) bool { return routeID(r) == panelRouteID })
	var want []byte
	if panel != nil {
		if want, err = json.Marshal(panelRoute(panel, filepath.Join(c.Runtime.Paths().Run, panelSocketName))); err != nil {
			return err
		}
	}
	if (panel == nil && i < 0) || (i >= 0 && panel != nil && sameJSON(raw[i], want)) {
		return nil
	}
	return c.mutate(ctx, func(routes []json.RawMessage) ([]json.RawMessage, error) {
		routes = slices.DeleteFunc(routes, func(r json.RawMessage) bool { return strings.HasPrefix(routeID(r), panelRouteID) })
		if panel == nil {
			return routes, nil
		}
		routes = append(routes, want)
		for _, rule := range plainHTTPRules(panel) {
			b, err := json.Marshal(rule)
			if err != nil {
				return nil, err
			}
			routes = append(routes, b)
		}
		return routes, nil
	})
}

// panelCertTag marks the certificate loaded for the panel, among any others.
const panelCertTag = "dockwell.panel"

// setPanelCert loads the certificate the panel has, so that moving behind Caddy
// can't take a working panel dark, or drops it once Caddy has its own. It lives in
// Caddy's config, in PEM, like what is in Caddy's data directory, rather than in
// a file whose going missing would stop Caddy starting.
func (c *Caddy) setPanelCert(ctx context.Context, panel *ingress.PanelSite) error {
	const path = "/config/apps/tls"
	b, _, err := c.call(ctx, http.MethodGet, path, nil, nil)
	var ae *adminError
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || strings.Contains(ae.Message, "invalid traversal path")) {
		b, err = []byte("null"), nil
	}
	if err != nil {
		return err
	}
	tlsApp := map[string]json.RawMessage{}
	if !bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		if err := json.Unmarshal(b, &tlsApp); err != nil {
			return fmt.Errorf("can't read Caddy's TLS config: %w", err)
		}
	}
	certs := map[string]json.RawMessage{}
	if err := unmarshalIf(tlsApp["certificates"], &certs); err != nil {
		return err
	}
	var loaded []map[string]any
	if err := unmarshalIf(certs["load_pem"], &loaded); err != nil {
		return err
	}
	isOurs := func(cert map[string]any) bool {
		tags, _ := cert["tags"].([]any)
		return slices.Contains(tags, any(panelCertTag))
	}
	has := slices.ContainsFunc(loaded, isOurs)
	want := panel != nil && panel.Cert != nil
	if want {
		if _, own := c.issued(panel.Domain); own {
			want = false
		}
	}
	if want == has {
		return nil
	}
	loaded = slices.DeleteFunc(loaded, isOurs)
	if want {
		loaded = append(loaded, map[string]any{"certificate": string(panel.Cert.Chain), "key": string(panel.Cert.Key), "tags": []string{panelCertTag}})
	}
	if len(loaded) == 0 {
		delete(certs, "load_pem")
	} else if certs["load_pem"], err = json.Marshal(loaded); err != nil {
		return err
	}
	if len(certs) == 0 {
		delete(tlsApp, "certificates")
	} else if tlsApp["certificates"], err = json.Marshal(certs); err != nil {
		return err
	}
	if len(tlsApp) == 0 {
		_, _, err = c.call(ctx, http.MethodDelete, path, nil, nil)
	} else {
		var body []byte
		if body, err = json.Marshal(tlsApp); err != nil {
			return err
		}
		_, _, err = c.call(ctx, http.MethodPost, path, body, nil)
	}
	if err != nil {
		return fmt.Errorf("can't change the panel's certificate in Caddy: %w", err)
	}
	c.keepRunning(ctx)
	return nil
}

func sameJSON(a, b []byte) bool {
	var ma, mb any
	if json.Unmarshal(a, &ma) != nil || json.Unmarshal(b, &mb) != nil {
		return false
	}
	ca, _ := json.Marshal(ma)
	cb, _ := json.Marshal(mb)
	return bytes.Equal(ca, cb)
}

// InFlight says how many connections are open to an upstream, as the kernel has
// them, from the server's own network namespace, which is Caddy's: it runs on the
// host's network, as this agent does. Caddy's own count is by what a config says to
// dial, which for a route that dials through a file is every container it has ever
// sent to. A connection with no request on it closes within upstreamIdle, so what is
// left is what is on its way. A request's connection may open a moment after a
// route moves, which is why Drain reads twice.
func (c *Caddy) InFlight(_ context.Context, up string) (int, error) {
	host, port, err := net.SplitHostPort(up)
	if err != nil {
		return 0, fmt.Errorf("%s isn't an address and a port: %w", up, err)
	}
	address, err := netip.ParseAddr(host)
	if err != nil || !address.Is4() {
		return 0, fmt.Errorf("%s isn't an IPv4 address, which is all the ingress network has", host)
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%s isn't a port: %w", port, err)
	}
	// The kernel writes an address in the host's order: little-endian here, as on
	// every architecture Dockwell ships for.
	b := address.As4()
	remote := fmt.Sprintf("%02X%02X%02X%02X:%04X", b[3], b[2], b[1], b[0], number)
	table := c.procNetTCP
	if table == "" {
		table = "/proc/net/tcp"
	}
	f, err := os.Open(table) //nolint:gosec // the kernel's own table, or a test's
	if err != nil {
		return 0, fmt.Errorf("can't read the server's connections: %w", err)
	}
	defer f.Close()
	const established = "01"
	n := 0
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) > 3 && fields[2] == remote && fields[3] == established {
			n++
		}
	}
	if err := scan.Err(); err != nil {
		return 0, fmt.Errorf("can't read the server's connections: %w", err)
	}
	return n, nil
}

// Retry starts a pending or failed certificate's next try now. Caddy takes
// the same config again only when told to, and a reload starts what it
// manages anew.
func (c *Caddy) Retry(ctx context.Context) error {
	servers, etag, up, err := c.readServers(ctx)
	if err != nil || !up || servers == nil {
		return err
	}
	body, err := json.Marshal(servers)
	if err != nil {
		return err
	}
	_, _, err = c.call(ctx, http.MethodPatch, serversPath, body, http.Header{"Cache-Control": {"must-revalidate"}, "If-Match": {etag}})
	// Changed meanwhile: that was a reload too, and starts what this would.
	var ae *adminError
	if errors.As(err, &ae) && ae.Status == http.StatusPreconditionFailed {
		return nil
	}
	return err
}

// Certificates says where each domain's certificate stands: issued if one is
// on disk and hasn't expired, and otherwise as Caddy's log says.
func (c *Caddy) Certificates(_ context.Context, domains []string) (map[string]ingress.CertState, error) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	if err := c.log.read(filepath.Join(c.dataDir(), tlsLogName)); err != nil {
		return nil, err
	}
	out := make(map[string]ingress.CertState, len(domains))
	for _, d := range domains {
		st := c.log.state(d)
		if notAfter, ok := c.issued(d); ok {
			st.Phase, st.NotAfter = ingress.CertIssued, notAfter
		}
		out[d] = st
	}
	return out, nil
}

// issuerDirs bounds how many certificate authorities' directories are looked in.
// A server has used one or two, and these are Caddy's to fill.
const issuerDirs = 64

// issued finds a domain's certificate in Caddy's storage, one directory for
// each CA it has used, and the latest that hasn't expired. It reads through
// nothing Caddy could have left there: only regular files, of a bounded size,
// inside its data directory.
func (c *Caddy) issued(domain string) (time.Time, bool) {
	var best time.Time
	root, err := os.OpenRoot(c.dataDir())
	if err != nil {
		return best, false
	}
	defer root.Close()
	const certificates = "caddy/certificates"
	dir, err := root.Open(certificates)
	if err != nil {
		return best, false
	}
	defer dir.Close()
	issuers, err := dir.ReadDir(issuerDirs)
	if err != nil {
		return best, false
	}
	for _, issuer := range issuers {
		name := filepath.Join(certificates, issuer.Name(), domain, domain+".crt")
		f, err := openRegular(root, name)
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, maxCertFile))
		_ = f.Close()
		if err != nil {
			continue
		}
		block, _ := pem.Decode(b)
		if block == nil {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err == nil && cert.NotAfter.After(best) {
			best = cert.NotAfter
		}
	}
	return best, best.After(time.Now())
}
