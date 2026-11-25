// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"cmp"
	"context"
	"crypto/x509"
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
}

// CaddyfileValidator is an optional Proxy capability that validates a website Caddyfile
// using Caddy's own adapter.
type CaddyfileValidator interface {
	ValidateCaddyfile(ctx context.Context, caddyfile, allowedRoot string) (valid bool, errMsg string, err error)
}

// CertificateRenewer is an optional Proxy capability that starts immediate certificate
// renewal for a domain.
type CertificateRenewer interface {
	RenewCertificate(ctx context.Context, domain string, force bool) (string, error)
}

// CertificateReader is an optional Proxy capability that reads a domain's active PEM
// certificate chain and optionally its private key from proxy storage.
type CertificateReader interface {
	ReadCertificate(ctx context.Context, domain string, includeKey bool) (*CertificateRead, error)
}

func (m *Manager) rateLimitDial() (string, error) {
	asker, ok := m.Proxy.(SignInAsker)
	if !ok {
		return "", fmt.Errorf("this server's %s can't enforce per-address rate limits. Update the agent", m.Proxy.Name())
	}
	return asker.SignInDial(), nil
}

// customCertFor returns the custom certificate kept for routeID, if any.
func (m *Manager) customCertFor(routeID string) *resourcev1.RouteCustomCert {
	m.loadCertificates()
	b := &m.bydns
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.certs {
		if b.certs[i].Custom && b.certs[i].RouteID == routeID {
			return &resourcev1.RouteCustomCert{
				CertPem: string(b.certs[i].Chain),
				KeyPem:  string(b.certs[i].Key),
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
func (m *Manager) RenewCertificate(ctx context.Context, domain string, force bool) (string, error) {
	if err := m.mu.lock(ctx); err != nil {
		return "", err
	}
	defer m.mu.unlock()
	served := m.servedByDNS()
	if c, ok := served[domain]; ok {
		if c.Custom {
			return "", fmt.Errorf("%s uses an uploaded custom certificate, which can't be renewed automatically. Upload a new certificate or switch back to automatic HTTPS", domain)
		}
		return "", fmt.Errorf("%s uses a DNS-01 certificate (%s) renewed by the control plane", domain, c.Name)
	}
	if r, ok := m.Proxy.(CertificateRenewer); ok {
		msg, err := r.RenewCertificate(ctx, domain, force)
		if err == nil {
			m.watchNow()
		}
		return msg, err
	}
	if err := m.Proxy.Retry(ctx); err != nil {
		return "", err
	}
	m.watchNow()
	return "Triggered certificate retry.", nil
}

// ReadCertificate reads the active PEM certificate chain (and optionally its private key)
// for domain from custom/DNS-01 storage or Caddy's ACME storage.
func (m *Manager) ReadCertificate(ctx context.Context, domain string, includeKey bool) (*CertificateRead, error) {
	if err := m.mu.lock(ctx); err != nil {
		return nil, err
	}
	defer m.mu.unlock()
	served := m.servedByDNS()
	if c, ok := served[domain]; ok {
		out := &CertificateRead{
			CertPEM:  string(c.Chain),
			NotAfter: c.NotAfter,
			Custom:   c.Custom,
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
		return out, nil
	}
	r, ok := m.Proxy.(CertificateReader)
	if !ok {
		return nil, errors.New("this server's ingress doesn't support reading certificates")
	}
	return r.ReadCertificate(ctx, domain, includeKey)
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
