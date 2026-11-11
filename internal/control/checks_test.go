// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/alerts"
	"github.com/dockwell/dockwell/internal/control/checks"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/store"
)

// checkStand is a site that fails when told, a check of it applied with a plan, and
// an evaluator on a test clock, with what it sends to the hook.
type checkStand struct {
	t       *testing.T
	f       *fullServer
	h       *hook
	failing atomic.Bool
	site    *httptest.Server
	client  *http.Client
	at      *clock
	e       *alerts.Evaluator
	runner  *checks.Runner
	id      string
	host    string
	c       dockwellv1connect.ChangeServiceClient
}

func newCheckStand(t *testing.T) *checkStand {
	t.Helper()
	return checkStandOf(t, &resourcev1.AlertCheck{Address: "http://shop.example.com/"})
}

// checkStandOf is a stand whose check is spec.
func checkStandOf(t *testing.T, spec *resourcev1.AlertCheck) *checkStand {
	t.Helper()
	s := &checkStand{t: t}
	s.site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if s.failing.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, "Welcome")
	}))
	t.Cleanup(s.site.Close)
	s.h = newHook(t)
	f, c, b := evalStand(t, s.h.srv.URL+"/hook")
	s.f = f
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Watch the shop", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "shop"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: spec}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, made.Msg.GetChange())
	listed, err := dockwellv1connect.NewAlertServiceClient(b, b.origin).ListAlertChecks(t.Context(), connect.NewRequest(&dockwellv1.ListAlertChecksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(listed.Msg.GetChecks()); n != 1 {
		t.Fatalf("%d checks listed, want 1", n)
	}
	got := listed.Msg.GetChecks()[0]
	if got.GetState() != dockwellv1.AlertCheckState_ALERT_CHECK_STATE_WAITING || got.GetFrom() != checks.From ||
		got.GetSpec().GetEvery() != cmp.Or(spec.GetEvery(), "1m") || got.GetSpec().GetDownAfter() != 2 || got.GetSpec().GetExpectTo() != 399 || got.GetSpec().GetInsideNetwork() {
		t.Errorf("a check just made lists as %v", got)
	}
	s.id = got.GetId()
	s.host = "shop.example.com"
	s.c = c
	s.at = &clock{now: time.Now()}
	// The shop's name leads to the test's site, as DNS would to a real one.
	to := s.site.Listener.Addr().String()
	var d net.Dialer
	s.client = &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, to)
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	s.e = evaluatorOn(f, s.at)
	s.restart(f.keeper, true)
	return s
}

// restart is the control plane starting again: a runner that knows nothing, read
// now or not yet.
func (s *checkStand) restart(keeper checks.Decoder, load bool) {
	s.t.Helper()
	s.runner = &checks.Runner{DB: s.f.store, Keeper: keeper, Client: s.client, Inside: s.client, Now: s.at.Now}
	s.e.Checks = s.runner
	if load {
		s.load()
	}
}

// edit changes the check with a plan, and the runner reads it.
func (s *checkStand) edit(spec *resourcev1.AlertCheck) {
	s.t.Helper()
	made, err := s.c.ProposeChange(s.t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Change the shop's check", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: "shop"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: spec}},
	}}}))
	if err != nil {
		s.t.Fatal(err)
	}
	apply(s.t, s.c, made.Msg.GetChange())
	s.load()
}

func (s *checkStand) load() {
	s.t.Helper()
	if err := s.runner.Load(s.t.Context()); err != nil {
		s.t.Fatal(err)
	}
}

// run runs the check now, wants it to read want, and ticks the evaluator at once
// and once more after half a minute.
func (s *checkStand) run(want dockwellv1.AlertCheckState) checks.Status {
	s.t.Helper()
	st, ok := s.runner.RunNow(s.t.Context(), s.id)
	if !ok || st.State != want {
		s.t.Fatalf("the check reads %v after its run, want %v: %v", st.State, want, st.Runs)
	}
	s.tick(31 * time.Second)
	return st
}

// tick ticks the evaluator, then again after d.
func (s *checkStand) tick(d time.Duration) {
	s.t.Helper()
	tickOn(s.t, s.e)
	s.at.add(d)
	tickOn(s.t, s.e)
}

func (s *checkStand) sent() []string { return s.h.of("check-down") }

// cadence runs the check once each every, as the runner would, failing as pattern
// says, rounds times over, and ticks the evaluator each 15 s between, as it does.
func (s *checkStand) cadence(every time.Duration, pattern []bool, rounds int) {
	s.t.Helper()
	for range rounds {
		for _, fail := range pattern {
			s.failing.Store(fail)
			if _, ok := s.runner.RunNow(s.t.Context(), s.id); !ok {
				s.t.Fatal("the check didn't run")
			}
			for gone := time.Duration(0); gone < every; gone += 15 * time.Second {
				tickOn(s.t, s.e)
				s.at.add(15 * time.Second)
			}
		}
	}
}

// count is how many of sent start with prefix.
func count(sent []string, prefix string) int {
	n := 0
	for _, m := range sent {
		if strings.HasPrefix(m, prefix) {
			n++
		}
	}
	return n
}

