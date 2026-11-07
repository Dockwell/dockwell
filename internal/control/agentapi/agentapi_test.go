// SPDX-License-Identifier: AGPL-3.0-only

package agentapi_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/logging"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/serve"
	"github.com/dockwell/dockwell/internal/store"
)

type stack struct {
	auth   *identity.Authority
	hub    *fleet.Hub
	agents *agentapi.Agents
	url    string
	// addHost has the listener's certificate name one more host.
	addHost func(string)
}

// startStack runs the agent listener with short timers on a loopback port.
func startStack(t *testing.T, silence time.Duration) *stack {
	t.Helper()
	return startStackLogging(t, silence, slog.New(slog.DiscardHandler))
}

// startStackLogging is startStack with the control plane logging to logger.
func startStackLogging(t *testing.T, silence time.Duration, logger *slog.Logger) *stack {
	t.Helper()
	return startStackWith(t, silence, logger, true)
}

// startStackWith is startStackLogging that makes agents reachable for calls
// only withAgents. Without, streams only report.
func startStackWith(t *testing.T, silence time.Duration, logger *slog.Logger, withAgents bool) *stack {
	t.Helper()
	dir := t.TempDir()
	key, err := seal.LoadOrCreate(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	auth, err := identity.Bootstrap(t.Context(), db, key, logger)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := fleet.New(t.Context(), db, logger)
	if err != nil {
		t.Fatal(err)
	}
	agents := agentapi.NewAgents(resource.NewRegistry(&container.Driver{}, &firewall.Driver{}, &service.Driver{}), logger)
	opts := []agentapi.Option{agentapi.WithStreamTiming(silence, 50*time.Millisecond)}
	if withAgents {
		opts = append(opts, agentapi.WithAgents(agents))
	}
	h := agentapi.NewHandler(auth, hub, logger, opts...)

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	listenerTLS, addHost := agentapi.TLSConfig(auth, []string{"127.0.0.1"})
	go func() {
		done <- serve.Serve(ctx, ln, h, serve.Options{
			Name: "agent listener", Logger: logger, TLS: listenerTLS,
			StreamGrace: 10 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("agent listener: %v", err)
		}
	})
	return &stack{auth: auth, hub: hub, agents: agents, url: "https://" + ln.Addr().String(), addHost: addHost}
}

// enrollLocal makes an identity the way the control server's own agent gets one.
func (s *stack) enrollLocal(t *testing.T) *enroll.Identity {
	t.Helper()
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := s.auth.EnrollLocal(t.Context(), csr, "hz-1")
	if err != nil {
		t.Fatal(err)
	}
	return enroll.New(joined.Server.ID, joined.Server.Name, s.url, key, joined.Cert, s.auth.CA().Certificate())
}

func fakeCollector() *collect.Collector {
	return &collect.Collector{
		Root: fstest.MapFS{
			"etc/os-release": {Data: []byte("PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\n")},
			"proc/meminfo":   {Data: []byte("MemTotal: 4194304 kB\nMemAvailable: 1048576 kB\n")},
			"proc/stat":      {Data: []byte("cpu 1 0 1 8 0 0 0 0 0 0\n")},
		},
		Statfs:    func(string) (uint64, uint64, error) { return 0, 0, nil },
		Hostname:  func() (string, error) { return "hz-1", nil },
		Addresses: func() ([]net.Addr, error) { return nil, nil },
		Metadata:  collect.ClosedMetadata,
		STUN:      []string{},
	}
}

// waitFor polls the fleet until cond holds for the server.
func waitFor(t *testing.T, hub *fleet.Hub, id, what string, cond func(*dockwellv1.Server) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := hub.Get(id)
		if err == nil && cond(s) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s; last state %v (%v)", what, s, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentStreamsToTheFleet(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = agent.Run(ctx, agent.Config{Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(), Collector: fakeCollector()})
		close(done)
	}()

	waitFor(t, s.hub, id.ServerID, "the agent connects and reports", func(srv *dockwellv1.Server) bool {
		return srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED &&
			srv.GetInventory().GetOsName() == "Debian GNU/Linux 13 (trixie)" &&
			srv.GetStats().GetMemoryUsedBytes() == 3<<30
	})

	cancel()
	<-done
	waitFor(t, s.hub, id.ServerID, "the stream closes", func(srv *dockwellv1.Server) bool {
		return srv.GetConnection() == dockwellv1.Connection_CONNECTION_UNREACHABLE
	})
}

