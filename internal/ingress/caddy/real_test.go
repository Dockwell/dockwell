// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/dockwell/dockwell/internal/ingress"
)

// envCaddy names a Caddy binary of the pinned version, which these tests run
// on high ports, with its admin API on a unix socket, to prove that the JSON
// and the reload behaviour are real. CI's Docker job sets it. A test here ends
// OnRealCaddy, so -run OnRealCaddy runs them all.
const envCaddy = "DOCKWELL_TEST_CADDY"

func needCaddy(t *testing.T) string {
	t.Helper()
	bin := os.Getenv(envCaddy)
	if bin == "" {
		t.Skipf("%s isn't set, so this doesn't run against real Caddy. Set it to a Caddy %s binary to run it", envCaddy, Tag)
	}
	return bin
}

// process runs the binary as Container runs the image, with the same
// arguments, on the server's paths. Only Docker is left out.
type process struct {
	bin, dir string
	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{}
	runs     int
}

func (p *process) Paths() Paths {
	return Paths{Data: filepath.Join(p.dir, "data"), Config: filepath.Join(p.dir, "config"), Run: filepath.Join(p.dir, "run"), Boot: filepath.Join(p.dir, bootDirName)}
}

func (p *process) Status(context.Context) (ingress.Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil {
		return ingress.Status{}, nil
	}
	select {
	case <-p.exited:
		return ingress.Status{Exists: true}, nil
	default:
		return ingress.Status{Exists: true, Running: true}, nil
	}
}

func (p *process) Pull(context.Context) error { return nil }

func (p *process) Run(_ context.Context, config string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	log, err := os.OpenFile(filepath.Join(p.dir, "caddy.out"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(p.bin, "run", "--config", config, "--resume")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+filepath.Join(p.dir, "data"), "XDG_CONFIG_HOME="+filepath.Join(p.dir, "config"), AdminEnv(p.Paths()))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = log.Close()
		close(exited)
	}()
	p.cmd, p.exited = cmd, exited
	p.runs++
	return nil
}

func (p *process) stop(sig syscall.Signal) {
	p.mu.Lock()
	cmd, exited := p.cmd, p.exited
	p.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
}

func (p *process) Abandon(context.Context) error {
	p.stop(syscall.SIGKILL)
	return nil
}

func (p *process) Remove(context.Context) error {
	p.stop(syscall.SIGTERM)
	p.mu.Lock()
	p.cmd = nil
	p.mu.Unlock()
	return nil
}

// freePort finds a port nothing holds, on TCP and on UDP, on every address, as
// Caddy binds. It is below the kernel's ephemeral range, so that an outgoing
// connection or another test's listener doesn't take it in the meantime.
func freePort(t *testing.T) int {
	t.Helper()
	for range 200 {
		port := 20000 + mrand.IntN(12000)
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue
		}
		_ = l.Close()
		if u, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", fmt.Sprintf(":%d", port)); err == nil {
			_ = u.Close()
			return port
		}
	}
	t.Fatal("no free port")
	return 0
}

// shortDir is a directory whose sockets fit the 108 bytes Linux allows.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dwc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// realCaddy is Caddy on high ports, with certificates from Caddy's own local
// CA, so no CA is needed.
type realCaddy struct {
	*Caddy
	proc *process
	http int
	tls  int
}

func startCaddy(t *testing.T) *realCaddy {
	t.Helper()
	bin := needCaddy(t)
	dir := shortDir(t)
	proc := &process{bin: bin, dir: dir}
	c := &Caddy{
		Dir: dir, Runtime: proc, HTTPPort: freePort(t), HTTPSPort: freePort(t),
		issuerForTest: map[string]any{"module": "internal"},
	}
	t.Cleanup(func() { proc.stop(syscall.SIGKILL) })
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	if err := c.Start(ctx, nil, false); err != nil {
		out, _ := os.ReadFile(filepath.Join(dir, "caddy.out"))
		t.Fatalf("%v\n%s", err, out)
	}
	return &realCaddy{Caddy: c, proc: proc, http: c.HTTPPort, tls: c.HTTPSPort}
}

// clientFor is a client that sends every request to Caddy, for any name, and
// takes its certificates.
func (r *realCaddy) client(serverName string) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: serverName},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", r.tls))
		},
		ForceAttemptHTTP2: true,
	}}
}

