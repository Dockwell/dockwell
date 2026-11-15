// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// What a change that ran took a container on as counts as taken on, applied,
// applying or failed, as a failed one keeps its revisions until its rollback
// commits; a take-on planned or discarded doesn't (ADR 0096).
func TestWhatWasTakenOn(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		at := time.UnixMilli(1_000_000)
		revise(t, db, func(tx *store.RevisionTx) error {
			for name, state := range map[string]string{
				"applied": store.ChangeApplied, "applying": store.ChangeApplying, "planned": store.ChangePlanned,
				"failed": store.ChangeFailed, "discarded": store.ChangeDiscarded,
			} {
				if err := tx.CreateChange(ctx, store.Change{
					ID: "chg_" + name, Summary: name, Actor: "user:usr_1", State: state, Fingerprint: "f", Plan: []byte("{}"), CreatedAt: at,
					Items: []store.ChangeItem{{Kind: "service", Scope: "env_1", Name: name, Spec: []byte{1}, Adopts: "c0ffee" + name}},
				}); err != nil {
					return err
				}
			}
			return tx.CreateChange(ctx, store.Change{
				ID: "chg_made", Summary: "made", Actor: "user:usr_1", State: store.ChangeApplied, Fingerprint: "f", Plan: []byte("{}"), CreatedAt: at,
				Items: []store.ChangeItem{{Kind: "service", Scope: "env_1", Name: "made", Spec: []byte{1}}},
			})
		})
		var names []store.TakenOnName
		for _, n := range []string{"applied", "applying", "planned", "failed", "discarded", "made"} {
			names = append(names, store.TakenOnName{Kind: "service", Scope: "env_1", Name: n})
		}
		var got map[store.TakenOnName]bool
		if err := db.View(ctx, func(tx *store.Tx) error {
			var err error
			got, err = tx.TakenOn(ctx, names)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if want := n.Name == "applied" || n.Name == "applying" || n.Name == "failed"; got[n] != want {
				t.Errorf("%s reads as taken on: %v", n.Name, got[n])
			}
		}
	})
}
