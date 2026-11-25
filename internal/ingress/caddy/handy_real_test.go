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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/agent/signin"
	"github.com/dockwell/dockwell/internal/ingress"
)

type memACMEAccountKeeper struct {
	data  []byte
	saves int
}

func (k *memACMEAccountKeeper) KeepACMEAccounts(plain []byte) error {
	k.data = append([]byte(nil), plain...)
	k.saves++
	return nil
}

func (k *memACMEAccountKeeper) KeptACMEAccounts() ([]byte, error) {
	if len(k.data) == 0 {
		return nil, nil
	}
	return append([]byte(nil), k.data...), nil
}

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

	// M1 & N2: every bypass confirmed in the review and re-check must be refused on real Caddy.
	bypasses := []struct {
		name string
		cfg  string
	}{
		{"dynamic reverse_proxy placeholder", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\treverse_proxy {http.request.uri.query.u}\n}\n"},
		{"root path placeholder", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\troot * /srv/{http.request.uri.query.p}\n\tfile_server\n}\n"},
		{"file placeholder in respond", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\trespond \"{file./proc/self/environ}\"\n}\n"},
		{"intercept handle_response metadata proxy", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\tintercept {\n\t\thandle_response {\n\t\t\treverse_proxy 169.254.169.254:80\n\t\t}\n\t}\n\tfile_server\n}\n"},
		{"handle_errors root escape", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\thandle_errors {\n\t\tvars root /\n\t\tfile_server\n\t}\n\troot * /srv\n\tfile_server\n}\n"},
		{"custom log output net", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\tlog {\n\t\toutput net 127.0.0.1:9000\n\t}\n\trespond ok\n}\n"},
		{"disallowed templates handler", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\troot * /srv\n\ttemplates\n\tfile_server\n}\n"},
		{"disallowed host matcher", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\t@other host evil.example.com\n\trespond @other ok\n}\n"},
		{"php_fastcgi root placeholder", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\tphp_fastcgi 127.0.0.1:9000 {\n\t\troot /srv/{http.request.uri.query.p}\n\t}\n}\n"},
		// Re-check N2 live bypasses:
		{"N2(a) browse template_file /proc/self/environ", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\troot * /srv\n\tfile_server {\n\t\tbrowse /proc/self/environ\n\t}\n}\n"},
		{"N2(a) file_server browse default", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\troot * /srv\n\tfile_server browse\n}\n"},
		{"N2(b) vars dynamic query key/value", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\troot * /srv\n\tvars {http.request.uri.query.k} {http.request.uri.query.v}\n\tfile_server\n}\n"},
		{"N2(b) vars empty root", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\tvars root \"\"\n\tfile_server\n}\n"},
		{"N2(b) vars non-root key", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\troot * /srv\n\tvars custom_key /etc\n\tfile_server\n}\n"},
		{"M1 map handler", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\tmap {path} {myvar} {\n\t\tdefault /\n\t}\n\trespond ok\n}\n"},
		{"M1 query placeholder in respond", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\trespond \"{http.request.uri.query.secret}\" 200\n}\n"},
		{"M1 query placeholder in header", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\theader X-Q \"{http.request.uri.query.x}\"\n\trespond ok\n}\n"},
		{"M1 arbitrary vars placeholder in respond", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\trespond \"{http.vars.secret}\" 200\n}\n"},
		{"M1 header placeholder in rewrite (not header/body)", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\trewrite * /{http.request.header.X-Path}\n\trespond ok\n}\n"},
		{"M1 remote-address placeholder in rewrite (not header/body)", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\trewrite * /{http.request.remote.host}\n\trespond ok\n}\n"},
		// N3 upstream resolution bypasses:
		{"N3 ip6-localhost upstream", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\treverse_proxy ip6-localhost:8080\n}\n"},
		{"N3 private IP literal upstream", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\treverse_proxy http://10.0.0.5:8080\n}\n"},
		{"N3 private 192.168 upstream", "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\treverse_proxy http://192.168.1.10:8080\n}\n"},
	}
	for _, tc := range bypasses {
		ok, msg, err := c.ValidateCaddyfile(ctx, tc.cfg, "/srv")
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if ok {
			t.Errorf("bypass %q was ACCEPTED by ValidateCaddyfile (should be refused)", tc.name)
		} else {
			t.Logf("refused %q: %s", tc.name, msg)
		}
	}

	// N3: hostnames resolving to metadata, loopback or private addresses must be refused;
	// a hostname resolving to a public IP must be accepted.
	c.lookupForTest = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		switch host {
		case "meta.nip.io":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		case "loop.nip.io":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "priv.nip.io":
			return []netip.Addr{netip.MustParseAddr("10.20.30.40")}, nil
		case "public.example.test":
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		default:
			return nil, fmt.Errorf("no such host: %s", host)
		}
	}
	t.Cleanup(func() { c.lookupForTest = nil })
	for _, host := range []string{"meta.nip.io", "loop.nip.io", "priv.nip.io"} {
		cfg := fmt.Sprintf("{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\treverse_proxy http://%s:80\n}\n", host)
		ok, msg, err := c.ValidateCaddyfile(ctx, cfg, "/srv")
		if err != nil || ok {
			t.Errorf("N3 resolved %s: ok=%v msg=%q err=%v, want refused", host, ok, msg, err)
		}
	}
	pubCfg := "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\treverse_proxy https://public.example.test\n}\n"
	if ok, msg, err := c.ValidateCaddyfile(ctx, pubCfg, "/srv"); err != nil || !ok {
		t.Fatalf("N3 public resolved host refused: ok=%v msg=%q err=%v", ok, msg, err)
	}

	// Header and remote-address placeholders are allowed inside header and respond bodies.
	hdrBodyCfg := "{\n\tadmin off\n\tauto_https off\n}\n:8080 {\n\theader X-Client \"{http.request.remote.host}\"\n\theader X-Req-Id \"{http.request.header.X-Request-Id}\"\n\trespond /whoami \"you are {http.request.remote.host}\" 200\n}\n"
	if ok, msg, err := c.ValidateCaddyfile(ctx, hdrBodyCfg, "/srv"); err != nil || !ok {
		t.Fatalf("allowed header/remote-address placeholders in header & respond refused: ok=%v msg=%q err=%v", ok, msg, err)
	}

	// Dockwell's own standard site log block must still pass the allowlist.
	dockwellLogCfg := "{\n\tadmin off\n\tauto_https off\n\tpersist_config off\n}\n:8080 {\n\tlog {\n\t\toutput stderr\n\t\tformat console\n\t}\n\troot * /srv\n\tfile_server\n}\n"
	ok, msg, err = c.ValidateCaddyfile(ctx, dockwellLogCfg, "/srv")
	if err != nil || !ok {
		t.Fatalf("Dockwell standard log config refused: ok=%v msg=%q err=%v", ok, msg, err)
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
	acctKeeper := &memACMEAccountKeeper{}
	c := &Caddy{
		Dir:             dir,
		Runtime:         &process{bin: bin, dir: dir},
		ACMEDirectory:   p.directory,
		ACMERoots:       p.roots,
		ACMEAccounts:    acctKeeper,
		HTTPPort:        p.httpPort,
		HTTPSPort:       p.tlsPort,
		adminUIDForTest: &uid,
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if err := c.Start(ctx, nil, false); err != nil {
		t.Fatal(err)
	}
	const domain = "renew.example.com"
	const otherDomain = "steady.example.com"
	p.addRecord(t, domain)
	if err := c.PutRoute(ctx, ingress.Route{
		ID:        "dockwell.env.renew",
		Domain:    domain,
		Upstreams: []string{strings.TrimPrefix(up.URL, "http://")},
	}); err != nil {
		t.Fatal(err)
	}
	// Second route on the same Caddy instance: must experience zero drops during forced renewal (M3).
	if err := c.PutRoute(ctx, ingress.Route{
		ID:             "dockwell.env.steady",
		Domain:         otherDomain,
		Upstreams:      []string{strings.TrimPrefix(up.URL, "http://")},
		Plain:          true,
		PlainForAnyone: true,
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

	dialSerial := func() *big.Int {
		t.Helper()
		conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", c.HTTPSPort), &tls.Config{
			ServerName:         domain,
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Fatalf("TLS dial %s failed: %v", domain, err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber
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

	// Continuously hit the other route while forced renewal runs to prove M3 drops zero routes.
	var steadyOK, steadyFail atomic.Int64
	stopProbe := make(chan struct{})
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		cl := &http.Client{Timeout: 2 * time.Second}
		for {
			select {
			case <-stopProbe:
				return
			default:
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", c.HTTPPort), http.NoBody)
			req.Host = otherDomain
			res, err := cl.Do(req)
			if err != nil || res.StatusCode != http.StatusOK {
				steadyFail.Add(1)
			} else {
				steadyOK.Add(1)
			}
			if res != nil {
				res.Body.Close()
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()

	// Force renewal via Pebble and verify a new serial is issued and served live.
	_, renewed, err := c.RenewCertificate(ctx, domain, true)
	close(stopProbe)
	<-probeDone
	if err != nil || !renewed {
		out, _ := os.ReadFile(dir + "/caddy.out")
		t.Fatalf("RenewCertificate(force=true) = renewed=%v err=%v\ncaddy.out:\n%s", renewed, err, out)
	}
	if steadyFail.Load() > 0 || steadyOK.Load() == 0 {
		t.Fatalf("other route experienced drops during forced renewal: ok=%d fail=%d", steadyOK.Load(), steadyFail.Load())
	}
	second := waitIssued(first.SerialNumber)
	if second.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatalf("expected new certificate serial after forced renewal")
	}
	if liveSerial := dialSerial(); liveSerial.Cmp(second.SerialNumber) != 0 {
		t.Fatalf("live TLS serial = %v, want renewed serial %v", liveSerial, second.SerialNumber)
	}
	// Verify N4: CertMagic .json metadata was updated alongside .key and .crt, and ACME account was saved once.
	if acctKeeper.saves != 1 {
		t.Fatalf("ACME account saves = %d, want 1", acctKeeper.saves)
	}
	var jsonFound bool
	_ = filepath.WalkDir(filepath.Join(dir, "data", "caddy", "certificates"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == domain+".json" {
			raw, rerr := os.ReadFile(p)
			if rerr == nil {
				var meta struct {
					SANs       []string       `json:"sans"`
					IssuerData map[string]any `json:"issuer_data"`
				}
				if json.Unmarshal(raw, &meta) == nil && len(meta.SANs) == 1 && meta.SANs[0] == domain && meta.IssuerData["ca"] == c.ACMEDirectory {
					jsonFound = true
				}
			}
		}
		return nil
	})
	if !jsonFound {
		t.Fatalf("expected updated CertMagic %s.json metadata in Caddy storage", domain)
	}

	// Verify N1: a later custom or DNS-01 certificate for the same domain immediately wins
	// over the forced-renewal bridge pin and drops the pin from Caddy's config.
	customPEM, customKey := makeTestCert(t, domain, "Later Custom CA")
	if err := c.SetCertificates(ctx, []ingress.DNSCert{{
		ID:      "custom.dockwell.env.renew",
		Name:    "custom",
		Names:   []string{domain},
		Serves:  []string{domain},
		Chain:   []byte(customPEM),
		Key:     []byte(customKey),
		Custom:  true,
		RouteID: "dockwell.env.renew",
	}}, []string{domain}); err != nil {
		t.Fatalf("SetCertificates(custom) failed: %v", err)
	}
	conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", c.HTTPSPort), &tls.Config{
		ServerName:         domain,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("TLS dial after custom cert failed: %v", err)
	}
	peerOrg := conn.ConnectionState().PeerCertificates[0].Issuer.Organization
	_ = conn.Close()
	if len(peerOrg) == 0 || peerOrg[0] != "Later Custom CA" {
		t.Fatalf("later custom cert did not win over forced-renewal pin: got issuer org %v", peerOrg)
	}

	// Remove the custom cert, force-renew a second time, and verify the SAME ACME account is reused (saves still 1).
	if err := c.SetCertificates(ctx, nil, nil); err != nil {
		t.Fatalf("clear SetCertificates failed: %v", err)
	}
	if _, renewed, err := c.RenewCertificate(ctx, domain, true); err != nil || !renewed {
		t.Fatalf("second RenewCertificate(force=true) = renewed=%v err=%v", renewed, err)
	}
	if acctKeeper.saves != 1 {
		t.Fatalf("second forced renewal created a new ACME account (saves=%d, want 1)", acctKeeper.saves)
	}
	third := waitIssued(second.SerialNumber)
	if liveSerial := dialSerial(); liveSerial.Cmp(third.SerialNumber) != 0 {
		t.Fatalf("live TLS serial after 2nd forced renew = %v, want %v", liveSerial, third.SerialNumber)
	}

	// Verify N1: once the forced certificate enters its renewal window (or Caddy renews it),
	// PruneRenewedCerts removes both load_pem and tls_connection_policies, and Caddy serves
	// its own managed certificate from CertMagic instead of the pinned one.
	c.nowForTest = func() time.Time { return third.NotAfter.Add(-10 * 24 * time.Hour) }
	if err := c.PruneRenewedCerts(ctx, []string{domain}); err != nil {
		t.Fatalf("PruneRenewedCerts failed: %v", err)
	}
	c.nowForTest = nil
	cfgRaw, _, err := c.call(ctx, http.MethodGet, "/config/", nil, nil)
	if err != nil {
		t.Fatalf("read Caddy config after PruneRenewedCerts: %v", err)
	}
	if strings.Contains(string(cfgRaw), renewedCertTag) || strings.Contains(string(cfgRaw), "tls_connection_policies") {
		t.Fatalf("forced-renewal pin or tls_connection_policies remained after PruneRenewedCerts:\n%s", cfgRaw)
	}
	// Caddy continues serving its own managed certificate over TLS.
	if liveAfterPrune := dialSerial(); liveAfterPrune == nil {
		t.Fatalf("Caddy served no certificate after pruning forced-renewal pin")
	}

	// M3 failure safety: if CA is unreachable during forced renewal, the existing valid
	// certificate stays intact on disk and keeps serving over TLS.
	savedDir := c.ACMEDirectory
	c.ACMEDirectory = "https://127.0.0.1:1/dir"
	if _, renewed, err := c.RenewCertificate(ctx, domain, true); err == nil || renewed {
		t.Fatalf("expected forced renewal against unreachable CA to fail, got renewed=%v err=%v", renewed, err)
	}
	c.ACMEDirectory = savedDir
	if afterFail := dialSerial(); afterFail == nil {
		t.Fatalf("existing valid cert was lost after failed renewal")
	}

	// N4 symlink safety: a symlink planted in Caddy's certificate dir pointing outside dataDir
	// must never be followed by writeRenewedCertToStorage.
	outsideFile := filepath.Join(t.TempDir(), "outside.crt")
	if err := os.WriteFile(outsideFile, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkDir := filepath.Join(dir, "data", "caddy", "certificates", "symlink-issuer")
	_ = os.MkdirAll(filepath.Dir(symlinkDir), 0o700)
	_ = os.Symlink(filepath.Dir(outsideFile), symlinkDir)
	_ = c.writeRenewedCertToStorage(ctx, "sym.example.com", forcedACMECert{
		certPEM: []byte(customPEM),
		keyPEM:  []byte(customKey),
		caURL:   "https://symlink-issuer/",
	})
	if got, _ := os.ReadFile(outsideFile); string(got) != "untouched" {
		t.Fatalf("writeRenewedCertToStorage followed symlink outside dataDir: got %q", got)
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
