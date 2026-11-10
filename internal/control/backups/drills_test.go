// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A scheduled drill that didn't run tries again at the policy's next run, and one
// that ran waits for the next drill (ADR 0068).
func TestADrillThatDidntRunTriesAtTheNextRun(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	ctx := t.Context()
	sunday := time.Date(2026, 11, 8, 4, 0, 0, 0, time.UTC) // an hour after a daily 03:00
	p := policy{id: "res_pol", madeAt: sunday.AddDate(0, -1, 0), spec: &resourcev1.BackupPolicy{
		Volume: "data", Every: resourcev1.BackupPolicy_EVERY_DAY, At: "03:00", TimeZone: "UTC", Keep: 7,
		Drill: resourcev1.BackupPolicy_DRILL_WEEKLY,
	}}
	update := func(fn func(tx *store.Tx) error) {
		t.Helper()
		if err := db.Update(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	update(func(tx *store.Tx) error {
		return tx.CreateBackup(ctx, store.Backup{
			ID: "bkp_1", EnvironmentID: "env_1", Volume: "data", VolumeID: "res_vol", ServerID: "srv_1",
			State: store.BackupDone, Actor: "system", StartedAt: sunday.Add(-time.Hour), PolicyID: p.id,
		})
	})
	drilled := func(id, result string, at time.Time) {
		t.Helper()
		update(func(tx *store.Tx) error {
			d := store.Drill{
				ID: id, BackupID: "bkp_1", EnvironmentID: "env_1", VolumeID: "res_vol", ServerID: "srv_1",
				Result: store.DrillRunning, Actor: "system", StartedAt: at, PolicyID: p.id, BackupAt: sunday.Add(-time.Hour),
			}
			if err := tx.CreateDrill(ctx, d); err != nil {
				return err
			}
			d.Result, d.FinishedAt = result, &at
			return tx.FinishDrill(ctx, d)
		})
	}
	due := func(now time.Time) bool {
		t.Helper()
		var ok bool
		if err := db.View(ctx, func(tx *store.Tx) (err error) {
			_, ok, err = drillIsDue(ctx, tx, p, "srv_1", now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	drilled("drl_1", store.DrillNotChecked, sunday)
	if due(sunday.Add(23*time.Hour + 30*time.Minute)) {
		t.Error("a drill that didn't run is due again before the policy's next run")
	}
	if !due(sunday.Add(24*time.Hour + time.Minute)) {
		t.Error("a drill that didn't run Sunday isn't due an hour after Monday's run")
	}
	drilled("drl_2", store.DrillPassed, sunday.Add(24*time.Hour))
	if due(sunday.Add(48*time.Hour + time.Minute)) {
		t.Error("a drill that passed Monday is due again Tuesday")
	}
	if !due(sunday.AddDate(0, 0, 7).Add(time.Minute)) {
		t.Error("a drill that passed isn't due the Sunday after")
	}
}
