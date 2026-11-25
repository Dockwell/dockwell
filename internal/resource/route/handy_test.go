// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
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

func TestCustomCertChainChecksAndObservedStripping(t *testing.T) {
	d := &Driver{}
	now := time.Now()

	// 1. Refuse CA certificate as leaf (L3).
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(10),
		Subject:               pkix.Name{CommonName: "ca.example.com"},
		DNSNames:              []string{"ca.example.com"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	caKeyDER, _ := x509.MarshalECPrivateKey(caKey)
	caCertPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	caKeyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER}))
	if _, _, err := CheckCustomCertChain("ca.example.com", caCertPEM, caKeyPEM, now); err == nil || !strings.Contains(err.Error(), "CA certificate") {
		t.Fatalf("expected CA cert refusal, got %v", err)
	}

	// 2. Refuse certificate missing serverAuth EKU (L3).
	clientKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	clientTpl := &x509.Certificate{
		SerialNumber: big.NewInt(11),
		Subject:      pkix.Name{CommonName: "client.example.com"},
		DNSNames:     []string{"client.example.com"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, _ := x509.CreateCertificate(rand.Reader, clientTpl, clientTpl, &clientKey.PublicKey, clientKey)
	clientKeyDER, _ := x509.MarshalECPrivateKey(clientKey)
	if _, _, err := CheckCustomCertChain("client.example.com",
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER})),
		now); err == nil || !strings.Contains(err.Error(), "serverAuth") {
		t.Fatalf("expected missing serverAuth refusal, got %v", err)
	}

	// 3. Refuse RSA < 2048 bits (L3).
	weakRSA, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	weakTpl := &x509.Certificate{
		SerialNumber: big.NewInt(12),
		Subject:      pkix.Name{CommonName: "weak.example.com"},
		DNSNames:     []string{"weak.example.com"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	weakDER, _ := x509.CreateCertificate(rand.Reader, weakTpl, weakTpl, &weakRSA.PublicKey, weakRSA)
	weakKeyDER := x509.MarshalPKCS1PrivateKey(weakRSA)
	if _, _, err := CheckCustomCertChain("weak.example.com",
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: weakDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: weakKeyDER})),
		now); err == nil || !strings.Contains(err.Error(), "2048") {
		t.Fatalf("expected weak RSA refusal, got %v", err)
	}

	// 4. Refuse unrelated / out-of-order intermediate certificate in chain (L3).
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(13),
		Subject:      pkix.Name{CommonName: "chain.example.com"},
		DNSNames:     []string{"chain.example.com"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTpl, caCert, &leafKey.PublicKey, caKey)
	leafKeyDER, _ := x509.MarshalECPrivateKey(leafKey)
	leafPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}))
	leafKeyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}))

	unrelatedCert, _ := selfSignedPair(t, []string{"unrelated.example.com"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	if _, _, err := CheckCustomCertChain("chain.example.com", leafPEM+"\n"+unrelatedCert, leafKeyPEM, now); err == nil || !strings.Contains(err.Error(), "isn't signed by") {
		t.Fatalf("expected unrelated intermediate refusal, got %v", err)
	}

	// 5. Valid leaf -> intermediate chain passes and returns an untrusted-root warning (L3).
	_, trustWarning, err := CheckCustomCertChain("chain.example.com", leafPEM+"\n"+caCertPEM, leafKeyPEM, now)
	if err != nil {
		t.Fatalf("valid leaf+CA chain failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(trustWarning), "browsers won't trust") {
		t.Fatalf("expected untrusted-root warning, got %q", trustWarning)
	}

	// 6. Validate populates KeyFingerprint; AsObserved strips KeyPem; Readopted restores KeyPem when fingerprint matches (N3).
	desired := &resourcev1.Route{
		Domain:     "chain.example.com",
		Service:    "web",
		Port:       8080,
		CustomCert: &resourcev1.RouteCustomCert{CertPem: leafPEM + "\n" + caCertPEM, KeyPem: leafKeyPEM},
	}
	if err := d.Validate(desired); err != nil {
		t.Fatalf("Validate(desired) = %v", err)
	}
	if desired.GetCustomCert().GetKeyFingerprint() == "" {
		t.Fatal("Validate did not populate KeyFingerprint")
	}
	obs := d.AsObserved(desired).(*resourcev1.Route)
	if obs.GetCustomCert().GetKeyPem() != "" {
		t.Fatal("AsObserved leaked KeyPem")
	}
	if obs.GetCustomCert().GetKeyFingerprint() != desired.GetCustomCert().GetKeyFingerprint() {
		t.Fatal("AsObserved lost KeyFingerprint")
	}
	readopted := d.Readopted(desired, obs).(*resourcev1.Route)
	if readopted.GetCustomCert().GetKeyPem() != desired.GetCustomCert().GetKeyPem() {
		t.Fatal("Readopted failed to restore KeyPem when KeyFingerprint matched")
	}
}
