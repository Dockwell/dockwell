// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A write whose context ends under it, as a dockwell run systemd stops ends, is
// rolled back by database/sql, and its commit then says the transaction is done.
// The store says the stop instead, so run reads it as one and exits 0 (#758). The
// write leaves nothing behind.
func TestAWriteItsStopEndsIsTheStop(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx, cancel := context.WithCancel(t.Context())
		err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.PutSetting(ctx, "stop.test", []byte("written")); err != nil {
				return err
			}
			cancel()
			// Until database/sql has rolled the transaction back, as it does on its
			// own once the context ends, before the commit.
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				if _, err := tx.GetSetting(context.Background(), "stop.test"); errors.Is(err, sql.ErrTxDone) {
					return nil
				}
			}
			t.Fatal("database/sql never rolled the write back")
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a write its stop ended = %v, want the stop", err)
		}
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			_, err := tx.GetSetting(t.Context(), "stop.test")
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("after the stop the setting reads %v, want nothing", err)
		}
	})
}
