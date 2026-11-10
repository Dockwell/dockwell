// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"cmp"
	"net/http"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/alerts"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/revision"
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

// An alert's event is placed by what it's about: someone with a project's role
// reads those about that project's apps, and none about another project's, a
// server or a mute, which are the install's (ADR 0044, ADR 0049).
func TestAnAlertsEventIsReadWhereItsSubjectIs(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		about := map[string]string{s.apiID: "blog's api", s.webID: "default's web", s.server: "the server", "disk": "a muted rule"}
		err := s.f.store.Update(ctx, func(tx *store.Tx) error {
			for subject := range about {
				action := alerts.ActionFiring
				if subject == "disk" {
					action = alerts.ActionMute
				}
				if err := tx.RecordEvent(ctx, store.Event{
					ID: id.New(id.Event), At: time.Now(), Actor: revision.SystemActor, Action: action,
					Subject: subject, Target: s.server, Detail: `{}`, Surface: store.SurfaceAutomatic,
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		read := func() []string {
			t.Helper()
			events, err := eventClient(s.b, s.b.origin).ListEvents(ctx, connect.NewRequest(&dockwellv1.ListEventsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, e := range events.Msg.GetEvents() {
				if e.GetKind() == dockwellv1.EventKind_EVENT_KIND_ALERT {
					out = append(out, about[cmp.Or(e.GetSubject().GetId(), "disk")])
				}
			}
			slices.Sort(out)
			return out
		}
		if got := read(); len(got) != len(about) {
			t.Fatalf("the owner reads alert events about %v, want all %d", got, len(about))
		}
		s.hold(in(s.blogID, store.RoleViewer))
		if got := read(); !slices.Equal(got, []string{"blog's api"}) {
			t.Errorf("blog's viewer reads alert events about %v, want blog's api alone", got)
		}
		s.hold(onInstall(store.RoleDeveloper))
		if got := read(); !slices.Equal(got, []string{"blog's api", "default's web"}) {
			t.Errorf("a developer on the install reads alert events about %v, want every project's and none of the install's", got)
		}
	})
}
