// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// RateLimit is a per-visitor-address rate limit enforced by Caddy via the agent's
// signin.sock /ratelimit/<routeID> endpoint.
type RateLimit struct {
	RequestsPerMinute uint32
	Burst             uint32
	Dial              string
}

// RateLimitDropper is an optional capability on SignIns that frees in-memory
// rate-limit buckets when a route or its rate limit is removed.
type RateLimitDropper interface {
	DropRateLimit(id string)
}

// CertificateRead holds an active certificate's PEM chain, metadata, and optionally its
// private key PEM (when requested with step-up authorization).
type CertificateRead struct {
	CertPEM   string
	KeyPEM    string
	Issuer    string
	Subject   string
	SANs      []string
	NotBefore time.Time
	NotAfter  time.Time
	Custom    bool
	ByDNS     string
	DNSNames  []string
	DNSServes []string
}

// CaddyfileValidator is an optional Proxy capability that validates a website Caddyfile
// using Caddy's own adapter.
type CaddyfileValidator interface {
	ValidateCaddyfile(ctx context.Context, caddyfile, allowedRoot string) (valid bool, errMsg string, err error)
}

// CertificateRenewer is an optional Proxy capability that starts immediate certificate
// renewal for a domain.
type CertificateRenewer interface {
	RenewCertificate(ctx context.Context, domain string, force bool) (msg string, renewed bool, err error)
}

// CertificateReader is an optional Proxy capability that reads a domain's active PEM
// certificate chain and optionally its private key from proxy storage.
type CertificateReader interface {
	ReadCertificate(ctx context.Context, domain string, includeKey bool) (*CertificateRead, error)
}

// KeyFingerprint returns the canonical SHA-256 fingerprint of a PEM private key so
// Observe can report key identity without transmitting the private key itself.
func KeyFingerprint(key []byte) string {
	trimmed := bytes.TrimSpace(key)
	if len(trimmed) == 0 {
		return ""
	}
	sum := sha256.Sum256(trimmed)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CheckDomainName validates a domain name on the agent before any certificate
// storage lookup or renewal path uses it, rejecting empty values, path separators,
// wildcards, and invalid DNS characters.
func CheckDomainName(domain string) error {
	if domain == "" || len(domain) > 253 {
		return errors.New("a valid domain name is required")
	}
	if strings.ContainsAny(domain, "/\\:* \t\r\n") || strings.Contains(domain, "..") {
		return fmt.Errorf("%q isn't a valid domain name", domain)
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%q isn't a valid domain name", domain)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("%q isn't a valid domain name", domain)
		}
		for i := range len(l) {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return fmt.Errorf("%q isn't a valid domain name", domain)
			}
		}
	}
	return nil
}

// CertCoversName reports whether certName (exact or single-label wildcard) matches host.
func CertCoversName(certName, host string) bool {
	certName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(certName), "."))
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if certName == "" || host == "" {
		return false
	}
	if rest, ok := strings.CutPrefix(certName, "*."); ok {
		first, under, found := strings.Cut(host, ".")
		return found && first != "" && under == rest
	}
	return certName == host
}

func (m *Manager) rateLimitDial() (string, error) {
	asker, ok := m.Proxy.(SignInAsker)
	if !ok {
		return "", fmt.Errorf("this server's %s can't enforce per-address rate limits. Update the agent", m.Proxy.Name())
	}
	return asker.SignInDial(), nil
}

func (m *Manager) dropRateLimit(id string) {
	if d, ok := m.SignIns.(RateLimitDropper); ok {
		d.DropRateLimit(id)
	}
}

// customCertFor returns the custom certificate kept for routeID with its key
// fingerprint instead of the private key PEM.
func (m *Manager) customCertFor(routeID string) *resourcev1.RouteCustomCert {
	m.loadCertificates()
	b := &m.bydns
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.certs {
		if b.certs[i].Custom && b.certs[i].RouteID == routeID {
			return &resourcev1.RouteCustomCert{
				CertPem:        string(b.certs[i].Chain),
				KeyFingerprint: KeyFingerprint(b.certs[i].Key),
			}
		}
	}
	return nil
}

// putCustomCert stores or updates a route's custom certificate in sealed storage and
// loads it into the proxy. Caller holds m.mu.
func (m *Manager) putCustomCert(ctx context.Context, routeID, domain string, cc *resourcev1.RouteCustomCert) error {
	cert := DNSCert{
		ID:      "custom." + routeID,
		Name:    "custom",
		Names:   []string{domain},
		Serves:  []string{domain},
		Chain:   []byte(cc.GetCertPem()),
		Key:     []byte(cc.GetKeyPem()),
		Custom:  true,
		RouteID: routeID,
	}
	if err := readCert(&cert); err != nil {
		return err
	}
	m.loadCertificates()
	b := &m.bydns
	b.mu.Lock()
	replaced := false
	for i := range b.certs {
		if b.certs[i].Custom && b.certs[i].RouteID == routeID {
			b.certs[i] = cert
			replaced = true
			break
		}
	}
	if !replaced {
		b.certs = append(b.certs, cert)
	}
	all := slices.Clone(b.certs)
	b.mu.Unlock()
	if err := m.keepCertificates(all); err != nil {
		return err
	}
	return m.applyCertificates(ctx)
}