// Scripts count connected servers from the control plane's JSON log, with
// scripts/connected.awk, because the API needs a session they don't have.
// This pins what they read: the messages, and the server attribute that
// carries the ID. Renaming any of them fails here, not silently there.
func TestConnectionsCanBeCountedFromTheLog(t *testing.T) {
	logs := &syncBuffer{}
	logger, err := logging.New(logs, logging.Options{Level: "info", Format: logging.FormatJSON})
	if err != nil {
		t.Fatal(err)
	}
	s := startStackLogging(t, 5*time.Second, logger)
	own := s.enrollLocal(t)
	tk, err := s.auth.CreateTicket(t.Context(), s.url, "cli:test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := enroll.Join(t.Context(), tk.Value, "hz-2", buildinfo.Read())
	if err != nil {
		t.Fatal(err)
	}
	// Two servers, so a count that lost track of which is which shows.
	start := func(id *enroll.Identity) (stop func()) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			_ = agent.Run(ctx, agent.Config{Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(), Collector: fakeCollector()})
			close(done)
		}()
		waitFor(t, s.hub, id.ServerID, "the agent connects", func(srv *dockwellv1.Server) bool {
			return srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED
		})
		waitForLog(t, logs, "server connected", id.ServerID)
		return func() { cancel(); <-done }
	}
	stopOwn := start(own)
	defer stopOwn()
	stopJoined := start(joined)
	countConnected(t, logs, "2")

	stopJoined()
	waitForLog(t, logs, "server disconnected", joined.ServerID)
	countConnected(t, logs, "1")
}