func (r *realCaddy) rawConn(t *testing.T, serverName string) *tls.Conn {
	t.Helper()
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, ServerName: serverName, NextProtos: []string{"http/1.1"}}}
	conn, err := d.DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", r.tls))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn.(*tls.Conn)
}

// waitServing retries until a domain's certificate is there and it answers.
func (r *realCaddy) waitServing(t *testing.T, domain, path string) *http.Response {
	t.Helper()
	c := r.client(domain)
	var last error
	for range 100 {
		res, err := c.Get("https://" + domain + path)
		if err == nil {
			return res
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never answered: %v", domain, last)
	return nil
}

// backend is a server the way an app would be, with what proves a reload
// drops nothing: a slow request, a stream and an upgrade.
func backend(t *testing.T, name string) (dial string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/who") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name": name, "host": r.Host, "xff": r.Header.Get("X-Forwarded-For"), "proto": r.Header.Get("X-Forwarded-Proto"), "path": r.URL.Path,
			"real": r.Header.Get("X-Real-Ip"), "early": r.Header.Get("Early-Data"),
		})
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		_, _ = io.WriteString(w, "slow done")
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 40 {
			_, _ = fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, rw)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func aRoute(id, domain, path string, dials ...string) ingress.Route {
	return ingress.Route{ID: id, Domain: domain, Path: path, Upstreams: dials}
}

func put(t *testing.T, c *realCaddy, r ingress.Route) {
	t.Helper()
	if err := c.PutRoute(t.Context(), r); err != nil {
		t.Fatalf("PutRoute(%s): %v", r.ID, err)
	}
}

// whoAt waits for a domain to answer, and reads what its upstream saw.
func (r *realCaddy) whoAt(t *testing.T, domain, path string) map[string]string {
	t.Helper()
	res := r.waitServing(t, domain, path)
	defer res.Body.Close()
	return who(t, res)
}

// who reads what the upstream saw.
func who(t *testing.T, res *http.Response) map[string]string {
	t.Helper()
	defer res.Body.Close()
	var out map[string]string
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheAdminAPIIsOnlyOnItsSocketOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	info, err := os.Stat(filepath.Join(c.Dir, "run", adminSocketName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("the admin socket is %v, want a socket with mode 0600", info.Mode())
	}
	run, err := os.Stat(filepath.Join(c.Dir, "run"))
	if err != nil {
		t.Fatal(err)
	}
	if run.Mode().Perm() != 0o700 {
		t.Errorf("the socket's directory has mode %v, want 0700", run.Mode().Perm())
	}
	// Caddy's default admin address, if the config hadn't named another.
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", "127.0.0.1:2019")
	if err == nil {
		_ = conn.Close()
		// Something else on this machine may have it; Caddy's config says not.
		b, _, cerr := c.call(t.Context(), http.MethodGet, "/config/admin", nil, nil)
		if cerr != nil || !strings.Contains(string(b), "unix/") {
			t.Errorf("something answers on 127.0.0.1:2019, and Caddy's admin config is %s, %v", b, cerr)
		}
	}
}

// A domain with a route to one path answers a 404 for the rest, as a plain route does.
// Caddy would otherwise answer an empty 200, which reads as a site that works.
func TestAPathNothingCoversIsA404OverHTTPSOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	api := backend(t, "api")
	put(t, c, aRoute("dockwell.env.api", "app.example.net", "/api", api))
	if got := c.whoAt(t, "app.example.net", "/api/who"); got["name"] != "api" {
		t.Fatalf("/api reaches %v", got)
	}
	res, err := c.client("app.example.net").Get("https://app.example.net/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "no site at this address") {
		t.Errorf("the root of a domain with /api alone answers %d, %q, want a 404", res.StatusCode, body)
	}
	// It isn't one of the routes, and comes back as it was each time they change.
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Edited || routes[0].Path != "/api" {
		t.Errorf("it reads back as %+v", routes)
	}
	// A route to the root comes ahead of it.
	web := backend(t, "web")
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))
	if got := c.whoAt(t, "app.example.net", "/who"); got["name"] != "web" {
		t.Errorf("the root, with a route to it, reaches %v", got)
	}
	if got := c.whoAt(t, "app.example.net", "/api/who"); got["name"] != "api" {
		t.Errorf("/api reaches %v", got)
	}
}

func TestRoutesServeDomainsAndPathsOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	web, api := backend(t, "web"), backend(t, "api")
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))
	put(t, c, aRoute("dockwell.env.api", "app.example.net", "/api", api))
	put(t, c, aRoute("dockwell.env.other", "other.example.net", "", api))

	got := func(domain, path string) string {
		t.Helper()
		res := c.waitServing(t, domain, path)
		defer res.Body.Close()
		if res.StatusCode == http.StatusNotFound {
			return res.Header.Get("X-Upstream")
		}
		return who(t, res)["name"]
	}
	for _, tc := range []struct{ domain, path, want string }{
		{"app.example.net", "/who", "web"},
		{"app.example.net", "/api/who", "api"},
		{"other.example.net", "/who", "api"},
	} {
		if name := got(tc.domain, tc.path); name != tc.want {
			t.Errorf("%s%s went to %s, want %s", tc.domain, tc.path, name, tc.want)
		}
	}
	// /apis only starts the same as /api, so it is the whole domain's.
	if name := got("app.example.net", "/apis/who"); name != "web" {
		t.Errorf("/apis/who went to %s, want web", name)
	}
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Fatalf("Routes = %+v, want 3", routes)
	}
	for _, r := range routes {
		if r.Edited {
			t.Errorf("%+v reads as edited, as written", r)
		}
	}
	if routes[0].ID != "dockwell.env.api" {
		t.Errorf("the first route is %s, want the longest path first", routes[0].ID)
	}
}

// A client can't tell a service who it is, as X-Forwarded-For goes through
// Caddy's own hands; and a request whose Host isn't its TLS name is refused.
func TestHeadersAreCaddysOwnOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	res := c.waitServing(t, "app.example.net", "/who")
	_ = res.Body.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://app.example.net/who", http.NoBody)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Forwarded-Proto", "http")
	res, err := c.client("app.example.net").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	seen := who(t, res)
	if seen["xff"] != "127.0.0.1" {
		t.Errorf("the service reads X-Forwarded-For %q, want the real client, 127.0.0.1", seen["xff"])
	}
	if seen["proto"] != "https" {
		t.Errorf("the service reads X-Forwarded-Proto %q, want https", seen["proto"])
	}

	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, "https://app.example.net/who", http.NoBody)
	req.Host = "other.example.net"
	res, err = c.client("app.example.net").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("a Host that isn't the TLS name gets %d, want 421", res.StatusCode)
	}
	// A name with no route gets no certificate, and so no answer.
	if res, err := c.client("nope.example.net").Get("https://nope.example.net/who"); err == nil {
		_ = res.Body.Close()
		t.Error("a name with no route got an answer over TLS")
	}
}

// Apps moved from another proxy often trust one of these for per-address
// limits and allowlists, so none of what a client says in them may arrive, and
// X-Real-IP must be the real address, or they'd stop working.
func TestNoClientAddressHeaderReachesAServiceOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	var mu sync.Mutex
	seen := http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", strings.TrimPrefix(srv.URL, "http://")))
	_ = c.waitServing(t, "app.example.net", "/").Body.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://app.example.net/", http.NoBody)
	names := slices.Concat([]string{"X-Forwarded-For", "X-Real-Ip"}, forgeableHeaders, claimsOfTheRequest)
	for _, h := range names {
		req.Header.Set(h, "6.6.6.6")
	}
	req.Header["X_forwarded_for"] = []string{"6.6.6.6"}
	req.Header["X_real_ip"] = []string{"6.6.6.6"}
	res, err := c.client("app.example.net").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	for k, v := range seen {
		if strings.Contains(strings.Join(v, ","), "6.6.6.6") {
			t.Errorf("a client's %s: %v reached the service", k, v)
		}
	}
	if got := seen.Get("X-Real-Ip"); got != "127.0.0.1" {
		t.Errorf("the service reads X-Real-IP %q, want the real client, 127.0.0.1", got)
	}
	if got := seen.Get("X-Forwarded-For"); got != "127.0.0.1" {
		t.Errorf("the service reads X-Forwarded-For %q, want the real client, 127.0.0.1", got)
	}
}

