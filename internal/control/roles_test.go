// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// aMember hands ownership to someone else, as the owner's own role changes only by
// a transfer, and returns a way to set what the signed-in person holds: their
// grants, in place of whatever they held.
func aMember(t *testing.T, f *fullServer) func(grants ...store.Grant) {
	t.Helper()
	var person string
	err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		u, err := tx.Owner(t.Context())
		if err != nil {
			return err
		}
		person = u.ID
		if err := tx.CreateUser(t.Context(), store.User{ID: "usr_keeper", Email: "keeper@example.com", Name: "Keeper", WebAuthnHandle: []byte("keeper"), CreatedAt: time.Now()}); err != nil {
			return err
		}
		return tx.TransferOwnership(t.Context(), "usr_keeper", time.Now(), "dockwell")
	})
	if err != nil {
		t.Fatal(err)
	}
	return func(grants ...store.Grant) {
		t.Helper()
		err := f.store.Update(t.Context(), func(tx *store.Tx) error {
			held, err := tx.Grants(t.Context(), person)
			if err != nil {
				return err
			}
			for _, g := range held {
				if err := tx.RemoveGrant(t.Context(), person, g.ProjectID); err != nil {
					return err
				}
			}
			for _, g := range grants {
				g.UserID, g.CreatedAt = person, time.Now()
				if err := tx.SetGrant(t.Context(), g); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// onInstall is a grant of role on the whole install.
func onInstall(role string) store.Grant { return store.Grant{ProjectID: store.Install, Role: role} }

// Each call needs a role, checked first against the highest the caller holds
// anywhere, read on every call, over Connect and REST alike: a person's own, or
// for an API token, its maker's, so a token loses what its maker loses. Someone
// with no role left can only sign out (ADR 0049).
func TestARoleGatesEveryCall(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		writes := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		hold := aMember(t, f)
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
		rest := func(path string) (int, string) {
			t.Helper()
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, b.origin+path, http.NoBody)
			res, err := b.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			return res.StatusCode, res.Header.Get("Dockwell-Error")
		}

		hold(onInstall(store.RoleDeveloper))
		_, err := person.audit.ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
		refused("a developer reading the audit log", err, "only an admin or the owner can do this, and you're a developer, so it didn't happen")
		if code, reason := rest("/v1/auditEvents"); code != http.StatusForbidden || reason != "needs_role" {
			t.Errorf("a developer reading the audit log over REST: %d %q, want 403 needs_role", code, reason)
		}
		// Refused twice, audited once (ADR 0049).
		err = f.store.View(t.Context(), func(tx *store.Tx) error {
			evs, err := tx.ListEvents(t.Context(), 1_000)
			var refusals []string
			for _, e := range evs {
				if e.Action == "role.refuse" {
					refusals = append(refusals, e.Detail)
				}
			}
			if len(refusals) != 1 || !strings.Contains(refusals[0], "/dockwell.v1.AuditService/ListAuditEvents, which needs an admin") ||
				!strings.Contains(refusals[0], "the caller is a developer") {
				t.Errorf("role.refuse = %q, want the one refused method, once", refusals)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		allowed("a developer planning", propose(person.changes))
		allowed("a developer's token planning", propose(script))

		hold(onInstall(store.RoleViewer))
		refused("a viewer planning", propose(person.changes), "only a developer or more can do this, and you're a viewer, so it didn't happen. Ask an admin to change your role")
		refused("a viewer's token planning, whatever its scope", propose(script), "this API token's maker is a viewer, so it didn't happen. Ask an admin to change its maker's role")
		_, err = person.services.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		allowed("a viewer reading services", err)

		// What someone holds is the highest of their grants.
		hold(store.Grant{ProjectID: "prj_somewhere", Role: store.RoleDeveloper}, onInstall(store.RoleViewer))
		allowed("someone who's a developer somewhere, planning", propose(person.changes))

		hold(onInstall(store.RoleAdmin))
		_, err = person.audit.ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
		allowed("an admin reading the audit log", err)

		hold()
		_, err = person.services.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		refused("someone with no role reading", err, "you hold no role on this install any more, so you can only sign out")
		refused("a token whose maker holds no role", propose(script), "this API token's maker holds no role")
		if _, err := b.authClient().SignOut(t.Context(), connect.NewRequest(&dockwellv1.SignOutRequest{})); err != nil {
			t.Errorf("someone with no role signing out: %v", err)
		}
	})
}

// An open stream checks its caller's role again on each recheck, as well as its
// session or token, and ends once the role, or a token's maker's, is gone. So a
// removed person's streams don't outlive their grants (ADR 0048).
func TestAStreamEndsWithItsRole(t *testing.T) {
	testRecheck = 50 * time.Millisecond
	t.Cleanup(func() { testRecheck = 0 })
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		reads := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY)
		hold := aMember(t, f)
		for _, who := range []string{"a session", "a token"} {
			hold(onInstall(store.RoleViewer))
			ev := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON())
			if who == "a token" {
				ev = dockwellv1connect.NewEventServiceClient(bearer{token: reads, rt: h2cTransport(t, f.api)}, b.origin, connect.WithProtoJSON())
			}
			ctx, cancel := context.WithCancel(t.Context())
			stream, err := ev.WatchEvents(ctx, connect.NewRequest(&dockwellv1.WatchEventsRequest{}))
			if err != nil {
				t.Fatalf("%s watching events: %v", who, err)
			}
			ended := make(chan error, 1)
			go func() {
				for stream.Receive() {
				}
				ended <- stream.Err()
			}()
			time.Sleep(200 * time.Millisecond)
			select {
			case err := <-ended:
				t.Fatalf("%s's stream ended while its role held: %v", who, err)
			default:
			}
			hold()
			select {
			case err := <-ended:
				if reasonOf(err) != "needs_role" {
					t.Errorf("%s's stream ended with %v, want needs_role", who, err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("%s's stream is open 5 s after its role went", who)
			}
			cancel()
		}
	})
}

// Below an admin, what projects name on the install reads by name and state:
// a server's name, whether it's up and its address, not its inventory, stats or
// builds; a destination's name and how it's doing, not its endpoint or bucket. An
// admin sees all of it (ADR 0049).
func TestNamedObjectsReadByNameAndStateBelowAnAdmin(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		bucket := s3test.New(t)
		apply(t, c, propose(t, c, "Send backups to R2", destinationEdit(bucket, "wJalrXUtnFEMI-K7MDENG-the-secret")))
		hold := aMember(t, f)
		servers := dockwellv1connect.NewServerServiceClient(b, b.origin, connect.WithProtoJSON())
		backups := backupClient(b, b.origin)
		read := func() (*dockwellv1.Server, *dockwellv1.BackupDestinationInfo) {
			t.Helper()
			ss, err := servers.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
			if err != nil || len(ss.Msg.GetServers()) == 0 {
				t.Fatalf("servers: %v, %v", ss, err)
			}
			ds, err := backups.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
			if err != nil || len(ds.Msg.GetDestinations()) == 0 {
				t.Fatalf("destinations: %v, %v", ds, err)
			}
			return ss.Msg.GetServers()[0], ds.Msg.GetDestinations()[0]
		}

		hold(onInstall(store.RoleViewer))
		s, d := read()
		if s.GetName() == "" || s.GetConnection() == dockwellv1.Connection_CONNECTION_UNSPECIFIED {
			t.Errorf("a viewer reads a server as %v, want its name and whether it's up", s)
		}
		if s.GetInventory() != nil || s.GetStats() != nil || len(s.GetHistory()) > 0 || s.GetAgentBuild() != nil || s.GetIngress() != nil {
			t.Errorf("a viewer reads a server's inventory, stats or builds: %v", s)
		}
		if d.GetName() != "r2" || d.GetEndpoint() != "" || d.GetBucket() != "" || d.GetPrefix() != "" {
			t.Errorf("a viewer reads a destination as %v, want its name and not where it is", d)
		}
		// A stream is trimmed as a response is.
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		watch, err := servers.WatchServers(ctx, connect.NewRequest(&dockwellv1.WatchServersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for watch.Receive() {
			if w := watch.Msg().GetServer(); w != nil {
				if w.GetName() == "" || w.GetInventory() != nil || len(w.GetHistory()) > 0 || w.GetAgentBuild() != nil {
					t.Errorf("a viewer watches a server as %v, want its name and state alone", w)
				}
				break
			}
		}
		if err := watch.Err(); err != nil {
			t.Fatalf("a viewer watching servers: %v", err)
		}
		cancel()

		hold(onInstall(store.RoleAdmin))
		s, d = read()
		if s.GetInventory() == nil || d.GetBucket() == "" {
			t.Errorf("an admin reads a server as %v and a destination as %v, want all of them", s, d)
		}
	})
}
