// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An invite names an email, a role and a place, works once, can be revoked, and
// stops working when it expires. Nobody is invited as owner, and an admin's
// place is the install (ADR 0049).
func TestAnInviteWorksOnce(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
		invite := func(id, role, project string, expires time.Time) store.Invite {
			return store.Invite{
				ID: id, TokenHash: []byte("hash-" + id), Email: id + "@example.com", Role: role, ProjectID: project,
				CreatedAt: now, CreatedBy: "user:usr_mira", ExpiresAt: expires,
			}
		}
		err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateUser(ctx, store.User{ID: "usr_mira", Email: "mira@example.com", Name: "Mira", Role: store.RoleOwner, WebAuthnHandle: []byte("m"), CreatedAt: now}); err != nil {
				return err
			}
			if err := tx.CreateUser(ctx, store.User{ID: "usr_ada", Email: "ada@example.com", Name: "Ada", WebAuthnHandle: []byte("a"), CreatedAt: now}); err != nil {
				return err
			}
			for _, i := range []store.Invite{
				invite("inv_ada", store.RoleDeveloper, "prj_shop", now.Add(7*24*time.Hour)),
				invite("inv_sam", store.RoleAdmin, store.Install, now.Add(24*time.Hour)),
				invite("inv_old", store.RoleViewer, "prj_shop", now.Add(-time.Minute)),
			} {
				if err := tx.CreateInvite(ctx, i); err != nil {
					return err
				}
			}
			if err := tx.CreateInvite(ctx, invite("inv_boss", store.RoleOwner, store.Install, now.Add(time.Hour))); !errors.Is(err, store.ErrOwner) {
				t.Errorf("an invite as owner: %v, want ErrOwner", err)
			}
			if err := tx.CreateInvite(ctx, invite("inv_shopadmin", store.RoleAdmin, "prj_shop", now.Add(time.Hour))); !errors.Is(err, store.ErrNotAPlace) {
				t.Errorf("an admin's invite to a project: %v, want ErrNotAPlace", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = db.Update(ctx, func(tx *store.Tx) error {
			open, err := tx.ListOpenInvites(ctx, now)
			if err != nil || len(open) != 2 {
				t.Errorf("open invites: %+v, %v, want ada's and sam's, not the expired one", open, err)
			}
			got, err := tx.GetInviteByHash(ctx, []byte("hash-inv_ada"))
			if err != nil || got.Email != "inv_ada@example.com" || got.Role != store.RoleDeveloper || got.ProjectID != "prj_shop" || !got.Open(now) {
				t.Errorf("ada's invite reads %+v, %v", got, err)
			}
			if err := tx.AcceptInvite(ctx, "inv_ada", "usr_ada", now); err != nil {
				t.Fatal(err)
			}
			if err := tx.AcceptInvite(ctx, "inv_ada", "usr_ada", now); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("accepting it again: %v, want ErrNotFound", err)
			}
			if err := tx.AcceptInvite(ctx, "inv_old", "usr_ada", now); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("accepting an expired invite: %v, want ErrNotFound", err)
			}
			if err := tx.RevokeInvite(ctx, "inv_sam", "user:usr_mira", now); err != nil {
				t.Fatal(err)
			}
			if err := tx.RevokeInvite(ctx, "inv_ada", "user:usr_mira", now); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("revoking an accepted invite: %v, want ErrNotFound", err)
			}
			if open, _ := tx.ListOpenInvites(ctx, now); len(open) != 0 {
				t.Errorf("open invites after one was accepted and one revoked: %+v", open)
			}
			sam, err := tx.GetInvite(ctx, "inv_sam")
			if err != nil || sam.RevokedAt == nil || sam.RevokedBy != "user:usr_mira" || sam.Open(now) {
				t.Errorf("sam's revoked invite reads %+v, %v", sam, err)
			}
			if _, err := tx.GetInviteByHash(ctx, []byte("nothing")); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("an unknown secret: %v, want ErrNotFound", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