// Early data can be replayed by anyone who captured it, and its source address
// isn't proven. This asks for it as a browser that remembers a server does. A
// request Caddy took in early data reaches its upstream with Early-Data: 1, as
// RFC 8470 asks; with 0-RTT off, the client's early data is refused and it
// sends the same request again, once the handshake is done.
func TestEarlyDataIsRefusedOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()

	sessions := tls.NewLRUClientSessionCache(4)
	tlsConfig := &tls.Config{InsecureSkipVerify: true, ServerName: "app.example.net", ClientSessionCache: sessions, NextProtos: []string{http3.NextProtoH3}}
	var last atomic.Pointer[quic.Conn]
	transport := func() *http3.Transport {
		return &http3.Transport{
			TLSClientConfig: tlsConfig, QUICConfig: &quic.Config{Allow0RTT: true},
			Dial: func(ctx context.Context, _ string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
				conn, err := quic.DialAddrEarly(ctx, fmt.Sprintf("127.0.0.1:%d", c.tls), tlsCfg, cfg)
				if err == nil {
					last.Store(conn)
				}
				return conn, err
			},
		}
	}
	// The first connection has nothing to resume, and gets a ticket to.
	first := transport()
	defer first.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://app.example.net/who", http.NoBody)
	res, err := first.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := who(t, res); got["early"] != "" {
		t.Fatalf("a first connection's request reads as early data: %v", got)
	}
	time.Sleep(200 * time.Millisecond) // for the ticket, which comes after the handshake

	second := transport()
	defer second.Close()
	req, _ = http.NewRequestWithContext(t.Context(), http3.MethodGet0RTT, "https://app.example.net/who", http.NoBody)
	res, err = second.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	got := who(t, res)
	conn := last.Load()
	<-conn.HandshakeComplete()
	if used := conn.ConnectionState().Used0RTT; got["early"] != "" || used {
		t.Errorf("Caddy took a request in early data, replayable by anyone who captured it: the upstream read Early-Data %q, and the connection says used 0-RTT %v", got["early"], used)
	}
	if got["name"] != "web" {
		t.Errorf("the request answered %v, want it served once the handshake was done", got)
	}
}

// The test the issue asks for: sustained requests, long ones and WebSockets,
// through config changes, counting failures. It would catch Caddy's default of
// closing upgraded connections on a reload, which is what stream_close_delay
// is for.
func TestReloadsDropNothingOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	a, b := backend(t, "a"), backend(t, "b")
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", a))
	put(t, c, aRoute("dockwell.env.side", "side.example.net", "", b))
	res := c.waitServing(t, "app.example.net", "/who")
	_ = res.Body.Close()

	// A WebSocket that echoes through every reload.
	ws := c.rawConn(t, "app.example.net")
	_, _ = fmt.Fprint(ws, "GET /ws HTTP/1.1\r\nHost: app.example.net\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: x\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(ws)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("the upgrade answered %q, %v", status, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	var wsErr atomic.Value
	var echoes atomic.Int64
	stop := make(chan struct{})
	var work sync.WaitGroup
	work.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			msg := fmt.Sprintf("m%d\n", i)
			if _, err := io.WriteString(ws, msg); err != nil {
				wsErr.Store(err.Error())
				return
			}
			_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
			got, err := br.ReadString('\n')
			if err != nil || got != msg {
				wsErr.Store(fmt.Sprintf("read %q, %v", got, err))
				return
			}
			echoes.Add(1)
		}
	})

	// Sustained short requests, each over its own or a kept connection.
	var ok atomic.Int64
	var failures sync.Map
	var failed atomic.Int64
	client := c.client("app.example.net")
	for range 8 {
		work.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := client.Get("https://app.example.net/who")
				switch {
				case err != nil:
					failed.Add(1)
					failures.Store(err.Error(), true)
				case res.StatusCode != http.StatusOK:
					failed.Add(1)
					failures.Store(res.Status, true)
					_ = res.Body.Close()
				default:
					_, _ = io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
					ok.Add(1)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}

	// A stream and a slow request across every reload.
	streamed := make(chan int, 1)
	work.Go(func() {
		res, err := client.Get("https://app.example.net/stream")
		if err != nil {
			failures.Store(err.Error(), true)
			failed.Add(1)
			streamed <- 0
			return
		}
		defer res.Body.Close()
		n := 0
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data:") {
				n++
			}
		}
		streamed <- n
	})
	slow := make(chan string, 1)
	work.Go(func() {
		res, err := client.Get("https://app.example.net/slow")
		if err != nil {
			slow <- err.Error()
			return
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		slow <- string(b)
	})

	// Config changes, of the route under load and of another.
	time.Sleep(200 * time.Millisecond)
	const reloads = 16
	for i := range reloads {
		up := a
		if i%2 == 0 {
			up = b
		}
		put(t, c, aRoute("dockwell.env.www", "app.example.net", "", up))
		put(t, c, aRoute(fmt.Sprintf("dockwell.env.extra%d", i%3), fmt.Sprintf("extra%d.example.net", i%3), "", b))
		time.Sleep(200 * time.Millisecond)
	}
	if got := <-streamed; got != 40 {
		t.Errorf("the stream got %d events through the reloads, want all 40", got)
	}
	if got := <-slow; got != "slow done" {
		t.Errorf("the slow request ended with %q", got)
	}
	close(stop)
	work.Wait()
	if v := wsErr.Load(); v != nil {
		t.Errorf("the WebSocket didn't last the reloads, after %d echoes: %v", echoes.Load(), v)
	}
	if echoes.Load() < 10 {
		t.Errorf("the WebSocket echoed %d times, too few to mean anything", echoes.Load())
	}
	failures.Range(func(k, _ any) bool {
		t.Errorf("a request failed: %v", k)
		return true
	})
	if failed.Load() != 0 {
		t.Errorf("%d of %d requests failed through %d reloads", failed.Load(), failed.Load()+ok.Load(), reloads*2)
	}
	if ok.Load() < 100 {
		t.Errorf("only %d requests ran through the reloads, too few to mean anything", ok.Load())
	}
	t.Logf("%d requests, %d WebSocket echoes, a stream and a slow request through %d config changes, with none dropped", ok.Load(), echoes.Load(), reloads*2)
}