// waitForLog waits until the log has a record with msg whose "server"
// attribute is serverID.
func waitForLog(t *testing.T, logs *syncBuffer, msg, serverID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for line := range strings.Lines(logs.String()) {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg && rec["server"] == serverID {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the control plane never logged %q with server %s, which scripts/connected.awk counts:\n%s", msg, serverID, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// countConnected runs scripts/connected.awk on the log, as the scripts do.
func countConnected(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Skip("no awk to run scripts/connected.awk with")
	}
	path := filepath.Join(t.TempDir(), "control.log")
	if err := os.WriteFile(path, []byte(logs.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), awk, "-f", filepath.Join("..", "..", "..", "scripts", "connected.awk"), path).CombinedOutput()
	if err != nil {
		t.Fatalf("scripts/connected.awk: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("scripts/connected.awk counts %s connected, want %s, from:\n%s", got, want, logs.String())
	}
}

// syncBuffer collects log lines from any goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestAJoinedServerIsListedBeforeItsAgentConnects(t *testing.T) {
	s := startStack(t, 5*time.Second)
	tk, err := s.auth.CreateTicket(t.Context(), s.url, "cli:test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, err := enroll.Join(t.Context(), tk.Value, "hz-2", buildinfo.Read())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := s.hub.Get(id.ServerID)
	if err != nil {
		t.Fatalf("the fleet doesn't know the server that just joined: %v", err)
	}
	if srv.GetName() != "hz-2" || srv.GetConnection() != dockwellv1.Connection_CONNECTION_PENDING {
		t.Errorf("got %s %v, want hz-2 waiting for its agent", srv.GetName(), srv.GetConnection())
	}
}

func TestRevokingClosesAnOpenStream(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_ = agent.Run(ctx, agent.Config{Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(), Collector: fakeCollector()})
	}()
	waitFor(t, s.hub, id.ServerID, "the agent connects", func(srv *dockwellv1.Server) bool {
		return srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED
	})

	if err := s.auth.Revoke(t.Context(), id.ServerID, "cli:test"); err != nil {
		t.Fatal(err)
	}
	// The stream closes and the server leaves the fleet, rather than
	// lingering as unreachable.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := s.hub.Get(id.ServerID)
		if errors.Is(err, fleet.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the revoked server is still in the fleet (%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestASilentStreamIsClosed(t *testing.T) {
	s := startStack(t, 150*time.Millisecond)
	id := s.enrollLocal(t)
	client, release, err := id.Client()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stream := client.Connect(t.Context())
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Hello{Hello: &agentv1.Hello{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("no welcome: %v", err)
	}
	// Then say nothing: the control plane closes the stream.
	_, err = nextCall(stream)
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Errorf("after silence = %v, want DeadlineExceeded", err)
	}
}

func TestAStreamMustSayHelloInTime(t *testing.T) {
	s := startStack(t, 150*time.Millisecond)
	id := s.enrollLocal(t)
	client, release, err := id.Client()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stream := client.Connect(t.Context())
	// Open the stream with an empty send of headers only, then say nothing.
	if err := stream.Send(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Errorf("a stream without a hello = %v, want DeadlineExceeded", err)
	}
}

func TestAnOutdatedCertificateCantConnect(t *testing.T) {
	s := startStack(t, 5*time.Second)
	old := s.enrollLocal(t)
	// Two renewals later, the first certificate is neither current nor the
	// one being replaced.
	serial := pki.SerialString(old.Cert())
	for range 2 {
		_, csr, err := pki.NewServerKey()
		if err != nil {
			t.Fatal(err)
		}
		cert, err := s.auth.Renew(t.Context(), old.ServerID, serial, csr)
		if err != nil {
			t.Fatal(err)
		}
		serial = pki.SerialString(cert)
	}
	client, release, err := old.Client()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stream := client.Connect(t.Context())
	_ = stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Hello{Hello: &agentv1.Hello{}}})
	if _, err := stream.Receive(); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("connecting with an outdated certificate = %v, want PermissionDenied", err)
	}
}

func TestRenewalsWorkWhileTheStreamIsOpen(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_ = agent.Run(ctx, agent.Config{Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(), Collector: fakeCollector()})
	}()
	waitFor(t, s.hub, id.ServerID, "the agent connects", func(srv *dockwellv1.Server) bool {
		return srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED
	})
	// Each renewal must present the certificate before it, not the one the
	// stream's connection was made with.
	for i := range 3 {
		if err := id.Renew(t.Context()); err != nil {
			t.Fatalf("renewal %d: %v", i+1, err)
		}
	}
	waitFor(t, s.hub, id.ServerID, "the stream comes back on the new certificate", func(srv *dockwellv1.Server) bool {
		return srv.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED
	})
}

func TestAStreamMustStartWithHello(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	client, release, err := id.Client()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stream := client.Connect(t.Context())
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Stats{Stats: &dockwellv1.ServerStats{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("stats before a hello = %v, want InvalidArgument", err)
	}
}

// openStream connects to the agent listener as id's agent would, says
// hello with kinds, as an agent that writes a command's values in, and
// returns the stream once the control plane welcomes it.
func openStream(t *testing.T, id *enroll.Identity, kinds ...string) *connect.BidiStreamForClient[agentv1.ConnectRequest, agentv1.ConnectResponse] {
	t.Helper()
	client, release, err := id.Client()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stream := client.Connect(ctx)
	t.Cleanup(func() { _ = stream.CloseRequest(); _ = stream.CloseResponse() })
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Hello{Hello: &agentv1.Hello{Kinds: kinds, WritesReferences: true}}}); err != nil {
		t.Fatal(err)
	}
	if msg, err := stream.Receive(); err != nil || msg.GetWelcome() == nil {
		t.Fatalf("the hello was answered with %v, %v, want a welcome", msg, err)
	}
	return stream
}

