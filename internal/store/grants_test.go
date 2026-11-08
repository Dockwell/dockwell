// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A person holds one role in each place, the install or a project. The owner and
// admins hold the install alone, there is one owner, and a user's role is what
// they hold on the install (ADR 0048, ADR 0049).
func TestGrantsAreARoleInAPlace(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
		person := func(id, email, role string) store.User {
			return store.User{ID: id, Email: email, Name: id, Role: role, WebAuthnHandle: []byte(id), CreatedAt: now}
		}
		err := db.Update(ctx, func(tx *store.Tx) error {
			if _, err := tx.Owner(ctx); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("an unclaimed install's owner: %v, want ErrNotFound", err)
			}
			for _, u := range []store.User{person("usr_mira", "mira@example.com", store.RoleOwner), person("usr_ada", "ada@example.com", ""), person("usr_sam", "sam@example.com", store.RoleAdmin)} {
				if err := tx.CreateUser(ctx, u); err != nil {
					return err
				}
			}
			for _, g := range []store.Grant{
				{UserID: "usr_ada", ProjectID: "prj_shop", Role: store.RoleDeveloper, CreatedAt: now, CreatedBy: "user:usr_mira"},
				{UserID: "usr_ada", ProjectID: "prj_blog", Role: store.RoleViewer, CreatedAt: now, CreatedBy: "user:usr_mira"},
			} {
				if err := tx.SetGrant(ctx, g); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		err = db.Update(ctx, func(tx *store.Tx) error {
			owner, err := tx.Owner(ctx)
			if err != nil || owner.ID != "usr_mira" || owner.Role != store.RoleOwner {
				t.Errorf("the owner reads %+v, %v", owner, err)
			}
			ada, err := tx.GetUser(ctx, "usr_ada")
			if err != nil || ada.Role != "" {
				t.Errorf("someone who holds projects alone reads role %q, %v, want none on the install", ada.Role, err)
			}
			got, err := tx.Grants(ctx, "usr_ada")
			if err != nil || len(got) != 2 || got[0].ProjectID != "prj_blog" || got[0].Role != store.RoleViewer ||
				got[1].ProjectID != "prj_shop" || got[1].Role != store.RoleDeveloper || got[1].CreatedBy != "user:usr_mira" {
				t.Errorf("ada's grants = %+v, %v", got, err)
			}

			// One role a place: granting again replaces it.
			if err := tx.SetGrant(ctx, store.Grant{UserID: "usr_ada", ProjectID: "prj_blog", Role: store.RoleDeveloper, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if got, _ := tx.Grants(ctx, "usr_ada"); len(got) != 2 || got[0].Role != store.RoleDeveloper {
				t.Errorf("after granting blog again, ada holds %+v, want developer there, once", got)
			}
			for _, bad := range []store.Grant{
				{UserID: "usr_ada", ProjectID: "prj_shop", Role: store.RoleAdmin},
				{UserID: "usr_ada", ProjectID: "prj_shop", Role: store.RoleOwner},
			} {
				if err := tx.SetGrant(ctx, bad); !errors.Is(err, store.ErrNotAPlace) {
					t.Errorf("granting %s on a project: %v, want ErrNotAPlace", bad.Role, err)
				}
			}
			if err := tx.SetGrant(ctx, store.Grant{UserID: "usr_ada", Role: "superuser"}); err == nil {
				t.Error("granting a role that isn't one worked")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// A second owner is refused, and leaves the first.
		err = db.Update(ctx, func(tx *store.Tx) error {
			return tx.SetGrant(ctx, store.Grant{UserID: "usr_sam", ProjectID: store.Install, Role: store.RoleOwner, CreatedAt: now})
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("a second owner: %v, want ErrConflict", err)
		}
		err = db.Update(ctx, func(tx *store.Tx) error {
			return tx.CreateUser(ctx, person("usr_eve", "eve@example.com", store.RoleOwner))
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("creating a second owner: %v, want ErrConflict", err)
		}

		err = db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.RemoveGrant(ctx, "usr_ada", "prj_shop"); err != nil {
				t.Fatal(err)
			}
			if err := tx.RemoveGrant(ctx, "usr_ada", "prj_shop"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("taking back what isn't held: %v, want ErrNotFound", err)
			}
			all, err := tx.ListGrants(ctx)
			if err != nil {
				return err
			}
			held := map[string]string{}
			for _, g := range all {
				held[g.UserID+" "+g.ProjectID] = g.Role
			}
			want := map[string]string{"usr_mira ": store.RoleOwner, "usr_sam ": store.RoleAdmin, "usr_ada prj_blog": store.RoleDeveloper}
			if len(held) != len(want) {
				t.Errorf("every grant: %v, want %v", held, want)
			}
			for k, v := range want {
				if held[k] != v {
					t.Errorf("every grant: %v, want %v", held, want)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestRoleRank(t *testing.T) {
	order := []string{"", store.RoleViewer, store.RoleDeveloper, store.RoleAdmin, store.RoleOwner}
	for i := 1; i < len(order); i++ {
		if store.RoleRank(order[i]) <= store.RoleRank(order[i-1]) {
			t.Errorf("%q ranks %d, not above %q's %d", order[i], store.RoleRank(order[i]), order[i-1], store.RoleRank(order[i-1]))
		}
	}
}

// An install claimed before grants keeps its owner: the one user an older
// dockwell made owner holds the install, so it doesn't read as unclaimed, and
// nobody can claim it again.
func TestUpgradeKeepsTheOwner(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		location := storetest.New(t, b)
		old, err := store.OpenAt(t.Context(), location, 24)
		if err != nil {
			t.Fatal(err)
		}
		_ = old.Close()
		handle := `x'01'`
		if b == store.Postgres {
			handle = `'\x01'`
		}
		storetest.Exec(t, location, `INSERT INTO users (id, email, name, role, webauthn_handle, created_at) VALUES ('usr_mira', 'mira@example.com', 'Mira Chen', 'owner', `+handle+`, 5)`)
		db, err := store.Open(t.Context(), location)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		err = db.View(t.Context(), func(tx *store.Tx) error {
			owner, err := tx.Owner(t.Context())
			if err != nil || owner.ID != "usr_mira" || owner.Role != store.RoleOwner || owner.Name != "Mira Chen" {
				t.Errorf("after the upgrade the owner reads %+v, %v", owner, err)
			}
			got, err := tx.Grants(t.Context(), "usr_mira")
			if err != nil || len(got) != 1 || got[0].Role != store.RoleOwner || got[0].ProjectID != store.Install || got[0].CreatedAt.UnixMilli() != 5 {
				t.Errorf("after the upgrade the owner holds %+v, %v, want the install, since they were made", got, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
