// SPDX-License-Identifier: AGPL-3.0-only

// Package caddy runs a server's routes on Caddy, as an ingress.Proxy: a
// container on the server's own network, pinned by digest, managed through its
// admin API on a unix socket with JSON config. See
// docs/adr/0024-the-caddy-ingress-driver.md.
package caddy

import (
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
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/ingress"
)

// Name is the proxy's name, for people.
const Name = "Caddy"

// serverName is the HTTP server in Caddy's config that holds the routes, and
// httpServerName the one that keeps the panel's rules on plain HTTP.
const (
	serverName     = "dockwell"
	httpServerName = "dockwell_http"
	panelRouteID   = "dockwell.panel"
	closedRouteID  = "dockwell.closed"
	routesPath     = "/config/apps/http/servers/" + serverName + "/routes"
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
	// server may have takes under a second.
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
}

type upstream struct {
	Dial string `json:"dial"`
}

type loadBalancing struct {
	TryDuration string `json:"try_duration,omitempty"`
	TryInterval string `json:"try_interval,omitempty"`
}

type transport struct {
	Protocol string   `json:"protocol"`
	Versions []string `json:"versions,omitempty"`
}

type handler struct {
	Handler          string         `json:"handler"`
	Upstreams        []upstream     `json:"upstreams,omitempty"`
	FlushInterval    int            `json:"flush_interval,omitempty"`
	StreamCloseDelay string         `json:"stream_close_delay,omitempty"`
	LoadBalancing    *loadBalancing `json:"load_balancing,omitempty"`
	Transport        *transport     `json:"transport,omitempty"`
	Headers          *headers       `json:"headers,omitempty"`
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

// clientAddress is what every route sends upstream of where a request came
// from: X-Real-IP is the client's real address, so an app moved from OpenResty
// keeps working, and what else a client might claim is gone.
func clientAddress() *headers {
	return &headers{Request: &headerOps{
		Set:    map[string][]string{"X-Real-Ip": {"{http.request.remote.host}"}},
		Delete: forgeableHeaders,
	}}
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
	ups := make([]upstream, len(r.Upstreams))
	for i, u := range r.Upstreams {
		ups[i] = upstream{Dial: u}
	}
	return route{ID: r.ID, Match: []matcher{m}, Terminal: true, Handle: []handler{{
		Handler: "reverse_proxy", Upstreams: ups, FlushInterval: -1, StreamCloseDelay: streamClose,
		LoadBalancing: &loadBalancing{TryDuration: tryDuration, TryInterval: tryInterval},
		Headers:       clientAddress(),
	}}}
}

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
// the server's loopback.
func (c *Caddy) bootstrap(panel *ingress.PanelSite) ([]byte, error) {
	paths := c.Runtime.Paths()
	httpPort, httpsPort := c.ports()
	routes := []route{}
	servers := map[string]any{}
	if panel != nil {
		routes = append(routes, panelRoute(panel, filepath.Join(paths.Run, panelSocketName)))
		servers[httpServerName] = map[string]any{"listen": []string{":" + strconv.Itoa(httpPort)}, "routes": plainHTTPRules(panel)}
	}
	servers[serverName] = map[string]any{
		"listen": []string{":" + strconv.Itoa(httpsPort)}, "strict_sni_host": true, "routes": routes,
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
	if err := c.prepare(panel); err != nil {
		return err
	}
	return c.Runtime.Pull(ctx)
}

// Start runs Caddy, and returns once its admin API answers.
func (c *Caddy) Start(ctx context.Context, panel *ingress.PanelSite) error {
	if err := c.prepare(panel); err != nil {
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
func (c *Caddy) prepare(panel *ingress.PanelSite) error {
	if err := EnsureDirs(c.Dir); err != nil {
		return err
	}
	b, err := c.bootstrap(panel)
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

// Stop stops Caddy and frees its ports, keeping its certificates. Its last
// config is set aside, not left to resume, and the agent's copy goes: a server's
// next first route starts from the bootstrap, with what this build sets.
func (c *Caddy) Stop(ctx context.Context) error {
	if err := c.Runtime.Remove(ctx); err != nil {
		return err
	}
	root, err := os.OpenRoot(c.configDir())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("can't set aside Caddy's last config: %w", err)
	default:
		defer root.Close()
		if err := root.Rename(autosaveRel, autosaveRel+".last"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("can't set aside Caddy's last config: %w", err)
		}
	}
	return c.forgetGood()
}

// rawRoutes reads the routes with Caddy's own ETag, or from its last config
// when it is down, whereupon the ETag is empty.
func (c *Caddy) rawRoutes(ctx context.Context) ([]json.RawMessage, string, bool, error) {
	b, etag, err := c.call(ctx, http.MethodGet, routesPath, nil, nil)
	if err == nil {
		routes, err := decodeRoutes(b)
		return routes, etag, true, err
	}
	if !errors.Is(err, errDown) {
		// A Caddy with no config at all says the path to the routes is invalid.
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
				Servers map[string]struct {
					Routes []json.RawMessage `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(saved, &config); err != nil {
		return nil, "", false, fmt.Errorf("can't read Caddy's last config in %s: %w", c.autosaveFile(), err)
	}
	return config.Apps.HTTP.Servers[serverName].Routes, "", false, nil
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
	raw, _, _, err := c.rawRoutes(ctx)
	if err != nil {
		return nil, err
	}
	var out []ingress.Route
	for _, r := range raw {
		if read, ok := readRoute(r); ok {
			out = append(out, read)
		}
	}
	return out, nil
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
	knownMatchKeys   = []string{"host", "path"}
	knownHandlerKeys = []string{"handler", "upstreams", "flush_interval", "stream_close_delay", "load_balancing", "headers"}
)

// readRoute reads one of Dockwell's routes; ok is false for another's, which
// Dockwell leaves alone.
func readRoute(raw json.RawMessage) (ingress.Route, bool) {
	id := routeID(raw)
	if !strings.HasPrefix(id, "dockwell.") || strings.HasPrefix(id, panelRouteID) || id == closedRouteID {
		return ingress.Route{}, false
	}
	out := ingress.Route{ID: id}
	var r route
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Match) != 1 || len(r.Match[0].Host) != 1 || len(r.Handle) != 1 || r.Handle[0].Handler != "reverse_proxy" {
		out.Edited = true
		return out, true
	}
	out.Domain = r.Match[0].Host[0]
	switch p := r.Match[0].Path; {
	case len(p) == 0:
	case len(p) == 2 && p[1] == p[0]+"/*":
		out.Path = p[0]
	default:
		out.Edited = true
	}
	for _, u := range r.Handle[0].Upstreams {
		out.Upstreams = append(out.Upstreams, u.Dial)
	}
	out.Edited = out.Edited || !onlyKnownKeys(raw) || !headersAreOurs(raw)
	return out, true
}

// headersAreOurs reports a route that changes the headers it sends as Dockwell
// has it do, or, as an older config may, not at all. Its key alone isn't enough:
// a header added by hand is a hand edit, and would otherwise read as nothing.
func headersAreOurs(raw json.RawMessage) bool {
	var r struct {
		Handle []struct {
			Headers json.RawMessage `json:"headers"`
		} `json:"handle"`
	}
	if json.Unmarshal(raw, &r) != nil || len(r.Handle) != 1 {
		return false
	}
	if len(r.Handle[0].Headers) == 0 {
		return true
	}
	ours, err := json.Marshal(clientAddress())
	return err == nil && sameJSON(r.Handle[0].Headers, ours)
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
	return in(top, knownRouteKeys) && len(r.Match) == 1 && in(r.Match[0], knownMatchKeys) && len(r.Handle) == 1 && in(r.Handle[0], knownHandlerKeys)
}

// mutate changes the routes as one reload, against the version it read, so a
// change made between the two isn't lost; it tries again if there was one.
// Routes that aren't Dockwell's stay, so a hand-added one isn't lost either.
func (c *Caddy) mutate(ctx context.Context, edit func([]json.RawMessage) ([]json.RawMessage, error)) error {
	for try := 0; ; try++ {
		raw, etag, up, err := c.rawRoutes(ctx)
		if err != nil {
			return err
		}
		if !up {
			return fmt.Errorf("%w, so it can't change its routes. Start it with docker start %s; the agent starts it within a minute", errDown, ingress.ContainerName)
		}
		next, err := edit(raw)
		if err != nil {
			return err
		}
		if next == nil {
			next = []json.RawMessage{} // "null" isn't a list of routes
		}
		sortRoutes(next)
		body, err := json.Marshal(next)
		if err != nil {
			return err
		}
		header := http.Header{}
		if etag != "" {
			header.Set("If-Match", etag)
		}
		_, _, err = c.call(ctx, http.MethodPatch, routesPath, body, header)
		var ae *adminError
		if errors.As(err, &ae) && ae.Status == http.StatusPreconditionFailed && try < 3 {
			continue
		}
		if err == nil {
			c.keepRunning(ctx)
		}
		return err
	}
}

// sortRoutes puts what is most specific first, as Caddy takes the first route
// that matches: the panel, routes that aren't Dockwell's, what closes the rest,
// then each domain's longest paths first. It is stable, so a config reads the
// same each time.
func sortRoutes(routes []json.RawMessage) {
	type key struct {
		rank         int
		domain, path string
	}
	keyOf := func(raw json.RawMessage) key {
		id := routeID(raw)
		switch {
		case id == panelRouteID:
			return key{rank: 0}
		case !strings.HasPrefix(id, "dockwell."):
			return key{rank: 1}
		case id == closedRouteID:
			return key{rank: 2}
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
		case ka.rank < 3:
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
	body, err := json.Marshal(routeJSON(r))
	if err != nil {
		return err
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

// DeleteRoute removes a route, if Caddy has it. With Caddy down it comes out of
// the last config, which is what Caddy serves when it starts again, so a route
// can always be removed: starting Caddy for it may be just what another proxy on
// the ports stops. A Caddy that runs and doesn't answer, as under docker pause,
// would write its own config back over that, so that says what to do instead.
func (c *Caddy) DeleteRoute(ctx context.Context, id string) error {
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
// dialled. Certificates come from the names in those routes, which stay.
func closedRoute() ([]byte, error) {
	return json.Marshal(map[string]any{
		"@id": closedRouteID, "terminal": true,
		"handle": []map[string]any{{
			"handler": "static_response", "status_code": http.StatusServiceUnavailable,
			"body":    "This site can't be reached right now. Try again in a little while.\n",
			"headers": map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}, "Cache-Control": {"no-store"}, "Retry-After": {"60"}},
		}},
	})
}

// SetClosed has every route answer 503, or takes that away, if it isn't so
// already. With Caddy down, it goes in the last config, which is what Caddy
// serves when it starts again.
func (c *Caddy) SetClosed(ctx context.Context, closed bool) error {
	raw, _, up, err := c.rawRoutes(ctx)
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
	raw, _, up, err := c.rawRoutes(ctx)
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
	if err := c.mutate(ctx, func(routes []json.RawMessage) ([]json.RawMessage, error) {
		routes = slices.DeleteFunc(routes, func(r json.RawMessage) bool { return routeID(r) == panelRouteID })
		if panel != nil {
			routes = append(routes, want)
		}
		return routes, nil
	}); err != nil {
		return err
	}
	return c.setPlainHTTP(ctx, panel)
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

// setPlainHTTP puts the panel's rules on plain HTTP, or takes them off.
func (c *Caddy) setPlainHTTP(ctx context.Context, panel *ingress.PanelSite) error {
	path := "/config/apps/http/servers/" + httpServerName
	if panel == nil {
		_, _, err := c.call(ctx, http.MethodDelete, path, nil, nil)
		var ae *adminError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	httpPort, _ := c.ports()
	body, err := json.Marshal(map[string]any{"listen": []string{":" + strconv.Itoa(httpPort)}, "routes": plainHTTPRules(panel)})
	if err != nil {
		return err
	}
	_, _, err = c.call(ctx, http.MethodPatch, path, body, nil)
	var ae *adminError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		_, _, err = c.call(ctx, http.MethodPut, path, body, nil)
	}
	if err == nil {
		c.keepRunning(ctx)
	}
	return err
}

// InFlight says how many requests an upstream has now, across configs.
func (c *Caddy) InFlight(ctx context.Context, up string) (int, error) {
	b, _, err := c.call(ctx, http.MethodGet, "/reverse_proxy/upstreams", nil, nil)
	if err != nil {
		return 0, err
	}
	var list []struct {
		Address     string `json:"address"`
		NumRequests int    `json:"num_requests"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return 0, fmt.Errorf("can't read what Caddy says of its upstreams: %w", err)
	}
	total := 0
	for _, u := range list {
		if u.Address == up {
			total += u.NumRequests
		}
	}
	return total, nil
}

// Retry starts a pending or failed certificate's next try now. Caddy takes
// the same config again only when told to, and a reload starts what it
// manages anew.
func (c *Caddy) Retry(ctx context.Context) error {
	raw, etag, up, err := c.rawRoutes(ctx)
	if err != nil || !up || raw == nil {
		return err
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	_, _, err = c.call(ctx, http.MethodPatch, routesPath, body, http.Header{"Cache-Control": {"must-revalidate"}, "If-Match": {etag}})
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
