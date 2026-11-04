// SPDX-License-Identifier: AGPL-3.0-only

// Package pki is the control plane's certificate authority (ADR 0007).
//
// The control plane signs a short-lived client certificate for every
// server; agents present it on their outbound mTLS connection. A server's
// identity is a SPIFFE-style URI in its certificate,
// spiffe://<install>/server/<id>, never its hostname. Joining servers pin
// the CA by the SHA-256 of its public key, so the first connection is
// verified without a prior copy of the CA certificate.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"
)

// Lifetimes. Servers renew at two thirds of a certificate's life.
const (
	CALifetime      = 10 * 365 * 24 * time.Hour
	ServerLifetime  = 30 * 24 * time.Hour
	ServingLifetime = 90 * 24 * time.Hour
	// clockSkew backdates certificates so a server whose clock is a little
	// behind still accepts them.
	clockSkew = 5 * time.Minute
)

// PinPrefix starts every CA pin: "sha256:<hex of the CA's public key>".
const PinPrefix = "sha256:"

// CA signs certificates for one Dockwell install.
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey
	domain  string
}

// NewCA creates a CA for the install with the given ID.
func NewCA(installID string, now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Dockwell CA " + installID, Organization: []string{"Dockwell"}},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(CALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		SubjectKeyId:          keyID(&key.PublicKey),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("can't create the CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return LoadCA(pemCert(der), keyDER)
}

// LoadCA restores a CA from its certificate (PEM) and private key (PKCS #8 DER).
func LoadCA(certPEM, keyDER []byte) (*CA, error) {
	cert, err := ParseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("can't read the CA certificate: %w", err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("can't read the CA key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("the CA key doesn't match the CA certificate")
	}
	domain, ok := strings.CutPrefix(cert.Subject.CommonName, "Dockwell CA ")
	if !ok || domain == "" {
		return nil, errors.New("the CA certificate doesn't name a Dockwell install")
	}
	return &CA{cert: cert, certPEM: certPEM, key: key, domain: strings.ToLower(domain)}, nil
}

// CertPEM returns the CA certificate.
func (ca *CA) CertPEM() []byte { return ca.certPEM }

// Certificate returns the parsed CA certificate.
func (ca *CA) Certificate() *x509.Certificate { return ca.cert }

// KeyDER returns the private key, PKCS #8 encoded, for sealing at rest.
func (ca *CA) KeyDER() ([]byte, error) { return x509.MarshalPKCS8PrivateKey(ca.key) }

// Pin identifies the CA's public key, for servers joining for the first time.
func (ca *CA) Pin() string { return Pin(ca.cert) }

// Pool returns a pool that trusts only this CA.
func (ca *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// IssueServing creates a key and certificate for the control plane's agent
// endpoint, valid for the given host names and IP addresses.
func (ca *CA) IssueServing(hosts []string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "Dockwell control plane"},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(ServingLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("can't issue the control plane's certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	// The chain includes the CA, so joining servers can check it against their pin.
	return tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

// SignServer signs a certificate request from a server's agent. The CSR
// only contributes its public key: identity comes from serverID, never from
// what the request claims.
func (ca *CA) SignServer(csrDER []byte, serverID string, now time.Time) (*x509.Certificate, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("the certificate request isn't valid: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the certificate request's signature doesn't verify: %w", err)
	}
	if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
		return nil, errors.New("the certificate request must use an ECDSA key")
	}
	id := ca.ServerURI(serverID)
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: serverID},
		URIs:         []*url.URL{id},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(ServerLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("can't sign the server's certificate: %w", err)
	}
	return x509.ParseCertificate(der)
}

// ServerURI is a server's identity in its certificate.
func (ca *CA) ServerURI(serverID string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: ca.domain, Path: "/server/" + serverID}
}

// ServerID returns the server a verified client certificate identifies.
// The certificate must already be verified against this CA.
func (ca *CA) ServerID(cert *x509.Certificate) (string, error) {
	for _, u := range cert.URIs {
		if u.Scheme == "spiffe" && u.Host == ca.domain {
			if id, ok := strings.CutPrefix(u.Path, "/server/"); ok && id != "" && !strings.Contains(id, "/") {
				return id, nil
			}
		}
	}
	return "", errors.New("the certificate doesn't identify a server of this install")
}

// KeyHash is the SHA-256 of a certificate's public key.
func KeyHash(cert *x509.Certificate) []byte {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return sum[:]
}

// Pin returns "sha256:<hex>" of a certificate's public key.
func Pin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return PinPrefix + hex.EncodeToString(sum[:])
}

// VerifyPinned returns a tls.Config.VerifyConnection function that accepts
// a server only if its chain includes a CA matching pin and the leaf is
// valid for that CA and the dialled host. It is for the first connection
// of a joining server, which has a pin but no CA certificate yet.
func VerifyPinned(pin string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("the control plane sent no certificate")
		}
		pool := x509.NewCertPool()
		var found bool
		for _, c := range cs.PeerCertificates[1:] {
			if subtle.ConstantTimeCompare([]byte(Pin(c)), []byte(pin)) == 1 && c.IsCA {
				pool.AddCert(c)
				found = true
			}
		}
		if !found {
			return errors.New("the control plane's CA doesn't match the join ticket. Check the address, and create a new ticket on the control server")
		}
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
			Roots:     pool,
			DNSName:   cs.ServerName,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			return fmt.Errorf("the control plane's certificate isn't valid: %w", err)
		}
		return nil
	}
}

// NewServerKey creates the key a server's agent keeps, and a certificate
// request for it.
func NewServerKey() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := CSR(key)
	return key, csr, err
}

// CSR makes a certificate request for an existing key, for renewal.
func CSR(key crypto.Signer) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "dockwell agent"},
	}, key)
}

// RenewAt is when a certificate should be renewed: two thirds of its life.
func RenewAt(cert *x509.Certificate) time.Time {
	life := cert.NotAfter.Sub(cert.NotBefore)
	return cert.NotBefore.Add(life * 2 / 3)
}

// ParseCertPEM parses one PEM certificate.
func ParseCertPEM(b []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// EncodeCertPEM encodes a certificate as PEM.
func EncodeCertPEM(cert *x509.Certificate) []byte { return pemCert(cert.Raw) }

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// SerialString formats a certificate's serial for storage and logs.
func SerialString(cert *x509.Certificate) string { return cert.SerialNumber.Text(16) }

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand doesn't fail
	}
	return n.Add(n, big.NewInt(1))
}

func keyID(pub *ecdsa.PublicKey) []byte {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		panic(err) // a valid ECDSA key always marshals
	}
	sum := sha256.Sum256(der)
	return sum[:20]
}
