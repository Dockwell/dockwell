// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A token keeps the projects it acts in and the role it acts as at most, and one
// made without either reads back as acting as its maker does (ADR 0071).
func TestATokenKeepsItsProjectsAndRole(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		ctx := t.Context()
		db, _ := storetest.Open(t, b)
		at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
		user := store.User{ID: "usr_ada", Email: "ada@example.com", Name: "Ada Park", Role: store.RoleAdmin, WebAuthnHandle: []byte("h"), CreatedAt: at}
		if err := db.Update(ctx, func(tx *store.Tx) error { return tx.CreateUser(ctx, user) }); err != nil {
			t.Fatal(err)
		}
		narrowed := store.APIToken{
			ID: "tok_narrowed", TokenHash: []byte("narrowed"), UserID: user.ID, Name: "Shop deploy", Hint: "dwt_a",
			Scope: store.ScopeReadWrite, CreatedAt: at, Projects: []string{"prj_shop", "prj_blog"}, Role: store.RoleDeveloper,
		}
		plain := store.APIToken{
			ID: "tok_plain", TokenHash: []byte("plain"), UserID: user.ID, Name: "Reads", Hint: "dwt_b",
			Scope: store.ScopeReadOnly, CreatedAt: at,
		}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateAPIToken(ctx, narrowed); err != nil {
				return err
			}
			return tx.CreateAPIToken(ctx, plain)
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.View(ctx, func(tx *store.Tx) error {
			for _, want := range []store.APIToken{narrowed, plain} {
				got, err := tx.GetAPITokenByHash(ctx, want.TokenHash)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("token read back = %+v, want %+v", got, want)
				}
			}
			if plain.Narrowed() || !narrowed.Narrowed() {
				t.Error("Narrowed() doesn't tell the narrowed token from the other")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		// The store holds a token's role to the two a token may be capped at.
		admin := narrowed
		admin.ID, admin.TokenHash, admin.Role = "tok_admin", []byte("admin"), store.RoleAdmin
		if err := db.Update(ctx, func(tx *store.Tx) error { return tx.CreateAPIToken(ctx, admin) }); err == nil {
			t.Error("a token capped at admin was stored, want it refused")
		}
	})
}
