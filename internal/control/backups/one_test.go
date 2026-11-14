// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A backup of one app's database alone is kept as that only where the agent says it
// dumped that database: one from before ignores what it's asked and backs up the
// whole volume, which fails, as it isn't the app's to restore (ADR 0084).
func TestAnAgentThatBacksUpTheWholeVolumeIsntKeptAsTheApps(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	h := &Handler{db: db, logger: slog.New(slog.DiscardHandler)}
	for _, tc := range []struct {
		id, said string
		want     string
	}{
		{"bkp_old", "", store.BackupFailed},
		{"bkp_other", "blog", store.BackupFailed},
		{"bkp_new", "shop", store.BackupDone},
	} {
		b := store.Backup{
			ID: tc.id, EnvironmentID: "env_1", Volume: "data", VolumeID: "vol_1", ServerID: "srv_A", State: store.BackupRunning,
			StartedAt: time.Now(), OneDatabase: "shop", ForLink: "lnk_1",
		}
		if err := db.Update(t.Context(), func(tx *store.Tx) error { return tx.CreateBackup(t.Context(), b) }); err != nil {
			t.Fatal(err)
		}
		h.finish(t.Context(), b, &agentv1.BackupResult{Database: "PostgreSQL", SizeBytes: 10, OneDatabase: tc.said}, nil, store.SurfaceAutomatic)
		var got store.Backup
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			var err error
			got, err = tx.GetBackup(t.Context(), tc.id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got.State != tc.want {
			t.Errorf("an agent that said it dumped %q: %s (%s)", tc.said, got.State, got.Error)
		}
		if tc.want == store.BackupFailed && !strings.Contains(got.Error, "backed up the whole volume, not shop alone") {
			t.Errorf("it says %q", got.Error)
		}
	}
}
