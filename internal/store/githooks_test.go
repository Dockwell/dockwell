// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A service has one hook at most; a delivery seen before is refused, so it does
// nothing; deliveries read newest first, and old ones go.
func TestAHookKeepsWhatItsDeliveriesCameTo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		hook := store.GitHook{ID: "ghk_1", ResourceID: "svc_1", UserID: "usr_1", Secret: []byte("sealed"), CreatedAt: at, SecretAt: at}
		err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateGitHook(ctx, hook); err != nil {
				return err
			}
			for i, id := range []string{"d1", "d2", "d3"} {
				if err := tx.RecordGitHookDelivery(ctx, store.GitHookDelivery{HookID: "ghk_1", DeliveryID: id, At: at.Add(time.Duration(i) * time.Hour), Provider: "github", Ref: "refs/heads/main", Outcome: store.HookQueued}); err != nil {
					return err
				}
			}
			if err := tx.RecordGitHookDelivery(ctx, store.GitHookDelivery{HookID: "ghk_1", DeliveryID: "d2", At: at, Provider: "github", Outcome: store.HookQueued}); !errors.Is(err, store.ErrConflict) {
				t.Errorf("a delivery seen before = %v", err)
			}
			return tx.SetGitHookDelivery(ctx, store.GitHookDelivery{HookID: "ghk_1", DeliveryID: "d3", Commit: "abc", Outcome: store.HookDeploying, ChangeID: "chg_1", Detail: "deploying abc"})
		})
		if err != nil {
			t.Fatal(err)
		}
		err = db.Update(ctx, func(tx *store.Tx) error {
			return tx.CreateGitHook(ctx, store.GitHook{ID: "ghk_2", ResourceID: "svc_1", UserID: "usr_1", Secret: []byte("x"), CreatedAt: at, SecretAt: at})
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("a second hook for one service = %v", err)
		}
		err = db.Update(ctx, func(tx *store.Tx) error {
			got, err := tx.GitHookOf(ctx, "svc_1")
			if err != nil || got.ID != "ghk_1" || string(got.Secret) != "sealed" || !got.CreatedAt.Equal(at) {
				t.Errorf("the hook of svc_1 = %+v, %v", got, err)
			}
			list, err := tx.GitHookDeliveries(ctx, "ghk_1", 2)
			if err != nil || len(list) != 2 || list[0].DeliveryID != "d3" || list[0].Outcome != store.HookDeploying || list[0].ChangeID != "chg_1" {
				t.Errorf("the newest deliveries = %+v, %v", list, err)
			}
			if err := tx.SetGitHookSecret(ctx, "ghk_1", []byte("new"), at.Add(time.Hour)); err != nil {
				return err
			}
			if err := tx.ForgetGitHookDeliveries(ctx, at.Add(90*time.Minute)); err != nil {
				return err
			}
			if list, _ := tx.GitHookDeliveries(ctx, "ghk_1", 10); len(list) != 1 {
				t.Errorf("after forgetting the old, %d deliveries", len(list))
			}
			if err := tx.DeleteGitHook(ctx, "ghk_1"); err != nil {
				return err
			}
			if _, err := tx.GetGitHook(ctx, "ghk_1"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("a deleted hook = %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
