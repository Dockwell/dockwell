// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"bytes"
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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/certificates"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

type pebbleChallAPI struct {
	mgmt string
}

func (a pebbleChallAPI) Verify(context.Context) error { return nil }
func (a pebbleChallAPI) Zones(context.Context) ([]dnsproviders.Zone, error) {
	return []dnsproviders.Zone{{ID: "z1", Name: "example.com"}}, nil
}

func (a pebbleChallAPI) AppendTXT(ctx context.Context, _ dnsproviders.Zone, name, value string) (string, error) {
	body, _ := json.Marshal(map[string]string{"host": name + ".", "value": value})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.mgmt+"/set-txt", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("challtestsrv set-txt status %d", res.StatusCode)
	}
	return name, nil
}

func (a pebbleChallAPI) TXTAt(context.Context, dnsproviders.Zone, string) ([]dnsproviders.TXT, error) {
	return nil, nil
}

func (a pebbleChallAPI) Delete(ctx context.Context, _ dnsproviders.Zone, id string) error {
	body, _ := json.Marshal(map[string]string{"host": id + "."})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.mgmt+"/clear-txt", bytes.NewReader(body))
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	return nil
}

type stubCertDelivery struct {
	serial string
	chain  string
}

func (d *stubCertDelivery) Connected() []string { return []string{"srv_on"} }
func (d *stubCertDelivery) Loader(server string) (certificates.Loader, bool) {
	if server == "srv_on" {
		return d, true
	}
	return nil, false
}
func (d *stubCertDelivery) LoadsCertificates() bool { return true }
func (d *stubCertDelivery) PutCertificates(_ context.Context, certs []agentapi.Certificate) (int, error) {
	for i := range certs {
		c := &certs[i]
		if block, _ := pem.Decode(c.Chain); block != nil {
			if leaf, err := x509.ParseCertificate(block.Bytes); err == nil {
				d.serial = leaf.SerialNumber.Text(16)
				d.chain = string(c.Chain)
			}
		}
	}
	return len(certs), nil
}

