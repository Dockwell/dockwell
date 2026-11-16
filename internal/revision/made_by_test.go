// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A change a hook or a narrowed token planned is its maker's to discard, in person:
// a hook's by whom it acted for as it planned, a token's by who made it. Anyone
// else, and the maker through a token of theirs, discards it as someone else's
// (ADR 0071, #711).
func TestWhatAHookOrATokenPlannedIsItsMakersToDiscard(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		if err := db.Update(ctx, func(tx *store.Tx) error {
			for _, u := range []string{"usr_ada", "usr_bo"} {
				if err := tx.CreateUser(ctx, store.User{ID: u, Email: u + "@example.com", Name: u, WebAuthnHandle: []byte(u), CreatedAt: time.Now()}); err != nil {
					return err
				}
			}
			return tx.CreateAPIToken(ctx, store.APIToken{
				ID: "tok_ci", TokenHash: []byte("ci"), UserID: "usr_ada", Name: "ci", Scope: "read_write",
				CreatedAt: time.Now(), Projects: []string{"prj_shop"}, Role: store.RoleDeveloper,
			})
		}); err != nil {
			t.Fatal(err)
		}
		ada := place.Caller{UserID: "usr_ada"}
		for _, c := range []struct {
			what   string
			caller place.Caller
			change store.Change
			want   bool
		}{
			{"the hook's maker", ada, store.Change{Actor: "hook:ghk_1", ActsFor: "usr_ada"}, true},
			{"someone else, of a hook's", place.Caller{UserID: "usr_bo"}, store.Change{Actor: "hook:ghk_1", ActsFor: "usr_ada"}, false},
			{"the hook's maker, through a token", place.Caller{UserID: "usr_ada", TokenID: "tok_other"}, store.Change{Actor: "hook:ghk_1", ActsFor: "usr_ada"}, false},
			{"the narrowed token's maker", ada, store.Change{Actor: "token:tok_ci"}, true},
			{"someone else, of a token's", place.Caller{UserID: "usr_bo"}, store.Change{Actor: "token:tok_ci"}, false},
			{"anyone, of a token that's gone", ada, store.Change{Actor: "token:tok_gone"}, false},
			{"anyone, of a person's", ada, store.Change{Actor: "user:usr_bo"}, false},
		} {
			var got bool
			if err := db.View(ctx, func(tx *store.Tx) (err error) {
				got, err = madeBy(ctx, tx, c.caller, &c.change)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("%s discards it as their own: %v, want %v", c.what, got, c.want)
			}
		}
	})
}
