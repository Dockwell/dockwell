// SPDX-License-Identifier: AGPL-3.0-only

package enroll_test

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/ticket"
)

type controlPlane struct {
	auth *identity.Authority
	url  string
}

// startControlPlane runs a real control plane with an agent listener on a
// loopback port.
func startControlPlane(t *testing.T) *controlPlane {
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
	srv, err := control.New(control.Config{Logger: logger, Identity: auth, AgentHosts: []string{"127.0.0.1"}, Panel: fstest.MapFS{}})
	if err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ServeAgents(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("agent listener: %v", err)
		}
	})
	return &controlPlane{auth: auth, url: "https://" + ln.Addr().String()}
}

func (cp *controlPlane) ticket(t *testing.T) string {
	t.Helper()
	tk, err := cp.auth.CreateTicket(t.Context(), cp.url, "cli:test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tk.Value
}

func TestJoinSaveLoadRenew(t *testing.T) {
	cp := startControlPlane(t)
	id, err := enroll.Join(t.Context(), cp.ticket(t), "hz-2.example.com", buildinfo.Read())
	if err != nil {
		t.Fatal(err)
	}
	if id.ServerName != "hz-2" || !strings.HasPrefix(id.ServerID, "srv_") {
		t.Errorf("joined as %s (%s)", id.ServerName, id.ServerID)
	}

	dir := filepath.Join(t.TempDir(), "agent")
	if err := id.Save(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "server.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("server.key mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	loaded, err := enroll.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ServerID != id.ServerID || !loaded.Key.Equal(id.Key) || pki.SerialString(loaded.Cert) != pki.SerialString(id.Cert) {
		t.Fatal("Load didn't return what Save wrote")
	}

	before := pki.SerialString(loaded.Cert)
	if err := loaded.Renew(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pki.SerialString(loaded.Cert) == before {
		t.Error("Renew kept the old certificate")
	}
	if loaded.NeedsRenewal(time.Now()) {
		t.Error("a fresh certificate shouldn't need renewal")
	}
	if !loaded.NeedsRenewal(time.Now().Add(pki.ServerLifetime)) {
		t.Error("a certificate at the end of its life needs renewal")
	}

	// A removed server can no longer renew.
	if err := cp.auth.Revoke(t.Context(), id.ServerID, "cli:test"); err != nil {
		t.Fatal(err)
	}
	err = loaded.Renew(t.Context())
	if connect.CodeOf(errors.Unwrap(err)) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "removed from Dockwell") {
		t.Errorf("renewing a revoked server = %v", err)
	}
}

func TestJoinRefusesASpentTicket(t *testing.T) {
	cp := startControlPlane(t)
	tk := cp.ticket(t)
	if _, err := enroll.Join(t.Context(), tk, "a", buildinfo.Read()); err != nil {
		t.Fatal(err)
	}
	_, err := enroll.Join(t.Context(), tk, "b", buildinfo.Read())
	if err == nil || !strings.Contains(err.Error(), "invalid, already used or expired") {
		t.Errorf("second join = %v", err)
	}
}

func TestJoinRefusesAnImpostor(t *testing.T) {
	cp := startControlPlane(t)
	other := startControlPlane(t)
	// A ticket that pins another install's CA must not trust this one.
	tk, err := ticket.Decode(other.ticket(t))
	if err != nil {
		t.Fatal(err)
	}
	tk.ControlUrl = cp.url
	_, err = enroll.Join(t.Context(), ticket.Encode(tk), "a", buildinfo.Read())
	if err == nil || !strings.Contains(err.Error(), "doesn't match the join ticket") {
		t.Errorf("joining a control plane the ticket doesn't pin = %v", err)
	}
}

func TestMethodsNeedAServerCertificate(t *testing.T) {
	cp := startControlPlane(t)
	// Connect with only the CA trusted, and no client certificate.
	var p http.Protocols
	p.SetHTTP2(true)
	client := agentv1connect.NewAgentServiceClient(&http.Client{Transport: &http.Transport{
		Protocols:       &p,
		TLSClientConfig: &tls.Config{RootCAs: cp.auth.CA().Pool(), MinVersion: tls.VersionTLS13, ServerName: "127.0.0.1"},
	}}, cp.url, connect.WithGRPC())
	_, err := client.RenewCertificate(t.Context(), connect.NewRequest(&agentv1.RenewCertificateRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("RenewCertificate without a certificate = %v, want Unauthenticated", err)
	}
}

func TestLoadWithoutJoining(t *testing.T) {
	if _, err := enroll.Load(t.TempDir()); !errors.Is(err, enroll.ErrNotJoined) {
		t.Errorf("Load on an empty directory = %v", err)
	}
}
