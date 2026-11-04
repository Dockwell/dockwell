// SPDX-License-Identifier: AGPL-3.0-only

package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 3, 21, 40, 0, 0, time.UTC)

func newCA(t *testing.T) *CA {
	t.Helper()
	ca, err := NewCA("ins_01JD6Z3R8M9QF2K7T4XW5BNC0V", now)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestCARoundTrip(t *testing.T) {
	ca := newCA(t)
	keyDER, err := ca.KeyDER()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCA(ca.CertPEM(), keyDER)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Pin() != ca.Pin() || !strings.HasPrefix(ca.Pin(), PinPrefix) || len(ca.Pin()) != len(PinPrefix)+64 {
		t.Errorf("pin = %q, loaded %q", ca.Pin(), loaded.Pin())
	}
	other := newCA(t)
	otherKey, _ := other.KeyDER()
	if _, err := LoadCA(ca.CertPEM(), otherKey); err == nil {
		t.Error("LoadCA must reject a key that doesn't match the certificate")
	}
}

func TestSignServerTakesIdentityFromTheCA(t *testing.T) {
	ca := newCA(t)
	_, csr, err := NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.SignServer(csr, "srv_01JD6Z3R8M9QF2K7T4XW5BNC0V", now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ca.ServerID(cert)
	if err != nil || id != "srv_01JD6Z3R8M9QF2K7T4XW5BNC0V" {
		t.Fatalf("ServerID = %q, %v", id, err)
	}
	if got := cert.NotAfter.Sub(now); got != ServerLifetime {
		t.Errorf("lifetime = %v, want %v", got, ServerLifetime)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: ca.Pool(), CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("server certificate doesn't verify as a client certificate: %v", err)
	}
	if RenewAt(cert).After(cert.NotAfter) || !RenewAt(cert).After(now) {
		t.Errorf("RenewAt = %v", RenewAt(cert))
	}

	// Another install's CA can't vouch for this install's servers.
	otherInstall, err := NewCA("ins_other", now)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := otherInstall.SignServer(csr, "srv_x", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.ServerID(foreign); err == nil {
		t.Error("a certificate from another install must not identify a server here")
	}
}

func TestSignServerRejectsBadRequests(t *testing.T) {
	ca := newCA(t)
	if _, err := ca.SignServer([]byte("nope"), "srv_1", now); err == nil {
		t.Error("garbage CSR accepted")
	}
	_, csr, _ := NewServerKey()
	csr[len(csr)-1] ^= 1
	if _, err := ca.SignServer(csr, "srv_1", now); err == nil {
		t.Error("CSR with a broken signature accepted")
	}
}

// dialPinned runs a TLS server with cert and dials it, verifying with pin.
func dialPinned(t *testing.T, cert tls.Certificate, pin, serverName string) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true, // VerifyPinned does the verifying; that's what is tested
		VerifyConnection:   VerifyPinned(pin),
		MinVersion:         tls.VersionTLS13,
	})
	if err == nil {
		_ = conn.Close()
	}
	return err
}

func TestVerifyPinned(t *testing.T) {
	ca := newCA(t)
	serving, err := ca.IssueServing([]string{"127.0.0.1", "cp.example.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := dialPinned(t, serving, ca.Pin(), "127.0.0.1"); err != nil {
		t.Errorf("the right pin should connect: %v", err)
	}

	impostor := newCA(t)
	if err := dialPinned(t, serving, impostor.Pin(), "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "doesn't match the join ticket") {
		t.Errorf("a wrong pin = %v", err)
	}

	// A certificate for another host fails even with the right pin.
	if err := dialPinned(t, serving, ca.Pin(), "elsewhere.example.com"); err == nil {
		t.Error("a certificate for another host must not verify")
	}

	// A self-signed leaf can't pose as the CA.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	self := &x509.Certificate{SerialNumber: serial(), NotBefore: now, NotAfter: now.Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, _ := x509.CreateCertificate(rand.Reader, self, self, &key.PublicKey, key)
	parsed, _ := x509.ParseCertificate(der)
	if err := dialPinned(t, tls.Certificate{Certificate: [][]byte{der, der}, PrivateKey: key}, Pin(parsed), "127.0.0.1"); err == nil {
		t.Error("a pinned certificate that isn't a CA must be refused")
	}
}