// dropCustomCert removes any custom certificate stored for routeID. Caller holds m.mu.
func (m *Manager) dropCustomCert(ctx context.Context, routeID string) error {
	m.loadCertificates()
	b := &m.bydns
	b.mu.Lock()
	before := len(b.certs)
	b.certs = slices.DeleteFunc(b.certs, func(c DNSCert) bool {
		return c.Custom && c.RouteID == routeID
	})
	changed := len(b.certs) != before
	all := slices.Clone(b.certs)
	b.mu.Unlock()
	if !changed {
		return nil
	}
	if err := m.keepCertificates(all); err != nil {
		return err
	}
	st, err := m.Proxy.Status(ctx)
	if err != nil || !st.Running || !Active(m.Dir) {
		return nil //nolint:nilerr // ingress isn't running yet, so clearing disk state is enough
	}
	return m.applyCertificates(ctx)
}

// ValidateCaddyfile validates a website's Caddyfile using Caddy's own adapter on this server.
func (m *Manager) ValidateCaddyfile(ctx context.Context, caddyfile, allowedRoot string) (bool, string, error) {
	v, ok := m.Proxy.(CaddyfileValidator)
	if !ok {
		return false, "", fmt.Errorf("this server's %s can't validate Caddyfile configs", m.Proxy.Name())
	}
	return v.ValidateCaddyfile(ctx, caddyfile, allowedRoot)
}

// RenewCertificate triggers immediate renewal for domain on this server's proxy.
func (m *Manager) RenewCertificate(ctx context.Context, domain string, force bool) (string, bool, error) {
	if err := CheckDomainName(domain); err != nil {
		return "", false, err
	}
	if err := m.mu.lock(ctx); err != nil {
		return "", false, err
	}
	defer m.mu.unlock()
	served := m.servedByDNS()
	if c, ok := served[domain]; ok {
		if c.Custom {
			return "", false, fmt.Errorf("%s uses an uploaded custom certificate, which can't be renewed automatically. Upload a new certificate or switch back to automatic HTTPS", domain)
		}
		return "", false, fmt.Errorf("%s uses a DNS-01 certificate (%s) renewed by the control plane", domain, c.Name)
	}
	if r, ok := m.Proxy.(CertificateRenewer); ok {
		msg, renewed, err := r.RenewCertificate(ctx, domain, force)
		if err == nil {
			m.watchNow()
		}
		return msg, renewed, err
	}
	if err := m.Proxy.Retry(ctx); err != nil {
		return "", false, err
	}
	m.watchNow()
	return "Triggered certificate retry.", false, nil
}

// ReadCertificate reads the active PEM certificate chain (and optionally its private key)
// for domain from custom/DNS-01 storage or Caddy's ACME storage. A key whose certificate
// covers the panel's domain is never returned.
func (m *Manager) ReadCertificate(ctx context.Context, domain string, includeKey bool) (*CertificateRead, error) {
	if err := CheckDomainName(domain); err != nil {
		return nil, err
	}
	if err := m.mu.lock(ctx); err != nil {
		return nil, err
	}
	defer m.mu.unlock()

	var out *CertificateRead
	served := m.servedByDNS()
	if c, ok := served[domain]; ok {
		out = &CertificateRead{
			CertPEM:   string(c.Chain),
			NotAfter:  c.NotAfter,
			Custom:    c.Custom,
			DNSNames:  slices.Clone(c.Names),
			DNSServes: slices.Clone(c.Serves),
		}
		if !c.Custom {
			out.ByDNS = c.Name
		}
		if includeKey {
			out.KeyPEM = string(c.Key)
		}
		if leaf := parseLeafPEM(c.Chain); leaf != nil {
			out.NotBefore = leaf.NotBefore
			out.NotAfter = leaf.NotAfter
			out.Issuer = cmp.Or(leaf.Issuer.CommonName, strings.Join(leaf.Issuer.Organization, ", "))
			out.Subject = cmp.Or(leaf.Subject.CommonName, firstOrEmpty(leaf.DNSNames))
			out.SANs = slices.Clone(leaf.DNSNames)
		}
	} else {
		r, ok := m.Proxy.(CertificateReader)
		if !ok {
			return nil, errors.New("this server's ingress doesn't support reading certificates")
		}
		var err error
		out, err = r.ReadCertificate(ctx, domain, includeKey)
		if err != nil {
			return nil, err
		}
	}

	if includeKey && m.Panel != nil && m.Panel.Domain != "" {
		panelDomain := strings.ToLower(strings.TrimSpace(m.Panel.Domain))
		if certCoversAny(out, domain, panelDomain) {
			out.KeyPEM = ""
			return nil, fmt.Errorf("this certificate covers the panel's domain (%s), so its private key can't be downloaded", panelDomain)
		}
	}
	return out, nil
}

func certCoversAny(read *CertificateRead, domain, target string) bool {
	if CertCoversName(domain, target) {
		return true
	}
	for _, list := range [][]string{read.SANs, read.DNSNames, read.DNSServes} {
		for _, n := range list {
			if CertCoversName(n, target) {
				return true
			}
		}
	}
	return false
}

func parseLeafPEM(chain []byte) *x509.Certificate {
	block, _ := pem.Decode(chain)
	if block == nil {
		return nil
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return leaf
}

func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
