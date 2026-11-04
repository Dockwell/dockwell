// SPDX-License-Identifier: AGPL-3.0-only

package https

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