// A stream with no calls to wait for, as when the control plane reaches no
// agent over it, answers Draining at once: the agent moves to its renewed
// certificate without waiting out its drain limit.
func TestDrainingIsAnsweredWithoutCalls(t *testing.T) {
	s := startStackWith(t, 5*time.Second, slog.New(slog.DiscardHandler), false)
	stream := openStream(t, s.enrollLocal(t))
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Draining{Draining: &agentv1.Draining{}}}); err != nil {
		t.Fatal(err)
	}
	answered := make(chan *agentv1.ConnectResponse, 1)
	go func() {
		msg, _ := nextCall(stream)
		answered <- msg
	}()
	select {
	case msg := <-answered:
		if msg.GetDrained() == nil {
			t.Errorf("Draining was answered with %v, want Drained", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Draining wasn't answered; the agent would wait out its drain limit")
	}
}

// An agent from before a command could name a value is warned of as it connects,
// with what that holds back and how to fix it. One that writes a command's values in
// isn't.
func TestAnOlderAgentIsWarnedOfAsItConnects(t *testing.T) {
	const warning = "the agent is from before a command could name a value, so nothing on this server that names one is read, planned or applied until you update the agent"
	logs := &syncBuffer{}
	logger, err := logging.New(logs, logging.Options{Level: "info", Format: logging.FormatJSON})
	if err != nil {
		t.Fatal(err)
	}
	s := startStackLogging(t, 5*time.Second, logger)
	id := s.enrollLocal(t)
	records := func(msg string) (n int) {
		for line := range strings.Lines(logs.String()) {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg && rec["server"] == id.ServerID &&
				(msg != warning || rec["level"] == "WARN" && rec["agent"] == "v0.2.0") {
				n++
			}
		}
		return n
	}
	// say says hello, closes the stream once it is welcome, and waits for the
	// control plane to log the nth disconnect, which comes after whatever it logged
	// of that stream.
	say := func(hello *agentv1.Hello, nth int) {
		client, release, err := id.Client()
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stream := client.Connect(ctx)
		if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Hello{Hello: hello}}); err != nil {
			t.Fatal(err)
		}
		if msg, err := stream.Receive(); err != nil || msg.GetWelcome() == nil {
			t.Fatalf("the hello was answered with %v, %v, want a welcome", msg, err)
		}
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
		for deadline := time.Now().Add(5 * time.Second); records("server disconnected") < nth; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the control plane never logged the agent as gone:\n%s", logs.String())
			}
		}
	}
	older := buildinfo.Info{Version: "v0.2.0"}.Proto()
	say(&agentv1.Hello{Build: older}, 1)
	if n := records(warning); n != 1 {
		t.Errorf("an older agent connecting was warned of %d times, want once, with its version, as a warning:\n%s", n, logs.String())
	}
	say(&agentv1.Hello{Build: older, WritesReferences: true}, 2)
	if n := records(warning); n != 1 || records("server connected") != 2 {
		t.Errorf("an agent that writes references in was warned of too:\n%s", logs.String())
	}
}

type heardAddress struct {
	mu    sync.Mutex
	heard []*agentv1.PublicAddress
}

func (h *heardAddress) SetPublicAddress(known *agentv1.PublicAddress) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heard = append(h.heard, known)
}

func (h *heardAddress) last() *agentv1.PublicAddress {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.heard) == 0 {
		return nil
	}
	return h.heard[len(h.heard)-1]
}

