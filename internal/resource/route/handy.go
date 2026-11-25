// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
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
	// minCustomCertRSABits is the minimum RSA key strength accepted for uploaded certificates.
	minCustomCertRSABits = 2048
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
		if _, _, err := CheckCustomCertChain(r.GetDomain(), certPEM, keyPEM, time.Now()); err != nil {
			return err
		}
		cc.KeyFingerprint = ingress.KeyFingerprint([]byte(keyPEM))
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
// chain and private key, that the leaf certificate is not expired at now, covers domain,
// is not a CA, carries serverAuth EKU, uses a strong key, and has a properly linked chain.
func CheckCustomCert(domain, certPEM, keyPEM string, now time.Time) (*x509.Certificate, error) {
	leaf, _, err := CheckCustomCertChain(domain, certPEM, keyPEM, now)
	return leaf, err
}

// CheckCustomCertChain validates certPEM and keyPEM and also verifies the certificate chain
// against the system roots. When system-root verification fails (for example, a self-signed
// or private-CA certificate), it still returns err == nil along with a plain-language
// trustWarning for the plan note.
func CheckCustomCertChain(domain, certPEM, keyPEM string, now time.Time) (leaf *x509.Certificate, trustWarning string, err error) {
	certPEM = strings.TrimSpace(certPEM)
	keyPEM = strings.TrimSpace(keyPEM)
	if certPEM == "" {
		return nil, "", errors.New("a custom certificate needs its PEM certificate chain")
	}
	if keyPEM == "" {
		return nil, "", errors.New("a custom certificate needs its PEM private key")
	}
	if len(certPEM) > MaxCustomCertPEMBytes || len(keyPEM) > MaxCustomCertPEMBytes {
		return nil, "", errors.New("the certificate or private key is larger than 128 KiB")
	}
	chain, err := parseCertChainPEM(certPEM)
	if err != nil {
		return nil, "", err
	}
	if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
		return nil, "", fmt.Errorf("the certificate and private key don't match or aren't valid PEM: %w", err)
	}
	leaf = chain[0]
	if err := checkLeafForDomain(leaf, domain, now); err != nil {
		return nil, "", err
	}
	if err := checkChainOrder(chain); err != nil {
		return nil, "", err
	}
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	opts := x509.VerifyOptions{
		DNSName:       domain,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, verr := leaf.Verify(opts); verr != nil {
		trustWarning = fmt.Sprintf("Browsers won't trust this certificate: %s.", verr.Error())
	}
	return leaf, trustWarning, nil
}

// CheckCustomCertOnly verifies a PEM certificate chain without its key (for edits that
// keep the stored key via Keep, or for Plan comparisons where only the key fingerprint travels).
func CheckCustomCertOnly(domain, certPEM string, now time.Time) (*x509.Certificate, error) {
	certPEM = strings.TrimSpace(certPEM)
	if certPEM == "" {
		return nil, errors.New("a custom certificate needs its PEM certificate chain")
	}
	if len(certPEM) > MaxCustomCertPEMBytes {
		return nil, errors.New("the certificate chain is larger than 128 KiB")
	}
	chain, err := parseCertChainPEM(certPEM)
	if err != nil {
		return nil, err
	}
	if err := checkLeafForDomain(chain[0], domain, now); err != nil {
		return nil, err
	}
	if err := checkChainOrder(chain); err != nil {
		return nil, err
	}
	return chain[0], nil
}

func parseCertChainPEM(certPEM string) ([]*x509.Certificate, error) {
	rest := []byte(certPEM)
	var chain []*x509.Certificate
	for strings.TrimSpace(string(rest)) != "" {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("the certificate chain contains invalid PEM data")
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the certificate chain contains a %q block instead of CERTIFICATE", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("can't read certificate %d in the chain: %w", len(chain)+1, err)
		}
		chain = append(chain, cert)
		if len(chain) > 16 {
			return nil, errors.New("the certificate chain has more than 16 certificates")
		}
	}
	if len(chain) == 0 {
		return nil, errors.New("the certificate PEM holds no certificate")
	}
	return chain, nil
}

func checkChainOrder(chain []*x509.Certificate) error {
	for i := range len(chain) - 1 {
		if err := chain[i].CheckSignatureFrom(chain[i+1]); err != nil {
			return fmt.Errorf("certificate %d in the chain isn't signed by certificate %d (%w). Put the leaf certificate first, followed by its signing intermediates in order", i+1, i+2, err)
		}
	}
	return nil
}

func checkLeafForDomain(leaf *x509.Certificate, domain string, now time.Time) error {
	if leaf.IsCA {
		return errors.New("a CA certificate can't be used as a server certificate. Put the leaf certificate for the domain first, followed by any intermediates")
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return errors.New("the certificate lacks the TLS Web Server Authentication (serverAuth) extended key usage")
	}
	if err := checkPublicKey(leaf.PublicKey); err != nil {
		return err
	}
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

func checkPublicKey(pub any) error {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() < minCustomCertRSABits {
			return fmt.Errorf("RSA key is %d bits, under the %d-bit minimum. Generate at least a 2048-bit RSA, ECDSA or Ed25519 key", k.N.BitLen(), minCustomCertRSABits)
		}
		return nil
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
			return nil
		default:
			return fmt.Errorf("ECDSA curve %s isn't supported for TLS certificates. Use P-256, P-384 or P-521", k.Curve.Params().Name)
		}
	case ed25519.PublicKey:
		return nil
	default:
		return fmt.Errorf("unsupported certificate public key type %T. Use RSA (2048+ bits), ECDSA (P-256/P-384/P-521) or Ed25519", pub)
	}
}
