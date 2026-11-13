// SPDX-License-Identifier: AGPL-3.0-only

package certificates

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

type fakeAgent struct {
	mu    sync.Mutex
	loads bool
	fail  bool
	puts  [][]string
	// given are the certificates given, as given, to see their keys cleared.
	given []agentapi.Certificate
}

func (f *fakeAgent) LoadsCertificates() bool { return f.loads }

func (f *fakeAgent) PutCertificates(_ context.Context, certs []agentapi.Certificate) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, errors.New("the stream went away")
	}
	var ids []string
	for _, c := range certs {
		if len(c.Key) == 0 || len(c.Chain) == 0 {
			ids = append(ids, c.ID+":empty")
			continue
		}
		ids = append(ids, c.ID)
	}
	f.puts = append(f.puts, ids)
	f.given = append(f.given, certs...)
	return len(certs), nil
}

type fakeAgents map[string]*fakeAgent

func (f fakeAgents) Connected() []string {
	var out []string
	for s := range f {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func (f fakeAgents) Loader(server string) (Loader, bool) {
	a, ok := f[server]
	return a, ok
}

// madeCertificate is a certificate for names from a made-up CA, with its key.
func madeCertificate(t *testing.T, names ...string) *issued {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Made-up CA"}, NotBefore: now, NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, SubjectKeyId: []byte{1, 2, 3, 4}}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: serial, DNSNames: names, NotBefore: now, NotAfter: now.Add(80 * 24 * time.Hour)}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	return &issued{
		Chain: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		Key:   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}),
		Names: names, Serial: serial.Text(16), NotAfter: now.Add(80 * 24 * time.Hour), IssuedAt: now,
	}
}

