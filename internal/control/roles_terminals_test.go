// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A terminal is root on the server. Only an admin in a signed-in session can
// open, read or control one, and an attachment ends when that role is lost.
// Detached terminals still follow the session without keeping it alive.
func TestATerminalNeedsAnAdminThroughout(t *testing.T) {
	testRecheck = 50 * time.Millisecond
	t.Cleanup(func() { testRecheck = 0 })
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		server := f.addServer(t, false).id.ServerID
		hold := aMember(t, f)
		hold(onInstall(store.RoleAdmin))
		term := openTerminal(t, b, server)
		attached := attachTo(t, b, term.GetId(), 0)
		hold(onInstall(store.RoleDeveloper))
		select {
		case err := <-attached.errs:
			if reasonOf(err) != "needs_role" {
				t.Errorf("an attachment after its caller lost admin = %v, want needs_role", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the attachment is still open after its caller lost admin")
		}
		client := terminalClient(b, b.origin)
		refused := func(what string, err error) {
			t.Helper()
			if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_role" {
				t.Errorf("%s: %v, want needs_role", what, err)
			}
		}
		for _, grant := range []store.Grant{onInstall(store.RoleViewer), onInstall(store.RoleDeveloper), {ProjectID: "prj_somewhere", Role: store.RoleDeveloper}} {
			hold(grant)
			_, err := client.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: server, Columns: 80, Rows: 24}))
			refused("opening", err)
			_, err = client.ListTerminals(t.Context(), connect.NewRequest(&dockwellv1.ListTerminalsRequest{ServerId: server}))
			refused("listing", err)
			_, err = client.WriteTerminal(t.Context(), connect.NewRequest(&dockwellv1.WriteTerminalRequest{TerminalId: term.GetId(), Data: []byte("id\n")}))
			refused("typing", err)
			_, err = client.CloseTerminal(t.Context(), connect.NewRequest(&dockwellv1.CloseTerminalRequest{TerminalId: term.GetId()}))
			refused("closing", err)
			stream, err := client.AttachTerminal(t.Context(), connect.NewRequest(&dockwellv1.AttachTerminalRequest{TerminalId: term.GetId()}))
			if err == nil {
				for stream.Receive() {
					t.Errorf("a lower role read %v", stream.Msg())
				}
				err = stream.Err()
			}
			refused("attaching", err)
			if code, reason, body := callREST(t, b, http.MethodPost, "/v1/servers/"+server+"/terminals", nil, "{"); code != http.StatusForbidden || reason != "needs_role" {
				t.Errorf("REST terminal role check before parsing = %d %s %s", code, reason, body)
			}
		}
		hold(onInstall(store.RoleAdmin))
		if _, err := client.CloseTerminal(t.Context(), connect.NewRequest(&dockwellv1.CloseTerminalRequest{TerminalId: term.GetId()})); err != nil {
			t.Errorf("an admin closing the terminal = %v", err)
		}
	})
}
