// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
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
			if err := tx.SetGrant(ctx, store.Grant{UserID: "usr_ada", ProjectID: "prj_shop", Role: store.RoleAdmin}); !errors.Is(err, store.ErrNotAPlace) {
				t.Errorf("granting admin on a project: %v, want ErrNotAPlace", err)
			}
			if err := tx.SetGrant(ctx, store.Grant{UserID: "usr_ada", ProjectID: "prj_shop", Role: store.RoleOwner}); !errors.Is(err, store.ErrOwner) {
				t.Errorf("granting owner on a project: %v, want ErrOwner", err)
			}
			if err := tx.SetGrant(ctx, store.Grant{UserID: "usr_ada", Role: "superuser"}); err == nil {
				t.Error("granting a role that isn't one worked")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// The owner's role is given and taken only by a transfer: not granted, not
		// changed, not taken back.
		for what, change := range map[string]func(tx *store.Tx) error{
			"granting the owner's role": func(tx *store.Tx) error {
				return tx.SetGrant(ctx, store.Grant{UserID: "usr_sam", ProjectID: store.Install, Role: store.RoleOwner, CreatedAt: now})
			},
			"demoting the owner": func(tx *store.Tx) error {
				return tx.SetGrant(ctx, store.Grant{UserID: "usr_mira", ProjectID: store.Install, Role: store.RoleViewer, CreatedAt: now})
			},
			"taking the owner's grant back": func(tx *store.Tx) error { return tx.RemoveGrant(ctx, "usr_mira", store.Install) },
		} {
			if err := db.Update(ctx, change); !errors.Is(err, store.ErrOwner) {
				t.Errorf("%s: %v, want ErrOwner", what, err)
			}
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
		for _, version := range []int{24, 25, 26} {
			t.Run(fmt.Sprintf("from_%d", version), func(t *testing.T) {
				location := storetest.New(t, b)
				old, err := store.OpenAt(t.Context(), location, version)
				if err != nil {
					t.Fatal(err)
				}
				// Version 25 is main with scheduled tasks, and 26 adds alerts, before grants. Task runs
				// must survive, so the task migration cannot be replaced or run again.
				encoded, err := proto.Marshal(&dockwellv1.TaskRun{Id: "run_before_grants", TaskId: "tsk_before_grants", State: dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED})
				if err != nil {
					t.Fatal(err)
				}
				if version >= 25 {
					err = old.Update(t.Context(), func(tx *store.Tx) error {
						return tx.PutTaskRun(t.Context(), store.TaskRun{ID: "run_before_grants", TaskID: "tsk_before_grants", ServerID: "srv_web", State: 2, StartedAt: time.UnixMilli(6), Run: encoded})
					})
					if err != nil {
						t.Fatal(err)
					}
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
					if version >= 25 {
						run, err := tx.GetTaskRun(t.Context(), "run_before_grants")
						if err != nil || run.TaskID != "tsk_before_grants" || run.State != 2 || !bytes.Equal(run.Run, encoded) {
							t.Errorf("task run after granting the owner = %+v, %v", run, err)
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	})
}

// Ownership moves only by a transfer, which makes the owner before an admin, so
// there is always exactly one owner.
func TestOwnershipMovesByTransfer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
		err := db.Update(ctx, func(tx *store.Tx) error {
			for _, u := range []store.User{
				{ID: "usr_mira", Email: "mira@example.com", Name: "Mira", Role: store.RoleOwner, WebAuthnHandle: []byte("m"), CreatedAt: now},
				{ID: "usr_ada", Email: "ada@example.com", Name: "Ada", WebAuthnHandle: []byte("a"), CreatedAt: now},
			} {
				if err := tx.CreateUser(ctx, u); err != nil {
					return err
				}
			}
			if err := tx.SetGrant(ctx, store.Grant{UserID: "usr_ada", ProjectID: "prj_shop", Role: store.RoleDeveloper, CreatedAt: now}); err != nil {
				return err
			}
			return tx.TransferOwnership(ctx, "usr_ada", now, "cli:mira@web-1")
		})
		if err != nil {
			t.Fatal(err)
		}
		err = db.View(ctx, func(tx *store.Tx) error {
			owner, err := tx.Owner(ctx)
			if err != nil || owner.ID != "usr_ada" {
				t.Errorf("after the transfer the owner is %+v, %v, want ada", owner, err)
			}
			mira, err := tx.GetUser(ctx, "usr_mira")
			if err != nil || mira.Role != store.RoleAdmin {
				t.Errorf("after the transfer the owner before is %q, %v, want an admin", mira.Role, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// The owner before is an admin like any other now, so their role can change.
		err = db.Update(ctx, func(tx *store.Tx) error {
			return tx.SetGrant(ctx, store.Grant{UserID: "usr_mira", ProjectID: store.Install, Role: store.RoleViewer, CreatedAt: now})
		})
		if err != nil {
			t.Errorf("demoting the owner before: %v", err)
		}
		if err := db.Update(ctx, func(tx *store.Tx) error { return tx.TransferOwnership(ctx, "usr_nobody", now, "") }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("transferring to nobody: %v, want ErrNotFound", err)
		}
	})
}
