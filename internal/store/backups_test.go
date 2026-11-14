// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"cmp"
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

// Backups refiled under a moved volume go with it on the server it moved on only, in
// its new environment; those taken for a moved link go with the link (ADR 0084, #572).
func TestBackupsFollowWhatMoved(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Millisecond)
		err := db.Update(ctx, func(tx *store.Tx) error {
			for _, bk := range []store.Backup{
				{ID: "bkp_a", ServerID: "srv_1"},
				{ID: "bkp_b", ServerID: "srv_2"},
				{ID: "bkp_c", ServerID: "srv_1", OneDatabase: "shop", ForLink: "lnk_old"},
				{ID: "bkp_d", ServerID: "srv_3", PolicyID: "pol_old"},
			} {
				if bk.ID == "bkp_d" {
					bk.VolumeID = "res_other"
				}
				bk.EnvironmentID, bk.Volume, bk.State, bk.Actor, bk.StartedAt = "env_old", "data", store.BackupDone, "usr_1", now
				bk.VolumeID = cmp.Or(bk.VolumeID, "res_old")
				if err := tx.CreateBackup(ctx, bk); err != nil {
					return err
				}
			}
			n, err := tx.RefileBackups(ctx, store.Refile{Server: "srv_1", FromVolume: "res_old", ToVolume: "res_new", ToEnvironment: "env_new"})
			if err != nil || n != 2 {
				t.Errorf("refiling the volume's moved %d, %v", n, err)
			}
			n, err = tx.RefileBackups(ctx, store.Refile{FromLink: "lnk_old", ToLink: "lnk_new"})
			if err != nil || n != 1 {
				t.Errorf("refiling the link's moved %d, %v", n, err)
			}
			// A policy's go with it, and with no server named, a volume's on every one.
			n, err = tx.RefileBackups(ctx, store.Refile{FromPolicy: "pol_old", ToPolicy: "pol_new", FromVolume: "res_other", ToVolume: "res_moved", ToEnvironment: "env_new"})
			if err != nil || n != 2 {
				t.Errorf("refiling the policy's and on every server moved %d, %v", n, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *store.Tx) error {
			for id, want := range map[string]string{"bkp_a": "env_new res_new  ", "bkp_b": "env_old res_old  ", "bkp_c": "env_new res_new lnk_new ", "bkp_d": "env_new res_moved  pol_new"} {
				got, err := tx.GetBackup(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if s := got.EnvironmentID + " " + got.VolumeID + " " + got.ForLink + " " + got.PolicyID; s != want {
					t.Errorf("%s is filed as %q, want %q", id, s, want)
				}
			}
			return nil
		})
	})
}
