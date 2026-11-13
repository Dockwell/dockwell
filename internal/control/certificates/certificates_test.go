// SPDX-License-Identifier: AGPL-3.0-only

package certificates

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/certificate"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	db, _ := storetest.Open(t, store.SQLite)
	return &Manager{DB: db, Keeper: secretstest.Keeper(db, resource.NewRegistry()), ControlServer: func() string { return "srv_control" }}
}

// A server's routes proven by DNS share one certificate of the server's own; a
// shared one serves the projects it names alone, with names its provider's zones
// hold; and one that reaches the panel's domain, as served or as saved, goes to no
// server but the control server (ADR 0088).
func TestWhichCertificateServesARoute(t *testing.T) {
	m := newManager(t)
	providers := map[string]*resourcev1.DnsProvider{
		"cloudflare": {Zones: []string{"example.com"}, Projects: []string{"prj_shop"}},
	}
	shared := func(id string, projects []string, names ...string) certificate.Shared {
		return certificate.Shared{ID: id, Name: id, Spec: &resourcev1.DnsCertificate{Domains: names, DnsProvider: "cloudflare", Projects: projects}}
	}
	wild := shared("crt_wild", []string{"prj_shop"}, "*.example.com")
	nobody := shared("crt_nobody", nil, "*.example.com")
	panel := []string{"panel.example.com"}

	out := map[Holder]*want{}
	m.place(out, []certificate.Shared{nobody}, providers, "a.example.com", "prj_shop", "srv_1", panel)
	m.place(out, []certificate.Shared{nobody}, providers, "b.example.com", "prj_shop", "srv_1", panel)
	m.place(out, []certificate.Shared{nobody}, providers, "c.example.com", "prj_shop", "srv_2", panel)
	if w := out["srv_1"]; w == nil || strings.Join(w.Names, ",") != "a.example.com,b.example.com" || out["crt_nobody"] != nil {
		t.Errorf("a shared certificate that names no project, or a server's own: %+v", out)
	}
	if w := out["srv_2"]; w == nil || strings.Join(w.Names, ",") != "c.example.com" || !slices.Equal(w.Servers, []string{"srv_2"}) {
		t.Errorf("each server has its own: %+v", out["srv_2"])
	}

	out = map[Holder]*want{}
	m.place(out, []certificate.Shared{wild}, providers, "a.example.com", "prj_shop", "srv_control", panel)
	m.place(out, []certificate.Shared{wild}, providers, "b.example.com", "prj_shop", "srv_1", panel)
	if w := out["crt_wild"]; w == nil || !slices.Equal(w.Servers, []string{"srv_control"}) {
		t.Errorf("a wildcard over the panel's domain went beyond the control server: %+v", w)
	}
	if w := out["srv_1"]; w == nil || strings.Join(w.Names, ",") != "b.example.com" {
		t.Errorf("a route under it elsewhere doesn't get its server's own: %+v", out)
	}

	// The panel's domain as saved, which it isn't served at yet, counts as well, as
	// does a name under it.
	for _, c := range []certificate.Shared{wild, shared("crt_under", []string{"prj_shop"}, "a.example.com", "shop.panel.example.com")} {
		out = map[Holder]*want{}
		m.place(out, []certificate.Shared{c}, providers, "a.example.com", "prj_shop", "srv_1", []string{"", "panel.example.com"})
		if out[c.ID] != nil || out["srv_1"] == nil {
			t.Errorf("%s, by the panel's saved domain, went to another server: %+v", c.ID, out)
		}
	}

	out = map[Holder]*want{}
	m.place(out, []certificate.Shared{wild}, providers, "a.example.com", "prj_shop", "srv_1", []string{"panel.example.org"})
	if w := out["crt_wild"]; w == nil || !slices.Equal(w.Servers, []string{"srv_1"}) {
		t.Errorf("a shared wildcard doesn't serve its project: %+v", out)
	}
	out = map[Holder]*want{}
	m.place(out, nil, providers, "a.example.com", "prj_blog", "srv_1", panel)
	if len(out) != 0 {
		t.Errorf("a provider proved a name for a project it doesn't name: %+v", out)
	}
	// A shared certificate with a name outside its provider's zones isn't ordered.
	out = map[Holder]*want{}
	m.place(out, []certificate.Shared{shared("crt_wide", []string{"prj_shop"}, "*.example.com", "example.net")}, providers, "a.example.com", "prj_shop", "srv_1", panel)
	if out["crt_wide"] != nil {
		t.Errorf("a certificate with a name outside its provider's zones: %+v", out)
	}
}

