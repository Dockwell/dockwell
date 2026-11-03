// SPDX-License-Identifier: AGPL-3.0-only

package identity

import (
	"crypto/x509"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/ticket"
)

type fixture struct {
	a   *Authority
	db  *store.DB
	key *seal.Key
	dir string
}

func newFixture(t *testing.T) *fixture {
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
	a, err := Bootstrap(t.Context(), db, key, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{a: a, db: db, key: key, dir: dir}
}

func csr(t *testing.T) []byte {
	t.Helper()
	_, req, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func newTicket(t *testing.T, f *fixture, ttl time.Duration) *agentv1.JoinTicket {
	t.Helper()
	tk, err := f.a.CreateTicket(t.Context(), "https://203.0.113.7:7443", "cli:test", ttl)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ticket.Decode(tk.Value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestBootstrapKeepsTheSameIdentity(t *testing.T) {
	f := newFixture(t)
	again, err := Bootstrap(t.Context(), f.db, f.key, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if again.InstallID() != f.a.InstallID() || again.CA().Pin() != f.a.CA().Pin() {
		t.Fatal("a second start must load the same install and CA")
	}

	wrongKey, err := seal.LoadOrCreate(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(t.Context(), f.db, wrongKey, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "can't unseal the CA key") {
		t.Fatalf("Bootstrap with the wrong key = %v", err)
	}

	// The CA key is sealed, never stored in the clear.
	_ = f.db.View(t.Context(), func(tx *store.Tx) error {
		sealed, err := tx.GetSetting(t.Context(), settingCAKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := x509.ParsePKCS8PrivateKey(sealed); err == nil {
			t.Error("the stored CA key parses as a plain private key")
		}
		return nil
	})
}

func TestTicketRoundTrip(t *testing.T) {
	f := newFixture(t)
	tk, err := f.a.CreateTicket(t.Context(), "https://203.0.113.7:7443", "cli:test", DefaultTicketTTL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tk.Value, "dwj1.") || strings.ContainsAny(tk.Value, " \n=+/") {
		t.Errorf("ticket %q should be one copyable word", tk.Value)
	}
	got, err := ticket.Decode("  " + tk.Value + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCaPin() != f.a.CA().Pin() || got.GetControlUrl() != "https://203.0.113.7:7443" || got.GetTokenId() != tk.TokenID {
		t.Errorf("decoded ticket = %v", got)
	}

	for _, bad := range []string{"", "dwj1.", "dwj2.abc", "dwj1.!!!", tk.Value[:20]} {
		if _, err := ticket.Decode(bad); !errors.Is(err, ticket.ErrMalformed) {
			t.Errorf("Decode(%q) = %v", bad, err)
		}
	}
	if _, err := f.a.CreateTicket(t.Context(), "http://insecure:7443", "cli:test", time.Hour); err == nil {
		t.Error("a ticket must point at an https control plane")
	}
	for _, ttl := range []time.Duration{0, -time.Minute, 25 * time.Hour} {
		if _, err := f.a.CreateTicket(t.Context(), "https://x:7443", "cli:test", ttl); err == nil {
			t.Errorf("CreateTicket accepted a %v lifetime", ttl)
		}
	}
}

func TestJoinSpendsTheTicketOnce(t *testing.T) {
	f := newFixture(t)
	tk := newTicket(t, f, time.Hour)

	joined, err := f.a.Join(t.Context(), tk.GetTokenId(), tk.GetSecret(), csr(t), "HZ-2.example.com", "198.51.100.4")
	if err != nil {
		t.Fatal(err)
	}
	if joined.Server.Name != "hz-2" {
		t.Errorf("name = %q, want hz-2", joined.Server.Name)
	}
	if id, err := f.a.CA().ServerID(joined.Cert); err != nil || id != joined.Server.ID {
		t.Errorf("certificate identifies %q (%v), want %q", id, err, joined.Server.ID)
	}

	_, err = f.a.Join(t.Context(), tk.GetTokenId(), tk.GetSecret(), csr(t), "hz-3", "198.51.100.5")
	if !errors.Is(err, ErrTicketRejected) {
		t.Fatalf("a second join with the same ticket = %v", err)
	}

	_ = f.db.View(t.Context(), func(tx *store.Tx) error {
		events, _ := tx.ListEvents(t.Context(), 10)
		var actions []string
		for _, e := range events {
			actions = append(actions, e.Action)
		}
		if strings.Join(actions, ",") != "server.join,join_token.create,install.create" {
			t.Errorf("audit log = %v", actions)
		}
		return nil
	})
}

func TestJoinRejects(t *testing.T) {
	f := newFixture(t)
	tk := newTicket(t, f, time.Hour)

	wrong := append([]byte(nil), tk.GetSecret()...)
	wrong[0] ^= 1
	if _, err := f.a.Join(t.Context(), tk.GetTokenId(), wrong, csr(t), "a", "x"); !errors.Is(err, ErrTicketRejected) {
		t.Errorf("wrong secret = %v", err)
	}
	if _, err := f.a.Join(t.Context(), "jtk_nope", tk.GetSecret(), csr(t), "a", "x"); !errors.Is(err, ErrTicketRejected) {
		t.Errorf("unknown token = %v", err)
	}

	expired := newTicket(t, f, time.Minute)
	f.a.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := f.a.Join(t.Context(), expired.GetTokenId(), expired.GetSecret(), csr(t), "a", "x"); !errors.Is(err, ErrTicketRejected) {
		t.Errorf("expired ticket = %v", err)
	}
	f.a.now = time.Now

	// A bad CSR fails without spending the ticket.
	if _, err := f.a.Join(t.Context(), tk.GetTokenId(), tk.GetSecret(), []byte("garbage"), "a", "x"); err == nil || errors.Is(err, ErrTicketRejected) {
		t.Errorf("bad CSR = %v", err)
	}
	if _, err := f.a.Join(t.Context(), tk.GetTokenId(), tk.GetSecret(), csr(t), "a", "x"); err != nil {
		t.Errorf("the ticket should still work after a failed join: %v", err)
	}
}

func TestNamesStayUnique(t *testing.T) {
	f := newFixture(t)
	var names []string
	for range 3 {
		tk := newTicket(t, f, time.Hour)
		j, err := f.a.Join(t.Context(), tk.GetTokenId(), tk.GetSecret(), csr(t), "web", "x")
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, j.Server.Name)
	}
	if strings.Join(names, ",") != "web,web-2,web-3" {
		t.Errorf("names = %v", names)
	}
}

func TestRenewAndRevoke(t *testing.T) {
	f := newFixture(t)
	joined, err := f.a.EnrollLocal(t.Context(), csr(t), "hz-1")
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := f.a.Renew(t.Context(), joined.Server.ID, csr(t))
	if err != nil {
		t.Fatal(err)
	}
	if pki.SerialString(renewed) == pki.SerialString(joined.Cert) {
		t.Error("renewal must issue a new certificate")
	}
	s, err := f.a.CheckServer(t.Context(), joined.Server.ID)
	if err != nil || s.CertSerial != pki.SerialString(renewed) {
		t.Errorf("after renewal: %+v, %v", s, err)
	}

	if err := f.a.Revoke(t.Context(), joined.Server.ID, "cli:test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.CheckServer(t.Context(), joined.Server.ID); !errors.Is(err, ErrRevoked) {
		t.Errorf("revoked server = %v", err)
	}
	if _, err := f.a.Renew(t.Context(), joined.Server.ID, csr(t)); !errors.Is(err, ErrRevoked) {
		t.Errorf("a revoked server renewed: %v", err)
	}
	if _, err := f.a.CheckServer(t.Context(), "srv_nope"); !errors.Is(err, ErrUnknownServer) {
		t.Errorf("unknown server = %v", err)
	}
}

func TestServerName(t *testing.T) {
	for in, want := range map[string]string{
		"hz-1":                  "hz-1",
		"HZ-1.example.com":      "hz-1",
		"ip-10-0-0-4.ec2":       "ip-10-0-0-4",
		"web_server  01":        "web-server-01",
		"--odd--":               "odd",
		"":                      "server",
		"!!!":                   "server",
		strings.Repeat("a", 80): strings.Repeat("a", 56),
	} {
		if got := ServerName(in); got != want {
			t.Errorf("ServerName(%q) = %q, want %q", in, got, want)
		}
	}
}
