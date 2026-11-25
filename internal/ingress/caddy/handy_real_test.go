// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/agent/signin"
	"github.com/dockwell/dockwell/internal/ingress"
)

func makeTestCert(t *testing.T, domain, org string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: domain, Organization: []string{org}},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

// TestRateLimitOnRealCaddy verifies per-address rate limiting through Caddy -> signin.sock
// and drift-free route read-back.
func TestRateLimitOnRealCaddy(t *testing.T) {
	c, up := startReal(t)
	ctx := t.Context()

	// Start signin.Serve on c's signin.sock as the agent does.
	ln, err := net.Listen("unix", SignInSocket(c.Dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := &signin.Server{}
	go func() { _ = signin.Serve(ln, srv, 10*time.Second, slog.New(slog.DiscardHandler)) }()

	r := ingress.Route{
		ID:             "dockwell.env.rl",
		Domain:         "rl.example.com",
		Upstreams:      []string{up},
		Plain:          true,
		PlainForAnyone: true,
		RateLimit: &ingress.RateLimit{
			RequestsPerMinute: 60,
			Burst:             2,
			Dial:              c.SignInDial(),
		},
	}
	if err := c.PutRoute(ctx, r); err != nil {
		t.Fatal(err)
	}

	// Verify route reads back cleanly with zero drift (not Edited).
	routes, err := c.Routes(ctx)
	if err != nil || len(routes) != 1 {
		t.Fatalf("Routes = %v, %v", routes, err)
	}
	got := routes[0]
	if got.Edited || got.RateLimit == nil || got.RateLimit.RequestsPerMinute != 60 || got.RateLimit.Burst != 2 {
		t.Fatalf("read back route = %+v, rl=%+v", got, got.RateLimit)
	}

	// Capacity is 1 + burst(2) = 3 immediate requests, 4th gets 429 + Retry-After.
	client := &http.Client{Timeout: 5 * time.Second}
	hit := func() *http.Response {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", c.HTTPPort), http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "rl.example.com"
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for i := range 3 {
		res := hit()
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, res.StatusCode)
		}
	}
	res := hit()
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("4th request = %d Retry-After=%q, want 429 with Retry-After", res.StatusCode, res.Header.Get("Retry-After"))
	}
}

// TestValidateCaddyfileOnRealCaddy verifies Caddy's own /adapt validation on a live Caddy
// instance, accepting valid directives and rejecting syntax errors and breakouts.
func TestValidateCaddyfileOnRealCaddy(t *testing.T) {
	c, _ := startReal(t)
	ctx := t.Context()

	validCfg := "{\n\tadmin off\n\tauto_https off\n\tpersist_config off\n}\n:8080 {\n\troot * /srv\n\theader X-Custom \"ok\"\n\tfile_server\n}\n"
	ok, msg, err := c.ValidateCaddyfile(ctx, validCfg, "/srv")
	if err != nil || !ok || msg != "" {
		t.Fatalf("valid config: ok=%v msg=%q err=%v", ok, msg, err)
	}

	syntaxErrCfg := "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\tunknown_directive_xyz 1 2 3\n}\n"
	ok, msg, err = c.ValidateCaddyfile(ctx, syntaxErrCfg, "/srv")
	if err != nil || ok || msg == "" {
		t.Fatalf("syntax error config: ok=%v msg=%q err=%v", ok, msg, err)
	}

	escapeRootCfg := "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\troot * /etc\n\tfile_server\n}\n"
	ok, msg, err = c.ValidateCaddyfile(ctx, escapeRootCfg, "/srv")
	if err != nil || ok || !strings.Contains(msg, "/srv") {
		t.Fatalf("escaped root config: ok=%v msg=%q err=%v", ok, msg, err)
	}

	metadataCfg := "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\treverse_proxy http://169.254.169.254\n}\n"
	ok, msg, err = c.ValidateCaddyfile(ctx, metadataCfg, "/srv")
	if err != nil || ok || !strings.Contains(msg, "metadata") {
		t.Fatalf("metadata upstream config: ok=%v msg=%q err=%v", ok, msg, err)
	}
}