// What a rollout rests on: an upstream taken out of a route gets no new
// requests, and what it has in flight finishes and is counted till it does.
func TestAnUpstreamDrainsOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	old, next := backend(t, "old"), backend(t, "next")
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", old))
	res := c.waitServing(t, "app.example.net", "/who")
	_ = res.Body.Close()
	client := c.client("app.example.net")
	done := make(chan string, 1)
	go func() {
		res, err := client.Get("https://app.example.net/slow")
		if err != nil {
			done <- err.Error()
			return
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		done <- string(b)
	}()
	var inFlight int
	for range 50 {
		time.Sleep(20 * time.Millisecond)
		n, err := c.InFlight(t.Context(), old)
		if err != nil {
			t.Fatal(err)
		}
		if inFlight = n; n == 1 {
			break
		}
	}
	if inFlight != 1 {
		t.Fatalf("Caddy says %s has %d requests in flight, want 1", old, inFlight)
	}
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", next))
	res, err := client.Get("https://app.example.net/who")
	if err != nil {
		t.Fatal(err)
	}
	if name := who(t, res)["name"]; name != "next" {
		t.Errorf("a new request went to %s, want next", name)
	}
	if n, err := c.InFlight(t.Context(), old); err != nil || n != 1 {
		t.Errorf("after the swap, Caddy says the old upstream has %d in flight, %v, want 1", n, err)
	}
	if got := <-done; got != "slow done" {
		t.Errorf("the request in flight ended with %q", got)
	}
	// Caddy keeps a connection with nothing on it for upstreamIdle.
	for range 200 {
		n, err := c.InFlight(t.Context(), old)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the old upstream still counts a request that finished")
}

// Decision 5 of ADR 0024: with no agent, Caddy starts again with its last
// config, and the agent, as it comes back, reads it even with Caddy down.
func TestCaddyResumesItsLastConfigOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	web := backend(t, "web")
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "/api", web))
	res := c.waitServing(t, "app.example.net", "/api/who")
	_ = res.Body.Close()
	c.proc.stop(syscall.SIGKILL)

	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatalf("reading routes with Caddy down: %v", err)
	}
	if len(routes) != 1 || routes[0].ID != "dockwell.env.www" || routes[0].Path != "/api" {
		t.Errorf("with Caddy down, Routes = %+v, want the last config's route", routes)
	}
	if err := c.PutRoute(t.Context(), aRoute("dockwell.env.b", "b.example.net", "", web)); err == nil {
		t.Error("a route went in with Caddy down")
	}

	// As Docker does after a reboot: the same command, and no agent in it.
	if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	again := c.waitServing(t, "app.example.net", "/api/who")
	defer again.Body.Close()
	if name := who(t, again)["name"]; name != "web" {
		t.Errorf("after a restart, the route goes to %s", name)
	}
}