// downAndAlerting takes the check down and waits out the rule's 2 minutes, from the
// first failed run, so its alert is open.
func (s *checkStand) downAndAlerting() {
	s.t.Helper()
	s.run(dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP)
	s.failing.Store(true)
	s.run(dockwellv1.AlertCheckState_ALERT_CHECK_STATE_FAILING)
	down := s.run(dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN)
	if said := down.Runs[0].GetSaid(); said != "502, outside 200 to 399" {
		s.t.Errorf("the run that made it down says %q", said)
	}
	if sent := s.sent(); len(sent) != 0 {
		s.t.Fatalf("a check down for less than its rule's 2 minutes sent %q", sent)
	}
	// Opened at 2 minutes, it goes once its group has waited, at the next tick.
	s.tick(time.Minute)
	s.tick(31 * time.Second)
	if sent := s.sent(); len(sent) != 1 || !strings.HasPrefix(sent[0], "firing: "+s.host+" is down") {
		s.t.Fatalf("a check down for its rule's 2 minutes sent %q", sent)
	}
}

// A check is made with a plan, as a rule is, and lists as waiting until it runs.
// Down for its rule's 2 minutes, it alerts. It reads up again after 2 runs in a
// row pass, as it read down after 2 failed, and up for 2 minutes, it resolves
// (ADR 0066).
func TestACheckThatGoesDownAlertsAndComesBack(t *testing.T) {
	s := newCheckStand(t)
	s.downAndAlerting()
	s.failing.Store(false)
	s.run(dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN)
	s.run(dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP)
	if sent := s.sent(); len(sent) != 1 {
		t.Fatalf("a check up for half a minute sent %q", sent)
	}
	s.tick(2 * time.Minute)
	s.tick(31 * time.Second)
	if sent := s.sent(); len(sent) != 2 || !strings.HasPrefix(sent[1], "resolved: ") {
		t.Errorf("a check up for its rule's 2 minutes sent %q", sent)
	}
}

// A site that fails twice and passes once, round and round, is one outage: never
// two passes in a row, it stays down, at any interval, so it alerts once and
// doesn't resolve (ADR 0066). Every 5 minutes, each round would otherwise outlast
// the 2-minute hold and alert again; every minute, it would never stay down 2
// minutes and never alert.
func TestACheckThatFlapsIsOneAlert(t *testing.T) {
	for _, every := range []time.Duration{5 * time.Minute, time.Minute} {
		t.Run(alert.FormatDuration(every), func(t *testing.T) {
			s := checkStandOf(t, &resourcev1.AlertCheck{Address: "http://shop.example.com/", Every: alert.FormatDuration(every)})
			s.cadence(every, []bool{false}, 1)
			s.cadence(every, []bool{true, true, false}, int(20*time.Minute/every))
			if sent := s.sent(); count(sent, "firing: ") != 1 || count(sent, "resolved: ") != 0 {
				t.Errorf("a check every %v that failed 2 runs in 3 sent %q, want the one firing", every, sent)
			}
		})
	}
}

// Back up and clearing, a check that then fails every other run isn't back: each
// failure starts its clear time again, and it doesn't resolve.
func TestACheckFailingEveryOtherRunDoesntResolve(t *testing.T) {
	s := newCheckStand(t)
	s.downAndAlerting()
	s.cadence(time.Minute, []bool{false, false}, 1)
	s.cadence(time.Minute, []bool{true, false}, 5)
	if sent := s.sent(); len(sent) != 1 {
		t.Errorf("a check failing every other run sent %q, want the one firing", sent)
	}
}

// The control plane starts again during an outage. Until its runner has read the
// checks, it knows nothing of them; then one whose alert is open reads down since
// the alert opened, and stays down through a failed run. The alert stays open
// throughout, and says nothing.
func TestACheckDownAcrossARestartStaysOneAlert(t *testing.T) {
	s := newCheckStand(t)
	s.downAndAlerting()
	s.restart(s.f.keeper, false)
	s.tick(31 * time.Second)
	if sent := s.sent(); len(sent) != 1 {
		t.Fatalf("an evaluator ticking before the checks were read sent %q", sent)
	}
	s.load()
	if st := s.runner.Statuses()[0]; st.State != dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN || st.Since.IsZero() {
		t.Errorf("read again with its alert open, the check reads %v since %v, want down since the alert opened", st.State, st.Since)
	}
	s.tick(31 * time.Second)
	s.run(dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN)
	if sent := s.sent(); len(sent) != 1 {
		t.Fatalf("a restart while it was down sent %q", sent)
	}
}

