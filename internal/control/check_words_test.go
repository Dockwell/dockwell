// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/checks"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A far end that redirects to a host written with %ff puts bytes that aren't UTF-8
// in its run's words. They can't stop the look that opens the alert, so another
// check's alert still goes out, and the lists still read.
func TestAFarEndsBytesCantStopAnAlert(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		h := newHook(t)
		f := startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.ManualAlerts = true })
		b := signedIn(t, f)
		c := changeClient(b, b.origin)
		ac := dockwellv1connect.NewAlertServiceClient(b, b.origin)
		for deadline := time.Now().Add(10 * time.Second); ; {
			res, err := ac.ListAlertRules(t.Context(), connect.NewRequest(&dockwellv1.ListAlertRulesRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Msg.GetRules()) == len(alert.Defaults()) || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		edits := []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: "ops"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertChannel{AlertChannel: &resourcev1.AlertChannel{
				To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: h.srv.URL + "/hook"}},
			}}},
		}, {
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "bad"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: &resourcev1.AlertCheck{Address: "http://bad.example.com/", Keyword: "Welcome"}}},
		}, {
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "good"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: &resourcev1.AlertCheck{Address: "http://good.example.com/"}}},
		}}
		made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Watch two", Edits: edits}))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, c, made.Msg.GetChange())

		site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.Host, "bad.") {
				w.Header().Set("Location", "https://%ff%fe.example/")
				w.WriteHeader(http.StatusFound)
				return
			}
			w.WriteHeader(http.StatusBadGateway)
		}))
		t.Cleanup(site.Close)
		to := site.Listener.Addr().String()
		var d net.Dialer
		client := &http.Client{
			Transport:     &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) { return d.DialContext(ctx, network, to) }},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		at := &clock{now: time.Now()}
		e := evaluatorOn(f, at)
		runner := &checks.Runner{DB: f.store, Keeper: f.keeper, Client: client, Inside: client, Now: at.Now}
		e.Checks = runner
		f.server.alerts.Checks = runner // the API's lists read this runner
		if err := runner.Load(t.Context()); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, st := range runner.Statuses() {
			ids = append(ids, st.ID)
		}
		tick := func() {
			if err := e.Tick(t.Context()); err != nil {
				t.Fatalf("a look at %s: %v", at.Now().Format("15:04:05"), err)
			}
			e.Wait()
		}
		for range 2 {
			for _, id := range ids {
				runner.RunNow(t.Context(), id)
			}
			for range 4 {
				tick()
				at.add(15 * time.Second)
			}
		}
		for range 8 {
			tick()
			at.add(15 * time.Second)
		}
		for _, st := range runner.Statuses() {
			if said := st.Runs[0].GetSaid(); !utf8.ValidString(said) {
				t.Errorf("check %s said %q", st.Name, said)
			}
		}
		if got := h.of("check-down"); len(got) == 0 {
			t.Error("the hook heard nothing of the checks going down")
		}
		if _, err := ac.ListAlertChecks(t.Context(), connect.NewRequest(&dockwellv1.ListAlertChecksRequest{})); err != nil {
			t.Errorf("ListAlertChecks: %v", err)
		}
		res, err := ac.ListAlerts(t.Context(), connect.NewRequest(&dockwellv1.ListAlertsRequest{}))
		if err != nil {
			t.Fatalf("ListAlerts: %v", err)
		}
		if len(res.Msg.GetAlerts()) == 0 {
			t.Error("no alert opened")
		}
	})
}
