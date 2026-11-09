// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/alerts"
	"github.com/dockwell/dockwell/internal/control/health"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// hook is a webhook's far end, in this test, which keeps what it's sent.
type hook struct {
	mu   sync.Mutex
	got  []map[string]any
	srv  *httptest.Server
	code int
}

func newHook(t *testing.T) *hook {
	h := &hook{code: http.StatusOK}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		h.mu.Lock()
		h.got = append(h.got, m)
		code := h.code
		h.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// of is what came for a rule, as each message's status and title.
func (h *hook) of(rule string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.got {
		r, _ := m["rule"].(map[string]any)
		if r != nil && r["name"] == rule {
			out = append(out, m["status"].(string)+": "+m["title"].(string))
		}
	}
	return out
}

// An install starts with its rules, and a channel added through a plan gets a
// test, then an alert as a server goes and comes back, each on Activity; a mute is
// no change, and goes on Activity too.
func TestAnAlertFromItsRuleToItsWebhook(t *testing.T) { storetest.Each(t, testAnAlert) }

func testAnAlert(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	sites := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
	hz1 := f.addServer(t, true)

	// The first rules come with the first start, as Dockwell's own change.
	var rules []*dockwellv1.AlertRuleInfo
	for deadline := time.Now().Add(10 * time.Second); ; {
		res, err := sites.ListAlertRules(t.Context(), connect.NewRequest(&dockwellv1.ListAlertRulesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if rules = res.Msg.GetRules(); len(rules) == 9 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(rules) != 9 || !slices.ContainsFunc(rules, func(r *dockwellv1.AlertRuleInfo) bool {
		return r.GetName() == "server-unreachable" && r.GetBuiltIn() && r.GetTitle() == "Server unreachable" && r.GetExpression() == "server.connected == false"
	}) {
		t.Fatalf("an install starts with the rules %v", rules)
	}

	// A token makes a channel, as an edit like any: it reaches what any container
	// does.
	h := newHook(t)
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := dockwellv1connect.NewAlertServiceClient(bearer{token: token, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
	scriptChanges := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	made, err := scriptChanges.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Add the ops webhook", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: "ops"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertChannel{AlertChannel: &resourcev1.AlertChannel{
			To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: h.srv.URL + "/hook?token=t0ken", Secret: "s3cret"}},
		}}},
	}}}))
	if err != nil {
		t.Fatalf("adding a channel: %v", err)
	}
	plan, _ := json.Marshal(made.Msg.GetChange().GetPlan())
	if strings.Contains(string(plan), "t0ken") || strings.Contains(string(plan), "s3cret") {
		t.Errorf("the plan says what signs in: %s", plan)
	}
	if got := apply(t, scriptChanges, made.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("adding the channel: %v", got)
	}
	listed, err := sites.ListAlertChannels(t.Context(), connect.NewRequest(&dockwellv1.ListAlertChannelsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := listed.Msg.GetChannels(); len(got) != 1 || got[0].GetType() != "webhook" || got[0].GetRules() != 9 || strings.Contains(got[0].GetTarget(), "t0ken") || !strings.HasPrefix(got[0].GetTarget(), "127.0.0.1:") {
		t.Errorf("ListAlertChannels = %v", got)
	}
	tested, err := sites.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{Name: "ops"}))
	if err != nil || !tested.Msg.GetDelivery().GetOk() || tested.Msg.GetDelivery().GetSaid() != "200 OK" {
		t.Fatalf("TestAlertChannel = %v, %v", tested, err)
	}
	if _, err := sites.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{
		Channel: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: "https://169.254.169.254/latest"}}},
	})); reasonOf(err) != "invalid" {
		t.Errorf("testing a channel to the metadata service = %v, want it refused", err)
	}

	// The control plane's own evaluator runs by the clock; this one runs by the test's.
	now := time.Now()
	e := &alerts.Evaluator{
		DB: f.store, Keeper: f.keeper, Engine: f.engine, Fleet: f.config.Fleet, Domains: f.agents,
		Health: health.New(f.store, f.agents, slog.New(slog.DiscardHandler)), Logger: slog.New(slog.DiscardHandler),
		Panel: "https://panel.example.com", Now: func() time.Time { return now },
		Sender: &alerts.Sender{},
	}
	tick := func() {
		t.Helper()
		if err := e.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		e.Wait()
	}

	// The server goes: its alert waits out the rule's 2 minutes, fires, and waits out
	// 30 seconds for others before it's sent.
	hz1.stop()
	waitConnected(t, f, hz1.id.ServerID, false)
	tick()
	now = now.Add(3 * time.Minute)
	tick()
	firing, err := sites.ListAlerts(t.Context(), connect.NewRequest(&dockwellv1.ListAlertsRequest{FiringOnly: true}))
	if err != nil {
		t.Fatal(err)
	}
	var down *dockwellv1.Alert
	for _, a := range firing.Msg.GetAlerts() {
		if a.GetRuleName() == "server-unreachable" {
			down = a
		}
	}
	if down == nil || down.GetTitle() != "hz-1 can't be reached" || down.GetSeverity() != resourcev1.AlertRule_SEVERITY_CRITICAL || down.GetServerName() != "hz-1" || down.GetSentTime() != nil {
		t.Fatalf("what's firing is %v", firing.Msg.GetAlerts())
	}
	if got := h.of("server-unreachable"); len(got) != 0 {
		t.Errorf("sent before others had the chance to join it: %v", got)
	}
	now = now.Add(31 * time.Second)
	tick()
	if got := h.of("server-unreachable"); len(got) != 1 || got[0] != "firing: hz-1 can't be reached" {
		t.Errorf("the webhook got %v", got)
	}
	// Muting the rule is no change, and goes on Activity.
	muted, err := script.MuteAlerts(t.Context(), connect.NewRequest(&dockwellv1.MuteAlertsRequest{Rule: "server-unreachable", Duration: "1h", Reason: "moving it to a new rack"}))
	if err != nil || muted.Msg.GetMute().GetRule() != "server-unreachable" {
		t.Fatalf("MuteAlerts = %v, %v", muted, err)
	}
	if _, err := script.MuteAlerts(t.Context(), connect.NewRequest(&dockwellv1.MuteAlertsRequest{Rule: "nothing", Duration: "1h"})); reasonOf(err) != "not_found" {
		t.Errorf("muting a rule that isn't there = %v", err)
	}
	if _, err := script.UnmuteAlerts(t.Context(), connect.NewRequest(&dockwellv1.UnmuteAlertsRequest{Rule: "server-unreachable"})); err != nil {
		t.Fatalf("UnmuteAlerts: %v", err)
	}

	// It comes back: once it has stayed back for the rule's 2 minutes, the alert
	// resolves, and says so where it was sent.
	hz1.connect(t, f)
	tick()
	now = now.Add(time.Minute)
	tick()
	if got := h.of("server-unreachable"); len(got) != 1 {
		t.Errorf("a minute after it came back, the webhook got %v; want it to wait out the rule's 2 minutes", got)
	}
	now = now.Add(time.Minute)
	tick()
	if got := h.of("server-unreachable"); len(got) != 2 || got[1] != "resolved: Resolved: hz-1 can't be reached" {
		t.Errorf("the webhook got %v", got)
	}

	// A failed change is told once, and is over.
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		return tx.RecordEvent(t.Context(), store.Event{ID: id.New(id.Event), At: now, Actor: revision.SystemActor, Action: "change.failed", Subject: "chg_01kat4zr6e3v0y5xbkzb5d9q6m", Detail: "Deploy shop", Surface: store.SurfaceAutomatic})
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	tick()
	now = now.Add(31 * time.Second)
	tick()
	tick()
	if got := h.of("change-failed"); len(got) != 1 || got[0] != "firing: A change failed: Deploy shop" {
		t.Errorf("the webhook got %v for the failed change", got)
	}

	// Each is on Activity, beside the changes around it.
	events, err := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON()).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ALERT}, PageSize: 50,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, ev := range events.Msg.GetEvents() {
		said = append(said, ev.GetAction()+" "+ev.GetAlert().GetRule()+" "+ev.GetAlert().GetTitle())
		switch a := ev.GetAlert(); {
		case ev.GetAction() == "alert.mute" && a.GetReason() != "moving it to a new rack":
			t.Errorf("a mute's event says why as %q", a.GetReason())
		case ev.GetAction() == "alert.firing" && a.GetRule() == "server-unreachable" && !strings.Contains(a.GetDetail(), "hasn't heard from its agent"):
			t.Errorf("a firing alert's event says what it means as %q", a.GetDetail())
		}
	}
	if slices.Contains(said, "alert.resolved change-failed A change failed: Deploy shop") {
		t.Errorf("a failed change's alert says it resolved, on Activity: %q", said)
	}
	for _, want := range []string{
		"alert.firing server-unreachable hz-1 can't be reached",
		"alert.resolved server-unreachable hz-1 can't be reached",
		"alert.mute server-unreachable ",
		"alert.unmute server-unreachable ",
		"alert.firing change-failed A change failed: Deploy shop",
		"alert.test  ",
	} {
		if !slices.Contains(said, want) {
			t.Errorf("Activity's alerts are %q, and no %q", said, want)
		}
	}

	// Deleting the channel is a plan like any.
	gone, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Remove the ops webhook", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: "ops"}, Delete: true,
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, c, gone.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("deleting the channel: %v", got)
	}
	if listed, _ := sites.ListAlertChannels(t.Context(), connect.NewRequest(&dockwellv1.ListAlertChannelsRequest{})); len(listed.Msg.GetChannels()) != 0 {
		t.Errorf("after deleting, ListAlertChannels = %v", listed.Msg.GetChannels())
	}
}

