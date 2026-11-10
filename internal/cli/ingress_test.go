// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
)

func TestOnlyAPanelOnThePublicPortsHandsThemOver(t *testing.T) {
	for _, tc := range []struct {
		https, http string
		want        bool
	}{
		{":443", ":80", true},
		{"0.0.0.0:443", "0.0.0.0:80", true},
		{":https", ":http", true},
		{":8443", ":80", true},
		{":443", ":8080", true},
		{":8443", ":8080", false},
		{"127.0.0.1:0", "127.0.0.1:0", false},
	} {
		if got := onPublicPorts(runFlags{httpsListen: tc.https, httpListen: tc.http}); got != tc.want {
			t.Errorf("%s and %s: %v, want %v", tc.https, tc.http, got, tc.want)
		}
	}
}

// With its proxy holding the public ports, dockwell run leaves them alone,
// whether the proxy is up or not, and serves the panel over a socket only the
// proxy and root reach. Taking them back would stop Caddy starting.
func TestRunLeavesThePortsToItsProxy(t *testing.T) {
	dir, err := os.MkdirTemp("", "dwr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := datadir.Dir{Root: dir}
	if err := caddy.EnsureDirs(d.Ingress()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Ingress(), "active"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Ports it could never bind in this test, as they are privileged or taken.
	out, logs, stop := startRunWithLogs(t, dir, "/claim#",
		"--domain", "panel.example.com", "--https-listen", "127.0.0.1:443", "--http-listen", "127.0.0.1:80",
		"--acme-directory-url", "https://acme.invalid/directory")
	defer stop()
	socket := caddy.PanelSocket(d.Ingress())
	deadline := time.Now().Add(startUp)
	var info os.FileInfo
	for time.Now().Before(deadline) {
		if info, err = os.Stat(socket); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("no panel socket: %v\n%s\n%s", err, out.String(), logs.String())
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("the panel's socket is %v, want a socket with mode 0600", info.Mode())
	}
	run, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		t.Fatal(err)
	}
	if run.Mode().Perm() != 0o700 {
		t.Errorf("the socket's directory has mode %v, want 0700", run.Mode().Perm())
	}
	// It says so once it has made the socket, so the line can come a moment after it.
	tunnel := func() bool {
		return strings.Contains(logs.String(), "SSH tunnel") && strings.Contains(logs.String(), "ssh -L ")
	}
	for deadline := time.Now().Add(startUp); !tunnel() && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if !tunnel() {
		t.Errorf("the log doesn't say how to reach the panel if the proxy is down:\n%s", logs.String())
	}
}

// The panel's socket is in a directory Caddy owns, so a compromised Caddy can
// leave anything at its name. None of it may stop the control plane starting, as
// that takes the SSH tunnel to the panel with it, and none of it is followed.
func TestWhatStandsAtThePanelsSocketDoesntStopTheControlPlane(t *testing.T) {
	dir, err := os.MkdirTemp("", "dws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := datadir.Dir{Root: dir}
	if err := caddy.EnsureDirs(d.Ingress()); err != nil {
		t.Fatal(err)
	}
	path := caddy.PanelSocket(d.Ingress())
	outside := t.TempDir()
	kept := filepath.Join(outside, "kept")
	if err := os.WriteFile(kept, []byte("root's"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, leave := range map[string]func() error{
		"a directory with something in it": func() error {
			if err := os.MkdirAll(filepath.Join(path, "deeper"), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(path, "deeper", "file"), nil, 0o600)
		},
		"a link to a directory outside": func() error { return os.Symlink(outside, path) },
		"a file":                        func() error { return os.WriteFile(path, nil, 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := leave(); err != nil {
				t.Fatal(err)
			}
			ln, err := listenForIngress(t.Context(), d)
			if err != nil {
				t.Fatalf("with %s at the panel's socket, the control plane can't start: %v", name, err)
			}
			_ = ln.Close()
			if _, err := os.Stat(kept); err != nil {
				t.Errorf("what a link led to went: %v", err)
			}
			_ = os.RemoveAll(path)
		})
	}
}

// A real failed listen, as a path too long for a socket is: dockwell run starts all
// the same, so the SSH tunnel to the panel is there whatever Caddy keeps doing.
func TestRunStartsWithoutThePanelsSocket(t *testing.T) {
	top, err := os.MkdirTemp("", "dwl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(top) })
	dir := filepath.Join(top, strings.Repeat("d", 120))
	d := datadir.Dir{Root: dir}
	if err := caddy.EnsureDirs(d.Ingress()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Ingress(), "active"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenForIngress(t.Context(), d); err == nil {
		t.Fatal("that socket can be listened on, so this proves nothing")
	}
	_, logs, stop := startRunWithLogs(t, dir, "/claim#",
		"--domain", "panel.example.com", "--https-listen", "127.0.0.1:443", "--http-listen", "127.0.0.1:80",
		"--acme-directory-url", "https://acme.invalid/directory")
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "can't listen for what the ingress sends the panel") {
		if time.Now().After(deadline) {
			t.Fatalf("it doesn't say the panel's domain is down:\n%s", logs.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// A compromised Caddy can leave a directory at run/panel.sock and go on writing in it.
// Clearing that has no end while it writes: with 2 or 4 writers it was still running
// after 15 s and 700,000 files, and start waited for it, and with it the panel and the
// SSH tunnel. Nothing does now, so dockwell run serves with a writer still going, and
// the panel takes its socket once they stop.
func TestRunServesWhileSomethingFillsThePanelsSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "dwf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := datadir.Dir{Root: dir}
	if err := caddy.EnsureDirs(d.Ingress()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Ingress(), "active"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := caddy.PanelSocket(d.Ingress())
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	var wrote atomic.Int64
	quiet := make(chan struct{})
	var writers sync.WaitGroup
	for w := range 4 {
		writers.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-quiet:
					return
				default:
				}
				if os.WriteFile(filepath.Join(path, fmt.Sprintf("%d-%d", w, i)), nil, 0o600) != nil {
					_ = os.Mkdir(path, 0o700) // as Caddy would, while it can
				}
				wrote.Add(1)
			}
		})
	}
	silence := sync.OnceFunc(func() {
		close(quiet)
		writers.Wait()
	})
	t.Cleanup(silence)
	// Long enough for a loaded runner's start, and short enough that a run that waits for
	// the writers doesn't fill tmp: then they stop, and the test says start waited. This
	// outlives a failure, as stopping dockwell run waits for it too.
	var timedOut atomic.Bool
	watchdog := time.AfterFunc(30*time.Second, func() { timedOut.Store(true); silence() })
	t.Cleanup(func() { watchdog.Stop() })

	// out is only printed once the panel serves, so that is start finished.
	_, logs, stop := startRunWithLogs(t, dir, "/claim#",
		"--domain", "panel.example.com", "--https-listen", "127.0.0.1:443", "--http-listen", "127.0.0.1:80",
		"--acme-directory-url", "https://acme.invalid/directory")
	defer stop()
	at := wrote.Load()
	deadline := time.Now().Add(5 * time.Second)
	for wrote.Load() == at {
		if time.Now().After(deadline) {
			if timedOut.Load() {
				t.Fatal("start took more than 30 s while something filled the panel's socket, so it may wait for it")
			}
			t.Fatal("the writers stopped before start finished, so this proves nothing")
		}
		time.Sleep(time.Millisecond)
	}
	silence()
	// Clearing what they left takes a moment, and a try that gave up waits for the next.
	deadline = time.Now().Add(30 * time.Second)
	for {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("it never took the socket, once nothing was writing there: %v\n%s", err, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A try that doesn't end holds nothing up: starting, saying so, or stopping. A socket
// it gets after that is closed, not left open with nobody serving it.
func TestATryThatDoesntEndHoldsNothingUp(t *testing.T) {
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	release := make(chan struct{})
	got := &closeNoted{closed: make(chan struct{})}
	started := make(chan net.Listener, 1)
	go func() {
		started <- listenForIngressWhenItCan(logger, "/nowhere/panel.sock", time.Millisecond, func() (net.Listener, error) {
			<-release
			return got, nil
		})
	}()
	var l net.Listener
	select {
	case l = <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("start waited for a try that doesn't end")
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "hasn't finished") {
		if time.Now().After(deadline) {
			t.Fatalf("it doesn't say the panel's domain is waiting:\n%s", logs.String())
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopping waited for a try that doesn't end")
	}
	close(release)
	select {
	case <-got.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("what the try got after stopping was left open")
	}
}

// A first try that took long enough to warn of says when it's over, and one that
// didn't has nothing to take back.
func TestALongTryThatEndsSaysSo(t *testing.T) {
	dir, err := os.MkdirTemp("", "dwl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "panel.sock")
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	release := make(chan struct{})
	l := listenForIngressWhenItCan(logger, path, time.Millisecond, func() (net.Listener, error) {
		<-release
		return (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	})
	defer l.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "hasn't finished") {
		if time.Now().After(deadline) {
			t.Fatalf("it doesn't say it's waiting:\n%s", logs.String())
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for !strings.Contains(logs.String(), "answers on its domain again") {
		if time.Now().After(deadline) {
			t.Fatalf("it doesn't say the panel's back:\n%s", logs.String())
		}
		time.Sleep(time.Millisecond)
	}

	quiet := &syncBuffer{}
	first := filepath.Join(dir, "first.sock")
	at := listenForIngressWhenItCan(slog.New(slog.NewTextHandler(quiet, nil)), first, time.Hour, func() (net.Listener, error) {
		return (&net.ListenConfig{}).Listen(t.Context(), "unix", first)
	})
	defer at.Close()
	conn, err := dialWithin(t, first)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if got := quiet.String(); got != "" {
		t.Errorf("a socket that came at once logs:\n%s", got)
	}
}

// dialWithin dials a socket that's on its way.
func dialWithin(t *testing.T, path string) (net.Conn, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
		if err == nil || time.Now().After(deadline) {
			return conn, err
		}
		time.Sleep(time.Millisecond)
	}
}

// closeNoted is a listener that says when it's closed.
type closeNoted struct {
	net.Listener
	closed chan struct{}
}

func (c *closeNoted) Close() error {
	close(c.closed)
	return nil
}

// Once it can listen, what was waiting to serve on it does, and it says so.
func TestThePanelsSocketIsTriedAgain(t *testing.T) {
	dir, err := os.MkdirTemp("", "dwt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "panel.sock")
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	var tries atomic.Int32
	l := listenForIngressWhenItCan(logger, path, time.Millisecond, func() (net.Listener, error) {
		if tries.Add(1) < 4 {
			return nil, errors.New("something took it")
		}
		return (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	})
	defer l.Close()
	if l.Addr().String() != path {
		t.Errorf("Addr = %s", l.Addr())
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := l.Accept(); err == nil {
			accepted <- conn
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	var conn net.Conn
	for {
		if conn, err = (&net.Dialer{}).DialContext(t.Context(), "unix", path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("it never listened: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	defer conn.Close()
	select {
	case got := <-accepted:
		_ = got.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("what was waiting on it was never given the connection")
	}
	for _, want := range []string{"can't listen for what the ingress sends the panel", "something took it", "SSH tunnel", "answers on its domain again"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log doesn't say %q:\n%s", want, logs.String())
		}
	}
}

// Stopping doesn't wait for a socket that never comes.
func TestAWaitForThePanelsSocketStops(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	l := listenForIngressWhenItCan(logger, "/nowhere/panel.sock", time.Millisecond, func() (net.Listener, error) {
		return nil, errors.New("something took it")
	})
	failed := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		failed <- err
	}()
	if err := l.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	select {
	case err := <-failed:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept = %v, want it closed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept went on waiting after Close")
	}
}

// anAgent applies anything, as what is read is the store.
type anAgent struct{}

func (anAgent) Plan(_ context.Context, desired []resource.Desired) ([]resource.Step, error) {
	out := make([]resource.Step, len(desired))
	for i := range desired {
		out[i] = resource.Step{Ref: desired[i].Ref, Decision: resource.Decision{Action: resource.ActionCreate}}
	}
	return out, nil
}

func (anAgent) Apply(context.Context, resource.Step, proto.Message) error { return nil }

func (anAgent) Observe(context.Context, []resource.Ref) ([]proto.Message, error) { return nil, nil }

type anyAgent struct{}

func (anyAgent) Agent(string) (resource.Agent, bool) { return anAgent{}, true }

// A route that claims a name can't find itself under the panel's passkeys, so
// the panel can't take it, or what it is under.
func TestRunRefusesADomainARouteHolds(t *testing.T) {
	ctx := t.Context()
	logger := slog.New(slog.DiscardHandler)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := revision.Setup(ctx, db, logger); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(ctx, func(tx *store.Tx) error {
		return tx.CreateServer(ctx, store.Server{ID: "srv_A", Name: "hz-1", CreatedAt: now, CertSerial: "x", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&service.Driver{}, &route.Driver{})
	e := revision.New(db, kinds, anyAgent{}, secretstest.Keeper(db, kinds), logger)
	var tr scope.Tree
	if err := db.View(ctx, func(tx *store.Tx) (err error) {
		tr, err = scope.Load(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	env := tr.Projects[0].Environments[0].ID
	p, err := e.Propose(ctx, revision.Proposal{Summary: "Run", Actor: "usr_test", Edits: []revision.Edit{
		{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "web"}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}}},
		{Ref: resource.Ref{Kind: route.Kind, Scope: env, Name: "www"}, Spec: &resourcev1.Route{Domain: "app.panel.example.com", Service: "web", Port: 80}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(ctx, p.Change); err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"app.panel.example.com", "panel.example.com"} {
		err := refuseClaimedDomain(ctx, db, domain)
		if err == nil || !strings.Contains(err.Error(), "the route www serves app.panel.example.com") {
			t.Errorf("--domain %s = %v, want it refused, naming the route", domain, err)
		}
	}
	if err := refuseClaimedDomain(ctx, db, "other.example.com"); err != nil {
		t.Errorf("--domain other.example.com = %v", err)
	}
}

// What no route may send to is every port the control plane listens on, and those
// it takes back when the proxy goes, on whatever flags moved them.
func TestOwnPortsAreEveryListener(t *testing.T) {
	listen := func() net.Listener {
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		return l
	}
	port := func(l net.Listener) int { return l.Addr().(*net.TCPAddr).Port }
	api, agents, secure, plain := listen(), listen(), listen(), listen()
	got := ownPorts(control.Listeners{API: api, Agents: agents}, runFlags{})
	if want := []int{port(api), port(agents)}; !slices.Equal(got, want) {
		t.Errorf("with no domain, got %v, want %v", got, want)
	}
	got = ownPorts(control.Listeners{API: api, Agents: agents, HTTPS: secure, HTTP: plain}, runFlags{})
	if want := []int{port(api), port(agents), port(secure), port(plain)}; !slices.Equal(got, want) {
		t.Errorf("with its public listeners, got %v, want %v", got, want)
	}
	// With the proxy holding its ports, it has no listeners there, and they're
	// still its own.
	got = ownPorts(control.Listeners{API: api, Agents: agents}, runFlags{domain: "panel.example.com", httpsListen: ":8443", httpListen: "127.0.0.1:8080"})
	if want := []int{port(api), port(agents), 8443, 8080}; !slices.Equal(got, want) {
		t.Errorf("handed over, got %v, want %v", got, want)
	}
}