// After a restart or an edit during an outage, a check whose alert is open takes
// 2 runs in a row that pass to read up, as before: one pass alone doesn't resolve
// its alert (ADR 0066).
func TestACheckThatStartsAgainDuringAnOutageTakesTwoPasses(t *testing.T) {
	for _, how := range []string{"restart", "edit"} {
		t.Run(how, func(t *testing.T) {
			s := newCheckStand(t)
			s.downAndAlerting()
			if how == "restart" {
				s.restart(s.f.keeper, true)
			} else {
				s.edit(&resourcev1.AlertCheck{Address: "http://shop.example.com/", Timeout: "9s"})
			}
			s.failing.Store(false)
			s.run(dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN)
			s.tick(3 * time.Minute)
			if sent := s.sent(); len(sent) != 1 {
				t.Fatalf("one pass after the %s sent %q, want the firing alone", how, sent)
			}
			s.run(dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP)
			s.tick(2 * time.Minute)
			s.tick(31 * time.Second)
			if sent := s.sent(); len(sent) != 2 || !strings.HasPrefix(sent[1], "resolved: ") {
				t.Errorf("2 passes after the %s sent %q, want it resolved", how, sent)
			}
		})
	}
}

// unreadable fails to read one resource's spec, as a key file restored from
// another backup would, and reads the rest.
type unreadable struct {
	checks.Decoder
	id string
}

func (u unreadable) Decode(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) (proto.Message, error) {
	if at.ResourceID == u.id {
		return nil, errors.New("the spec doesn't open with this key")
	}
	return u.Decoder.Decode(ctx, tx, at, stored)
}

// A check whose spec can't be read stays, with why, and keeps the alert it had
// open: what can't be read isn't up.
func TestACheckThatCantBeReadKeepsItsAlert(t *testing.T) {
	s := newCheckStand(t)
	s.downAndAlerting()
	s.restart(unreadable{Decoder: s.f.keeper, id: s.id}, true)
	s.tick(5 * time.Minute)
	if sent := s.sent(); len(sent) != 1 {
		t.Fatalf("a check that can't be read sent %q", sent)
	}
	st, ok := s.runner.Status(s.id)
	if !ok || st.Problem != checks.Unreadable {
		t.Errorf("a check that can't be read stands as %+v, %t", st, ok)
	}
	if _, ran := s.runner.RunNow(s.t.Context(), s.id); ran {
		t.Error("a check that can't be read ran")
	}
	// One that a running runner could read, and then can't, stops running too,
	// though the runner still holds what it read before.
	s.restart(s.f.keeper, true)
	s.runner.Keeper = unreadable{Decoder: s.f.keeper, id: s.id}
	s.load()
	if _, ran := s.runner.RunNow(s.t.Context(), s.id); ran {
		t.Error("a check that became unreadable ran")
	}
}

// Up again and clearing, a check that can no longer be read keeps its alert open
// though it last read up: what can't be read isn't up.
func TestACheckThatBecomesUnreadableAsItClearsKeepsItsAlert(t *testing.T) {
	s := newCheckStand(t)
	s.downAndAlerting()
	s.failing.Store(false)
	s.run(dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN)
	s.run(dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP)
	s.runner.Keeper = unreadable{Decoder: s.f.keeper, id: s.id}
	s.load()
	if st, _ := s.runner.Status(s.id); st.State != dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP || st.Problem != checks.Unreadable {
		t.Fatalf("the check stands as %+v", st)
	}
	s.tick(5 * time.Minute)
	s.tick(31 * time.Second)
	if sent := s.sent(); len(sent) != 1 {
		t.Errorf("a check that became unreadable as it cleared sent %q", sent)
	}
}

// An install keeps 200 checks at most, as each is a request every interval.
func TestAnInstallKeepsAtMost200Checks(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	edits := make([]*dockwellv1.Edit, 0, alert.MaxChecks+1)
	for i := range alert.MaxChecks + 1 {
		edits = append(edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: fmt.Sprintf("site-%d", i)},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: &resourcev1.AlertCheck{Address: fmt.Sprintf("https://site-%d.example.com/", i)}}},
		})
	}
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Watch every site", Edits: edits}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "keeps 200 at most") {
		t.Fatalf("a plan for 201 checks = %v", err)
	}
	if _, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Watch the sites", Edits: edits[:alert.MaxChecks]})); err != nil {
		t.Errorf("a plan for 200 checks = %v", err)
	}

	// Plans made side by side at 199 each count 199 and add one; as they apply,
	// the first takes the last place, and the rest are refused.
	first, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Watch 199 sites", Edits: edits[:alert.MaxChecks-1]}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, first.Msg.GetChange())
	var plans []*dockwellv1.Change
	for i := range 5 {
		p, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Watch one more", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: string(alert.CheckKind), Name: fmt.Sprintf("more-%d", i)},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AlertCheck{AlertCheck: &resourcev1.AlertCheck{Address: fmt.Sprintf("https://more-%d.example.com/", i)}}},
		}}}))
		if err != nil {
			t.Fatalf("plan %d at 199 checks: %v", i, err)
		}
		plans = append(plans, p.Msg.GetChange())
	}
	applied := 0
	for _, p := range plans {
		switch _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: p.GetId(), Fingerprint: p.GetPlan().GetFingerprint()})); {
		case err == nil:
			applied++
		case connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "keeps 200 at most"):
			t.Errorf("applying a plan past the cap = %v", err)
		}
	}
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		have, err := tx.ListKind(t.Context(), string(alert.CheckKind))
		if applied != 1 || len(have) != alert.MaxChecks {
			t.Errorf("%d of 5 plans made at 199 applied, and the install has %d checks", applied, len(have))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