// An order waits: on a failure's backoff, an hour after the last for names that
// changed, and a day for a name that was dropped; and no registered domain is
// ordered for more than weeklyCap times a week (ADR 0088).
func TestOrdersAreSpacedAndCapped(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	names := []string{"a.example.com", "b.example.com"}
	issuedFor := func(wanted ...string) *held {
		return &held{Gen: 1, Wanted: wanted, RenewAt: now.Add(50 * 24 * time.Hour), LastOrder: now.Add(-10 * time.Minute)}
	}
	for _, c := range []struct {
		what string
		s    *held
		want bool
	}{
		{"never issued", &held{}, true},
		{"never issued, backing off", &held{Failures: 1, Next: now.Add(time.Minute)}, false},
		{"the same names", issuedFor(names...), false},
		{"names that changed ten minutes after the last order", issuedFor("a.example.com"), false},
		{"names that changed an hour after", &held{Gen: 1, Wanted: []string{"a.example.com"}, RenewAt: now.Add(50 * 24 * time.Hour), LastOrder: now.Add(-time.Hour)}, true},
		{"time to renew", &held{Gen: 1, Wanted: names, RenewAt: now.Add(-time.Minute), LastOrder: now.Add(-60 * 24 * time.Hour)}, true},
		{"a name dropped, within the day", &held{Gen: 1, Wanted: names, Dropped: []string{"b.example.com"}, RenewAt: now.Add(50 * 24 * time.Hour), LastOrder: now.Add(-2 * time.Hour), Next: now.Add(22 * time.Hour)}, false},
		{"a name dropped, a day on", &held{Gen: 1, Wanted: names, Dropped: []string{"b.example.com"}, RenewAt: now.Add(50 * 24 * time.Hour), LastOrder: now.Add(-24 * time.Hour), Next: now}, true},
	} {
		if got := dueNow(c.s, names, now); got != c.want {
			t.Errorf("%s: due %v, want %v", c.what, got, c.want)
		}
	}

	if r := registered("*.shop.example.co.uk"); r != "example.co.uk" {
		t.Errorf("registered = %q", r)
	}
	ordered := map[string][]time.Time{"example.com": {now.Add(-8 * 24 * time.Hour)}}
	for range weeklyCap - 1 {
		countOrder(ordered, []string{"*.example.com", "example.com", "app.example.org"}, now)
	}
	if busy := atCap(ordered, names, now); busy != "" {
		t.Errorf("%d orders this week are at the cap for %s", weeklyCap-1, busy)
	}
	countOrder(ordered, []string{"x.example.com"}, now)
	if busy := atCap(ordered, names, now); busy != "example.com" {
		t.Errorf("%d orders this week aren't at the cap: %q", weeklyCap, busy)
	}
	if busy := atCap(ordered, []string{"app.example.org"}, now); busy != "" {
		t.Errorf("another registered domain is at the cap: %q", busy)
	}
	if kept := prune(ordered, now); len(kept["example.com"]) != weeklyCap {
		t.Errorf("prune kept %d of example.com's orders, want this week's %d", len(kept["example.com"]), weeklyCap)
	}
	if busy := atCap(ordered, names, now.Add(7*24*time.Hour)); busy != "" {
		t.Errorf("a week on, still at the cap for %s", busy)
	}
}

// ARI's ID is the authority key ID and the serial as DER encodes it, with a leading
// zero where its high bit is set (RFC 9773, section 4.1, whose example this is).
func TestARIsCertificateID(t *testing.T) {
	aki, _ := hex.DecodeString("69885B6B87464041E1B37B847BA0AE2CDE01C8D4")
	leaf := &x509.Certificate{AuthorityKeyId: aki, SerialNumber: big.NewInt(0x87654321)}
	if got := ariID(leaf); got != "aYhba4dGQEHhs3uEe6CuLN4ByNQ.AIdlQyE" {
		t.Errorf("ariID = %s", got)
	}
	leaf.SerialNumber = big.NewInt(0x7f)
	if got := ariID(leaf); got != "aYhba4dGQEHhs3uEe6CuLN4ByNQ.fw" {
		t.Errorf("ariID of a low serial = %s", got)
	}
}

