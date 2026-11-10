// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/store"
)

// A server gets its host as its agent connects, by Dockwell's own change, and
// only once (ADR 0064). An agent that doesn't run hosts yet, as an older one,
// gets none and holds up no other server's.
func TestAServerGetsItsHostAsItsAgentConnects(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	older := f.addServer(t, false)
	web := f.addHostServer(t, &packages{installed: map[string]string{}})

	hosts := func() []string {
		t.Helper()
		var scopes []string
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			have, err := tx.ListKind(t.Context(), string(host.Kind))
			for i := range have {
				scopes = append(scopes, have[i].Scope)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return scopes
	}
	deadline := time.Now().Add(15 * time.Second)
	for !slices.Equal(hosts(), []string{web.id.ServerID}) {
		if time.Now().After(deadline) {
			t.Fatalf("the hosts are %v, want web-1's alone", hosts())
		}
		time.Sleep(50 * time.Millisecond)
	}
	made := 0
	for _, ch := range listChanges(t, c) {
		if ch.GetSummary() != HostsSummary {
			continue
		}
		made++
		if ch.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || ch.GetProposedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL {
			t.Errorf("the host's change is %v, by %v", ch.GetState(), ch.GetProposedBy())
		}
	}
	if made != 1 {
		t.Errorf("%d changes made hosts, want 1", made)
	}

	// Again, with nothing missing that can be made, it makes nothing.
	h := &Hosts{DB: f.store, Engine: f.engine, Agents: f.agents}
	if err := h.Make(t.Context()); err != nil {
		t.Fatalf("making hosts again = %v, though the older agent can't run one", err)
	}
	if after := hosts(); !slices.Equal(after, []string{web.id.ServerID}) || slices.Contains(after, older.id.ServerID) {
		t.Errorf("the hosts are %v after another round", after)
	}
}
