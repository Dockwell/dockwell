// SPDX-License-Identifier: AGPL-3.0-only

package certificates

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// addServers puts live servers in the store, as joining does.
func addServers(t *testing.T, m *Manager, ids ...string) {
	t.Helper()
	now := time.Now()
	if err := m.DB.Update(t.Context(), func(tx *store.Tx) error {
		for _, s := range ids {
			if err := tx.CreateServer(t.Context(), store.Server{ID: s, Name: s, CreatedAt: now, CertSerial: "x", CertNotAfter: now.Add(time.Hour)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func revokeServer(t *testing.T, m *Manager, id string) {
	t.Helper()
	if err := m.DB.Update(t.Context(), func(tx *store.Tx) error { return tx.RevokeServer(t.Context(), id, time.Now()) }); err != nil {
		t.Fatal(err)
	}
}

// read gives back what the manager keeps: every holder's state and the keys given.
func read(t *testing.T, m *Manager) (map[string]*held, map[string]*given) {
	t.Helper()
	var st map[string]*held
	var gv map[string]*given
	if err := m.DB.View(t.Context(), func(tx *store.Tx) error {
		var err error
		if st, err = states(t.Context(), tx); err != nil {
			return err
		}
		gv, err = givenKeys(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return st, gv
}

// A server that goes takes its keys off the timeline of what's in use: each one
// whose holder still serves the servers left is ordered again at once, past the
// hour between orders and the week's cap, and the server's every key is marked to
// be revoked; what's no longer wanted is kept apart until then (ADR 0088).
func TestAServerThatGoesMarksItsKeys(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	gv := map[string]*given{
		"aa": {Holder: "crt_wild", Gen: 2, Servers: []string{"srv_1", "srv_2"}, NotAfter: now.Add(80 * 24 * time.Hour)},
		"bb": {Holder: "crt_wild", Gen: 1, Servers: []string{"srv_1"}, NotAfter: now.Add(20 * 24 * time.Hour), Retired: true},
		"cc": {Holder: "srv_1", Gen: 1, Servers: []string{"srv_1"}, NotAfter: now.Add(80 * 24 * time.Hour)},
		"dd": {Holder: "srv_2", Gen: 1, Servers: []string{"srv_2"}, NotAfter: now.Add(80 * 24 * time.Hour)},
	}
	st := map[string]*held{
		"crt_wild": {Gen: 2, Serial: "aa", Wanted: []string{"*.example.com"}, RenewAt: now.Add(50 * 24 * time.Hour), LastOrder: now.Add(-time.Minute)},
		"srv_1":    {Gen: 1, Serial: "cc"},
		"srv_2":    {Gen: 1, Serial: "dd"},
	}
	m := &Manager{}
	m.markGone(gv, st, map[string]string{"srv_2": "hz-2"}, nil)
	for serial, want := range map[string][]string{"aa": {"srv_1"}, "bb": {"srv_1"}, "cc": {"srv_1"}, "dd": nil} {
		if !slices.Equal(gv[serial].Gone, want) {
			t.Errorf("%s: gone %v, want %v", serial, gv[serial].Gone, want)
		}
	}
	if !slices.Equal(gv["aa"].Servers, []string{"srv_2"}) || len(gv["cc"].Servers) != 0 {
		t.Errorf("servers left: aa %v, cc %v", gv["aa"].Servers, gv["cc"].Servers)
	}
	if !st["crt_wild"].Rekey || st["srv_2"].Rekey {
		t.Errorf("rekey: wild %v, srv_2 %v", st["crt_wild"].Rekey, st["srv_2"].Rekey)
	}
	if !dueNow(st["crt_wild"], []string{"*.example.com"}, now) {
		t.Error("a key a server that's gone had waits for the hour between orders")
	}
	// Marking again changes nothing.
	m.markGone(gv, st, map[string]string{"srv_2": "hz-2"}, nil)
	if !slices.Equal(gv["aa"].Gone, []string{"srv_1"}) {
		t.Errorf("marked twice: %v", gv["aa"].Gone)
	}

	db := newManager(t)
	if err := db.DB.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.PutSetting(t.Context(), retiredSetting("bb"), []byte("sealed")); err != nil {
			return err
		}
		return pruneGiven(t.Context(), tx, gv, now.Add(30*24*time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	if gv["bb"] != nil || gv["aa"] == nil {
		t.Errorf("after bb's end, kept %v", gv)
	}
	_ = db.DB.View(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.GetSetting(t.Context(), retiredSetting("bb")); err == nil {
			t.Error("an ended certificate's copy is still kept")
		}
		return nil
	})
}

// A server that goes has every key it had revoked with the CA, each signed with its
// own key and the reason keyCompromise: a shared certificate once it's ordered
// again, with a new key, and given to the servers left, and the server's own as it's
// no longer wanted. Each revocation is on the gone server's timeline, and nothing is
// kept of the keys after (ADR 0088).
func TestAGoneServersKeysAreReplacedAndRevoked(t *testing.T) {
	bins := os.Getenv("DOCKWELL_TEST_PEBBLE")
	if bins == "" {
		t.Skip("DOCKWELL_TEST_PEBBLE isn't set, so this gets no certificate from a real CA. Set it to a directory with the pebble and pebble-challtestsrv binaries to run it")
	}
	p := startPebble(t, bins)
	m := newManager(t)
	m.Directory, m.HTTP = p.directory, p.client
	api := challtest{mgmt: p.management}
	m.ProviderAPI = func(context.Context, string) (dnsproviders.API, error) { return api, nil }
	agents := fakeAgents{"srv_1": {loads: true}, "srv_2": {loads: true}}
	m.Agents = agents
	addServers(t, m, "srv_1", "srv_2")
	confirm(t, m, map[string][]string{"srv_1": {"crt_wild", "srv_1"}, "srv_2": {"crt_wild"}})
	zones := map[string][]string{"test": {"example.com"}}
	wild := func(servers ...string) *want {
		return &want{Holder: "crt_wild", Name: "wild", Names: []string{"*.example.com"}, Provider: map[string]string{"*.example.com": "test"}, Servers: servers}
	}
	own := &want{Holder: "srv_1", Name: "srv_1", Names: []string{"one.example.com"}, Provider: map[string]string{"one.example.com": "test"}, Servers: []string{"srv_1"}}

	if err := m.tick(t.Context(), map[Holder]*want{"crt_wild": wild("srv_1", "srv_2"), "srv_1": own}, zones); err != nil {
		t.Fatal(err)
	}
	st, gv := read(t, m)
	oldWild, oldOwn := st["crt_wild"].Serial, st["srv_1"].Serial
	if oldWild == "" || oldOwn == "" || !slices.Equal(gv[oldWild].Servers, []string{"srv_1", "srv_2"}) || !slices.Equal(gv[oldOwn].Servers, []string{"srv_1"}) {
		t.Fatalf("given: %v, state %v", gv, st)
	}

	revokeServer(t, m, "srv_1")
	if err := m.tick(t.Context(), map[Holder]*want{"crt_wild": wild("srv_2")}, zones); err != nil {
		t.Fatal(err)
	}
	st, gv = read(t, m)
	newWild := st["crt_wild"].Serial
	if newWild == oldWild || st["crt_wild"].Rekey || st["srv_1"] != nil {
		t.Fatalf("after srv_1 went: wild %+v, own %+v", st["crt_wild"], st["srv_1"])
	}
	if got := agents["srv_2"].puts; len(got) != 2 || !slices.Equal(got[1], []string{"crt_wild"}) {
		t.Errorf("srv_2 was given %v, want the new wildcard", got)
	}
	if len(gv) != 1 || gv[newWild] == nil || !slices.Equal(gv[newWild].Servers, []string{"srv_2"}) {
		t.Errorf("kept after the revocations: %v", gv)
	}
	for _, serial := range []string{oldWild, oldOwn} {
		if s := certStatus(t, p, serial); s != "Revoked" {
			t.Errorf("%s is %s with the CA, want revoked", serial, s)
		}
		_ = m.DB.View(t.Context(), func(tx *store.Tx) error {
			if _, err := tx.GetSetting(t.Context(), retiredSetting(serial)); err == nil {
				t.Errorf("%s's copy is still kept", serial)
			}
			return nil
		})
	}
	if s := certStatus(t, p, newWild); s != "Valid" {
		t.Errorf("the new wildcard is %s", s)
	}
	revoked := 0
	var events []store.Event
	_ = m.DB.View(t.Context(), func(tx *store.Tx) error {
		var err error
		events, err = tx.ListEvents(t.Context(), 100)
		return err
	})
	for i := range events {
		if e := &events[i]; e.Action == ActionRevoke {
			revoked++
			if e.Subject != "srv_1" {
				t.Errorf("a revocation on %s's timeline", e.Subject)
			}
		}
	}
	if revoked != 2 {
		t.Errorf("%d revocations on the timeline, want 2", revoked)
	}
	var confirmed map[string]map[string]bool
	_ = m.DB.View(t.Context(), func(tx *store.Tx) error {
		var err error
		confirmed, err = revision.KeysConfirmed(t.Context(), tx)
		return err
	})
	if confirmed["srv_1"] != nil {
		t.Errorf("what srv_1 was let have is still kept: %v", confirmed)
	}
}

// A key still in use is revoked only once its holder has another, so the servers
// left aren't serving a revoked one while a new one could come; and a revocation
// the CA doesn't take is tried again later, saying why (ADR 0088).
func TestAKeyInUseIsReplacedBeforeItsRevoked(t *testing.T) {
	m := newManager(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	// A CA that's down holds the manager no longer than this.
	was := clientTimeout
	clientTimeout = 2 * time.Second
	t.Cleanup(func() { clientTimeout = was })
	var asked atomic.Int32
	ca := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.Error(w, "down for upkeep", http.StatusServiceUnavailable)
	}))
	t.Cleanup(ca.Close)
	m.Directory, m.HTTP = ca.URL+"/dir", ca.Client()
	st := map[string]*held{"crt_wild": {Gen: 2, Serial: "aa", Rekey: true}}
	wanted := map[Holder]*want{"crt_wild": {Holder: "crt_wild"}}
	gv := map[string]*given{
		"aa": {Holder: "crt_wild", Gen: 2, Gone: []string{"srv_1"}, NotAfter: now.Add(80 * 24 * time.Hour)},
		"cc": {Holder: "srv_1", Gen: 1, Gone: []string{"srv_1"}, Retired: true, NotAfter: now.Add(80 * 24 * time.Hour)},
	}
	if err := m.DB.Update(t.Context(), func(tx *store.Tx) error { return putJSON(t.Context(), tx, givenSetting, gv) }); err != nil {
		t.Fatal(err)
	}
	m.revokeDue(t.Context(), wanted, st)
	_, gv = read(t, m)
	if gv["aa"].Failures != 0 || gv["aa"].Message != "" {
		t.Errorf("a key still in use was revoked before its holder had another: %+v", gv["aa"])
	}
	if c := gv["cc"]; c.Failures != 1 || !strings.Contains(c.Message, "answered 503: down for upkeep") || !c.Next.Equal(now.Add(backoff(1))) || asked.Load() == 0 {
		t.Errorf("a revocation the CA didn't take: %+v, the CA asked %d times", c, asked.Load())
	}
	asked.Store(0)
	m.revokeDue(t.Context(), wanted, st)
	if asked.Load() != 0 {
		t.Error("a failed revocation was tried again before its wait")
	}
}

// certStatus is what Pebble says of a certificate: Valid or Revoked.
func certStatus(t *testing.T, p *pebble, serial string) string {
	t.Helper()
	resp, err := p.client.Get(p.admin + "/cert-status-by-serial/" + serial)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.Status
	}
	var out struct{ Status string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Status
}

// A key a server removed as compromised had is revoked at once, while its holder
// still serves it and no other has come, as a copy someone else may hold matters
// more than that gap; a revocation that fails is listed with why and since when,
// for an alert to say (ADR 0090).
func TestACompromisedServersKeyIsRevokedAtOnce(t *testing.T) {
	m := newManager(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	was := clientTimeout
	clientTimeout = 2 * time.Second
	t.Cleanup(func() { clientTimeout = was })
	var asked atomic.Int32
	ca := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.Error(w, "down for upkeep", http.StatusServiceUnavailable)
	}))
	t.Cleanup(ca.Close)
	m.Directory, m.HTTP = ca.URL+"/dir", ca.Client()

	// srv_1 goes as compromised, srv_3 as any server does.
	addServers(t, m, "srv_1", "srv_2", "srv_3")
	revokeServer(t, m, "srv_1")
	revokeServer(t, m, "srv_3")
	if err := m.DB.Update(t.Context(), func(tx *store.Tx) error {
		return tx.PutSetting(t.Context(), store.SettingCompromised("srv_1"), []byte(now.Format(time.RFC3339)))
	}); err != nil {
		t.Fatal(err)
	}
	st := map[string]*held{"crt_wild": {Gen: 2, Serial: "aa"}, "crt_api": {Gen: 1, Serial: "bb"}}
	gv := map[string]*given{
		"aa": {Holder: "crt_wild", Gen: 2, Servers: []string{"srv_1", "srv_2"}, Names: []string{"*.example.com"}, NotAfter: now.Add(80 * 24 * time.Hour)},
		"bb": {Holder: "crt_api", Gen: 1, Servers: []string{"srv_3", "srv_2"}, Names: []string{"api.example.com"}, NotAfter: now.Add(80 * 24 * time.Hour)},
	}
	live := map[string]string{"srv_2": "hz-2"}
	var compromised map[string]bool
	if err := m.DB.View(t.Context(), func(tx *store.Tx) error {
		var err error
		compromised, err = compromisedOf(t.Context(), tx, gv, live)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m.markGone(gv, st, live, compromised)
	if !gv["aa"].Compromised || gv["bb"].Compromised || !st["crt_wild"].Rekey {
		t.Fatalf("marked: aa %+v, bb %+v, wild %+v", gv["aa"], gv["bb"], st["crt_wild"])
	}
	if err := m.DB.Update(t.Context(), func(tx *store.Tx) error { return putJSON(t.Context(), tx, givenSetting, gv) }); err != nil {
		t.Fatal(err)
	}
	wanted := map[Holder]*want{"crt_wild": {Holder: "crt_wild"}, "crt_api": {Holder: "crt_api"}}
	m.revokeDue(t.Context(), wanted, st)
	_, gv = read(t, m)
	if a := gv["aa"]; a.Failures != 1 || !a.FailingSince.Equal(now) || !strings.Contains(a.Message, "down for upkeep") || asked.Load() == 0 {
		t.Errorf("the compromised server's key, still served, wasn't tried at once: %+v, the CA asked %d times", a, asked.Load())
	}
	if b := gv["bb"]; b.Failures != 0 {
		t.Errorf("a key in use whose server went uncompromised was tried before its holder had another: %+v", b)
	}
	list, err := m.Revocations(t.Context())
	if err != nil || len(list) != 2 || list[0].Serial != "aa" || !list[0].Compromised || list[0].Message == "" || !list[0].Since.Equal(now) ||
		list[1].Serial != "bb" || list[1].Message != "" || !slices.Equal(list[1].Servers, []string{"srv_3"}) {
		t.Errorf("the revocations listed: %+v, %v", list, err)
	}
}
