// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/ingress"
)

// envPebble names a directory with the pebble and pebble-challtestsrv
// binaries: Let's Encrypt's test CA, and the DNS server it asks. With it and
// envCaddy, these tests get real certificates, and real failures, on high
// ports, with no root and no Docker. CI's Docker job sets both.
const envPebble = "DOCKWELL_TEST_PEBBLE"

// pebble is a test CA, and the DNS it asks.
type pebble struct {
	directory, roots, management string
	httpPort, tlsPort            int
}

// localhostCert writes a self-signed certificate for localhost, which Pebble
// serves its API with, and Caddy trusts as the CA's root.
func localhostCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func startPebble(t *testing.T) *pebble {
	t.Helper()
	bins := os.Getenv(envPebble)
	if bins == "" {
		t.Skipf("%s isn't set, so this gets no certificate from a real CA. Set it to a directory with the pebble and pebble-challtestsrv binaries to run it", envPebble)
	}
	dir := shortDir(t)
	certFile, keyFile := localhostCert(t, dir)
	dnsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	mgmt := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	p := &pebble{roots: certFile, management: "http://" + mgmt, httpPort: freePort(t), tlsPort: freePort(t)}
	api := freePort(t)
	p.directory = fmt.Sprintf("https://localhost:%d/dir", api)
	config := map[string]any{"pebble": map[string]any{
		"listenAddress": fmt.Sprintf("127.0.0.1:%d", api), "managementListenAddress": fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"certificate": certFile, "privateKey": keyFile, "httpPort": p.httpPort, "tlsPort": p.tlsPort, "ocspResponderURL": "",
		"externalAccountBindingRequired": false, "retryAfter": map[string]int{"authz": 1, "order": 1},
	}}
	b, _ := json.Marshal(config)
	configFile := filepath.Join(dir, "pebble.json")
	if err := os.WriteFile(configFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	start := func(env []string, bin string, args ...string) {
		log, err := os.Create(filepath.Join(dir, bin+".out"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(filepath.Join(bins, bin), args...)
		cmd.Env, cmd.Stdout, cmd.Stderr = append(os.Environ(), env...), log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
			_ = log.Close()
		})
	}
	// No default address: a name has a record only once a test gives it one.
	start(nil, "pebble-challtestsrv", "-defaultIPv4", "", "-defaultIPv6", "", "-dnsserver", dnsAddr, "-doh", "", "-http01", "", "-https01", "", "-tlsalpn01", "", "-management", mgmt)
	start([]string{"PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0", "PEBBLE_AUTHZREUSE=0"}, "pebble", "-config", configFile, "-dnsserver", dnsAddr)

	roots := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(certFile)
	roots.AppendCertsFromPEM(pemBytes)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	for range 100 {
		if res, err := client.Get(p.directory); err == nil {
			_ = res.Body.Close()
			return p
		}
		time.Sleep(100 * time.Millisecond)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "pebble.out"))
	t.Fatalf("pebble never answered:\n%s", out)
	return nil
}

// addRecord points a name at an address, in the DNS the CA asks.
func (p *pebble) addRecord(t *testing.T, host, addr string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"host": host, "addresses": []string{addr}})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, p.management+"/add-a", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var res *http.Response
	for range 50 {
		if res, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
		req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, p.management+"/add-a", strings.NewReader(string(body)))
	}
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
}

// startCaddyWithCA is Caddy on the ports Pebble validates on, with issuance
// through challenges alone if it isn't empty.
func startCaddyWithCA(t *testing.T, p *pebble, panel *ingress.PanelSite, only string) *realCaddy {
	t.Helper()
	bin := needCaddy(t)
	dir := shortDir(t)
	proc := &process{bin: bin, dir: dir}
	c := &Caddy{Dir: dir, Runtime: proc, ACMEDirectory: p.directory, ACMERoots: p.roots, HTTPPort: p.httpPort, HTTPSPort: p.tlsPort}
	if only != "" {
		challenges := map[string]any{
			"http":     map[string]any{"alternate_port": p.httpPort},
			"tls-alpn": map[string]any{"alternate_port": p.tlsPort},
		}
		off := "tls-alpn"
		if only == "tls-alpn" {
			off = "http"
		}
		challenges[off] = map[string]any{"disabled": true}
		c.issuerForTest = map[string]any{
			"module": "acme", "ca": p.directory, "challenges": challenges,
			"trusted_roots_pem_files": []string{filepath.Join(proc.Paths().Config, acmeRootsName)},
		}
	}
	t.Cleanup(func() { proc.stop(syscall.SIGKILL) })
	if panel != nil {
		if err := EnsureDirs(dir); err != nil {
			t.Fatal(err)
		}
		fakePanel(t, dir)
	}
	if err := c.Start(t.Context(), panel); err != nil {
		out, _ := os.ReadFile(filepath.Join(dir, "caddy.out"))
		t.Fatalf("%v\n%s", err, out)
	}
	return &realCaddy{Caddy: c, proc: proc, http: c.HTTPPort, tls: c.HTTPSPort}
}

