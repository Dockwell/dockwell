// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// madeCert is a certificate for names, ending at notAfter, with its key, from a CA
// of its own.
func madeCert(t *testing.T, id string, notAfter time.Time, serves []string, names ...string) DNSCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: notAfter.Add(-90 * 24 * time.Hour), NotAfter: notAfter,
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Made-up CA"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	return DNSCert{
		ID: id, Name: id, Names: names, Serves: serves,
		Chain: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), Key: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}),
	}
}

// memoryKeeper keeps what it's given, as the agent's sealed cache does.
type memoryKeeper struct {
	mu   sync.Mutex
	kept []byte
}

func (k *memoryKeeper) KeepCertificates(b []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.kept = slices.Clone(b)
	return nil
}

func (k *memoryKeeper) KeptCertificates() ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.kept), nil
}

// The certificates by DNS a server is given are kept on its disk and loaded into
// the proxy, with exactly the names they serve skipped, as the proxy starts if it
// isn't running yet; and an agent that starts again has them from its disk (ADR
// 0088).
func TestCertificatesByDNSGoIntoTheProxy(t *testing.T) {
	s := newSetup(t)
	keeper := &memoryKeeper{}
	s.Certs = keeper
	now := time.Now()
	own := madeCert(t, "srv_1", now.Add(60*24*time.Hour), []string{"app.example.com"}, "app.example.com")
	wild := madeCert(t, "crt_wild", now.Add(80*24*time.Hour), []string{"api.example.com", "shop.example.com"}, "*.example.com", "example.com")

	loaded, err := s.PutCertificates(t.Context(), []DNSCert{own, wild})
	if err != nil {
		t.Fatal(err)
	}
	if loaded != 0 || s.proxy.dnsCerts != nil {
		t.Errorf("with the proxy not running, it loaded %d: %v", loaded, s.proxy.dnsCerts)
	}
	if len(keeper.kept) == 0 {
		t.Fatal("nothing was kept on disk")
	}

	s.apply(t, "www", spec("app.example.com", "", "web", 80))
	if got := s.proxy.skip; !slices.Equal(got, []string{"api.example.com", "app.example.com", "shop.example.com"}) {
		t.Errorf("as the proxy started, it skipped %v", got)
	}
	if len(s.proxy.dnsCerts) != 2 || s.proxy.dnsCerts[0].NotAfter.IsZero() {
		t.Errorf("as the proxy started, it loaded %v", s.proxy.dnsCerts)
	}
	loaded, err = s.PutCertificates(t.Context(), []DNSCert{own})
	if err != nil || loaded != 1 || !slices.Equal(s.proxy.skip, []string{"app.example.com"}) {
		t.Errorf("with the proxy running: loaded %d, skipped %v, %v", loaded, s.proxy.skip, err)
	}

	// The agent starts again, before the control plane sends anything.
	again := newSetup(t)
	again.Certs = keeper
	again.apply(t, "www", spec("app.example.com", "", "web", 80))
	if len(again.proxy.dnsCerts) != 1 || again.proxy.dnsCerts[0].ID != "srv_1" || !slices.Equal(again.proxy.skip, []string{"app.example.com"}) {
		t.Errorf("after a restart it loaded %v, skipping %v", again.proxy.dnsCerts, again.proxy.skip)
	}

	bad := own
	bad.Key = wild.Key
	if _, err := s.PutCertificates(t.Context(), []DNSCert{bad}); err == nil || !strings.Contains(err.Error(), "doesn't load") {
		t.Errorf("a key that isn't the certificate's: %v", err)
	}
}

// A name served by DNS reads as its certificate says, and names it; from 14 days
// before its end it says the control plane hasn't renewed it; within 7, with the
// control plane away, a name whose DNS points here goes back to the proxy, and
// comes back to its certificate once the control plane renews it (ADR 0088).
func TestACertificateByDNSNearItsEndFallsBack(t *testing.T) {
	s := newSetup(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	up := true
	s.HearControlPlane(func() bool { return up })
	s.DNS = &DNS{
		Lookup: func(_ context.Context, name string) ([]netip.Addr, error) {
			if name == "app.example.com" {
				return []netip.Addr{netip.MustParseAddr("203.0.113.50")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("198.51.100.9")}, nil
		},
		Addrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("203.0.113.50")}, nil },
	}
	s.apply(t, "www", spec("app.example.com", "", "web", 80))
	s.apply(t, "away", spec("away.example.com", "", "web", 80))
	cert := madeCert(t, "srv_1", now.Add(30*24*time.Hour), []string{"app.example.com", "away.example.com"}, "app.example.com", "away.example.com")
	if _, err := s.PutCertificates(t.Context(), []DNSCert{cert}); err != nil {
		t.Fatal(err)
	}
	status := func(name string) *dockwellv1.Certificate {
		for _, d := range s.Domains() {
			if d.GetDomain() == name {
				return d.GetCertificate()
			}
		}
		t.Fatalf("no status for %s", name)
		return nil
	}

	s.watchOnce(t.Context())
	if c := status("app.example.com"); c.GetState() != dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED || c.GetByDns() != "srv_1" || c.GetMessage() != "" {
		t.Errorf("a month from its end it reads %v", c)
	}

	now = now.Add(20 * 24 * time.Hour)
	s.watchOnce(t.Context())
	if c := status("app.example.com"); !strings.HasPrefix(c.GetMessage(), "Expires in 10 days, and the control plane hasn't renewed it") {
		t.Errorf("10 days from its end it reads %q", c.GetMessage())
	}

	now = now.Add(4 * 24 * time.Hour)
	s.watchOnce(t.Context())
	if !slices.Equal(s.proxy.skip, []string{"app.example.com", "away.example.com"}) {
		t.Errorf("with the control plane there, it fell back: %v", s.proxy.skip)
	}
	up = false
	s.watchOnce(t.Context())
	if !slices.Equal(s.proxy.skip, []string{"away.example.com"}) {
		t.Errorf("with the control plane away, it skips %v, want the name whose DNS points elsewhere alone", s.proxy.skip)
	}
	select {
	case <-s.watchWakes():
	default:
		t.Error("falling back didn't have the watch read again")
	}
	s.watchOnce(t.Context())
	if c := status("app.example.com"); c.GetByDns() != "" {
		t.Errorf("a name given back to the proxy still reads as served by DNS: %v", c)
	}

	renewed := madeCert(t, "srv_1", now.Add(90*24*time.Hour), []string{"app.example.com", "away.example.com"}, "app.example.com", "away.example.com")
	if _, err := s.PutCertificates(t.Context(), []DNSCert{renewed}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.proxy.skip, []string{"app.example.com", "away.example.com"}) {
		t.Errorf("once renewed, it skips %v", s.proxy.skip)
	}
}

func TestWhatAnExpiredCertificateByDNSSays(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	st := dnsState(DNSCert{Name: "wild", NotAfter: now.Add(-time.Hour)}, now)
	if st.Phase != CertFailed || !strings.HasPrefix(certMessage("app.example.com", st, nil), "Expired on 1 November 2026, and the control plane hasn't renewed it") {
		t.Errorf("expired: %+v, %q", st, certMessage("app.example.com", st, nil))
	}
	if got := dnsState(DNSCert{NotAfter: now.Add(20 * time.Hour)}, now).Warning; !strings.HasPrefix(got, "Expires in 1 day,") {
		t.Errorf("a day left: %q", got)
	}
}
