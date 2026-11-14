// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A backup of one database alone is kept with the one it holds, listed with its
// volume's backups or apart, and isn't the volume's newest backup to the alert that
// says how old that is: it holds one app's database, not the volume (ADR 0084).
func TestABackupOfOneDatabase(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Millisecond)
		err := db.Update(ctx, func(tx *store.Tx) error {
			for i, one := range []string{"", "shop"} {
				link := ""
				if one != "" {
					link = "lnk_shop"
				}
				if err := tx.CreateBackup(ctx, store.Backup{
					ID: "bkp_" + string(rune('1'+i)), EnvironmentID: "env_1", Volume: "data", VolumeID: "res_vol", ServerID: "srv_1",
					State: store.BackupDone, Actor: "usr_1", StartedAt: now.Add(time.Duration(i) * time.Minute), OneDatabase: one, ForLink: link,
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *store.Tx) error {
			ids := func(f store.BackupFilter) []string {
				t.Helper()
				list, err := tx.ListBackups(ctx, f)
				if err != nil {
					t.Fatal(err)
				}
				var out []string
				for _, b := range list {
					out = append(out, b.ID+":"+b.OneDatabase)
				}
				slices.Sort(out)
				return out
			}
			if got := ids(store.BackupFilter{VolumeID: "res_vol"}); !slices.Equal(got, []string{"bkp_1:", "bkp_2:shop"}) {
				t.Errorf("the volume's backups are %v", got)
			}
			if got := ids(store.BackupFilter{VolumeID: "res_vol", OneDatabase: "shop"}); !slices.Equal(got, []string{"bkp_2:shop"}) {
				t.Errorf("shop's backups are %v", got)
			}
			if got := ids(store.BackupFilter{ForLink: "lnk_shop"}); !slices.Equal(got, []string{"bkp_2:shop"}) {
				t.Errorf("the backups taken for the shop's link are %v", got)
			}
			if got := ids(store.BackupFilter{VolumeID: "res_vol", OneDatabase: "-"}); !slices.Equal(got, []string{"bkp_1:"}) {
				t.Errorf("the volume's whole backups are %v", got)
			}
			latest, err := tx.LatestBackups(ctx, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if got := latest["res_vol"]; got.ID != "bkp_1" {
				t.Errorf("the volume's newest backup reads as %+v", got)
			}
			return nil
		})
	})
}