// A real agent behind NAT, as on Oracle: what it finds out reaches the fleet, the
// fleet's word comes back to it over the stream, and a person setting the address
// goes the same way. Its own stream on loopback says nothing of where it is.
func TestAPublicAddressGoesRoundThroughARealStream(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/opc/v2/instance/id" || r.Header.Get("Authorization") != "Bearer Oracle" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ocid1.instance.oc1"))
	}))
	defer metadata.Close()
	collector := fakeCollector()
	collector.Metadata = metadata.URL
	// Metadata counts only where the machine's own firmware names that provider.
	if root, ok := collector.Root.(fstest.MapFS); ok {
		root["sys/class/dmi/id/chassis_asset_tag"] = &fstest.MapFile{Data: []byte("OracleCloud.com\n")}
	}
	collector.Addresses = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("10.0.0.25"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	heard := &heardAddress{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = agent.Run(ctx, agent.Config{Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(), Collector: collector, Public: heard, NoLookup: true})
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, s.hub, id.ServerID, "the metadata service says it's Oracle", func(srv *dockwellv1.Server) bool {
		return srv.GetNetwork().GetProvider() == dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE &&
			srv.GetNetwork().GetShape() == dockwellv1.NetworkShape_NETWORK_SHAPE_CLOUD_NAT
	})
	if srv, _ := s.hub.Get(id.ServerID); srv.GetNetwork().GetIpv4() != nil {
		t.Errorf("loopback or Oracle's metadata gave it an address: %v", srv.GetNetwork())
	}

	if _, err := s.hub.SetPublicAddress(t.Context(), id.ServerID, "203.0.113.50", false, fleet.Setting{Actor: "user:usr_1", Surface: "panel"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	want := &agentv1.PublicAddress{
		Addresses: []*agentv1.KnownAddress{{Address: "203.0.113.50", WayIn: true}},
		Provider:  dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE,
	}
	for !proto.Equal(heard.last(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the agent heard %v", heard.last())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A ticket can name where else to go, and a server that can't get through the
// first goes on: that's how one on the control server's own network joins when a
// public address doesn't come back in. It joins for good through the one that worked.
func TestATicketGoesOnToItsNextAddress(t *testing.T) {
	s := startStack(t, 5*time.Second)
	refused, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "https://" + refused.Addr().String()
	_ = refused.Close()

	tk, err := s.auth.CreateTicket(t.Context(), dead, "cli:test", time.Hour, s.url)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := enroll.Join(t.Context(), tk.Value, "hz-2", buildinfo.Read())
	if err != nil {
		t.Fatal(err)
	}
	if joined.ControlURL != s.url {
		t.Errorf("it joined through %s, want %s", joined.ControlURL, s.url)
	}

	// Spent, it's refused, and that's no reason to try anywhere else.
	_, err = enroll.Join(t.Context(), tk.Value, "hz-3", buildinfo.Read())
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Errorf("spending it again: %v", err)
	}

	nowhere, err := s.auth.CreateTicket(t.Context(), dead, "cli:test", time.Hour, dead)
	if err != nil {
		t.Fatal(err)
	}
	_, err = enroll.Join(t.Context(), nowhere.Value, "hz-4", buildinfo.Read())
	if err == nil || !strings.Contains(err.Error(), "can't reach the control plane at "+dead) || !strings.Contains(err.Error(), "Check that its port is open") {
		t.Errorf("with nowhere to go: %v", err)
	}
}

// A server joining checks the listener's certificate against the address it
// dials, and a public address is found after the listener starts.
func TestTheAgentListenerNamesAnAddressFoundLater(t *testing.T) {
	s := startStack(t, 5*time.Second)
	cfg, addHost := agentapi.TLSConfig(s.auth, []string{"127.0.0.1"})
	leaf := func() *x509.Certificate {
		t.Helper()
		cert, err := cfg.GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	if err := leaf().VerifyHostname("203.0.113.50"); err == nil {
		t.Fatal("it names an address it was never given")
	}
	addHost("203.0.113.50")
	got := leaf()
	if err := got.VerifyHostname("203.0.113.50"); err != nil {
		t.Errorf("after adding it: %v", err)
	}
	if err := got.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("it lost what it had: %v", err)
	}
}

// A server joins through an address found after the listener started, once its
// certificate names it, and not before: a joining server checks the name it dials.
func TestAJoinGoesThroughAnAddressTheListenerAddedLater(t *testing.T) {
	s := startStack(t, 5*time.Second)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(s.url, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	later := "https://localhost:" + port
	join := func() error {
		t.Helper()
		tk, err := s.auth.CreateTicket(t.Context(), later, "cli:test", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, err = enroll.Join(t.Context(), tk.Value, "hz-2", buildinfo.Read())
		return err
	}
	if err := join(); err == nil {
		t.Fatal("it joined through a name the certificate doesn't have")
	}
	s.addHost("localhost")
	if err := join(); err != nil {
		t.Errorf("once the certificate names it: %v", err)
	}
}
