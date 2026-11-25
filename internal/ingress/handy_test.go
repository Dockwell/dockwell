// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

type recordingSignIns struct {
	dropped []string
}

func (*recordingSignIns) Put(string, string, string, *resourcev1.RouteSignIn) error { return nil }
func (*recordingSignIns) Drop(string)                                               {}
func (*recordingSignIns) Get(string) *resourcev1.RouteSignIn                        { return nil }
func (*recordingSignIns) PutSetup(string, string, string, *resourcev1.RouteSetup) error {
	return nil
}
func (*recordingSignIns) DropSetup(string)                       {}
func (*recordingSignIns) GetSetup(string) *resourcev1.RouteSetup { return nil }
func (d *recordingSignIns) DropRateLimit(id string) {
	d.dropped = append(d.dropped, id)
}

func testCertPair(t *testing.T, dnsNames []string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(99),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

func TestReadCertificateRefusesPanelDomainKey(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := testCertPair(t, []string{"*.example.com"})
	p := &fakeProxy{status: Status{Running: true}}
	m := &Manager{
		Proxy:  p,
		Panel:  &Panel{Domain: "panel.example.com"},
		Logger: slog.New(slog.DiscardHandler),
	}
	m.bydns.loaded = true
	m.bydns.certs = []DNSCert{{
		ID:     "dns_wild",
		Name:   "wildcard",
		Names:  []string{"*.example.com"},
		Serves: []string{"app.example.com", "panel.example.com"},
		Chain:  certPEM,
		Key:    keyPEM,
	}}

	// Public cert without key is allowed and reports DNS names/serves metadata.
	pub, err := m.ReadCertificate(t.Context(), "app.example.com", false)
	if err != nil {
		t.Fatalf("ReadCertificate(includeKey=false) = %v", err)
	}
	if pub.KeyPEM != "" || pub.ByDNS != "wildcard" || !slices.Contains(pub.DNSNames, "*.example.com") {
		t.Fatalf("unexpected public read: %+v", pub)
	}

	// Private key MUST be refused because the wildcard covers m.Panel.Domain (H1).
	if _, err := m.ReadCertificate(t.Context(), "app.example.com", true); err == nil || !strings.Contains(err.Error(), "panel") {
		t.Fatalf("expected panel-domain key refusal, got %v", err)
	}
}

func TestObserveCustomCertReturnsFingerprintNotKeyAndDropsRateLimit(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := testCertPair(t, []string{"shop.example.com"})
	p := &fakeProxy{status: Status{Running: true}}
	dropper := &recordingSignIns{}
	m := &Manager{
		Proxy:   p,
		SignIns: dropper,
		Logger:  slog.New(slog.DiscardHandler),
	}
	ref := resource.Ref{Kind: "route", Scope: "env_1", Name: "site"}
	id := "dockwell." + ref.Scope + "." + ref.Name
	p.routes = []Route{{
		ID:        id,
		Domain:    "shop.example.com",
		Upstreams: []string{"127.0.0.1:8080"},
	}}
	m.bydns.loaded = true
	m.bydns.certs = []DNSCert{{
		ID:      "custom." + id,
		Names:   []string{"shop.example.com"},
		Serves:  []string{"shop.example.com"},
		Chain:   certPEM,
		Key:     keyPEM,
		Custom:  true,
		RouteID: id,
	}}

	// N3: customCertFor returns CertPem + KeyFingerprint, never KeyPem.
	obs := m.customCertFor(id)
	if obs == nil || obs.GetKeyPem() != "" || obs.GetKeyFingerprint() != KeyFingerprint(keyPEM) {
		t.Fatalf("customCertFor leaked KeyPem or missed KeyFingerprint: %+v", obs)
	}

	// M4: removing a route drops its rate-limiter buckets.
	m.dropRateLimit(id)
	if !slices.Equal(dropper.dropped, []string{id}) {
		t.Fatalf("dropped = %v, want [%s]", dropper.dropped, id)
	}
}
