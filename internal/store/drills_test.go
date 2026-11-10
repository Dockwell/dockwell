// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Restore drills, as the control plane keeps them (ADR 0068): each backup's newest,
// the running ones, a volume's, and none once their backup goes.
func TestRestoreDrills(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Millisecond)
		update := func(fn func(tx *store.Tx) error) {
			t.Helper()
			if err := db.Update(ctx, fn); err != nil {
				t.Fatal(err)
			}
		}
		update(func(tx *store.Tx) error {
			for _, id := range []string{"bkp_1", "bkp_2"} {
				if err := tx.CreateBackup(ctx, store.Backup{ID: id, EnvironmentID: "env_1", Volume: "data", VolumeID: "res_vol", ServerID: "srv_1", State: store.BackupDone, Actor: "usr_1", StartedAt: now}); err != nil {
					return err
				}
			}
			for i, d := range []store.Drill{
				{ID: "drl_1", BackupID: "bkp_1", Result: store.DrillRunning},
				{ID: "drl_2", BackupID: "bkp_1", Result: store.DrillRunning},
				{ID: "drl_3", BackupID: "bkp_2", Result: store.DrillRunning},
			} {
				d.EnvironmentID, d.VolumeID, d.ServerID, d.Actor = "env_1", "res_vol", "srv_1", "usr_1"
				d.StartedAt = now.Add(time.Duration(i) * time.Minute)
				if err := tx.CreateDrill(ctx, d); err != nil {
					return err
				}
			}
			return nil
		})
		finished := now.Add(time.Hour)
		update(func(tx *store.Tx) error {
			return tx.FinishDrill(ctx, store.Drill{ID: "drl_2", Result: store.DrillFailed, Why: "orders.items has no rows, and the dump fills it", Detail: "ERROR: '...'", RestoredBytes: 4096, Tables: 12, FinishedAt: &finished})
		})
		var newest map[string]store.Drill
		var running, volume []store.Drill
		if err := db.View(ctx, func(tx *store.Tx) (err error) {
			if newest, err = tx.NewestDrills(ctx, []string{"bkp_1", "bkp_2", "bkp_none"}); err != nil {
				return err
			}
			if running, err = tx.ListDrills(ctx, store.DrillFilter{Result: store.DrillRunning}); err != nil {
				return err
			}
			volume, err = tx.ListDrills(ctx, store.DrillFilter{VolumeID: "res_vol"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		got := newest["bkp_1"]
		if got.ID != "drl_2" || got.Result != store.DrillFailed || got.Tables != 12 || got.RestoredBytes != 4096 || got.FinishedAt == nil || !got.FinishedAt.Equal(finished) || got.Why == "" {
			t.Errorf("bkp_1's newest drill is %+v", got)
		}
		if newest["bkp_2"].ID != "drl_3" || len(newest) != 2 {
			t.Errorf("newest drills %v", newest)
		}
		if len(running) != 2 || running[0].ID != "drl_3" || running[1].ID != "drl_1" || len(volume) != 3 {
			t.Errorf("running %v, the volume's %d", running, len(volume))
		}

		update(func(tx *store.Tx) error { return tx.DeleteBackup(ctx, "bkp_1") })
		if err := db.View(ctx, func(tx *store.Tx) error {
			left, err := tx.ListDrills(ctx, store.DrillFilter{VolumeID: "res_vol"})
			if err == nil && (len(left) != 1 || left[0].ID != "drl_3") {
				t.Errorf("once bkp_1 went, its drills left %v", left)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}
