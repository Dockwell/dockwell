// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Each call needs a role, checked first against the highest the caller holds
// anywhere, read on every call: a person's own, or for an API token, its maker's,
// so a token loses what its maker loses. Someone with no role left can only sign
// out (ADR 0049).
func TestARoleGatesEveryCall(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		writes := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		owner := ""
		err := f.store.View(t.Context(), func(tx *store.Tx) error {
			u, err := tx.Owner(t.Context())
			owner = u.ID
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		// What the owner holds, as a demotion would leave it; the store's own rules
		// for the owner come with transfers.
		hold := func(role string) {
			t.Helper()
			err := f.store.Update(t.Context(), func(tx *store.Tx) error {
				if err := tx.RemoveGrant(t.Context(), owner, store.Install); err != nil {
					return err
				}
				if role == "" {
					return nil
				}
				return tx.SetGrant(t.Context(), store.Grant{UserID: owner, ProjectID: store.Install, Role: role, CreatedAt: time.Now()})
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		person := struct {
			changes  dockwellv1connect.ChangeServiceClient
			audit    dockwellv1connect.AuditServiceClient
			services dockwellv1connect.ServiceServiceClient
			servers  dockwellv1connect.ServerServiceClient
		}{
			changeClient(b, b.origin),
			dockwellv1connect.NewAuditServiceClient(b, b.origin, connect.WithProtoJSON()),
			dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON()),
			dockwellv1connect.NewServerServiceClient(b, b.origin, connect.WithProtoJSON()),
		}
		script := changeClient(bearer{token: writes, rt: b.client.Transport}, b.origin)
		propose := func(c dockwellv1connect.ChangeServiceClient) error {
			_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "nothing"}))
			return err
		}
		refused := func(what string, err error, says string) {
			t.Helper()
			if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_role" || !strings.Contains(err.Error(), says) {
				t.Errorf("%s: %v, want needs_role saying %q", what, err, says)
			}
		}
		allowed := func(what string, err error) {
			t.Helper()
			if reasonOf(err) == "needs_role" {
				t.Errorf("%s: %v, want it past the role's gate", what, err)
			}
		}

		hold(store.RoleDeveloper)
		_, err = person.audit.ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
		refused("a developer reading the audit log", err, "this needs an admin's role or more, and you hold a developer's role")
		allowed("a developer planning", propose(person.changes))
		allowed("a developer's token planning", propose(script))

		hold(store.RoleViewer)
		refused("a viewer planning", propose(person.changes), "this needs a developer's role or more, and you hold a viewer's role")
		refused("a viewer's token planning, whatever its scope", propose(script), "this API token's maker holds a viewer's role")
		_, err = person.services.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		allowed("a viewer reading services", err)
		_, err = person.servers.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
		allowed("a viewer reading servers by name", err)

		hold(store.RoleAdmin)
		_, err = person.audit.ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
		allowed("an admin reading the audit log", err)

		hold("")
		_, err = person.services.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		refused("someone with no role reading", err, "you hold no role on this install any more, so you can only sign out")
		refused("a token whose maker holds no role", propose(script), "this API token's maker holds no role")
		if _, err := b.authClient().SignOut(t.Context(), connect.NewRequest(&dockwellv1.SignOutRequest{})); err != nil {
			t.Errorf("someone with no role signing out: %v", err)
		}
	})
}
