// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A server's files are the install's, so every way to them is an admin's: the
// calls, and the two REST paths that carry a file's bytes past Vanguard, which
// refuse before they read anything of the request (ADR 0045, ADR 0049).
func TestAServersFilesAreAnAdmins(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		hold := aMember(t, f)
		files := dockwellv1connect.NewFileServiceClient(b, b.origin, connect.WithProtoJSON())
		alerts := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
		ctx := t.Context()

		hold(onInstall(store.RoleDeveloper))
		_, err := files.MakeFile(ctx, connect.NewRequest(&dockwellv1.MakeFileRequest{ServerId: "srv_none", Path: "/srv/site"}))
		needsRole(t, "a developer making a folder on a server", err)
		_, err = files.ListDeletedFiles(ctx, connect.NewRequest(&dockwellv1.ListDeletedFilesRequest{ServerId: "srv_none"}))
		needsRole(t, "a developer listing what was deleted on a server", err)
		for _, c := range []struct{ verb, path string }{
			// Neither offset is a number, nor either path one a server has: the role comes first.
			{http.MethodPut, "/v1/servers/srv_none/uploads/upl_none?offset=later"},
			{http.MethodGet, "/v1/servers/srv_none/files:download"},
		} {
			res, _ := rawREST(t, b, c.verb, c.path, []byte("abc"), nil)
			if res.StatusCode != http.StatusForbidden || res.Header.Get("Dockwell-Error") != "needs_role" {
				t.Errorf("a developer's %s %s: %d %q, want 403 needs_role", c.verb, c.path, res.StatusCode, res.Header.Get("Dockwell-Error"))
			}
		}

		// Alerts are about every server and project, and their channels say where
		// messages go, so reading and muting them is an admin's too.
		_, err = alerts.ListAlerts(ctx, connect.NewRequest(&dockwellv1.ListAlertsRequest{}))
		needsRole(t, "a developer reading alerts", err)
		_, err = alerts.ListAlertChannels(ctx, connect.NewRequest(&dockwellv1.ListAlertChannelsRequest{}))
		needsRole(t, "a developer reading alert channels", err)
		_, err = alerts.MuteAlerts(ctx, connect.NewRequest(&dockwellv1.MuteAlertsRequest{Rule: "disk", Duration: "1h"}))
		needsRole(t, "a developer muting a rule", err)

		hold(onInstall(store.RoleAdmin))
		if _, err := alerts.ListAlerts(ctx, connect.NewRequest(&dockwellv1.ListAlertsRequest{})); err != nil {
			t.Errorf("an admin reading alerts: %v", err)
		}
		res, _ := rawREST(t, b, http.MethodPut, "/v1/servers/srv_none/uploads/upl_none?offset=later", []byte("abc"), nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("an admin's upload with an offset that's no number: %d %q, want 400 for the offset", res.StatusCode, res.Header.Get("Dockwell-Error"))
		}
	})
}
