// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// What's observed of an app's owner moves from waiting to failed and back, but once
// made or opened it stays, so no later reading puts an install back on hold (ADR
// 0105, the re-check's M1).
func TestAnAppOwnerMadeOrOpenedStaysSo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
		set := func(id, state string) bool {
			t.Helper()
			var wrote bool
			err := db.Update(ctx, func(tx *store.Tx) (err error) {
				wrote, err = tx.SetAppOwnerState(ctx, store.AppOwnerState{ResourceID: id, Environment: "env_1", State: state, Error: "it answered 500", Tries: 1, UpdatedAt: at})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			return wrote
		}
		read := func() map[string]store.AppOwnerState {
			t.Helper()
			var got map[string]store.AppOwnerState
			if err := db.View(ctx, func(tx *store.Tx) (err error) {
				got, err = tx.AppOwnerStates(ctx, []string{"own_1", "own_2", "own_none"})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return got
		}
		if !set("own_1", store.OwnerWaiting) || !set("own_1", store.OwnerFailed) || !set("own_1", store.OwnerMade) {
			t.Fatal("a waiting owner's state didn't move")
		}
		if set("own_1", store.OwnerFailed) || set("own_1", store.OwnerWaiting) {
			t.Error("a made owner's state moved back")
		}
		set("own_2", store.OwnerFailed)
		set("own_2", store.OwnerOpened)
		if set("own_2", store.OwnerWaiting) {
			t.Error("an opened owner's state moved back")
		}
		got := read()
		if got["own_1"].State != store.OwnerMade || got["own_2"].State != store.OwnerOpened {
			t.Errorf("states = %+v", got)
		}
		if _, ok := got["own_none"]; ok {
			t.Error("an owner with no row reads as there")
		}
		if !got["own_1"].UpdatedAt.Equal(at) || got["own_1"].Environment != "env_1" {
			t.Errorf("own_1 = %+v", got["own_1"])
		}
	})
}