// The CA is asked for a certificate's renewal window once a day, or later where its
// Retry-After says, and the window moves when it renews (RFC 9773).
func TestTheRenewalWindowIsAskedDaily(t *testing.T) {
	m := newManager(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	start := now.Add(40 * 24 * time.Hour)
	var asked atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/dir", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"renewalInfo": srv.URL + "/ari/"})
	})
	mux.HandleFunc("/ari/", func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.Header().Set("Retry-After", "7200")
		_ = json.NewEncoder(w).Encode(map[string]any{"suggestedWindow": map[string]time.Time{"start": start, "end": start.Add(3 * 24 * time.Hour)}})
	})
	m.Directory, m.HTTP = srv.URL+"/dir", srv.Client()
	st := storeIssued(t, m, "srv_1", "app.example.com")
	s := st["srv_1"]
	m.checkWindow(t.Context(), "srv_1", s, now)
	if asked.Load() != 1 || !s.RenewAt.Equal(start.Add(24*time.Hour)) || !s.WindowNext.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("asked %d times, renews at %s, asks again at %s", asked.Load(), s.RenewAt, s.WindowNext)
	}
	m.checkWindow(t.Context(), "srv_1", s, now.Add(time.Hour))
	if asked.Load() != 1 {
		t.Error("the CA was asked again before its Retry-After")
	}
	m.checkWindow(t.Context(), "srv_1", s, now.Add(2*time.Hour))
	if asked.Load() != 2 {
		t.Error("the CA wasn't asked again once its Retry-After passed")
	}
}

func TestBackoffAndRenewal(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 2 * time.Minute, 2: 4 * time.Minute, 5: 32 * time.Minute, 20: 6 * time.Hour} {
		if got := backoff(n); got != want {
			t.Errorf("backoff(%d) = %s, want %s", n, got, want)
		}
	}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	c := &issued{IssuedAt: at, NotAfter: at.Add(90 * 24 * time.Hour)}
	if got := renewAt(c, time.Time{}, time.Time{}, false); !got.Equal(at.Add(60 * 24 * time.Hour)) {
		t.Errorf("a 90-day certificate renews at %s", got)
	}
	short := &issued{IssuedAt: at, NotAfter: at.Add(6 * 24 * time.Hour)}
	if got := renewAt(short, time.Time{}, time.Time{}, false); !got.Equal(at.Add(4 * 24 * time.Hour)) {
		t.Errorf("a 6-day certificate renews at %s", got)
	}
	start := at.Add(50 * 24 * time.Hour)
	if got := renewAt(c, start, start.Add(3*24*time.Hour), true); !got.Equal(start.Add(24 * time.Hour)) {
		t.Errorf("inside the CA's window, it renews at %s", got)
	}
}

// A crash mid-order leaves a noted record in a zone, which goes as the control
// plane next starts, found by its name and value where it never got its ID.
func TestACrashMidOrderLeavesNothingBehind(t *testing.T) {
	m := newManager(t)
	cf := cftest.New("token", "example.com")
	srv := httptest.NewServer(cf)
	t.Cleanup(srv.Close)
	api := &dnsproviders.Cloudflare{Token: "token", Base: srv.URL + "/client/v4", Client: srv.Client()}
	m.ProviderAPI = func(context.Context, string) (dnsproviders.API, error) { return api, nil }
	zone := dnsproviders.Zone{ID: "zone-example-com", Name: "example.com"}
	if _, err := api.AppendTXT(t.Context(), zone, "_acme-challenge.app.example.com", "left-by-a-crash"); err != nil {
		t.Fatal(err)
	}
	if err := m.note(t.Context(), noted{Provider: "cloudflare", ZoneID: zone.ID, Zone: zone.Name, Name: "_acme-challenge.app.example.com", Value: "left-by-a-crash"}, ""); err != nil {
		t.Fatal(err)
	}
	m.cleanupNoted(t.Context())
	if left := cf.Records("example.com"); len(left) != 0 {
		t.Errorf("a crash's record is still there: %v", left)
	}
	var list []noted
	_ = m.DB.View(t.Context(), func(tx *store.Tx) error {
		var err error
		list, err = journal(t.Context(), tx)
		return err
	})
	if len(list) != 0 {
		t.Errorf("the journal still holds %v", list)
	}
}