// waitFor waits for a domain's certificate to reach a phase.
func waitFor(t *testing.T, c *Caddy, domain string, want ingress.CertPhase, within time.Duration) ingress.CertState {
	t.Helper()
	var st ingress.CertState
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		states, err := c.Certificates(t.Context(), []string{domain})
		if err != nil {
			t.Fatal(err)
		}
		if st = states[domain]; st.Phase == want {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
	log, _ := os.ReadFile(filepath.Join(c.Dir, "data", tlsLogName))
	t.Fatalf("%s's certificate stayed %+v, want phase %d. Caddy's log:\n%s", domain, st, want, log)
	return st
}

// Each challenge gets a real certificate, on a config with the panel's rules
// on plain HTTP too, so they don't get in the way of HTTP-01.
func TestCertificatesComeOverEachChallengeOnRealCaddy(t *testing.T) {
	for _, challenge := range []string{"http", "tls-alpn"} {
		t.Run(challenge, func(t *testing.T) {
			p := startPebble(t)
			c := startCaddyWithCA(t, p, &ingress.PanelSite{Domain: "panel.example.net"}, challenge)
			p.addRecord(t, "app.example.net", "127.0.0.1")
			put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))

			st := waitFor(t, c.Caddy, "app.example.net", ingress.CertIssued, 45*time.Second)
			if st.NotAfter.Before(time.Now()) || st.Problem != ingress.ProblemNone {
				t.Errorf("the certificate reads %+v", st)
			}
			res := c.waitServing(t, "app.example.net", "/who")
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Errorf("the route answers %d with its certificate", res.StatusCode)
			}
		})
	}
}

// The promise of ADR 0024: a person is told why, in plain language, and fixing
// it works at once, not at Caddy's next try in hours.
func TestAFailureSaysWhyAndFixingDNSWorksAtOnceOnRealCaddy(t *testing.T) {
	p := startPebble(t)
	c := startCaddyWithCA(t, p, nil, "")
	// No record for this name, so the CA can't find it.
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	st := waitFor(t, c.Caddy, "app.example.net", ingress.CertFailed, 45*time.Second)
	if st.Problem != ingress.ProblemDNS {
		t.Errorf("a name with no record reads as problem %d (%q), want DNS", st.Problem, st.Detail)
	}

	p.addRecord(t, "app.example.net", "127.0.0.1")
	begun := time.Now()
	if err := c.Retry(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c.Caddy, "app.example.net", ingress.CertIssued, 20*time.Second)
	t.Logf("issued %s after asking Caddy to try again", time.Since(begun).Round(time.Millisecond))

	// Issued from disk, by a reader that has seen no log, as after a restart.
	fresh := &Caddy{Dir: c.Dir, Runtime: c.Runtime}
	states, err := fresh.Certificates(t.Context(), []string{"app.example.net"})
	if err != nil {
		t.Fatal(err)
	}
	if states["app.example.net"].Phase != ingress.CertIssued {
		t.Errorf("after a restart, the certificate reads %+v", states["app.example.net"])
	}
}

// A CA at another address gets the same config through DOCKWELL_ACME_ROOTS.
func TestTheBootstrapNamesItsCA(t *testing.T) {
	dir := shortDir(t)
	roots := filepath.Join(dir, "roots.pem")
	if err := os.WriteFile(roots, []byte("not a root yet"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Caddy{Dir: dir, Runtime: &process{dir: dir}, ACMEDirectory: "https://ca.example/dir", ACMERoots: roots}
	if err := c.prepare(nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config", bootstrapName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "https://ca.example/dir") || !strings.Contains(string(b), acmeRootsName) {
		t.Errorf("the bootstrap names no CA: %s", b)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "config", acmeRootsName)); string(got) != "not a root yet" {
		t.Errorf("the roots weren't copied where Caddy reads them: %q", got)
	}
}
