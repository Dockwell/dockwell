// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func selfSignedPair(t *testing.T, dnsNames []string, notBefore, notAfter time.Time) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func TestCustomCertValidationAndKeep(t *testing.T) {
	d := &Driver{}
	now := time.Now()
	cert, key := selfSignedPair(t, []string{"blog.example.com"}, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	_, otherKey := selfSignedPair(t, []string{"blog.example.com"}, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	expiredCert, expiredKey := selfSignedPair(t, []string{"blog.example.com"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	wrongDomainCert, wrongDomainKey := selfSignedPair(t, []string{"other.example.com"}, now.Add(-time.Hour), now.Add(30*24*time.Hour))

	good := &resourcev1.Route{
		Domain:     "blog.example.com",
		Service:    "web",
		Port:       8080,
		CustomCert: &resourcev1.RouteCustomCert{CertPem: cert, KeyPem: key},
	}
	if err := d.Validate(good); err != nil {
		t.Fatalf("Validate(good) = %v", err)
	}
	if err := d.CheckNew(good); err != nil {
		t.Fatalf("CheckNew(good) = %v", err)
	}

	// Keep preserves the private key when an edit keeps the same cert without resending key_pem.
	edit := &resourcev1.Route{
		Domain:     "blog.example.com",
		Service:    "web",
		Port:       8080,
		CustomCert: &resourcev1.RouteCustomCert{CertPem: cert},
	}
	if err := d.Validate(edit); err != nil {
		t.Fatalf("Validate(edit without key) = %v", err)
	}
	kept, err := d.Keep(good, edit)
	if err != nil || kept.Reason != "kept_certificate_key" || edit.GetCustomCert().GetKeyPem() != strings.TrimSpace(key) {
		t.Fatalf("Keep = %v, %v", kept, err)
	}

	for _, tc := range []struct {
		name string
		r    *resourcev1.Route
		says string
	}{
		{"mismatched key", &resourcev1.Route{Domain: "blog.example.com", Service: "web", Port: 8080, CustomCert: &resourcev1.RouteCustomCert{CertPem: cert, KeyPem: otherKey}}, "don't match"},
		{"expired", &resourcev1.Route{Domain: "blog.example.com", Service: "web", Port: 8080, CustomCert: &resourcev1.RouteCustomCert{CertPem: expiredCert, KeyPem: expiredKey}}, "expired"},
		{"wrong domain", &resourcev1.Route{Domain: "blog.example.com", Service: "web", Port: 8080, CustomCert: &resourcev1.RouteCustomCert{CertPem: wrongDomainCert, KeyPem: wrongDomainKey}}, "doesn't cover"},
		{"tls off", &resourcev1.Route{Domain: "blog.example.com", Service: "web", Port: 8080, Tls: "off", CustomCert: &resourcev1.RouteCustomCert{CertPem: cert, KeyPem: key}}, "tls off"},
	} {
		if err := d.Validate(tc.r); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: Validate = %v, want %q", tc.name, err, tc.says)
		}
	}
}

func TestRouteRateLimitValidation(t *testing.T) {
	d := &Driver{}
	r := &resourcev1.Route{
		Domain:    "blog.example.com",
		Service:   "web",
		Port:      8080,
		RateLimit: &resourcev1.RouteRateLimit{RequestsPerMinute: 0, Burst: 10},
	}
	if err := d.Validate(r); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	if r.GetRateLimit().GetRequestsPerMinute() != DefaultRateLimitRPM {
		t.Errorf("default rpm = %d, want %d", r.GetRateLimit().GetRequestsPerMinute(), DefaultRateLimitRPM)
	}
	r.RateLimit.RequestsPerMinute = MaxRateLimitRPM + 1
	if err := d.Validate(r); err == nil {
		t.Errorf("expected refusal for rpm over max")
	}
}