// A hand edit reads as one, and Dockwell leaves a hand-added route alone.
func TestHandEditsReadAsThemselvesOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	web := backend(t, "web")
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))

	mine := `{"@id":"by-hand","match":[{"host":["mine.example.net"]}],"handle":[{"handler":"static_response","body":"mine"}],"terminal":true}`
	if _, _, err := c.call(t.Context(), http.MethodPost, routesPath, []byte(mine), nil); err != nil {
		t.Fatalf("adding a route by hand: %v", err)
	}
	edit := `[{"@id":"dockwell.env.www","match":[{"host":["app.example.net"]}],"handle":[{"handler":"headers","response":{"set":{"X-Hand":["1"]}}},{"handler":"reverse_proxy","upstreams":[{"dial":"` + web + `"}]}],"terminal":true},` + mine + `]`
	if _, _, err := c.call(t.Context(), http.MethodPatch, routesPath, []byte(edit), nil); err != nil {
		t.Fatalf("editing a route by hand: %v", err)
	}
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || !routes[0].Edited {
		t.Fatalf("Routes = %+v, want Dockwell's one route, reading as edited", routes)
	}

	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))
	routes, err = c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Edited {
		t.Errorf("after putting it back, Routes = %+v", routes)
	}

	// A setting on the handler Dockwell doesn't write reads as an edit too,
	// though the shape around it is what it wrote.
	withHeaders := `{"@id":"dockwell.env.www","match":[{"host":["app.example.net"]}],"handle":[{"handler":"reverse_proxy","headers":{"request":{"set":{"X-Hand":["1"]}}},"upstreams":[{"dial":"` + web + `"}]}],"terminal":true}`
	if _, _, err := c.call(t.Context(), http.MethodPatch, routesPath, []byte("["+withHeaders+","+mine+"]"), nil); err != nil {
		t.Fatalf("editing a handler by hand: %v", err)
	}
	routes, err = c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || !routes[0].Edited {
		t.Errorf("with a header added by hand, Routes = %+v, want it reading as edited", routes)
	}
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))
	raw, _, err := c.rawRoutes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(string(raw[0]), string(raw[1])), "by-hand") {
		t.Errorf("the hand-added route is gone from %s", raw)
	}
}

// fakePanel answers as the panel, on its socket.
func fakePanel(t *testing.T, dir string) {
	t.Helper()
	path := PanelSocket(dir)
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// HTTP/1.1 and cleartext HTTP/2, as the panel's listeners take.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Protocols: &protocols, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"host": r.Host, "xff": r.Header.Get("X-Forwarded-For"), "origin": r.Header.Get("Origin"), "proto": r.Proto,
		})
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// The panel behind Caddy: over its socket, with Caddy's own idea of who is
// asking, and plain HTTP keeping the panel's rules.
func TestThePanelSitsBehindCaddyOnRealCaddy(t *testing.T) {
	bin := needCaddy(t)
	dir := shortDir(t)
	if err := EnsureDirs(dir); err != nil {
		t.Fatal(err)
	}
	fakePanel(t, dir)
	proc := &process{bin: bin, dir: dir}
	c := &Caddy{Dir: dir, Runtime: proc, HTTPPort: freePort(t), HTTPSPort: freePort(t), issuerForTest: map[string]any{"module": "internal"}}
	t.Cleanup(func() { proc.stop(syscall.SIGKILL) })
	site := &ingress.PanelSite{Domain: "panel.example.net"}
	if err := c.Start(t.Context(), site, false); err != nil {
		t.Fatal(err)
	}
	r := &realCaddy{Caddy: c, proc: proc, http: c.HTTPPort, tls: c.HTTPSPort}
	put(t, r, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))

	res := r.waitServing(t, "panel.example.net", "/")
	_ = res.Body.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://panel.example.net/x", http.NoBody)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("Origin", "https://panel.example.net")
	res, err := r.client("panel.example.net").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	seen := who(t, res)
	if seen["host"] != "panel.example.net" || seen["origin"] != "https://panel.example.net" {
		t.Errorf("the panel reads %v, want Host and Origin as the browser sent them", seen)
	}
	if seen["xff"] != "127.0.0.1" {
		t.Errorf("the panel reads X-Forwarded-For %q, want what Caddy wrote", seen["xff"])
	}
	if seen["proto"] != "HTTP/2.0" {
		t.Errorf("the panel gets %s from a client on HTTP/2, want HTTP/2.0, as gRPC needs", seen["proto"])
	}

	// Plain HTTP refuses what it must, for the panel's domain alone.
	plain := func(method, host string, header map[string]string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), method, fmt.Sprintf("http://127.0.0.1:%d/x", r.http), http.NoBody)
		req.Host = host
		for k, v := range header {
			req.Header.Set(k, v)
		}
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, body := plain(http.MethodGet, "panel.example.net", map[string]string{"Authorization": "Bearer dwt_x"}); code != http.StatusBadRequest || !strings.Contains(body, "revoke the token") {
		t.Errorf("a token over plain HTTP gets %d %q, want 400 saying to revoke it", code, body)
	}
	if code, _ := plain(http.MethodPost, "panel.example.net", nil); code != http.StatusBadRequest {
		t.Errorf("a POST over plain HTTP gets %d, want 400", code)
	}
	if code, _ := plain(http.MethodGet, "panel.example.net", nil); code != http.StatusPermanentRedirect {
		t.Errorf("a GET over plain HTTP gets %d, want a redirect", code)
	}
	if code, _ := plain(http.MethodPost, "app.example.net", nil); code == http.StatusBadRequest {
		t.Errorf("an app's POST over plain HTTP gets the panel's refusal")
	}

	// Setting the same site changes nothing; taking it away removes it.
	if err := c.SetPanel(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPanel(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	raw, _, err := c.rawRoutes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%s", raw), panelRouteID) {
		t.Errorf("the panel's route stayed after it went: %s", raw)
	}
	if code, _ := plain(http.MethodPost, "panel.example.net", nil); code == http.StatusBadRequest {
		t.Error("plain HTTP keeps the panel's rules after the panel went")
	}

	// A domain given to Dockwell later puts them back, on a Caddy already up.
	if err := c.SetPanel(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	if code, _ := plain(http.MethodPost, "panel.example.net", nil); code != http.StatusBadRequest {
		t.Errorf("after the panel came back, a POST over plain HTTP gets %d, want 400", code)
	}
	if res := r.waitServing(t, "panel.example.net", "/"); res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		t.Errorf("after the panel came back, it answers %d", res.StatusCode)
	} else {
		_ = res.Body.Close()
	}
}

