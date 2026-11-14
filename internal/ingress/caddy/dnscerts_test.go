// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/ingress"
)

// madeCertificate is a certificate for names from a CA of its own, as the control
// plane would have from Let's Encrypt, with its key, in PEM.
func madeCertificate(t *testing.T, names ...string) (chain, key []byte, serial *big.Int) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Made-up DNS CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]}, DNSNames: names, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(80 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(leafKey)
	chain = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	return chain, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), serial
}

// servedSerial is the serial of the certificate Caddy serves for name.
func (r *realCaddy) servedSerial(t *testing.T, name string) *big.Int {
	t.Helper()
	certs := r.rawConn(t, name).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		t.Fatalf("%s served no certificate", name)
	}
	return certs[0].SerialNumber
}

// waitSerial waits until Caddy serves name with the certificate serial is, or with
// another where is says false.
func (r *realCaddy) waitSerial(t *testing.T, name string, serial *big.Int, is bool) {
	t.Helper()
	var got *big.Int
	var last error
	for range 100 {
		d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, ServerName: name, NextProtos: []string{"http/1.1"}}}
		conn, err := d.DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", r.tls))
		if err == nil {
			certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
			_ = conn.Close()
			if len(certs) > 0 {
				got = certs[0].SerialNumber
				if (got.Cmp(serial) == 0) == is {
					return
				}
			}
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s serves %v, want %v that it's %s (%v)", name, got, is, serial.Text(16), last)
}

// A certificate the control plane proved by DNS serves the names it's for, in place
// of one Caddy had already got itself, while a name it doesn't serve keeps Caddy's
// own; it stays through Caddy's restart, as Caddy keeps it in its last config; and
// taken away, the name goes back to Caddy (ADR 0088).
func TestACertificateByDNSServesItsNamesOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	web := backend(t, "web")
	put(t, c, aRoute("dockwell.env.app", "app.example.net", "", web))
	put(t, c, aRoute("dockwell.env.other", "other.example.net", "", web))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()
	own := c.servedSerial(t, "app.example.net")

	chain, key, serial := madeCertificate(t, "app.example.net")
	certs := []ingress.DNSCert{{ID: "srv_1", Name: "hz-1", Names: []string{"app.example.net"}, Serves: []string{"app.example.net"}, Chain: chain, Key: key}}
	if err := c.SetCertificates(t.Context(), certs, []string{"app.example.net"}); err != nil {
		t.Fatal(err)
	}
	c.waitSerial(t, "app.example.net", serial, true)
	if got := c.servedSerial(t, "other.example.net"); got.Cmp(serial) == 0 || got.Cmp(own) == 0 {
		t.Errorf("other.example.net serves %s, want Caddy's own", got.Text(16))
	}
	// The same again changes nothing.
	if err := c.SetCertificates(t.Context(), certs, []string{"app.example.net"}); err != nil {
		t.Fatal(err)
	}

	c.proc.stop(syscall.SIGKILL)
	if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	c.waitSerial(t, "app.example.net", serial, true)

	if err := c.SetCertificates(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	c.waitSerial(t, "app.example.net", serial, false)
}