// challtest writes DNS into pebble-challtestsrv, which Pebble asks for TXT records,
// in zones, or example.com alone.
type challtest struct {
	mgmt  string
	zones []string
}

func (c challtest) Verify(context.Context) error { return nil }
func (c challtest) Zones(context.Context) ([]dnsproviders.Zone, error) {
	out := []dnsproviders.Zone{}
	for _, z := range append([]string{"example.com"}, c.zones...) {
		out = append(out, dnsproviders.Zone{ID: z, Name: z})
	}
	return out, nil
}

func (c challtest) post(path string, body any) error {
	b, _ := json.Marshal(body)
	resp, err := http.Post(c.mgmt+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", path, resp.StatusCode)
	}
	return nil
}

func (c challtest) AppendTXT(_ context.Context, _ dnsproviders.Zone, name, value string) (string, error) {
	return name + "|" + value, c.post("/set-txt", map[string]string{"host": name + ".", "value": value})
}

func (c challtest) TXTAt(context.Context, dnsproviders.Zone, string) ([]dnsproviders.TXT, error) {
	return nil, nil
}

func (c challtest) Delete(_ context.Context, _ dnsproviders.Zone, id string) error {
	name, _, _ := strings.Cut(id, "|")
	return c.post("/clear-txt", map[string]string{"host": name + "."})
}