// Stopping keeps certificates, and the next start needs no new one.
func TestStoppingKeepsCertificatesOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	web := backend(t, "web")
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))
	res := c.waitServing(t, "app.example.net", "/who")
	_ = res.Body.Close()
	serial := func() string {
		t.Helper()
		conn := c.rawConn(t, "app.example.net")
		if err := conn.HandshakeContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	}
	before := serial()
	certs, _ := filepath.Glob(filepath.Join(c.Dir, "data", "caddy", "certificates", "*", "app.example.net", "app.example.net.crt"))
	if len(certs) != 1 {
		t.Fatalf("certificates on disk: %v", certs)
	}
	if err := c.DeleteRoute(t.Context(), "dockwell.env.www"); err != nil {
		t.Fatal(err)
	}
	// Deleting what isn't there changes nothing.
	if err := c.DeleteRoute(t.Context(), "dockwell.env.www"); err != nil {
		t.Errorf("deleting a route that is gone: %v", err)
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(certs[0]); err != nil {
		t.Fatalf("the certificate went with Caddy: %v", err)
	}
	// Its last config goes, so the next first route starts from the bootstrap,
	// with what this build sets, and what it held of the panel's key goes with it.
	if _, err := os.Stat(c.autosaveFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Caddy's last config stayed where it would resume it: %v", err)
	}
	if conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", c.tls)); err == nil {
		_ = conn.Close()
		t.Error("Caddy stopped, and its port still answers")
	}
	if err := c.Start(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))
	res = c.waitServing(t, "app.example.net", "/who")
	_ = res.Body.Close()
	if after := serial(); after != before {
		t.Errorf("the certificate changed across a stop: %s, then %s", before, after)
	}
	states, err := c.Certificates(t.Context(), []string{"app.example.net", "new.example.net"})
	if err != nil {
		t.Fatal(err)
	}
	if st := states["app.example.net"]; st.Phase != ingress.CertIssued || st.NotAfter.IsZero() {
		t.Errorf("the certificate's state is %+v, want issued with an expiry", st)
	}
	if st := states["new.example.net"]; st.Phase != ingress.CertPending {
		t.Errorf("a domain with no certificate reads %+v, want pending", st)
	}
}
