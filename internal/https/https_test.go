// SPDX-License-Identifier: AGPL-3.0-only

package https

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

// The ACME source is checked without a CA: what it serves, what it would
// ask a certificate for, and where it keeps its state. Issuing against a
// real CA is covered by running with --acme-directory-url against Pebble or
// Let's Encrypt's staging CA.
func TestACMESource(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "control", "acme")
	src, err := NewACME(ACMEConfig{Domain: "panel.acme.dev", CacheDir: cache, DirectoryURL: "https://acme.invalid/dir"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cache)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("cache dir: %v, mode %v, want 0700", err, info.Mode().Perm())
	}
	if src.Domain() != "panel.acme.dev" {
		t.Errorf("Domain = %q", src.Domain())
	}
	if src.manager.Client == nil || src.manager.Client.DirectoryURL != "https://acme.invalid/dir" {
		t.Error("the directory URL isn't passed to the ACME client")
	}

	cfg := src.TLSConfig()
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x", cfg.MinVersion)
	}
	if !slices.Contains(cfg.NextProtos, acme.ALPNProto) || !slices.Contains(cfg.NextProtos, "h2") {
		t.Errorf("NextProtos = %v, want h2 and %s for TLS-ALPN-01", cfg.NextProtos, acme.ALPNProto)
	}
	// Any other name is refused before the CA is ever asked.
	if _, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.test"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("a certificate for another name = %v, want the host policy to refuse it", err)
	}

	h := src.HTTPHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://evil.test/.well-known/acme-challenge/x", http.NoBody))
	if rec.Code != http.StatusForbidden {
		t.Errorf("a challenge for another host = %d, want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://panel.acme.dev/.well-known/acme-challenge/unknown", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("an unknown challenge = %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://panel.acme.dev/servers?x=1", http.NoBody))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "https://panel.acme.dev/servers?x=1" {
		t.Errorf("a page over plain HTTP = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestRedirect(t *testing.T) {
	for _, tt := range []struct {
		port, method, host, target string
		code                       int
		location                   string
	}{
		{"", http.MethodGet, "panel.acme.dev", "/a/b?c=d", http.StatusMovedPermanently, "https://panel.acme.dev/a/b?c=d"},
		{"443", http.MethodHead, "panel.acme.dev", "/", http.StatusMovedPermanently, "https://panel.acme.dev/"},
		{"8443", http.MethodGet, "panel.acme.dev", "/x", http.StatusMovedPermanently, "https://panel.acme.dev:8443/x"},
		// The Host header never picks the target.
		{"", http.MethodGet, "evil.test", "/x", http.StatusMovedPermanently, "https://panel.acme.dev/x"},
		{"", http.MethodPost, "panel.acme.dev", "/x", http.StatusBadRequest, ""},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), tt.method, "http://"+tt.host+tt.target, http.NoBody)
		Redirect("panel.acme.dev", tt.port).ServeHTTP(rec, req)
		if rec.Code != tt.code || rec.Header().Get("Location") != tt.location {
			t.Errorf("%s %s%s = %d to %q, want %d to %q", tt.method, tt.host, tt.target, rec.Code, rec.Header().Get("Location"), tt.code, tt.location)
		}
	}
}

// A request with credentials over plain HTTP is refused, not redirected:
// many clients follow the redirect with the header still on, so a script set up
// with http:// would work, sending its token in the clear on every call.
func TestRedirectRefusesCredentials(t *testing.T) {
	for _, tt := range []struct{ method, auth string }{
		{http.MethodGet, "Bearer dwt_4f9K2xQ7Lr8eVb3NcR5tYw1ZaD6gHj0kMp9sUq2XnE4x9TaBc12"},
		{http.MethodHead, "Bearer dwt_4f9K2xQ7Lr8eVb3NcR5tYw1ZaD6gHj0kMp9sUq2XnE4x9TaBc12"},
		{http.MethodGet, "Basic bWlyYTpodW50ZXIy"},
		{http.MethodPost, "Bearer anything"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), tt.method, "http://panel.acme.dev/v1/servers", http.NoBody)
		req.Header.Set("Authorization", tt.auth)
		Redirect("panel.acme.dev", "").ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("%s with Authorization %q = %d to %q, want 400 and no redirect", tt.method, tt.auth, rec.Code, rec.Header().Get("Location"))
		}
		if tt.method != http.MethodHead && (!strings.Contains(body, "https://panel.acme.dev") || !strings.Contains(body, "may have seen it") ||
			!strings.Contains(body, "revoke the token")) {
			t.Errorf("%s with Authorization %q said %q, want it to say the token may be exposed and to use https", tt.method, tt.auth, body)
		}
	}
}

// autocert keeps a private key and then the chain in one file. Moving behind the
// proxy uses them only while they serve.
func TestWhatTheCacheHoldsIsSplitIfItServes(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notBefore, notAfter := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"panel.example.com"}, NotBefore: notBefore, NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cached := append(append([]byte{}, keyPEM...), certPEM...)

	chain, gotKey, err := splitCached(cached, time.Now())
	if err != nil || !bytes.Equal(chain, certPEM) || !bytes.Equal(gotKey, keyPEM) {
		t.Errorf("splitCached = %q, %q, %v", chain, gotKey, err)
	}
	if chain, _, err := splitCached(cached, notAfter.Add(time.Minute)); err != nil || chain != nil {
		t.Errorf("an expired certificate reads %q, %v", chain, err)
	}
	if chain, _, err := splitCached(certPEM, time.Now()); err != nil || chain != nil {
		t.Errorf("a chain with no key reads %q, %v", chain, err)
	}

	// Through the cache, which reads and never asks the CA for anything.
	dir := t.TempDir()
	a, err := NewACME(ACMEConfig{Domain: "panel.example.com", CacheDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if chain, _, err := a.Current(t.Context()); err != nil || chain != nil {
		t.Errorf("with nothing cached, Current = %q, %v", chain, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "panel.example.com"), cached, 0o600); err != nil {
		t.Fatal(err)
	}
	if chain, _, err := a.Current(t.Context()); err != nil || !bytes.Equal(chain, certPEM) {
		t.Errorf("Current = %q, %v", chain, err)
	}
}