func startDomainPebble(t *testing.T, bins string) (directory, management string, client *http.Client) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	freePort := func() int {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		return port
	}
	dnsAddr := fmt.Sprintf("127.0.0.1:%d", freePort())
	mgmt := fmt.Sprintf("127.0.0.1:%d", freePort())
	apiPort := freePort()
	cfgBytes, _ := json.Marshal(map[string]any{"pebble": map[string]any{
		"listenAddress": fmt.Sprintf("127.0.0.1:%d", apiPort), "managementListenAddress": fmt.Sprintf("127.0.0.1:%d", freePort()),
		"certificate": certFile, "privateKey": keyFile, "httpPort": freePort(), "tlsPort": freePort(),
		"ocspResponderURL": "", "externalAccountBindingRequired": false,
		"retryAfter": map[string]int{"authz": 1, "order": 1},
	}})
	cfgFile := filepath.Join(dir, "pebble.json")
	if err := os.WriteFile(cfgFile, cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	start := func(env []string, bin string, args ...string) {
		cmd := exec.Command(filepath.Join(bins, bin), args...)
		cmd.Env = append(os.Environ(), env...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
		})
	}
	start(nil, "pebble-challtestsrv", "-defaultIPv4", "", "-defaultIPv6", "", "-dnsserver", dnsAddr, "-doh", "", "-http01", "", "-https01", "", "-tlsalpn01", "", "-management", mgmt)
	start([]string{"PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0", "PEBBLE_AUTHZREUSE=0"}, "pebble", "-config", cfgFile, "-dnsserver", dnsAddr)

	roots := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(certFile)
	roots.AppendCertsFromPEM(pemBytes)
	cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	dirURL := fmt.Sprintf("https://localhost:%d/dir", apiPort)
	for range 100 {
		if res, err := cl.Get(dirURL); err == nil {
			_ = res.Body.Close()
			return dirURL, "http://" + mgmt, cl
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("pebble never answered")
	return "", "", nil
}

// TestRenewCertificateEndToEndWithPebbleH2 verifies H2 end to end through
// domains.Handler.RenewCertificate against a real Pebble ACME CA:
//   - early non-forced renewal is refused ("not due"),
//   - forced renewal gets past "not due" and checkRenewLimit (no nil-map panic),
//   - Pebble issues a fresh certificate with a new serial,
//   - the 5th forced renewal in a week is refused.
func TestRenewCertificateEndToEndWithPebbleH2(t *testing.T) {
	bins := os.Getenv("DOCKWELL_TEST_PEBBLE")
	if bins == "" {
		t.Skip("DOCKWELL_TEST_PEBBLE isn't set")
	}
	dirURL, mgmtURL, httpClient := startDomainPebble(t, bins)

	db, _ := storetest.Open(t, store.SQLite)
	logger := slog.New(slog.DiscardHandler)
	if _, err := revision.Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateServer(t.Context(), store.Server{ID: "srv_on", Name: "srv_on", CreatedAt: now, CertSerial: "s1", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&service.Driver{}, &route.Driver{}, &dnsprovider.Type{})
	keeper := secretstest.Keeper(db, kinds)
	eng := revision.New(db, kinds, agents{}, keeper, logger)

	var orgID, envID, projID string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		tr, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		orgID = tr.Organization.ID
		projID = tr.Projects[0].ID
		envID = tr.Projects[0].Environments[0].ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := eng.Propose(t.Context(), revision.Proposal{
		Summary: "add dns provider and route",
		Actor:   "user:usr_1",
		Edits: []revision.Edit{
			{
				Ref: resource.Ref{Kind: dnsprovider.Kind, Scope: orgID, Name: "pebble"},
				Spec: &resourcev1.DnsProvider{
					Provider: "cloudflare",
					ApiToken: "tok_test_1234567890",
					Zones:    []string{"example.com"},
					Projects: []string{projID},
				},
			},
			{
				Ref:     resource.Ref{Kind: service.Kind, Scope: envID, Name: "web"},
				Spec:    &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}},
				Servers: []string{"srv_on"},
			},
			{
				Ref:     resource.Ref{Kind: route.Kind, Scope: envID, Name: "main"},
				Spec:    &resourcev1.Route{Domain: "pebble.example.com", Service: "web", Port: 8080, Proof: route.ProofDNS},
				Servers: []string{"srv_on"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Apply(t.Context(), plan.Change); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return revision.ConfirmKeys(t.Context(), tx, map[string][]string{"srv_on": {"srv_on"}})
	}); err != nil {
		t.Fatal(err)
	}

	delivered := &stubCertDelivery{}
	mgr := &certificates.Manager{
		DB:          db,
		Keeper:      keeper,
		DNS:         dnsproviders.New(db, keeper),
		Directory:   dirURL,
		HTTP:        httpClient,
		Logger:      logger,
		Agents:      delivered,
		ProviderAPI: func(context.Context, string) (dnsproviders.API, error) { return pebbleChallAPI{mgmt: mgmtURL}, nil },
	}
	// First tick issues the initial certificate from Pebble.
	if err := mgr.Tick(t.Context()); err != nil {
		t.Fatalf("initial Tick: %v", err)
	}
	firstSerial := delivered.serial
	if firstSerial == "" {
		t.Fatal("initial certificate was not issued by Pebble")
	}

	rep := reports{
		"srv_on/pebble.example.com": {
			Domain: "pebble.example.com",
			Certificate: &dockwellv1.Certificate{
				State:      dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED,
				ByDns:      "pebble",
				NotBefore:  timestamppb.New(now.Add(-time.Hour)),
				ExpireTime: timestamppb.New(now.Add(89 * 24 * time.Hour)),
			},
		},
	}
	h := New(kinds, db, rep, nil, logger)
	h.Certificates = mgr
	clock := now
	h.Now = func() time.Time { return clock }
	adminCtx := place.WithCaller(t.Context(), place.Caller{Grants: []store.Grant{{ProjectID: projID, Role: store.RoleAdmin}}})

	// 1. Non-forced renewal is refused ("not due").
	if _, err := h.RenewCertificate(adminCtx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{
		Domain: "pebble.example.com",
	})); err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("non-forced early renewal = %v, want FailedPrecondition", err)
	}

	// 2. Forced renewal gets past "not due" and checkRenewLimit (H2) and renews on Pebble!
	resp, err := h.RenewCertificate(adminCtx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{
		Domain: "pebble.example.com",
		Force:  true,
	}))
	if err != nil {
		t.Fatalf("RenewCertificate(force=true) failed: %v", err)
	}
	if !strings.Contains(resp.Msg.GetMessage(), "pebble.example.com") {
		t.Fatalf("unexpected message: %q", resp.Msg.GetMessage())
	}
	if err := mgr.Tick(t.Context()); err != nil {
		t.Fatalf("renewal Tick: %v", err)
	}
	if delivered.serial == "" || delivered.serial == firstSerial {
		t.Fatalf("Pebble did not issue a new certificate serial (first=%s, after=%s)", firstSerial, delivered.serial)
	}

	// 3. 3 more forced renewals (total 4 in 7 days) succeed; the 5th is refused by weekly cap.
	for i := 2; i <= manualRenewWeeklyCap; i++ {
		clock = clock.Add(2 * time.Hour)
		if _, err := h.RenewCertificate(adminCtx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{
			Domain: "pebble.example.com",
			Force:  true,
		})); err != nil {
			t.Fatalf("forced renewal %d failed: %v", i, err)
		}
	}
	clock = clock.Add(2 * time.Hour)
	if _, err := h.RenewCertificate(adminCtx, connect.NewRequest(&dockwellv1.RenewCertificateRequest{
		Domain: "pebble.example.com",
		Force:  true,
	})); err == nil || connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("5th forced renewal in a week = %v, want ResourceExhausted", err)
	}
}
