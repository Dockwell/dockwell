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
		if rules = res.Msg.GetRules(); len(rules) == len(alert.Defaults()) || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(rules) != len(alert.Defaults()) || !slices.ContainsFunc(rules, func(r *dockwellv1.AlertRuleInfo) bool {
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
	if got := listed.Msg.GetChannels(); len(got) != 1 || got[0].GetType() != "webhook" || got[0].GetRules() != enabledDefaults() || strings.Contains(got[0].GetTarget(), "t0ken") || !strings.HasPrefix(got[0].GetTarget(), "127.0.0.1:") {
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

	// Someone says they're on it: who and when are on the alert, nothing is sent
	// for it, and saying it twice tells nobody twice.
	ack := func(c dockwellv1connect.AlertServiceClient, alertID string) (*dockwellv1.Alert, error) {
		res, err := c.AcknowledgeAlert(t.Context(), connect.NewRequest(&dockwellv1.AcknowledgeAlertRequest{Id: alertID}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetAlert(), nil
	}
	mine, err := ack(sites, down.GetId())
	if err != nil || mine.GetAcknowledgeTime() == nil || !strings.HasPrefix(mine.GetAcknowledgedBy().GetId(), "usr_") || mine.GetAcknowledgedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_USER || mine.GetAcknowledgedBy().GetName() == "" || mine.GetAcknowledgedBy().GetName() == mine.GetAcknowledgedBy().GetId() {
		t.Fatalf("AcknowledgeAlert = %v, %v; want who has it, by name and by ID, and since when", mine, err)
	}
	if again, err := ack(sites, down.GetId()); err != nil || !again.GetAcknowledgeTime().AsTime().Equal(mine.GetAcknowledgeTime().AsTime()) {
		t.Errorf("acknowledging what you have = %v, %v; want it as it stood", again, err)
	}
	// A token that can make changes takes it over, as a script that pages someone would.
	theirs, err := ack(script, down.GetId())
	if err != nil || !strings.HasPrefix(theirs.GetAcknowledgedBy().GetId(), "tok_") {
		t.Fatalf("a token's AcknowledgeAlert = %v, %v; want it taken over", theirs, err)
	}
	if listed, err := sites.ListAlerts(t.Context(), connect.NewRequest(&dockwellv1.ListAlertsRequest{FiringOnly: true})); err != nil || !slices.ContainsFunc(listed.Msg.GetAlerts(), func(a *dockwellv1.Alert) bool {
		return a.GetId() == down.GetId() && a.GetAcknowledgedBy().GetId() == theirs.GetAcknowledgedBy().GetId()
	}) {
		t.Errorf("ListAlerts doesn't say who has %s: %v, %v", down.GetId(), listed, err)
	}
	if _, err := ack(sites, "alt_nothing"); reasonOf(err) != "not_found" {
		t.Errorf("acknowledging an alert that isn't there = %v", err)
	}
	unackAs := func(c dockwellv1connect.AlertServiceClient, alertID string) error {
		_, err := c.UnacknowledgeAlert(t.Context(), connect.NewRequest(&dockwellv1.UnacknowledgeAlertRequest{Id: alertID}))
		return err
	}
	unack := func(alertID string) error { return unackAs(sites, alertID) }
	// Taking back is your own: the person it was taken over from can't take back the
	// token's, as a card that's out of date would, and it stays the token's.
	if err := unack(down.GetId()); reasonOf(err) != "not_yours" {
		t.Errorf("taking back someone else's acknowledgement = %v; want it refused as not yours", err)
	}
	if listed, err := sites.ListAlerts(t.Context(), connect.NewRequest(&dockwellv1.ListAlertsRequest{FiringOnly: true})); err != nil || !slices.ContainsFunc(listed.Msg.GetAlerts(), func(a *dockwellv1.Alert) bool {
		return a.GetId() == down.GetId() && a.GetAcknowledgedBy().GetId() == theirs.GetAcknowledgedBy().GetId()
	}) {
		t.Errorf("after a refused take-back, %s isn't the token's any more: %v, %v", down.GetId(), listed, err)
	}
	if err := unackAs(script, down.GetId()); err != nil {
		t.Fatalf("UnacknowledgeAlert, by who has it: %v", err)
	}
	if err := unack(down.GetId()); reasonOf(err) != "not_acknowledged" {
		t.Errorf("taking back what nobody has = %v", err)
	}
	if _, err := ack(sites, down.GetId()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	tick()
	if got := h.of("server-unreachable"); len(got) != 1 {
		t.Errorf("acknowledging sent something, or held something back: the webhook got %v", got)
	}

	// It comes back: once it has stayed back for the rule's 2 minutes, the alert
	// resolves, and says so where it was sent, acknowledged or not.
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

	// Resolved, there's nothing left to be on, and it keeps who had it.
	if _, err := ack(sites, down.GetId()); reasonOf(err) != "alert_resolved" {
		t.Errorf("acknowledging an alert that resolved = %v", err)
	}
	if err := unack(down.GetId()); reasonOf(err) != "alert_resolved" {
		t.Errorf("taking back the acknowledgement of an alert that resolved = %v", err)
	}
	if all, err := sites.ListAlerts(t.Context(), connect.NewRequest(&dockwellv1.ListAlertsRequest{})); err != nil || !slices.ContainsFunc(all.Msg.GetAlerts(), func(a *dockwellv1.Alert) bool {
		return a.GetId() == down.GetId() && a.GetState() == dockwellv1.AlertState_ALERT_STATE_RESOLVED && strings.HasPrefix(a.GetAcknowledgedBy().GetId(), "usr_")
	}) {
		t.Errorf("the resolved alert doesn't keep who had it: %v, %v", all, err)
	}

	// It goes again: that's another alert, and it starts as nobody's, whoever had the last.
	hz1.stop()
	waitConnected(t, f, hz1.id.ServerID, false)
	tick()
	now = now.Add(3 * time.Minute)
	tick()
	again, err := sites.ListAlerts(t.Context(), connect.NewRequest(&dockwellv1.ListAlertsRequest{FiringOnly: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(again.Msg.GetAlerts(), func(a *dockwellv1.Alert) bool {
		return a.GetRuleName() == "server-unreachable" && a.GetId() != down.GetId() && a.GetAcknowledgeTime() == nil && a.GetAcknowledgedBy() == nil
	}) {
		t.Errorf("the rule firing again isn't a new alert that nobody has: %v", again.Msg.GetAlerts())
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
	var said, seen []string
	for _, ev := range events.Msg.GetEvents() {
		said = append(said, ev.GetAction()+" "+ev.GetAlert().GetRule()+" "+ev.GetAlert().GetTitle())
		if a := ev.GetAction(); a == "alert.acknowledge" || a == "alert.unacknowledge" {
			// Who did it, by what, and from whom when it was taken over, newest first.
			by, _, _ := strings.Cut(ev.GetActor().GetId(), "_")
			from, _, _ := strings.Cut(ev.GetAlert().GetTakenFrom().GetId(), "_")
			seen = append(seen, strings.TrimPrefix(a, "alert.")+" by "+by+" from "+from+" on "+ev.GetSurface().String())
			if ev.GetAlert().GetId() != down.GetId() {
				t.Errorf("%s names the alert %q, want %s", a, ev.GetAlert().GetId(), down.GetId())
			}
		}
		switch a := ev.GetAlert(); {
		case ev.GetAction() == "alert.mute" && a.GetReason() != "moving it to a new rack":
			t.Errorf("a mute's event says why as %q", a.GetReason())
		case ev.GetAction() == "alert.firing" && a.GetRule() == "server-unreachable" && !strings.Contains(a.GetDetail(), "hasn't heard from its agent"):
			t.Errorf("a firing alert's event says what it means as %q", a.GetDetail())
		}
	}
	// One event each time something changed, and none for saying what already was:
	// Mira, the token taking it over from her, the token giving it back, Mira again.
	if want := []string{
		"acknowledge by usr from  on SURFACE_PANEL",
		"unacknowledge by tok from  on SURFACE_API",
		"acknowledge by tok from usr on SURFACE_API",
		"acknowledge by usr from  on SURFACE_PANEL",
	}; !slices.Equal(seen, want) {
		t.Errorf("Activity's acknowledgements are\n%q, want\n%q", seen, want)
	}
	if slices.Contains(said, "alert.resolved change-failed A change failed: Deploy shop") {
		t.Errorf("a failed change's alert says it resolved, on Activity: %q", said)
	}
	for _, want := range []string{
		"alert.firing server-unreachable hz-1 can't be reached",
		"alert.resolved server-unreachable hz-1 can't be reached",
		"alert.mute server-unreachable ",
		"alert.unmute server-unreachable ",
		"alert.acknowledge server-unreachable hz-1 can't be reached",
		"alert.unacknowledge server-unreachable hz-1 can't be reached",
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

// enabledDefaults is how many of an install's first rules are on: a channel counts
// those alone.
func enabledDefaults() int32 {
	n := int32(0)
	for _, d := range alert.Defaults() {
		if !d.Rule.GetDisabled() {
			n++
		}
	}
	return n
}
