// SPDX-License-Identifier: AGPL-3.0-only

package agentapi_test

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/serve"
	"github.com/dockwell/dockwell/internal/store"
)

type stack struct {
	auth *identity.Authority
	hub  *fleet.Hub
	url  string
}

// startStack runs the agent listener with short timers on a loopback port.
func startStack(t *testing.T, silence time.Duration) *stack {
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
	logger := slog.New(slog.DiscardHandler)
	auth, err := identity.Bootstrap(t.Context(), db, key, logger)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := fleet.New(t.Context(), db, logger)
	if err != nil {
		t.Fatal(err)
	}
	h := agentapi.NewHandler(auth, hub, logger, agentapi.WithStreamTiming(silence, 50*time.Millisecond))

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve.Serve(ctx, ln, h, serve.Options{
			Name: "agent listener", Logger: logger, TLS: agentapi.TLSConfig(auth, []string{"127.0.0.1"}),
			StreamGrace: 10 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("agent listener: %v", err)
		}
	})
	return &stack{auth: auth, hub: hub, url: "https://" + ln.Addr().String()}
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
	waitFor(t, s.hub, id.ServerID, "the revoked server's stream closes", func(srv *dockwellv1.Server) bool {
		return srv.GetConnection() != dockwellv1.Connection_CONNECTION_CONNECTED
	})
}

func TestASilentStreamIsClosed(t *testing.T) {
	s := startStack(t, 150*time.Millisecond)
	id := s.enrollLocal(t)
	client, err := id.Client()
	if err != nil {
		t.Fatal(err)
	}
	stream := client.Connect(t.Context())
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Hello{Hello: &agentv1.Hello{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("no welcome: %v", err)
	}
	// Then say nothing: the control plane closes the stream.
	_, err = stream.Receive()
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Errorf("after silence = %v, want DeadlineExceeded", err)
	}
}

func TestAStreamMustStartWithHello(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	client, err := id.Client()
	if err != nil {
		t.Fatal(err)
	}
	stream := client.Connect(t.Context())
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Stats{Stats: &dockwellv1.ServerStats{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("stats before a hello = %v, want InvalidArgument", err)
	}
}
