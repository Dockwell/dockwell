// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A check and a person each write their own of a service's updates, so neither
// undoes the other, and a service that's gone is forgotten.
func TestServiceUpdatesKeepWhatEachWrote(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		t0 := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
		week := t0.Add(7 * 24 * time.Hour)
		err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.SetServiceUpdateSkipped(ctx, "svc_a", `["17.0"]`); err != nil {
				return err
			}
			if err := tx.PutServiceUpdateCheck(ctx, store.ServiceUpdate{
				ServiceID: "svc_a", ServerID: "srv_1", Image: "postgres:16.3", CheckedAt: t0, TriedAt: t0, NextAt: t0.Add(6 * time.Hour),
				State: 2, Found: `[{"to":"16.4"}]`,
			}); err != nil {
				return err
			}
			if err := tx.SetServiceUpdateSnooze(ctx, "svc_a", week, "usr_1"); err != nil {
				return err
			}
			// A check that couldn't answer leaves when it last answered unset.
			return tx.PutServiceUpdateCheck(ctx, store.ServiceUpdate{ServiceID: "svc_b", TriedAt: t0, State: 3, Problem: 4})
		})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]store.ServiceUpdate
		if err := db.View(ctx, func(tx *store.Tx) (err error) {
			got, err = tx.ServiceUpdates(ctx, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		a := got["svc_a"]
		if a.Skipped != `["17.0"]` || a.Found != `[{"to":"16.4"}]` || a.State != 2 || !a.CheckedAt.Equal(t0) || !a.SnoozeUntil.Equal(week) || a.SnoozedBy != "usr_1" || a.Image != "postgres:16.3" {
			t.Errorf("svc_a = %+v, want the check's, the skip and the snooze together", a)
		}
		if b := got["svc_b"]; !b.CheckedAt.IsZero() || b.Problem != 4 || !b.SnoozeUntil.IsZero() {
			t.Errorf("svc_b = %+v, want a check that didn't answer, and no snooze", b)
		}
		err = db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.SetServiceUpdateSnooze(ctx, "svc_a", time.Time{}, ""); err != nil {
				return err
			}
			n, err := tx.DeleteServiceUpdates(ctx, []string{"svc_b", "svc_gone"})
			if n != 1 {
				t.Errorf("DeleteServiceUpdates forgot %d, want 1", n)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.View(ctx, func(tx *store.Tx) (err error) {
			got, err = tx.ServiceUpdates(ctx, []string{"svc_a", "svc_b"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !got["svc_a"].SnoozeUntil.IsZero() || got["svc_a"].Skipped != `["17.0"]` {
			t.Errorf("after the snooze ended and svc_b went: %+v", got)
		}
	})
}
