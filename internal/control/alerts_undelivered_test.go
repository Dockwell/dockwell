// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/alerts"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// An alert whose message no channel took says so, and its own messages list how
// each went; once the channel takes it, it reads delivered. Its messages are an
// admin's, as every channel's are, so asking for one alert's widens nothing: a
// developer is refused, as for the whole list (#794).
func TestAnAlertNoChannelTookSaysSo(t *testing.T) {
	t.Parallel()
	var refuse atomic.Bool
	refuse.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if refuse.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	f, _, b := evalStand(t, srv.URL+"/hook")
	sites := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
	at := &clock{now: time.Now()}
	e := evaluatorOn(f, at)
	e.Fleet = evalFleet{func() []*dockwellv1.Server {
		s := connectedAt(at.Now(), 50)
		s[0].Stats.Disks[0].UsedBytes = 99
		return s
	}}
	firing := func() *dockwellv1.Alert {
		t.Helper()
		res, err := sites.ListAlerts(t.Context(), connect.NewRequest(&dockwellv1.ListAlertsRequest{FiringOnly: true}))
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range res.Msg.GetAlerts() {
			if a.GetSentTime() != nil {
				return a
			}
		}
		return nil
	}
	var a *dockwellv1.Alert
	for range 60 {
		tickOn(t, e)
		at.add(alerts.Every)
		if a = firing(); a != nil && a.GetUndelivered() {
			break
		}
	}
	if a == nil || !a.GetUndelivered() {
		t.Fatalf("an alert no channel took reads %v", a)
	}
	mine := func() []*dockwellv1.AlertDelivery {
		t.Helper()
		res, err := sites.ListAlertDeliveries(t.Context(), connect.NewRequest(&dockwellv1.ListAlertDeliveriesRequest{AlertId: a.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetDeliveries()
	}
	if got := mine(); len(got) != 1 || got[0].GetOk() || got[0].GetChannel() != "ops" || got[0].GetAlertId() != a.GetId() || got[0].GetSaid() == "" {
		t.Fatalf("the alert's messages: %v", got)
	}

	// The channel takes it on a later try.
	refuse.Store(false)
	for range 60 {
		at.add(alerts.Every)
		tickOn(t, e)
		if got := firing(); got != nil && !got.GetUndelivered() {
			break
		}
	}
	if got := firing(); got == nil || got.GetUndelivered() {
		t.Fatalf("once the channel took it, the alert reads %v", got)
	}
	if got := mine(); len(got) != 1 || !got[0].GetOk() {
		t.Errorf("once taken, the alert's messages: %v", got)
	}
	if _, err := sites.ListAlertDeliveries(t.Context(), connect.NewRequest(&dockwellv1.ListAlertDeliveriesRequest{AlertId: "alt_none"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an alert there isn't: %v", err)
	}

	// Below an admin, one alert's messages are refused as the whole list is.
	var project string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		list, err := tx.ListKind(t.Context(), string(scope.ProjectKind))
		if len(list) > 0 {
			project = list[0].ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A token narrowed to a project, an admin's, reaches no install's messages either.
	secret, _ := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Projects: []string{project}})
	scripted := dockwellv1connect.NewAlertServiceClient(bearer{token: secret, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
	if _, err := scripted.ListAlertDeliveries(t.Context(), connect.NewRequest(&dockwellv1.ListAlertDeliveriesRequest{AlertId: a.GetId()})); err == nil {
		t.Error("a token narrowed to a project read one alert's messages")
	}
	aMember(t, f)(store.Grant{ProjectID: project, Role: store.RoleDeveloper})
	for name, req := range map[string]*dockwellv1.ListAlertDeliveriesRequest{"every": {}, "one alert's": {AlertId: a.GetId()}} {
		if _, err := sites.ListAlertDeliveries(t.Context(), connect.NewRequest(req)); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("a developer reads %s messages: %v", name, err)
		}
	}
}
