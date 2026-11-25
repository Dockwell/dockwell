// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/ingress"
)

const (
	// MaxCustomCertPEMBytes bounds an uploaded certificate chain or private key.
	MaxCustomCertPEMBytes = 128 << 10
	// DefaultRateLimitRPM is the default steady rate per visitor address.
	DefaultRateLimitRPM = 60
	// MaxRateLimitRPM is the highest rate limit allowed on a route.
	MaxRateLimitRPM = 60000
	// MaxRateLimitBurst is the largest burst allowed above the steady rate.
	MaxRateLimitBurst = 1000
)

func checkRateLimit(r *resourcev1.Route) error {
	rl := r.GetRateLimit()
	if rl == nil {
		return nil
	}
	if rl.GetRequestsPerMinute() == 0 {
		rl.RequestsPerMinute = DefaultRateLimitRPM
	}
	if rl.GetRequestsPerMinute() > MaxRateLimitRPM {
		return fmt.Errorf("a route's rate limit of %d requests/min is more than %d", rl.GetRequestsPerMinute(), MaxRateLimitRPM)
	}
	if rl.GetBurst() > MaxRateLimitBurst {
		return fmt.Errorf("a route's rate limit burst of %d is more than %d", rl.GetBurst(), MaxRateLimitBurst)
	}
	return nil
}

func checkCustomCertOnRoute(r *resourcev1.Route) error {
	cc := r.GetCustomCert()
	if cc == nil {
		return nil
	}
	if r.GetTls() == ingress.TLSOff {
		return fmt.Errorf("%s has tls off, so it takes no custom certificate. Turn HTTPS on first, or leave custom_cert empty", Where(r))
	}
	if r.GetProof() != "" {
		return fmt.Errorf("%s uses a custom certificate, so leave proof empty", Where(r))
	}
	certPEM := strings.TrimSpace(cc.GetCertPem())
	keyPEM := strings.TrimSpace(cc.GetKeyPem())
	if certPEM == "" {
		return errors.New("a custom certificate needs its PEM certificate chain")
	}
	if keyPEM != "" {
		if _, err := CheckCustomCert(r.GetDomain(), certPEM, keyPEM, time.Now()); err != nil {
			return err
		}
	} else {
		if _, err := CheckCustomCertOnly(r.GetDomain(), certPEM, time.Now()); err != nil {
			return err
		}
	}
	cc.CertPem = certPEM
	cc.KeyPem = keyPEM
	return nil
}

// CheckCustomCert verifies that certPEM and keyPEM are a valid matching PEM certificate
// chain and private key, that the leaf certificate is not expired at now, and that it
// covers domain.
func CheckCustomCert(domain, certPEM, keyPEM string, now time.Time) (*x509.Certificate, error) {
	certPEM = strings.TrimSpace(certPEM)
	keyPEM = strings.TrimSpace(keyPEM)
	if certPEM == "" {
		return nil, errors.New("a custom certificate needs its PEM certificate chain")
	}
	if keyPEM == "" {
		return nil, errors.New("a custom certificate needs its PEM private key")
	}
	if len(certPEM) > MaxCustomCertPEMBytes || len(keyPEM) > MaxCustomCertPEMBytes {
		return nil, errors.New("the certificate or private key is larger than 128 KiB")
	}
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("the certificate and private key don't match or aren't valid PEM: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, errors.New("the certificate PEM holds no certificate")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("can't read the leaf certificate: %w", err)
	}
	if err := checkLeafForDomain(leaf, domain, now); err != nil {
		return nil, err
	}
	return leaf, nil
}

// CheckCustomCertOnly verifies a PEM certificate chain without its key (for edits that
// keep the stored key via Keep).
func CheckCustomCertOnly(domain, certPEM string, now time.Time) (*x509.Certificate, error) {
	certPEM = strings.TrimSpace(certPEM)
	if certPEM == "" {
		return nil, errors.New("a custom certificate needs its PEM certificate chain")
	}
	if len(certPEM) > MaxCustomCertPEMBytes {
		return nil, errors.New("the certificate chain is larger than 128 KiB")
	}
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the certificate isn't a valid PEM CERTIFICATE block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("can't read the leaf certificate: %w", err)
	}
	if err := checkLeafForDomain(leaf, domain, now); err != nil {
		return nil, err
	}
	return leaf, nil
}

func checkLeafForDomain(leaf *x509.Certificate, domain string, now time.Time) error {
	if !now.Before(leaf.NotAfter) {
		return fmt.Errorf("the certificate expired on %s. Upload a valid, unexpired certificate", leaf.NotAfter.UTC().Format("2 January 2006"))
	}
	if now.Before(leaf.NotBefore) {
		return fmt.Errorf("the certificate isn't valid until %s", leaf.NotBefore.UTC().Format("2 January 2006"))
	}
	if domain != "" {
		if err := leaf.VerifyHostname(domain); err != nil {
			names := leaf.DNSNames
			if len(names) == 0 && leaf.Subject.CommonName != "" {
				names = []string{leaf.Subject.CommonName}
			}
			if len(names) > 0 {
				return fmt.Errorf("the certificate doesn't cover %s (it covers %s)", domain, strings.Join(names, ", "))
			}
			return fmt.Errorf("the certificate doesn't cover %s: %w", domain, err)
		}
	}
	return nil
}
