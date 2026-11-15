// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"slices"
	"testing"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Re-check: a store at main's 0043 with a hook, narrowed by 0043, upgrades to 0044
// and keeps it as a hook through its own URL.
func TestRecheckUpgradeFrom43KeepsHooks(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		location := storetest.New(t, backend)
		old, err := store.OpenAt(ctx, location, 43)
		if err != nil {
			t.Fatal(err)
		}
		blob := `X'00'`
		if backend == store.Postgres {
			blob = `'\x00'::bytea`
		}
		if err := old.Update(ctx, func(tx *store.Tx) error {
			return store.ExecForTest(ctx, tx, `INSERT INTO git_hooks (id, resource_id, user_id, secret, created_at, secret_at, projects, role) VALUES ('ghk_old', 'svc_web', 'usr_a', `+blob+`, 1, 1, 'prj_a', 'admin')`)
		}); err != nil {
			t.Fatal(err)
		}
		if err := old.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(ctx, location)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.View(ctx, func(tx *store.Tx) error {
			h, err := tx.GitHookOf(ctx, "svc_web")
			if err != nil {
				return err
			}
			if h.Via != "" || h.Role != store.RoleAdmin || !slices.Equal(h.Projects, []string{"prj_a"}) {
				t.Errorf("after 0044 the hook is via %q, role %q, projects %v", h.Via, h.Role, h.Projects)
			}
			via, err := tx.GitHooksVia(ctx, store.ViaGitHub)
			if err != nil || len(via) != 0 {
				t.Errorf("hooks via GitHub after the upgrade: %v, %v", via, err)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}
