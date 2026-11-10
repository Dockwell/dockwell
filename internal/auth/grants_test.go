// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// dockwell recover --owner makes someone who signs in already the owner, from the
// control server. The owner before becomes an admin, and the CLI's tokens that
// acted for them stop working (ADR 0048, ADR 0049).
func TestMakeOwnerMovesOwnership(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newFixture(t, backend, "")
		ctx := t.Context()
		owner := f.claim(t, localhost, &authtest.Authenticator{})
		local, err := f.svc.CreateLocalToken(ctx, "cli:mira@web-1", "Local access on web-1, read-write", store.ScopeReadWrite, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		err = f.db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateUser(ctx, store.User{ID: "usr_ada", Email: "ada@example.com", Name: "Ada", WebAuthnHandle: []byte("ada"), CreatedAt: f.clock.now()}); err != nil {
				return err
			}
			return tx.SetGrant(ctx, store.Grant{UserID: "usr_ada", ProjectID: "prj_shop", Role: store.RoleDeveloper, CreatedAt: f.clock.now()})
		})
		if err != nil {
			t.Fatal(err)
		}
		made, err := f.svc.MakeOwner(ctx, "Ada@Example.com", "cli:root@web-1")
		if err != nil || made.ID != "usr_ada" {
			t.Fatalf("making ada the owner: %+v, %v", made, err)
		}
		err = f.db.View(ctx, func(tx *store.Tx) error {
			now, err := tx.Owner(ctx)
			if err != nil || now.ID != "usr_ada" {
				t.Errorf("the owner is %+v, %v, want ada", now, err)
			}
			before, err := tx.GetUser(ctx, owner.User.ID)
			if err != nil || before.Role != store.RoleAdmin {
				t.Errorf("the owner before is %q, %v, want an admin", before.Role, err)
			}
			tok, err := tx.GetAPIToken(ctx, local.Token.ID)
			if err != nil || tok.RevokedAt == nil {
				t.Errorf("the owner before's local token: revoked %v, %v, want revoked", tok.RevokedAt, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := f.auditEvents(t, "owner.transfer"); len(got) != 1 || !strings.Contains(got[0].Detail, "in place of mira@acme.dev") {
			t.Errorf("the transfer's audit events: %+v", got)
		}
		if _, err := f.svc.MakeOwner(ctx, "nobody@example.com", "cli:root@web-1"); err == nil || !strings.Contains(err.Error(), "nobody signs in here as nobody@example.com") {
			t.Errorf("making nobody the owner: %v", err)
		}

		// An install an older dockwell left with people and no owner says how to give it one.
		storetest.Exec(t, f.path, `DELETE FROM grants WHERE role = 'owner'`)
		if _, _, err := f.svc.CreateSignInLink(ctx, "", "cli:root@web-1"); err == nil || !strings.Contains(err.Error(), "has people but no owner") ||
			!strings.Contains(err.Error(), "dockwell recover --owner EMAIL") {
			t.Errorf("a sign-in link with people but no owner: %v", err)
		}
	})
}

// A call refused for want of a role is audited at most once an hour for each
// person or token and method, with a count of the rest (ADR 0049).
func TestRoleRefusalsAreAuditedOnceAnHour(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newFixture(t, backend, "")
		ctx := t.Context()
		claimed := f.claim(t, localhost, &authtest.Authenticator{})
		owner := Principal{User: claimed.User, Session: claimed.Session}
		tok := store.APIToken{ID: "tok_ci"}
		const audit, settings = "/dockwell.v1.AuditService/ListAuditEvents", "/dockwell.v1.SettingsService/SetTimeZone"
		for range 3 {
			f.svc.RefuseRole(ctx, localhost, owner, audit, store.RoleViewer, store.RoleAdmin)
		}
		f.svc.RefuseRole(ctx, localhost, owner, settings, store.RoleViewer, store.RoleAdmin)
		f.svc.RefuseRole(ctx, localhost, Principal{User: owner.User, Token: &tok}, audit, "", store.RoleAdmin)
		got := f.auditEvents(t, "role.refuse")
		if len(got) != 3 {
			t.Fatalf("role.refuse = %+v, want one for each person or token and method", got)
		}
		if got[0].Actor != owner.Actor() || got[0].Subject != owner.User.ID ||
			got[0].Detail != audit+", which needs an admin, from 127.0.0.1: the caller is a viewer" {
			t.Errorf("the first refusal = %+v", got[0])
		}
		if !strings.Contains(got[2].Detail, "token tok_ci's maker holds no role") || got[2].Actor != "token:tok_ci" {
			t.Errorf("a token's refusal = %+v", got[2])
		}
		f.clock.advance(refusedReportEvery)
		f.svc.RefuseRole(ctx, localhost, owner, audit, store.RoleViewer, store.RoleAdmin)
		got = f.auditEvents(t, "role.refuse")
		if len(got) != 4 || !strings.HasSuffix(got[3].Detail, "and 2 more refused since the last report") {
			t.Errorf("an hour on, role.refuse = %+v, want another counting the 2 between", got)
		}
	})
}
