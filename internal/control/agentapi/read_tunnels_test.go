// SPDX-License-Identifier: AGPL-3.0-only

package agentapi_test

import (
	"context"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/tunnels"
	"github.com/dockwell/dockwell/internal/buildinfo"
)

// A server's tunnels reach the control plane twice: in its inventory as its agent
// connects, and as the agent reads them again when asked, with the connector ID of
// cloudflared's run now, which changes as it restarts (ADR 0104).
func TestReadTunnelsAsksTheAgentNow(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	const tunnelID = "327d9d41-b009-413d-8494-7eeeefb3ed2a"
	const secret = "the-secret-stays-on-the-server"
	token := base64.StdEncoding.EncodeToString([]byte(`{"a":"274faa1d111af716a33922b34ee5381d","t":"` + tunnelID + `","s":"` + secret + `"}`))
	proc := fstest.MapFS{
		"self/ns/net": {Data: []byte("net:[1]"), Mode: fs.ModeSymlink},
		"100/stat":    {Data: []byte("100 (cloudflared) S 1 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 9 0 500 1000 100")},
		"100/cmdline": {Data: []byte("cloudflared\x00tunnel\x00run\x00--token\x00" + token + "\x00")},
		"100/ns/net":  {Data: []byte("net:[1]"), Mode: fs.ModeSymlink},
	}
	var run atomic.Int32
	run.Store(1)
	finder := &tunnels.Finder{Proc: proc, ReadyIn: func(context.Context, int, string) (tunnels.Ready, error) {
		return tunnels.Ready{Connections: 4, ConnectorID: "connector-run-" + strings.Repeat("i", int(run.Load()))}, nil
	}}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = agent.Run(ctx, agent.Config{Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(), Collector: fakeCollector(), Tunnels: finder})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, s.hub, id.ServerID, "the agent reports its tunnel", func(srv *dockwellv1.Server) bool {
		tun := srv.GetInventory().GetTunnels()
		return len(tun) == 1 && tun[0].GetTunnelId() == tunnelID && tun[0].GetConnectorId() == "connector-run-i"
	})
	// The session the inventory came on may be a reconnect's, still saying hello
	// under load, so it's waited for as the inventory is.
	deadline := time.Now().Add(5 * time.Second)
	session, ok := s.agents.Session(id.ServerID)
	for (!ok || !session.ReadsTunnels()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		session, ok = s.agents.Session(id.ServerID)
	}
	if !ok || !session.ReadsTunnels() {
		t.Fatal("the agent should say it reads tunnels")
	}
	// cloudflared restarts: the inventory has the old run, and a read the new one.
	run.Store(2)
	got, err := session.ReadTunnels(t.Context())
	if err != nil || len(got) != 1 || got[0].GetConnectorId() != "connector-run-ii" || got[0].GetReaches() != tunnels.ReachesFromHost {
		t.Fatalf("read again: %v %v", got, err)
	}
	if strings.Contains(got[0].String(), secret) {
		t.Fatal("the tunnel's secret left the server")
	}
}

func TestReadTunnelsOfAnAgentThatDoesnt(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = agent.Run(ctx, agent.Config{Logger: slog.New(slog.DiscardHandler), Dir: t.TempDir(), Identity: id, Build: buildinfo.Read(), Collector: fakeCollector()})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitForAgent(t, s.agents, id.ServerID, true)
	session, ok := s.agents.Session(id.ServerID)
	if !ok {
		t.Fatal("no session")
	}
	if _, err := session.ReadTunnels(t.Context()); err == nil || !strings.Contains(err.Error(), "Update its agent") {
		t.Fatalf("an agent that doesn't read tunnels: %v", err)
	}
}