// A channel of every type keeps what signs in sealed: no plan, list, resource,
// event or log holds it, and an update that moves a channel's server asks for its
// password again rather than send the one it kept there.
func TestEveryChannelKeepsWhatSignsInSealed(t *testing.T) {
	logs := &syncBuffer{}
	f := startFullLogging(t, store.SQLite, nil, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	b := signedIn(t, f)
	rec := &recorder{next: b}
	// Uncompressed, so what's recorded can be searched.
	plain := []connect.ClientOption{connect.WithProtoJSON(), connect.WithAcceptCompression("gzip", nil, nil)}
	c := dockwellv1connect.NewChangeServiceClient(rec, b.origin, plain...)
	sites := dockwellv1connect.NewAlertServiceClient(rec, b.origin, plain...)
	channels := map[string]*resourcev1.AlertChannel{
		"hook": {To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: "https://hooks.example.com/mark-hook-url", Secret: "mark-hook-secret"}}},
		"mail": {To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{Host: "smtp.example.com", Username: "alerts", Password: "mark-mail-password", From: "alerts@example.com", To: []string{"ops@example.com"}}}},
		"push": {To: &resourcev1.AlertChannel_Ntfy{Ntfy: &resourcev1.AlertNtfy{Topic: "mark-ntfy-topic", Token: "mark-ntfy-token"}}},
		"chat": {To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: "https://hooks.slack.com/services/mark-slack-url"}}},
		"game": {To: &resourcev1.AlertChannel_Discord{Discord: &resourcev1.AlertDiscord{Url: "https://discord.com/api/webhooks/1/mark-discord-url"}}},
		"bot":  {To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{BotToken: "123456789:markmarkmarkmarkmarkmarkmarktelegram", ChatId: "-1001234567890"}}},
	}
	var edits []*dockwellv1.Edit
	for name, ch := range channels {
		edits = append(edits, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: name}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertChannel{AlertChannel: ch}}})
	}
	made := apply(t, c, propose(t, c, "Add a channel of each type", edits...))
	if made.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("adding the channels: %v", made)
	}
	listed, err := sites.ListAlertChannels(t.Context(), connect.NewRequest(&dockwellv1.ListAlertChannelsRequest{}))
	if err != nil || len(listed.Msg.GetChannels()) != 6 {
		t.Fatalf("ListAlertChannels = %v, %v", listed, err)
	}
	for _, info := range listed.Msg.GetChannels() {
		want := map[string][]string{
			"hook": {"webhook.url", "webhook.secret"}, "mail": {"email.password"}, "push": {"ntfy.topic", "ntfy.token"},
			"chat": {"slack.url"}, "game": {"discord.url"}, "bot": {"telegram.bot_token"},
		}[info.GetName()]
		if !slices.Equal(info.GetSealed(), want) || alert.Type(info.GetChannel()) != info.GetType() {
			t.Errorf("%s lists as sealing %v, as a %s; want %v", info.GetName(), info.GetSealed(), info.GetType(), want)
		}
	}
	if _, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: made.GetId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := dockwellv1connect.NewResourceServiceClient(rec, b.origin, plain...).ListResources(t.Context(), connect.NewRequest(&dockwellv1.ListResourcesRequest{PageSize: 1000})); err != nil {
		t.Fatal(err)
	}
	if _, err := dockwellv1connect.NewEventServiceClient(rec, b.origin, plain...).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{PageSize: 100})); err != nil {
		t.Fatal(err)
	}
	// Moving the email's server keeping its password is refused, in a plan, with
	// why; given again, it goes.
	moved := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{Host: "smtp.example.net", Username: "alerts", From: "alerts@example.com", To: []string{"ops@example.com"}}}}
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Move the mail", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: "mail"}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertChannel{AlertChannel: moved}},
	}}}))
	if err == nil || !strings.Contains(err.Error(), "needs its password again") {
		t.Errorf("moving the email's server keeping its password = %v", err)
	}
	// A test of a form that moves it is refused the same way.
	if _, err := sites.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{Name: "mail", Channel: moved})); reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "needs its password again") {
		t.Errorf("testing a form that moves the email's server = %v", err)
	}

	said := rec.String() + logs.String()
	if !strings.Contains(said, "smtp.example.com") || !strings.Contains(said, "ops@example.com") {
		t.Fatalf("the responses recorded hold no channel at all, so they prove nothing")
	}
	for _, mark := range []string{"mark-hook-url", "mark-hook-secret", "mark-mail-password", "mark-ntfy-topic", "mark-ntfy-token", "mark-slack-url", "mark-discord-url", "markmarkmarkmark"} {
		if strings.Contains(said, mark) {
			t.Errorf("%s left the control plane", mark)
		}
	}
}
