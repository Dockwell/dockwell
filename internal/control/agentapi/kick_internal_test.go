// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

// A stream of an agent's other than its main one, a terminal's, a forward's or a
// transfer's, ends at once as its server is removed, refused as the main one is;
// a kick of a server still in the fleet ends nothing (ADR 0090).
func TestARemovedServersOtherStreamsEndAtOnce(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.DiscardHandler)
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
	_, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := auth.EnrollLocal(t.Context(), csr, "hz-2")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{auth: auth, fleet: hub, agents: NewAgents(nil, logger), logger: logger}
	peer := Peer{ServerID: joined.Server.ID, Serial: joined.Server.CertSerial}

	run := func() (release chan struct{}, ended chan error) {
		release, ended = make(chan struct{}), make(chan error, 1)
		go func() { ended <- h.whileIn(t.Context(), peer, func() error { <-release; return nil }) }()
		return release, ended
	}
	// Kicked while still in the fleet, as a check finds it is, the stream goes on.
	release, ended := run()
	time.Sleep(20 * time.Millisecond)
	h.agents.Kick(peer.ServerID)
	select {
	case err := <-ended:
		t.Fatalf("a kick of a server in the fleet ended its stream: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-ended; err != nil {
		t.Fatalf("the stream's own end: %v", err)
	}

	// Removed, it ends at once, whatever the stream was doing.
	h.agents.kicks = nil
	release, ended = run()
	defer close(release)
	time.Sleep(20 * time.Millisecond)
	if err := db.Update(t.Context(), func(tx *store.Tx) error { return tx.RevokeServer(t.Context(), peer.ServerID, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	h.agents.Kick(peer.ServerID)
	select {
	case err := <-ended:
		var cerr *connect.Error
		if !errors.As(err, &cerr) || cerr.Meta().Get(ReasonKey) != ReasonServerRemoved {
			t.Errorf("a removed server's stream ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a removed server's stream didn't end")
	}
}
