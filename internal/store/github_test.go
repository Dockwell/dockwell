// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An installation is kept as GitHub last said it, in place of what it said before;
// one GitHub never named is not found; an App's installations go with it.
func TestAGitHubInstallationIsAsGitHubLastSaidIt(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
		in := store.GitHubInstallation{
			ID: 7, AppID: "gha_1", Account: "acme", Organization: true, Selection: "selected", UpdatedAt: at,
			Repositories: []store.GitHubRepository{{ID: 5, FullName: "acme/web", Private: true, DefaultBranch: "main"}},
		}
		err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.SetGitHubInstallation(ctx, in); err != nil {
				return err
			}
			if err := tx.SetGitHubInstallation(ctx, store.GitHubInstallation{ID: 8, AppID: "gha_2", Account: "someone", UpdatedAt: at}); err != nil {
				return err
			}
			suspended := at.Add(time.Hour)
			in.SuspendedAt, in.Repositories, in.UpdatedAt = &suspended, nil, suspended
			return tx.SetGitHubInstallation(ctx, in)
		})
		if err != nil {
			t.Fatal(err)
		}
		err = db.View(ctx, func(tx *store.Tx) error {
			got, err := tx.GetGitHubInstallation(ctx, 7)
			if err != nil {
				return err
			}
			if got.Usable() || got.SuspendedAt == nil || !got.SuspendedAt.Equal(at.Add(time.Hour)) || len(got.Repositories) != 0 || !got.Organization {
				t.Errorf("installation 7 reads %+v", got)
			}
			if _, err := tx.GetGitHubInstallation(ctx, 9); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("an installation GitHub never named = %v", err)
			}
			mine, err := tx.GitHubInstallationsOf(ctx, "gha_1")
			if err != nil || len(mine) != 1 || mine[0].ID != 7 {
				t.Errorf("gha_1's installations = %+v, %v", mine, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = db.Update(ctx, func(tx *store.Tx) error { return tx.DeleteGitHubInstallationsOf(ctx, "gha_1") })
		if err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *store.Tx) error {
			if left, _ := tx.GitHubInstallationsOf(ctx, "gha_1"); len(left) != 0 {
				t.Errorf("a removed App left %d installations", len(left))
			}
			if _, err := tx.GetGitHubInstallation(ctx, 8); err != nil {
				t.Errorf("another App's installation went too: %v", err)
			}
			return nil
		})
	})
}

// A hook GitHub's App delivers to says so, and one of its own doesn't.
func TestAHookSaysHowItsPushesCome(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
		err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateGitHook(ctx, store.GitHook{ID: "ghk_own", ResourceID: "svc_1", UserID: "usr_1", Secret: []byte("s"), CreatedAt: at, SecretAt: at}); err != nil {
				return err
			}
			return tx.CreateGitHook(ctx, store.GitHook{ID: "ghk_app", ResourceID: "svc_2", UserID: "usr_1", Secret: []byte("s"), CreatedAt: at, SecretAt: at, Via: store.ViaGitHub})
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *store.Tx) error {
			own, err1 := tx.GetGitHook(ctx, "ghk_own")
			app, err2 := tx.GetGitHook(ctx, "ghk_app")
			if err := errors.Join(err1, err2); err != nil || own.Via != "" || app.Via != store.ViaGitHub {
				t.Errorf("the hooks read %q and %q, %v", own.Via, app.Via, err)
			}
			return nil
		})
	})
}