// storeIssued keeps a made-up certificate for holder at generation 1, as an order
// would, and gives back every holder's state.
func storeIssued(t *testing.T, m *Manager, holder string, names ...string) map[string]*held {
	t.Helper()
	var st map[string]*held
	c := madeCertificate(t, names...)
	err := m.DB.Update(t.Context(), func(tx *store.Tx) error {
		var err error
		if st, err = states(t.Context(), tx); err != nil {
			return err
		}
		b, err := m.sealTo(t.Context(), tx, holder, 1, c)
		if err != nil {
			return err
		}
		st[holder] = &held{Gen: 1, Names: names, Wanted: names, Serial: c.Serial, NotAfter: c.NotAfter, IssuedAt: c.IssuedAt, RenewAt: c.NotAfter.Add(-30 * 24 * time.Hour)}
		if err := tx.PutSetting(t.Context(), issuedSetting(holder), b); err != nil {
			return err
		}
		return putJSON(t.Context(), tx, stateSetting, st)
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func confirm(t *testing.T, m *Manager, pairs map[string][]string) {
	t.Helper()
	if err := m.DB.Update(t.Context(), func(tx *store.Tx) error {
		return revision.ConfirmKeys(t.Context(), tx, pairs)
	}); err != nil {
		t.Fatal(err)
	}
}

func deliveries(t *testing.T, m *Manager) []map[string]any {
	t.Helper()
	var events []store.Event
	_ = m.DB.View(t.Context(), func(tx *store.Tx) error {
		var err error
		events, err = tx.ListEvents(t.Context(), 100)
		return err
	})
	var out []map[string]any
	for i := range events {
		if e := &events[i]; e.Action == ActionDeliver {
			var d map[string]any
			_ = json.Unmarshal([]byte(e.Detail), &d)
			d["server"] = e.Subject
			out = append(out, d)
		}
	}
	return out
}

// A server gets its certificates once an admin has let it have a key, and only if
// its agent loads them; what failed to go goes again, from the store, and each key
// that goes is on the server's timeline; the keys given are cleared after (ADR
// 0088).
func TestCertificatesGoToConfirmedServersAlone(t *testing.T) {
	m := newManager(t)
	agents := fakeAgents{"srv_1": {loads: true}, "srv_2": {loads: true}, "srv_old": {loads: false}}
	m.Agents = agents
	st := storeIssued(t, m, "srv_1", "app.example.com")
	st2 := storeIssued(t, m, "srv_2", "shop.example.com")
	st["srv_2"] = st2["srv_2"]
	wanted := map[Holder]*want{
		"srv_1":   {Holder: "srv_1", Names: []string{"app.example.com"}, Servers: []string{"srv_1"}},
		"srv_2":   {Holder: "srv_2", Names: []string{"shop.example.com"}, Servers: []string{"srv_2"}},
		"srv_old": {Holder: "srv_old", Names: []string{"old.example.com"}, Servers: []string{"srv_old"}},
	}

	// Before an admin lets it have a key, a server is given none: an empty set,
	// which takes away whatever it had.
	m.deliver(t.Context(), wanted, st)
	if got := agents["srv_1"].puts; len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("a server got %v before an admin let it have a key", got)
	}
	agents["srv_1"].puts, agents["srv_2"].puts = nil, nil
	confirm(t, m, map[string][]string{"srv_1": {"srv_1"}, "srv_2": {"srv_2"}, "srv_old": {"srv_old"}})
	agents["srv_2"].fail = true
	m.deliver(t.Context(), wanted, st)
	if got := agents["srv_1"].puts; len(got) != 1 || !slices.Equal(got[0], []string{"srv_1"}) {
		t.Errorf("srv_1 got %v", got)
	}
	if len(agents["srv_old"].puts) != 0 {
		t.Error("an agent that doesn't load certificates was given one")
	}
	for _, c := range agents["srv_1"].given {
		if slices.ContainsFunc(c.Key, func(b byte) bool { return b != 0 }) {
			t.Error("a key given to a server is still in memory after")
		}
	}
	// Nothing changed for srv_1, so nothing goes again; srv_2's failed, so it does.
	agents["srv_2"].fail = false
	m.deliver(t.Context(), wanted, st)
	if len(agents["srv_1"].puts) != 1 || len(agents["srv_2"].puts) != 1 {
		t.Errorf("after a failure: srv_1 %v, srv_2 %v", agents["srv_1"].puts, agents["srv_2"].puts)
	}
	// A reconnected agent is given what it has again.
	m.Reconnected("srv_1")
	m.deliver(t.Context(), wanted, st)
	if len(agents["srv_1"].puts) != 2 {
		t.Errorf("a reconnected agent wasn't given its certificates again: %v", agents["srv_1"].puts)
	}
	if d := deliveries(t, m); len(d) != 2 {
		t.Errorf("%d deliveries on the timeline, want one for each server's new key: %v", len(d), d)
	}
}

// A shared certificate goes to a server an admin let have that certificate, not
// one let have its own alone; until then the server gets what it may have (ADR
// 0088).
func TestASharedCertificateGoesWhereItWasConfirmed(t *testing.T) {
	m := newManager(t)
	agents := fakeAgents{"srv_1": {loads: true}}
	m.Agents = agents
	st := storeIssued(t, m, "srv_1", "app.example.com")
	st["crt_wild"] = storeIssued(t, m, "crt_wild", "*.example.com")["crt_wild"]
	wanted := map[Holder]*want{
		"srv_1":    {Holder: "srv_1", Names: []string{"app.example.com"}, Servers: []string{"srv_1"}},
		"crt_wild": {Holder: "crt_wild", Names: []string{"*.example.com"}, Servers: []string{"srv_1"}},
	}
	confirm(t, m, map[string][]string{"srv_1": {"srv_1"}})
	m.deliver(t.Context(), wanted, st)
	if got := agents["srv_1"].puts; len(got) != 1 || !slices.Equal(got[0], []string{"srv_1"}) {
		t.Fatalf("a server let have its own certificate got %v", got)
	}
	confirm(t, m, map[string][]string{"srv_1": {"crt_wild"}})
	m.deliver(t.Context(), wanted, st)
	if got := agents["srv_1"].puts; len(got) != 2 || !slices.Equal(got[1], []string{"crt_wild", "srv_1"}) {
		t.Errorf("once let have the wildcard, it got %v", got)
	}
}

// Each key a server gets is on its timeline once, by its serial, whatever restarts:
// the same key again isn't, and a new one is, even where its holder began again at
// the first generation; and the event names the certificate's names (ADR 0088).
func TestEachKeyIsOnTheTimelineOnce(t *testing.T) {
	m := newManager(t)
	agents := fakeAgents{"srv_1": {loads: true}}
	m.Agents = agents
	wanted := map[Holder]*want{"crt_wild": {Holder: "crt_wild", Names: []string{"*.example.com", "example.com"}, Servers: []string{"srv_1"}}}
	confirm(t, m, map[string][]string{"srv_1": {"crt_wild"}})
	st := storeIssued(t, m, "crt_wild", "*.example.com", "example.com")
	m.deliver(t.Context(), wanted, st)
	d := deliveries(t, m)
	if len(d) != 1 || d[0]["serial"] != st["crt_wild"].Serial || d[0]["server"] != "srv_1" || fmt.Sprint(d[0]["names"]) != "[*.example.com example.com]" {
		t.Fatalf("the timeline holds %v", d)
	}
	// The control plane restarts: a manager of its own gives the same key again,
	// and the timeline doesn't say so twice.
	again := &Manager{DB: m.DB, Keeper: m.Keeper, Agents: agents}
	again.deliver(t.Context(), wanted, st)
	if len(agents["srv_1"].puts) != 2 || len(deliveries(t, m)) != 1 {
		t.Errorf("after a restart: given %v, timeline %v", agents["srv_1"].puts, deliveries(t, m))
	}
	// The certificate went and came back, so its generation began again, with a
	// new key.
	st = storeIssued(t, m, "crt_wild", "*.example.com", "example.com")
	again.deliver(t.Context(), wanted, st)
	if d := deliveries(t, m); len(d) != 2 {
		t.Errorf("a new key at the same generation isn't on the timeline: %v", d)
	}
}
