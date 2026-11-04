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
	"github.com/dockwell/dockwell/internal/store/storetest"
	"github.com/dockwell/dockwell/internal/ticket"
)

type fixture struct {
	a   *Authority
	db  *store.DB
	key *seal.Key
	dir string
}

func newFixture(t *testing.T, backend store.Backend) *fixture {
	t.Helper()
	dir := t.TempDir()
	key, err := seal.LoadOrCreate(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	db, _ := storetest.Open(t, backend)
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
	storetest.Each(t, testBootstrapKeepsTheSameIdentity)
}

func testBootstrapKeepsTheSameIdentity(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend)
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

func TestTicketRoundTrip(t *testing.T) { storetest.Each(t, testTicketRoundTrip) }

func testTicketRoundTrip(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend)
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

func TestJoinSpendsTheTicketOnce(t *testing.T) { storetest.Each(t, testJoinSpendsTheTicketOnce) }

func testJoinSpendsTheTicketOnce(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend)
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

func TestJoinRejects(t *testing.T) { storetest.Each(t, testJoinRejects) }

func testJoinRejects(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend)
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
	if _, err := f.a.Join(t.Context(), tk.GetTokenId(), tk.GetSecret(), []byte("garbage"), "a", "x"); !errors.Is(err, ErrBadRequest) {
		t.Errorf("bad CSR = %v", err)
	}
	if _, err := f.a.Join(t.Context(), tk.GetTokenId(), tk.GetSecret(), csr(t), "a", "x"); err != nil {
		t.Errorf("the ticket should still work after a failed join: %v", err)
	}
}

func TestNamesStayUnique(t *testing.T) { storetest.Each(t, testNamesStayUnique) }

func testNamesStayUnique(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend)
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

func TestRenewRotatesKeysAndRefusesReplacedCertificates(t *testing.T) {
	storetest.Each(t, testRenewRotatesKeysAndRefusesReplacedCertificates)
}

func testRenewRotatesKeysAndRefusesReplacedCertificates(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend)
	joined, err := f.a.EnrollLocal(t.Context(), csr(t), "hz-1")
	if err != nil {
		t.Fatal(err)
	}
	first := pki.SerialString(joined.Cert)

	// The current certificate renews, with a new key.
	keyB := csr(t)
	second, err := f.a.Renew(t.Context(), joined.Server.ID, first, keyB)
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.a.CheckServer(t.Context(), joined.Server.ID)
	if err != nil || s.CertSerial != pki.SerialString(second) || s.PrevCertSerial != first {
		t.Fatalf("after renewal: %+v, %v", s, err)
	}

	// The same key again is refused: renewal must rotate.
	if _, err := f.a.Renew(t.Context(), joined.Server.ID, pki.SerialString(second), keyB); !errors.Is(err, ErrBadRequest) {
		t.Errorf("renewing with the same key = %v", err)
	}

	// A lost answer: the agent retries with its old certificate and the key
	// it already sent. That works, and changes nothing else.
	retried, err := f.a.Renew(t.Context(), joined.Server.ID, first, keyB)
	if err != nil {
		t.Fatalf("retry after a lost answer: %v", err)
	}

	// Someone with a copy of the first key can't renew with it.
	if _, err := f.a.Renew(t.Context(), joined.Server.ID, first, csr(t)); !errors.Is(err, ErrStaleCertificate) {
		t.Errorf("renewing a replaced certificate with another key = %v", err)
	}
	// And once the server renews again, the old certificate is out entirely.
	if _, err := f.a.Renew(t.Context(), joined.Server.ID, pki.SerialString(retried), csr(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.Renew(t.Context(), joined.Server.ID, first, keyB); !errors.Is(err, ErrStaleCertificate) {
		t.Errorf("a certificate two renewals old = %v", err)
	}

	_ = f.db.View(t.Context(), func(tx *store.Tx) error {
		events, _ := tx.ListEvents(t.Context(), 20)
		var rejected int
		for _, e := range events {
			if e.Action == "server.renew_rejected" {
				rejected++
			}
		}
		if rejected != 2 {
			t.Errorf("%d rejected renewals audited, want 2", rejected)
		}
		return nil
	})
}

func TestRevokedServersCantRenew(t *testing.T) { storetest.Each(t, testRevokedServersCantRenew) }

func testRevokedServersCantRenew(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend)
	joined, err := f.a.EnrollLocal(t.Context(), csr(t), "hz-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.a.Revoke(t.Context(), joined.Server.ID, "cli:test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.CheckServer(t.Context(), joined.Server.ID); !errors.Is(err, ErrRevoked) {
		t.Errorf("revoked server = %v", err)
	}
	if _, err := f.a.Renew(t.Context(), joined.Server.ID, pki.SerialString(joined.Cert), csr(t)); !errors.Is(err, ErrRevoked) {
		t.Errorf("a revoked server renewed: %v", err)
	}
	if _, err := f.a.CheckServer(t.Context(), "srv_nope"); !errors.Is(err, ErrUnknownServer) {
		t.Errorf("unknown server = %v", err)
	}
}

func TestOpenNeverCreatesAnInstall(t *testing.T) { storetest.Each(t, testOpenNeverCreatesAnInstall) }

func testOpenNeverCreatesAnInstall(t *testing.T, backend store.Backend) {
	key, err := seal.LoadOrCreate(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	db, _ := storetest.Open(t, backend)
	if _, err := Open(t.Context(), db, key, slog.New(slog.DiscardHandler)); !errors.Is(err, ErrNoInstall) {
		t.Fatalf("Open on an empty database = %v", err)
	}
	if _, err := Bootstrap(t.Context(), db, key, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), db, key, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("Open after Bootstrap = %v", err)
	}
}

func TestTicketLifetimeErrorReadsWell(t *testing.T) {
	storetest.Each(t, testTicketLifetimeErrorReadsWell)
}

func testTicketLifetimeErrorReadsWell(t *testing.T, backend store.Backend) {
	f := newFixture(t, backend)
	_, err := f.a.CreateTicket(t.Context(), "https://x:7443", "cli:test", 25*time.Hour)
	if err == nil || err.Error() != "a join ticket can last from 1 s to 24 h, not 25 h" {
		t.Errorf("error = %v", err)
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
