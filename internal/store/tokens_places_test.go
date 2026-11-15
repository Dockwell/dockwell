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

// A deploy-on-push hook keeps its projects and role as a token does, and a hook
// made before they were kept takes its service's project now, as a developer at
// most, or none once its service is gone, so an upgrade never widens one (ADR 0071).
func TestAHookIsNarrowedToItsServicesProject(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		ctx := t.Context()
		location := storetest.New(t, b)
		old, err := store.OpenAt(ctx, location, 42)
		if err != nil {
			t.Fatal(err)
		}
		at := "1794700000000"
		if err := old.Update(ctx, func(tx *store.Tx) error {
			for _, q := range []string{
				`INSERT INTO resources (id, kind, scope, name, revision) VALUES ('prj_shop', 'project', 'org_1', 'shop', 1)`,
				`INSERT INTO resources (id, kind, scope, name, revision) VALUES ('env_shop', 'environment', 'prj_shop', 'production', 1)`,
				`INSERT INTO resources (id, kind, scope, name, revision) VALUES ('svc_web', 'service', 'env_shop', 'web', 1)`,
				`INSERT INTO git_hooks (id, resource_id, user_id, secret, created_at, secret_at) VALUES ('ghk_web', 'svc_web', 'usr_ada', '', ` + at + `, ` + at + `)`,
				`INSERT INTO git_hooks (id, resource_id, user_id, secret, created_at, secret_at) VALUES ('ghk_gone', 'svc_gone', 'usr_ada', '', ` + at + `, ` + at + `)`,
			} {
				if err := store.ExecForTest(ctx, tx, q); err != nil {
					return err
				}
			}
			return nil
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
		err = db.Update(ctx, func(tx *store.Tx) error {
			for id, want := range map[string][]string{"ghk_web": {"prj_shop"}, "ghk_gone": {}} {
				h, err := tx.GetGitHook(ctx, id)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(h.Projects, want) || h.Role != store.RoleDeveloper {
					t.Errorf("hook %s from before acts in %#v as %q, want %#v, as a developer", id, h.Projects, h.Role, want)
				}
			}
			// One made now keeps what it's given, every project and its maker's role included.
			now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
			for id, h := range map[string]store.GitHook{
				"ghk_narrowed": {Projects: []string{"prj_shop", "prj_blog"}, Role: store.RoleViewer},
				"ghk_wide":     {},
			} {
				h.ID, h.ResourceID, h.UserID, h.Secret, h.CreatedAt, h.SecretAt = id, "svc_"+id, "usr_ada", []byte("x"), now, now
				if err := tx.CreateGitHook(ctx, h); err != nil {
					return err
				}
				got, err := tx.GetGitHook(ctx, id)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got.Projects, h.Projects) || got.Role != h.Role {
					t.Errorf("hook %s reads back in %#v as %q, want %#v as %q", id, got.Projects, got.Role, h.Projects, h.Role)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
