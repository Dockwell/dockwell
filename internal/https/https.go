// SPDX-License-Identifier: AGPL-3.0-only

// Package https gives the panel a public address: a domain, and
// certificates to serve it over HTTPS.
//
// A Source supplies both. ACME, with certificates from Let's Encrypt, is the
// one built in. The Dockwell hostname service, once it exists, will be
// another Source: a name under a Dockwell-run domain, with its certificate.
// The control plane doesn't care which it has (ADR 0011).
package https

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// Source provides the panel's domain and the certificates to serve it with.
type Source interface {
	// Domain is the hostname the panel answers on over HTTPS, such as
	// "panel.acme.dev".
	Domain() string
	// TLSConfig configures the HTTPS listener.
	TLSConfig() *tls.Config
	// HTTPHandler serves the plain-HTTP listener: certificate challenges,
	// and a redirect to HTTPS for everything else. It never serves the
	// panel or the API.
	HTTPHandler() http.Handler
}

// ACMEConfig configures certificates from an ACME CA.
type ACMEConfig struct {
	// Domain is the panel's hostname.
	Domain string
	// CacheDir keeps the ACME account key and the certificates, owner-only.
	CacheDir string
	// DirectoryURL is the CA's ACME directory. Empty means Let's Encrypt.
	DirectoryURL string
	// HTTPSPort is the port the HTTPS listener is on, for redirects. Empty
	// or "443" leaves it out of the URL.
	HTTPSPort string
}

// ACME gets and renews certificates for one domain from an ACME CA, Let's
// Encrypt by default. It answers TLS-ALPN-01 challenges on the HTTPS
// listener and HTTP-01 challenges on the plain-HTTP one, and refuses to ask
// for a certificate for any other name.
type ACME struct {
	domain    string
	httpsPort string
	manager   *autocert.Manager
}

// NewACME returns an ACME source. It makes the cache directory if needed.
func NewACME(cfg ACMEConfig) (*ACME, error) {
	if cfg.Domain == "" || cfg.CacheDir == "" {
		return nil, errors.New("https: ACMEConfig needs Domain and CacheDir")
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("can't create %s for certificates: %w", cfg.CacheDir, err)
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(cfg.CacheDir),
		HostPolicy: autocert.HostWhitelist(cfg.Domain),
	}
	if cfg.DirectoryURL != "" {
		m.Client = &acme.Client{DirectoryURL: cfg.DirectoryURL}
	}
	return &ACME{domain: cfg.Domain, httpsPort: cfg.HTTPSPort, manager: m}, nil
}

// Domain is the panel's hostname.
func (a *ACME) Domain() string { return a.domain }

// TLSConfig serves the certificate for the domain, getting it on the first
// connection and renewing it 30 days before it expires, and answers
// TLS-ALPN-01 challenges.
func (a *ACME) TLSConfig() *tls.Config {
	c := a.manager.TLSConfig()
	c.MinVersion = tls.VersionTLS12
	return c
}

// HTTPHandler answers HTTP-01 challenges and redirects the rest to HTTPS.
func (a *ACME) HTTPHandler() http.Handler {
	return a.manager.HTTPHandler(Redirect(a.domain, a.httpsPort))
}

// Static serves one certificate it is given, for tests and for operators
// who bring their own.
type Static struct {
	Name      string
	Cert      tls.Certificate
	HTTPSPort string
}

// Domain is the panel's hostname.
func (s *Static) Domain() string { return s.Name }

// TLSConfig serves the certificate.
func (s *Static) TLSConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{s.Cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
}

// HTTPHandler redirects to HTTPS.
func (s *Static) HTTPHandler() http.Handler { return Redirect(s.Name, s.HTTPSPort) }

// Redirect sends GET and HEAD requests to the same path on
// https://domain, and refuses anything else: a request body sent over
// plain HTTP has already been exposed, so it isn't forwarded. The target is
// built from domain, never from the request's Host header.
func Redirect(domain, httpsPort string) http.Handler {
	host := domain
	if httpsPort != "" && httpsPort != "443" {
		host = net.JoinHostPort(domain, httpsPort)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, "Dockwell answers only over HTTPS. Send this request to https://%s instead.\n", host)
			return
		}
		target := "https://" + host + r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		if !strings.HasPrefix(r.URL.EscapedPath(), "/") {
			target = "https://" + host + "/"
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently) //nolint:gosec // the host is the configured domain
	})
}