// A wildcard and its domain come from a real CA by DNS-01, each proven through the
// provider, with a new key, and every challenge record deleted after.
func TestAnOrderProvesItsNamesByDNS(t *testing.T) {
	bins := os.Getenv("DOCKWELL_TEST_PEBBLE")
	if bins == "" {
		t.Skip("DOCKWELL_TEST_PEBBLE isn't set, so this gets no certificate from a real CA. Set it to a directory with the pebble and pebble-challtestsrv binaries to run it")
	}
	p := startPebble(t, bins)
	m := newManager(t)
	m.Directory, m.HTTP = p.directory, p.client
	api := challtest{mgmt: p.management}
	m.ProviderAPI = func(context.Context, string) (dnsproviders.API, error) { return api, nil }
	w := &want{Holder: "srv_1", Names: []string{"*.example.com", "example.com"}, Provider: map[string]string{"*.example.com": "test", "example.com": "test"}}
	first, err := m.orderFor(t.Context(), w, w.Names, map[string][]string{"test": {"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	leaf := leafOf([]byte(first.Chain))
	if leaf == nil || !slices.Equal(first.Names, []string{"*.example.com", "example.com"}) || leaf.VerifyHostname("app.example.com") != nil || leaf.VerifyHostname("example.com") != nil {
		t.Fatalf("issued %v for %v", leaf, first.Names)
	}
	second, err := m.orderFor(t.Context(), w, w.Names, map[string][]string{"test": {"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(second.Key, first.Key) || second.Serial == first.Serial {
		t.Error("a second issuance kept the first key")
	}
	var list []noted
	_ = m.DB.View(t.Context(), func(tx *store.Tx) error {
		var err error
		list, err = journal(t.Context(), tx)
		return err
	})
	if len(list) != 0 {
		t.Errorf("challenge records are still noted: %v", list)
	}
}

// A name whose proof fails is dropped and the rest ordered, as one bad name
// shouldn't hold back the others: here one in a zone the token reaches but the
// provider wasn't saved with, which no record is written in (ADR 0088).
func TestANameThatFailsIsDroppedAndTheRestOrdered(t *testing.T) {
	bins := os.Getenv("DOCKWELL_TEST_PEBBLE")
	if bins == "" {
		t.Skip("DOCKWELL_TEST_PEBBLE isn't set, so this gets no certificate from a real CA. Set it to a directory with the pebble and pebble-challtestsrv binaries to run it")
	}
	p := startPebble(t, bins)
	m := newManager(t)
	m.Directory, m.HTTP = p.directory, p.client
	api := &recordingAPI{challtest: challtest{mgmt: p.management, zones: []string{"example.net"}}}
	m.ProviderAPI = func(context.Context, string) (dnsproviders.API, error) { return api, nil }
	w := &want{Holder: "srv_1", Names: []string{"*.example.com", "app.example.net"}, Provider: map[string]string{"*.example.com": "test", "app.example.net": "test"}}
	s := &held{}
	ordered := map[string][]time.Time{}
	m.issue(t.Context(), w, s, map[string][]string{"test": {"example.com"}}, ordered, map[string]*given{})
	if s.Gen != 1 || !slices.Equal(s.Names, []string{"*.example.com"}) || !slices.Equal(s.Dropped, []string{"app.example.net"}) || !slices.Equal(s.Wanted, w.Names) {
		t.Fatalf("issued %+v", s)
	}
	if !strings.HasPrefix(s.Message, "Issued without app.example.net: ") || !strings.Contains(s.Message, "doesn't reach a zone") {
		t.Errorf("the message reads %q", s.Message)
	}
	if !s.Next.After(m.now().Add(23 * time.Hour)) {
		t.Errorf("the dropped name is tried again at %s", s.Next)
	}
	for _, name := range api.written() {
		if strings.HasSuffix(name, "example.net") {
			t.Errorf("a record went into a zone the provider wasn't saved with: %s", name)
		}
	}
	if len(ordered["example.com"]) != 2 || len(ordered["example.net"]) != 1 {
		t.Errorf("orders counted: %v", ordered)
	}
	if err := m.DB.Update(t.Context(), func(tx *store.Tx) error {
		return putJSON(t.Context(), tx, stateSetting, map[string]*held{"srv_1": s})
	}); err != nil {
		t.Fatal(err)
	}
	_, names, _, err := m.issuedParts(t.Context(), "srv_1")
	if err != nil || !slices.Equal(names, []string{"*.example.com"}) {
		t.Errorf("kept %v, %v", names, err)
	}
}

// recordingAPI notes where records are written.
type recordingAPI struct {
	challtest
	mu    sync.Mutex
	names []string
}

func (r *recordingAPI) AppendTXT(ctx context.Context, z dnsproviders.Zone, name, value string) (string, error) {
	r.mu.Lock()
	r.names = append(r.names, name)
	r.mu.Unlock()
	return r.challtest.AppendTXT(ctx, z, name, value)
}

func (r *recordingAPI) written() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.names)
}

type pebble struct {
	// directory is the CA's, management pebble-challtestsrv's, and admin Pebble's
	// own, which says whether a certificate is revoked.
	directory, management, admin string
	client                       *http.Client
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startPebble runs Pebble with pebble-challtestsrv as its DNS, which a test writes
// challenge records into.
func startPebble(t *testing.T, bins string) *pebble {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile := localhostCert(t, dir)
	dnsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	mgmt := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	api := freePort(t)
	admin := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	config := map[string]any{"pebble": map[string]any{
		"listenAddress": fmt.Sprintf("127.0.0.1:%d", api), "managementListenAddress": admin,
		"certificate": certFile, "privateKey": keyFile, "httpPort": freePort(t), "tlsPort": freePort(t), "ocspResponderURL": "",
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
	start(nil, "pebble-challtestsrv", "-defaultIPv4", "", "-defaultIPv6", "", "-dnsserver", dnsAddr, "-doh", "", "-http01", "", "-https01", "", "-tlsalpn01", "", "-management", mgmt)
	start([]string{"PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0", "PEBBLE_AUTHZREUSE=0"}, "pebble", "-config", configFile, "-dnsserver", dnsAddr)
	roots := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(certFile)
	roots.AppendCertsFromPEM(pemBytes)
	p := &pebble{
		directory: fmt.Sprintf("https://localhost:%d/dir", api), management: "http://" + mgmt, admin: "https://" + admin,
		client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}},
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := p.client.Get(p.directory)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pebble didn't answer at %s: %v", p.directory, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return p
}
