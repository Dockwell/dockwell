// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// The outbox holds one message of a status an alert owes a channel at a time, has
// due what's due, and forgets, past retention, alerts that closed with nothing
// left owed, what they sent, and messages to channels.
func TestTheAlertsOutbox(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		old, recent := t0.Add(-100*24*time.Hour), t0.Add(-time.Hour)
		err := db.Update(ctx, func(tx *store.Tx) error {
			for _, a := range []store.Alert{
				{ID: "alt_old", RuleID: "alr_1", RuleName: "app-down", Thing: "svc:a", Title: "a", Detail: "", OpenedAt: old, ResolvedAt: &old},
				{ID: "alt_owes", RuleID: "alr_1", RuleName: "app-down", Thing: "svc:b", Title: "b", Detail: "", OpenedAt: old, ResolvedAt: &old},
				{ID: "alt_new", RuleID: "alr_1", RuleName: "app-down", Thing: "svc:c", Title: "c", Detail: "", OpenedAt: recent},
			} {
				if err := tx.OpenAlert(ctx, a); err != nil {
					return err
				}
			}
			for _, s := range []store.AlertSend{
				{ID: "snd_1", AlertID: "alt_old", RuleID: "alr_1", ChannelID: "ach_1", Status: "firing", QueuedAt: old, DueAt: old},
				{ID: "snd_2", AlertID: "alt_owes", RuleID: "alr_1", ChannelID: "ach_1", Status: "resolved", QueuedAt: old, DueAt: t0.Add(time.Minute)},
				{ID: "snd_3", AlertID: "alt_new", RuleID: "alr_1", ChannelID: "ach_1", Status: "firing", QueuedAt: recent, DueAt: recent},
			} {
				if err := tx.QueueSend(ctx, s); err != nil {
					return err
				}
			}
			again := store.AlertSend{ID: "snd_4", AlertID: "alt_new", RuleID: "alr_1", ChannelID: "ach_1", Status: "firing", QueuedAt: t0, DueAt: t0}
			if err := tx.QueueSend(ctx, again); !errors.Is(err, store.ErrConflict) {
				t.Errorf("a second firing message owed to one channel = %v, want ErrConflict", err)
			}
			if err := tx.FinishSend(ctx, "snd_1", old, true, "200 OK"); err != nil {
				return err
			}
			return tx.AddAlertDelivery(ctx, store.AlertDelivery{ID: "dlv_1", ChannelID: "ach_1", ChannelName: "ops", At: old, OK: true, Said: "200 OK", About: "a"})
		})
		if err != nil {
			t.Fatal(err)
		}
		err = db.Update(ctx, func(tx *store.Tx) error {
			due, err := tx.DueSends(ctx, t0, 10)
			if err != nil {
				return err
			}
			if len(due) != 1 || due[0].ID != "snd_3" {
				t.Errorf("due at %v: %+v, want the recent firing message alone", t0, due)
			}
			if err := tx.RetrySend(ctx, "snd_3", t0.Add(10*time.Second), "503 Service Unavailable"); err != nil {
				return err
			}
			if due, _ := tx.DueSends(ctx, t0, 10); len(due) != 0 {
				t.Errorf("a message tried again later is due now: %+v", due)
			}
			// A far end that asks to slow down puts it off, which isn't a try.
			if err := tx.PostponeSend(ctx, "snd_3", t0.Add(2*time.Minute), "429 Too Many Requests"); err != nil {
				return err
			}
			of := func(at time.Time) *store.AlertSend {
				due, _ := tx.DueSends(ctx, at, 10)
				for i := range due {
					if due[i].ID == "snd_3" {
						return &due[i]
					}
				}
				return nil
			}
			if s := of(t0.Add(time.Minute)); s != nil {
				t.Errorf("a message put off for two minutes is due after one: %+v", s)
			}
			if s := of(t0.Add(2 * time.Minute)); s == nil || s.Tries != 1 || s.Said != "429 Too Many Requests" {
				t.Errorf("a message put off is %+v, want it due with the one try it had", s)
			}
			gone, err := tx.PruneAlerts(ctx, t0.Add(-90*24*time.Hour))
			if err != nil {
				return err
			}
			if gone != 1 {
				t.Errorf("pruning forgot %d alerts, want the old one that owes nothing", gone)
			}
			left, err := tx.AlertsByID(ctx, []string{"alt_old", "alt_owes", "alt_new"})
			if err != nil {
				return err
			}
			if len(left) != 2 {
				t.Errorf("after pruning, %d of the alerts are left, want the one that still owes a message, and the recent one", len(left))
			}
			if d, _ := tx.RecentDeliveries(ctx, 10); len(d) != 0 {
				t.Errorf("an old message to a channel was kept: %+v", d)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// An acknowledgement is who has an open alert and since when. It replaces whoever
// had it, is taken back only where there is one, never touches an alert that
// resolved, and stays on the alert once it resolves.
func TestWhoHasAnAlert(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		update := func(f func(tx *store.Tx) error) {
			t.Helper()
			if err := db.Update(ctx, f); err != nil {
				t.Fatal(err)
			}
		}
		update(func(tx *store.Tx) error {
			for _, a := range []store.Alert{
				{ID: "alt_open", RuleID: "alr_1", RuleName: "app-down", Thing: "svc:a", Title: "a", OpenedAt: t0},
				{ID: "alt_done", RuleID: "alr_1", RuleName: "app-down", Thing: "svc:b", Title: "b", OpenedAt: t0, ResolvedAt: &t0},
			} {
				if err := tx.OpenAlert(ctx, a); err != nil {
					return err
				}
			}
			return nil
		})
		update(func(tx *store.Tx) error {
			a, err := tx.GetAlert(ctx, "alt_open")
			if err != nil || a.AcknowledgedAt != nil || a.AcknowledgedBy != "" {
				t.Errorf("a new alert is %+v, %v; want nobody's", a, err)
			}
			if had, err := tx.UnacknowledgeAlert(ctx, "alt_open", "usr_mira"); err != nil || had {
				t.Errorf("taking back what nobody has = %v, %v", had, err)
			}
			for _, who := range []string{"usr_mira", "usr_leo"} {
				if ok, err := tx.AcknowledgeAlert(ctx, "alt_open", t0.Add(time.Minute), who); err != nil || !ok {
					t.Errorf("AcknowledgeAlert by %s = %v, %v", who, ok, err)
				}
			}
			if ok, err := tx.AcknowledgeAlert(ctx, "alt_done", t0, "usr_mira"); err != nil || ok {
				t.Errorf("acknowledging an alert that resolved = %v, %v", ok, err)
			}
			if ok, err := tx.AcknowledgeAlert(ctx, "alt_nothing", t0, "usr_mira"); err != nil || ok {
				t.Errorf("acknowledging an alert that isn't there = %v, %v", ok, err)
			}
			return nil
		})
		update(func(tx *store.Tx) error {
			open, err := tx.OpenAlerts(ctx)
			if err != nil || len(open) != 1 || open[0].AcknowledgedBy != "usr_leo" || open[0].AcknowledgedAt == nil || !open[0].AcknowledgedAt.Equal(t0.Add(time.Minute)) {
				t.Errorf("OpenAlerts = %+v, %v; want usr_leo to have it since a minute past", open, err)
			}
			// Only whoever has it takes it back: someone it was taken over from doesn't.
			if had, err := tx.UnacknowledgeAlert(ctx, "alt_open", "usr_mira"); err != nil || had {
				t.Errorf("taking back someone else's acknowledgement = %v, %v", had, err)
			}
			if a, err := tx.GetAlert(ctx, "alt_open"); err != nil || a.AcknowledgedBy != "usr_leo" {
				t.Errorf("after that, the alert is %+v, %v; want usr_leo still on it", a, err)
			}
			if had, err := tx.UnacknowledgeAlert(ctx, "alt_open", "usr_leo"); err != nil || !had {
				t.Errorf("taking back your own = %v, %v", had, err)
			}
			if ok, err := tx.AcknowledgeAlert(ctx, "alt_open", t0.Add(2*time.Minute), "usr_leo"); err != nil || !ok {
				t.Errorf("acknowledging it again = %v, %v", ok, err)
			}
			// Resolving it keeps who had it, and nothing takes that back after.
			if err := tx.ResolveAlert(ctx, "alt_open", t0.Add(time.Hour), "", false); err != nil {
				return err
			}
			if had, err := tx.UnacknowledgeAlert(ctx, "alt_open", "usr_leo"); err != nil || had {
				t.Errorf("taking back the acknowledgement of an alert that resolved = %v, %v", had, err)
			}
			a, err := tx.GetAlert(ctx, "alt_open")
			if err != nil || a.AcknowledgedBy != "usr_leo" {
				t.Errorf("resolved, the alert is %+v, %v; want it to keep who had it", a, err)
			}
			return nil
		})
	})
}