// TestCustomCertAndReadOnRealCaddy verifies serving an uploaded custom certificate on real Caddy,
// and TestRenewCertificateWithPebble verifies forced ACME renewal + ReadCertificate against Pebble.
func TestCustomCertAndReadOnRealCaddy(t *testing.T) {
	c, up := startReal(t)
	ctx := t.Context()

	certPEM, keyPEM := makeTestCert(t, "custom.example.com", "Example Custom CA")
	if err := c.PutRoute(ctx, ingress.Route{
		ID:        "dockwell.env.custom",
		Domain:    "custom.example.com",
		Upstreams: []string{up},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCertificates(ctx, []ingress.DNSCert{{
		ID:      "custom.dockwell.env.custom",
		Name:    "custom",
		Names:   []string{"custom.example.com"},
		Serves:  []string{"custom.example.com"},
		Chain:   []byte(certPEM),
		Key:     []byte(keyPEM),
		Custom:  true,
		RouteID: "dockwell.env.custom",
	}}, []string{"custom.example.com"}); err != nil {
		t.Fatal(err)
	}

	conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", c.HTTPSPort), &tls.Config{
		ServerName:         "custom.example.com",
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("TLS dial failed: %v", err)
	}
	defer conn.Close()
	peer := conn.ConnectionState().PeerCertificates[0]
	if peer.Subject.CommonName != "custom.example.com" || len(peer.Issuer.Organization) == 0 || peer.Issuer.Organization[0] != "Example Custom CA" {
		t.Fatalf("served cert subject=%q issuer=%v", peer.Subject.CommonName, peer.Issuer.Organization)
	}
}

func TestRenewAndReadCertificateWithPebble(t *testing.T) {
	bin := needCaddy(t)
	p := startPebble(t)
	ctx := t.Context()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(up.Close)

	dir := shortDir(t)
	uid := syscallUID()
	c := &Caddy{
		Dir:             dir,
		Runtime:         &process{bin: bin, dir: dir},
		ACMEDirectory:   p.directory,
		ACMERoots:       p.roots,
		HTTPPort:        p.httpPort,
		HTTPSPort:       p.tlsPort,
		adminUIDForTest: &uid,
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if err := c.Start(ctx, nil, false); err != nil {
		t.Fatal(err)
	}
	const domain = "renew.example.com"
	p.addRecord(t, domain)
	if err := c.PutRoute(ctx, ingress.Route{
		ID:        "dockwell.env.renew",
		Domain:    domain,
		Upstreams: []string{strings.TrimPrefix(up.URL, "http://")},
	}); err != nil {
		t.Fatal(err)
	}

	waitIssued := func(prevSerial *big.Int) *x509.Certificate {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			certs, err := c.Certificates(ctx, []string{domain})
			if err == nil && certs[domain].Phase == ingress.CertIssued {
				got, rerr := c.ReadCertificate(ctx, domain, false)
				if rerr == nil {
					block, _ := pem.Decode([]byte(got.CertPEM))
					if block != nil {
						if leaf, perr := x509.ParseCertificate(block.Bytes); perr == nil {
							if prevSerial == nil || leaf.SerialNumber.Cmp(prevSerial) != 0 {
								return leaf
							}
						}
					}
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		out, _ := os.ReadFile(dir + "/caddy.out")
		t.Fatalf("timed out waiting for issued cert (prevSerial=%v)\ncaddy.out:\n%s", prevSerial, out)
		return nil
	}

	first := waitIssued(nil)

	// ReadCertificate without key omits KeyPEM; with key includes KeyPEM.
	pubOnly, err := c.ReadCertificate(ctx, domain, false)
	if err != nil || pubOnly.CertPEM == "" || pubOnly.KeyPEM != "" || pubOnly.Issuer == "" {
		t.Fatalf("ReadCertificate(includeKey=false) = %+v, %v", pubOnly, err)
	}
	withKey, err := c.ReadCertificate(ctx, domain, true)
	if err != nil || withKey.KeyPEM == "" {
		t.Fatalf("ReadCertificate(includeKey=true) = %+v, %v", withKey, err)
	}

	// Force renewal via Pebble and verify a new serial is issued.
	if _, err := c.RenewCertificate(ctx, domain, true); err != nil {
		t.Fatalf("RenewCertificate(force=true) = %v", err)
	}
	second := waitIssued(first.SerialNumber)
	if second.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatalf("expected new certificate serial after forced renewal")
	}
}

func startReal(t *testing.T) (*Caddy, string) {
	t.Helper()
	rc := startCaddy(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(up.Close)
	return rc.Caddy, strings.TrimPrefix(up.URL, "http://")
}

func syscallUID() int {
	return os.Geteuid()
}
