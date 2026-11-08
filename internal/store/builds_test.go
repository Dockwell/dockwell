// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A build's log keeps its first lines and its last, and says how many it left out
// between; a service keeps the logs of its newest builds.
func TestABuildsLogKeepsItsStartAndItsEnd(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
		err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateBuild(ctx, store.Build{ID: "bld_1", ChangeID: "chg_1", ResourceID: "svc_1", EnvironmentID: "env_1", Service: "web", ServerID: "srv_1", State: store.BuildQueued, CreatedAt: at}); err != nil {
				return err
			}
			total := store.KeepFirst + store.KeepLast + 500
			for start := 0; start < total; start += 1000 {
				var lines []store.BuildLine
				for i := start; i < min(start+1000, total); i++ {
					lines = append(lines, store.BuildLine{At: at, Text: fmt.Sprintf("line %d", i+1)})
				}
				if _, err := tx.AddBuildLines(ctx, "bld_1", lines); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var kept []store.BuildLine
		var build store.Build
		err = db.View(ctx, func(tx *store.Tx) error {
			var err error
			if build, err = tx.GetBuild(ctx, "bld_1"); err != nil {
				return err
			}
			kept, err = tx.BuildLines(ctx, "bld_1", 0, 100000)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(kept) != store.KeepFirst+store.KeepLast || build.LeftOut != 500 || build.Lines != int64(store.KeepFirst+store.KeepLast+500) {
			t.Fatalf("the log keeps %d lines and says %d of %d were left out", len(kept), build.LeftOut, build.Lines)
		}
		if kept[store.KeepFirst-1].Text != "line 1000" || kept[store.KeepFirst].Text != "line 1501" || kept[len(kept)-1].Text != "line 10500" {
			t.Errorf("the log keeps %q, then %q, and ends %q", kept[store.KeepFirst-1].Text, kept[store.KeepFirst].Text, kept[len(kept)-1].Text)
		}

		err = db.Update(ctx, func(tx *store.Tx) error {
			for i := 2; i <= store.KeepLogs+2; i++ {
				id := fmt.Sprintf("bld_%d", i)
				if err := tx.CreateBuild(ctx, store.Build{ID: id, ChangeID: fmt.Sprintf("chg_%d", i), ResourceID: "svc_1", EnvironmentID: "env_1", Service: "web", ServerID: "srv_1", State: store.BuildDone, CreatedAt: at.Add(time.Duration(i) * time.Minute)}); err != nil {
					return err
				}
				if _, err := tx.AddBuildLines(ctx, id, []store.BuildLine{{At: at, Text: "hello"}}); err != nil {
					return err
				}
			}
			return tx.ForgetOldBuildLogs(ctx, "svc_1", store.KeepLogs)
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *store.Tx) error {
			for _, c := range []struct {
				id   string
				kept bool
			}{{"bld_1", false}, {"bld_2", false}, {"bld_3", true}, {fmt.Sprintf("bld_%d", store.KeepLogs+2), true}} {
				lines, err := tx.BuildLines(ctx, c.id, 0, 10)
				if err != nil {
					t.Fatal(err)
				}
				if got := len(lines) > 0; got != c.kept {
					t.Errorf("%s's log is kept: %v, want %v", c.id, got, c.kept)
				}
			}
			if _, err := tx.GetBuild(ctx, "bld_1"); err != nil {
				t.Errorf("a build whose log went is gone too: %v", err)
			}
			return nil
		})
	})
}
