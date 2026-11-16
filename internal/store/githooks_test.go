// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"errors"
	"slices"
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
			// A new secret binds the hook to whoever gave it.
			if err := tx.SetGitHookSecret(ctx, "ghk_1", "usr_2", []string{"prj_new"}, []byte("new"), at.Add(time.Hour)); err != nil {
				return err
			}
			if h, err := tx.GetGitHook(ctx, "ghk_1"); err != nil || h.UserID != "usr_2" || string(h.Secret) != "new" || !h.SecretAt.Equal(at.Add(time.Hour)) ||
				!slices.Equal(h.Projects, []string{"prj_new"}) {
				t.Errorf("after a new secret, the hook is %+v, %v", h, err)
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

// A person's hooks are theirs as their tokens are: a plan a hook left is among
// what they planned, and their hooks go, with their deliveries, as their access
// ends (ADR 0046, ADR 0049).
func TestAHookGoesWithItsMakersAccess(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
		err := db.Update(ctx, func(tx *store.Tx) error {
			for _, h := range []store.GitHook{
				{ID: "ghk_ada1", ResourceID: "svc_web", UserID: "usr_ada", Secret: []byte("x"), CreatedAt: at, SecretAt: at},
				{ID: "ghk_ada2", ResourceID: "svc_api", UserID: "usr_ada", Secret: []byte("x"), CreatedAt: at, SecretAt: at},
				{ID: "ghk_sam", ResourceID: "svc_docs", UserID: "usr_sam", Secret: []byte("x"), CreatedAt: at, SecretAt: at},
			} {
				if err := tx.CreateGitHook(ctx, h); err != nil {
					return err
				}
			}
			return tx.RecordGitHookDelivery(ctx, store.GitHookDelivery{HookID: "ghk_ada1", DeliveryID: "d1", At: at, Provider: "github", Outcome: store.HookNeedsPerson})
		})
		if err != nil {
			t.Fatal(err)
		}
		revise(t, db, func(tx *store.RevisionTx) error {
			for _, c := range []store.Change{
				{ID: "chg_push", Summary: "Deploy web, pushed to main", Actor: "hook:ghk_ada1", State: store.ChangePlanned, Fingerprint: "f", Plan: []byte("{}"), CreatedAt: at},
				{ID: "chg_sams", Summary: "Deploy docs, pushed to main", Actor: "hook:ghk_sam", State: store.ChangePlanned, Fingerprint: "f", Plan: []byte("{}"), CreatedAt: at},
				// A plan left by a hook ada turned off says it acts for her.
				{ID: "chg_gone", Summary: "Deploy blog, pushed to main", Actor: "hook:ghk_gone", ActsFor: "usr_ada", State: store.ChangePlanned, Fingerprint: "f", Plan: []byte("{}"), CreatedAt: at.Add(-time.Minute)},
			} {
				if err := tx.CreateChange(ctx, c); err != nil {
					return err
				}
			}
			return nil
		})
		err = db.Update(ctx, func(tx *store.Tx) error {
			planned, err := tx.PlannedChangesOf(ctx, "usr_ada")
			if err != nil {
				return err
			}
			if len(planned) != 2 || planned[0].ID != "chg_push" || planned[1].ID != "chg_gone" || planned[1].ActsFor != "usr_ada" {
				t.Errorf("what ada planned: %v, want her hooks' plans, the one whose hook is gone too", planned)
			}
			ended, err := tx.EndAccess(ctx, "usr_ada", at)
			if err != nil {
				return err
			}
			if len(ended.Hooks) != 2 || ended.Hooks[0].ID != "ghk_ada1" || ended.Hooks[1].ID != "ghk_ada2" {
				t.Errorf("ending ada's access ended hooks %v, want both of hers", ended.Hooks)
			}
			for _, id := range []string{"ghk_ada1", "ghk_ada2"} {
				if _, err := tx.GetGitHook(ctx, id); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("hook %s after its maker's access ended: %v", id, err)
				}
			}
			if left, err := tx.GitHookDeliveries(ctx, "ghk_ada1", 10); err != nil || len(left) != 0 {
				t.Errorf("a deleted hook's deliveries: %v, %v", left, err)
			}
			if _, err := tx.GetGitHook(ctx, "ghk_sam"); err != nil {
				t.Errorf("sam's hook after ada's access ended: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
