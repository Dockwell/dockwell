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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

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

// Obtain gets the domain's certificate now, as a browser's first connection
// would, so a panel waiting on it needn't wait for someone to open it. It asks as
// a client that takes ECDSA does, which is the certificate Current reads.
func (a *ACME) Obtain(ctx context.Context) error {
	_, err := a.manager.GetCertificate(&tls.ClientHelloInfo{
		ServerName:        a.domain,
		CipherSuites:      []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		SignatureSchemes:  []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
		SupportedCurves:   []tls.CurveID{tls.CurveP256},
		SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12},
	})
	if err != nil {
		return fmt.Errorf("can't get a certificate for %s: %w", a.domain, err)
	}
	return ctx.Err()
}

// Current returns the certificate the cache holds for the domain, and its key,
// in PEM, or nothing if it holds none that serves. It only reads: it never asks
// the CA for one, which is for when this server's proxy takes the ports and the
// panel is no longer there to answer a challenge.
func (a *ACME) Current(ctx context.Context) (chain, key []byte, err error) {
	data, err := a.manager.Cache.Get(ctx, a.domain)
	if errors.Is(err, autocert.ErrCacheMiss) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("can't read the certificate for %s: %w", a.domain, err)
	}
	return splitCached(data, time.Now())
}

// splitCached splits what autocert caches, a private key and then a chain in PEM,
// and returns nothing for a chain that doesn't serve now.
func splitCached(data []byte, now time.Time) (chain, key []byte, err error) {
	var leaf *x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		switch {
		case strings.HasSuffix(block.Type, "PRIVATE KEY"):
			key = pem.EncodeToMemory(block)
		case block.Type == "CERTIFICATE":
			if leaf == nil {
				if leaf, err = x509.ParseCertificate(block.Bytes); err != nil {
					return nil, nil, fmt.Errorf("can't read the cached certificate: %w", err)
				}
			}
			chain = append(chain, pem.EncodeToMemory(block)...)
		}
	}
	if leaf == nil || key == nil || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, nil, nil
	}
	return chain, key, nil
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
// plain HTTP has already been exposed, so it isn't forwarded. It also
// refuses any request with an Authorization header, such as an API token:
// many clients follow a redirect with the header still on, so a script set up
// with http:// would work while sending its token in the clear on every
// call. The target is built from domain, never from the request's Host
// header.
func Redirect(domain, httpsPort string) http.Handler {
	host := domain
	if httpsPort != "" && httpsPort != "443" {
		host = net.JoinHostPort(domain, httpsPort)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			refuse(w, fmt.Sprintf("Dockwell answers only over HTTPS, and this request sent an Authorization header over plain HTTP, "+
				"so anyone on the network may have seen it. If it carried an API token, revoke the token under Profile and security "+
				"and create a new one. Then send requests to https://%s instead.", host))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			refuse(w, fmt.Sprintf("Dockwell answers only over HTTPS. Send this request to https://%s instead.", host))
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

// refuse answers a plain-HTTP request that isn't redirected.
func refuse(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = fmt.Fprintln(w, msg)
}
