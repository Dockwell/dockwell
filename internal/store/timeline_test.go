// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// The audit events recorded before migration 0009 kept a token's or
// passkey's ID only in their free text, and most kept no surface. The
// migration fills in each one's target and surface from what its writer
// wrote, so the timeline reads columns, never the text.
func TestUpgradeFillsInWhatAuditEventsActedOn(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		location := storetest.New(t, b)
		old, err := store.OpenAt(t.Context(), location, 8)
		if err != nil {
			t.Fatal(err)
		}
		_ = old.Close()
		const (
			mira  = "usr_01JC0000000000000000000000"
			tok   = "tok_01JC0000000000000000000001"
			key   = "pky_01JC0000000000000000000002"
			hz2   = "srv_01JC0000000000000000000003"
			other = "usr_01JC0000000000000000000004"
		)
		events := []struct{ id, actor, action, subject, detail, surface string }{
			{"evt_1", "user:" + mira, "token.create", mira, `token ` + tok + ` "deploy", read_write, never expires`, ""},
			{"evt_2", "token:" + tok, "token.revoke", mira, "token " + tok, ""},
			{"evt_3", "user:" + mira, "passkey.add", mira, key + " for dockwell.example from 192.0.2.7", ""},
			{"evt_4", "user:" + mira, "passkey.remove", mira, key, ""},
			{"evt_5", "server:" + hz2, "server.join", hz2, "hz-2 joined with jtk_1 from 192.0.2.8", ""},
			{"evt_6", "server:" + hz2, "server.renew", hz2, "certificate 7", ""},
			{"evt_7", "cli:mira@hz-1", "sign_in_link.create", other, "lnk_1 expires soon", ""},
			{"evt_8", "user:" + mira, "auth.sign_in", mira, "from 192.0.2.7", ""},
			{"evt_9", "dockwell", "change.applied", "chg_1", "Run web", "automatic"},
		}
		for i, e := range events {
			storetest.Exec(t, location, fmt.Sprintf(
				`INSERT INTO audit_events (id, at, actor, action, subject, detail, surface) VALUES ('%s', %d, '%s', '%s', '%s', '%s', '%s')`,
				e.id, i+1, e.actor, e.action, e.subject, e.detail, e.surface))
		}

		db, err := store.Open(t.Context(), location)
		if err != nil {
			t.Fatalf("upgrading: %v", err)
		}
		defer db.Close()
		want := map[string][2]string{ // target, surface
			"evt_1": {tok, "panel"},
			"evt_2": {tok, "api"},
			"evt_3": {key, "panel"},
			"evt_4": {key, "panel"},
			"evt_5": {hz2, "cli"},
			"evt_6": {hz2, "automatic"},
			"evt_7": {other, "cli"},
			"evt_8": {"", "panel"},
			"evt_9": {"", "automatic"},
		}
		_ = db.View(t.Context(), func(tx *store.Tx) error {
			got, err := tx.ListEvents(t.Context(), 100)
			if err != nil || len(got) != len(want) {
				t.Fatalf("ListEvents = %v, %v", got, err)
			}
			for _, e := range got {
				if w := want[e.ID]; e.Target != w[0] || e.Surface != w[1] {
					t.Errorf("%s %s: target %q, surface %q, want %q, %q", e.ID, e.Action, e.Target, e.Surface, w[0], w[1])
				}
			}
			return nil
		})
	})
}

// Paging forward through the timeline reads every entry, however many
// share a moment, and finds the changes discarded since, by when they
// were discarded.
func TestTheTimelinePagesForward(t *testing.T) { storetest.Each(t, testTheTimelinePagesForward) }

func testTheTimelinePagesForward(t *testing.T, b store.Backend) {
	db, _ := storetest.Open(t, b)
	ctx := t.Context()
	at := time.UnixMilli(1_000_000)
	var want []string
	err := db.Update(ctx, func(tx *store.Tx) error {
		for i := range 25 {
			id := fmt.Sprintf("evt_%02d", i)
			want = append(want, id)
			// Five at each moment.
			if err := tx.RecordEvent(ctx, store.Event{
				ID: id, At: at.Add(time.Duration(i/5) * time.Millisecond), Actor: "server:srv_A", Action: "server.renew",
				Subject: "srv_A", Target: "srv_A",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	revise(t, db, func(tx *store.RevisionTx) error {
		for _, id := range []string{"chg_kept", "chg_dropped"} {
			if err := tx.CreateChange(ctx, store.Change{
				ID: id, Summary: id, Actor: "user:usr_1", State: store.ChangePlanned, Fingerprint: "f", Plan: []byte("{}"),
				CreatedAt: at, Items: []store.ChangeItem{{Kind: "project", Scope: "org_1", Name: id, Spec: []byte{1}}},
			}); err != nil {
				return err
			}
		}
		if err := tx.SetChangeState(ctx, "chg_dropped", store.ChangeDiscarded, "", time.Time{}); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: "evt_discard", At: at.Add(time.Hour), Actor: "user:usr_1", Action: "change.discarded", Subject: "chg_dropped",
		})
	})
	want = append([]string{"chg_kept"}, want...)

	f := store.TimelineFilter{Changes: true, Actions: []string{"server.renew"}}
	var got []string
	var discarded []store.TimelinePlace
	after := &store.TimelineCursor{At: at}
	_ = db.View(ctx, func(tx *store.Tx) error {
		for range 20 {
			places, err := tx.TimelinePlaces(ctx, store.TimelineQuery{Filter: f, After: after, Discarded: true, Limit: 4})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range places {
				if p.Discarded {
					discarded = append(discarded, p)
				} else {
					got = append(got, p.ID)
				}
			}
			if len(places) < 4 {
				return nil
			}
			last := places[len(places)-1]
			after = &store.TimelineCursor{At: last.At, ID: last.ID}
		}
		t.Fatal("paging forward never reached the end")
		return nil
	})
	if !slices.Equal(got, want) {
		t.Errorf("paging forward read %v, want %v", got, want)
	}
	if len(discarded) != 1 || discarded[0].ID != "chg_dropped" || !discarded[0].At.Equal(at.Add(time.Hour)) || !discarded[0].Change {
		t.Errorf("the discarded changes are %+v, want chg_dropped at when it was discarded", discarded)
	}
}
