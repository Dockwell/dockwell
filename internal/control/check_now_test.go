// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/alerts"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Check now runs a check at once through the control plane's own runner, keeps the
// run with its others, and says how it stands; it's on Activity with who ran it, the
// host and whether it answered; a person runs 6 a minute; and it's an admin's (#832).
func TestCheckNowRunsTheCheckAtOnce(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		var failing atomic.Bool
		site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if failing.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			fmt.Fprint(w, "Welcome")
		}))
		t.Cleanup(site.Close)
		to := site.Listener.Addr().String()
		var d net.Dialer
		client := &http.Client{
			Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return d.DialContext(ctx, network, to)
			}},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		f := startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.CheckClient = client })
		b := signedIn(t, f)
		c := changeClient(b, b.origin)
		apply(t, c, propose(t, c, "Watch the shop", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "shop"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: &resourcev1.AlertCheck{Address: "http://shop.example.com/", Every: "15m"}}},
		}))
		person := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
		run := func() (*dockwellv1.AlertCheckInfo, error) {
			res, err := person.RunAlertCheck(t.Context(), connect.NewRequest(&dockwellv1.RunAlertCheckRequest{Name: "shop"}))
			if err != nil {
				return nil, err
			}
			return res.Msg.GetCheck(), nil
		}
		// The runner reads a check a plan made within moments, and may be running its
		// first run as it's asked: both are refusals that asked nothing, so they count
		// for nothing, and are asked again (the review's L2).
		var got *dockwellv1.AlertCheckInfo
		var err error
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			got, err = run()
			if err == nil || !notYet(err) || time.Now().After(deadline) {
				break
			}
		}
		if err != nil || len(got.GetRuns()) == 0 || !got.GetRuns()[0].GetOk() || got.GetRuns()[0].GetStatus() != 200 || got.GetState() != dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP {
			t.Fatalf("Check now = %v, %v", got, err)
		}
		failing.Store(true)
		got, err = run()
		if err != nil || got.GetRuns()[0].GetOk() || got.GetRuns()[0].GetStatus() != 502 || got.GetState() != dockwellv1.AlertCheckState_ALERT_CHECK_STATE_FAILING || len(got.GetRuns()) < 2 {
			t.Fatalf("Check now of a failing site = %v, %v", got, err)
		}
		if _, err := person.RunAlertCheck(t.Context(), connect.NewRequest(&dockwellv1.RunAlertCheckRequest{Name: "nothing"})); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("a check that isn't there: %v", err)
		}

		// Six a minute for a person: the runs above count, and so would a token of theirs.
		ran := 2
		for ; ran < alerts.TestsPerMinute; ran++ {
			if _, err := run(); err != nil {
				t.Fatalf("run %d: %v", ran+1, err)
			}
		}
		if _, err := run(); connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Errorf("a seventh run in a minute: %v", err)
		} else if reason := reasonOf(err); reason != "too_many_requests" {
			t.Errorf("a seventh run said %q", reason)
		}

		// Each on Activity, with the host and whether it answered.
		events, err := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON()).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
			Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ALERT}, PageSize: 50,
		}))
		if err != nil {
			t.Fatal(err)
		}
		var runs, answered int
		for _, ev := range events.Msg.GetEvents() {
			if ev.GetAction() != alerts.ActionCheckRun {
				continue
			}
			runs++
			if a := ev.GetAlert(); a.GetWhere() != "shop.example.com" || a.GetSubjectName() != "shop" {
				t.Errorf("a run's event says %v", a)
			} else if a.GetValue() == 1 {
				answered++
			}
		}
		if runs != alerts.TestsPerMinute || answered != 1 {
			t.Errorf("Activity has %d runs, %d answered", runs, answered)
		}

		// An admin's: a developer is refused.
		hold := aMember(t, f)
		hold(store.Grant{ProjectID: envProject(t, f, listedEnv(t, b)), Role: store.RoleDeveloper})
		if _, err := run(); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("a developer's Check now: %v", err)
		}
	})
}

// notYet reports a Check now refused before it asked anything, as the runner hasn't
// read the check yet or is running it now.
func notYet(err error) bool {
	return connect.CodeOf(err) == connect.CodeUnavailable ||
		connect.CodeOf(err) == connect.CodeResourceExhausted && strings.Contains(err.Error(), "running now")
}

// Review M7: a person who hangs up once the address is asked still has the run kept
// and on Activity, as answered: the run outlives the request.
func TestCheckNowKeepsARunWhoeverHangsUp(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		var hold atomic.Bool
		asked, answer := make(chan struct{}, 1), make(chan struct{})
		site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if hold.Load() {
				asked <- struct{}{}
				<-answer
			}
			fmt.Fprint(w, "Welcome")
		}))
		t.Cleanup(site.Close)
		to := site.Listener.Addr().String()
		var d net.Dialer
		client := &http.Client{
			Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return d.DialContext(ctx, network, to)
			}},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		f := startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.CheckClient = client })
		b := signedIn(t, f)
		c := changeClient(b, b.origin)
		apply(t, c, propose(t, c, "Watch the shop", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "shop"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: &resourcev1.AlertCheck{Address: "http://shop.example.com/", Every: "15m"}}},
		}))
		person := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
		// Once, so the runner has the check and its next run is 15 minutes off.
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			_, err := person.RunAlertCheck(t.Context(), connect.NewRequest(&dockwellv1.RunAlertCheckRequest{Name: "shop"}))
			if err == nil {
				break
			}
			if !notYet(err) || time.Now().After(deadline) {
				t.Fatalf("the first Check now: %v", err)
			}
		}

		hold.Store(true)
		ctx, hangUp := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := person.RunAlertCheck(ctx, connect.NewRequest(&dockwellv1.RunAlertCheckRequest{Name: "shop"}))
			done <- err
		}()
		<-asked
		hangUp()
		if err := <-done; err == nil {
			t.Error("a request hung up on answered")
		}
		close(answer)

		// Both runs on Activity as answered, and both kept with the check.
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			events, err := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON()).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
				Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ALERT}, PageSize: 50,
			}))
			if err != nil {
				t.Fatal(err)
			}
			answered := 0
			for _, ev := range events.Msg.GetEvents() {
				if ev.GetAction() == alerts.ActionCheckRun && ev.GetAlert().GetValue() == 1 && ev.GetAlert().GetVariant() == alerts.RunAnswered {
					answered++
				}
			}
			if answered == 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Activity has %d answered runs, want 2: %v", answered, events.Msg.GetEvents())
			}
		}
		checks, err := person.ListAlertChecks(t.Context(), connect.NewRequest(&dockwellv1.ListAlertChecksRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if runs := checks.Msg.GetChecks()[0].GetRuns(); len(runs) < 2 || !runs[0].GetOk() {
			t.Errorf("the check's runs after the hang-up: %v", runs)
		}
	})
}
