// SPDX-License-Identifier: AGPL-3.0-only

// Package alerts serves dockwell.v1.AlertService, and evaluates the alert rules:
// what's firing and what was, the rules and channels as changes, a channel's test,
// and mutes, which are no change. See docs/adr/0044-alerts.md.
package alerts

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/store"
)

// Bounds on what the API takes.
const (
	// DefaultPageSize and MaxPageSize bound a page of alerts that resolved.
	DefaultPageSize = 50
	MaxPageSize     = 500
	// MaxMute is the longest a mute holds.
	MaxMute = 30 * 24 * time.Hour
	// maxReason bounds why someone muted something.
	maxReason = 200
)

// Handler implements AlertService.
type Handler struct {
	db     *store.DB
	eval   *Evaluator
	logger *slog.Logger
	// tests counts each person's and token's tests in the last minute.
	tests *testers
}

var _ dockwellv1connect.AlertServiceHandler = (*Handler)(nil)

// New returns a handler that reads what eval evaluates, and sends tests
// through its sender. Rules and channels are planned as any edit is, through
// ChangeService.
func New(eval *Evaluator, logger *slog.Logger) *Handler {
	eval.init()
	return &Handler{db: eval.DB, eval: eval, logger: logger, tests: &testers{seen: map[string][]time.Time{}}}
}

func (h *Handler) internal(ctx context.Context, err error) error {
	var said *connect.Error
	if errors.As(err, &said) {
		return said
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "alert call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't complete the request; try again, and check its logs if it keeps failing"))
}

// alertOf is an alert as the API says it.
func alertOf(a store.Alert, serverName string, mutes []store.AlertMute, now time.Time) *dockwellv1.Alert {
	out := &dockwellv1.Alert{
		Id: a.ID, RuleId: a.RuleID, RuleName: a.RuleName,
		Condition: resourcev1.AlertRule_Condition(a.Condition), Severity: resourcev1.AlertRule_Severity(a.Severity),
		State: dockwellv1.AlertState_ALERT_STATE_FIRING, Title: a.Title, Detail: a.Detail,
		ServerId: a.ServerID, ServerName: serverName, SubjectId: a.SubjectID, SubjectName: a.SubjectName, Path: a.Path,
		OpenTime: timestamppb.New(a.OpenedAt),
	}
	if a.ResolvedAt != nil {
		out.State, out.ResolveTime = dockwellv1.AlertState_ALERT_STATE_RESOLVED, timestamppb.New(*a.ResolvedAt)
	}
	if a.SentAt != nil {
		out.SentTime = timestamppb.New(*a.SentAt)
	}
	facts := ReadFacts(a.Facts)
	out.Variant, out.Where, out.Value = facts.Variant, facts.Where, facts.Value
	if out.ServerName == "" {
		out.ServerName = facts.Server
	}
	out.Muted = a.ResolvedAt == nil && muted(mutes, a.RuleName, a.ServerID, now)
	return out
}

// ListAlerts returns what's firing, then what resolved, a page at a time.
func (h *Handler) ListAlerts(ctx context.Context, req *connect.Request[dockwellv1.ListAlertsRequest]) (*connect.Response[dockwellv1.ListAlertsResponse], error) {
	m := req.Msg
	size, err := paging.Size(m.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	after, more, err := paging.Read(m.GetPageToken(), "ListAlerts")
	if err != nil {
		return nil, err
	}
	now := h.eval.now()
	res := &dockwellv1.ListAlertsResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		servers, err := serverNames(ctx, tx)
		if err != nil {
			return err
		}
		mutes, err := tx.AlertMutes(ctx)
		if err != nil {
			return err
		}
		open, err := tx.OpenAlerts(ctx)
		if err != nil {
			return err
		}
		res.Firing = int32(len(open)) //nolint:gosec // as many as are open, far under 2^31
		if !more {
			slices.SortFunc(open, func(a, b store.Alert) int {
				return cmp.Or(cmp.Compare(b.Severity, a.Severity), b.OpenedAt.Compare(a.OpenedAt), strings.Compare(b.ID, a.ID))
			})
			for i := range open {
				res.Alerts = append(res.Alerts, alertOf(open[i], servers[open[i].ServerID], mutes, now))
			}
		}
		if m.GetFiringOnly() {
			return nil
		}
		var at *time.Time
		if more {
			at = &after.At
		}
		page, err := tx.ResolvedAlerts(ctx, at, after.ID, size+1)
		if err != nil {
			return err
		}
		if len(page) > size {
			page = page[:size]
			last := page[size-1]
			res.NextPageToken = paging.Token(paging.Cursor{At: last.OpenedAt, ID: last.ID})
		}
		for i := range page {
			res.Alerts = append(res.Alerts, alertOf(page[i], servers[page[i].ServerID], mutes, now))
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(res), nil
}

func serverNames(ctx context.Context, tx *store.Tx) (map[string]string, error) {
	list, err := tx.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for i := range list {
		out[list[i].ID] = list[i].Name
	}
	return out, nil
}

// ListAlertRules returns every rule, with what it's doing now.
func (h *Handler) ListAlertRules(ctx context.Context, _ *connect.Request[dockwellv1.ListAlertRulesRequest]) (*connect.Response[dockwellv1.ListAlertRulesResponse], error) {
	now := h.eval.now()
	builtIn := map[string]bool{}
	for _, d := range alert.Defaults() {
		builtIn[d.Name] = true
	}
	res := &dockwellv1.ListAlertRulesResponse{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		rules, err := readRules(ctx, tx, h.eval.Keeper)
		if err != nil {
			return err
		}
		fired, err := tx.RulesFired(ctx)
		if err != nil {
			return err
		}
		mutes, err := tx.AlertMutes(ctx)
		if err != nil {
			return err
		}
		for _, r := range rules {
			info := &dockwellv1.AlertRuleInfo{
				Id: r.id, Name: r.name, Spec: r.spec, Title: alert.Title(r.spec), Expression: alert.Expression(r.spec), BuiltIn: builtIn[r.name],
			}
			if f, ok := fired[r.id]; ok {
				info.Firing = int32(f.Open) //nolint:gosec // as many as are open
				info.LastFiredTime = timestamppb.New(f.Last)
			}
			for _, m := range mutes {
				if m.Target == "rule:"+r.name && m.Until.After(now) {
					info.MutedUntil = timestamppb.New(m.Until)
				}
			}
			res.Rules = append(res.Rules, info)
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(res), nil
}

// ListAlertChannels returns every channel, with nothing that signs in to it.
func (h *Handler) ListAlertChannels(ctx context.Context, _ *connect.Request[dockwellv1.ListAlertChannelsRequest]) (*connect.Response[dockwellv1.ListAlertChannelsResponse], error) {
	res := &dockwellv1.ListAlertChannelsResponse{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		channels, err := readChannels(ctx, tx, h.eval.Keeper)
		if err != nil {
			return err
		}
		rules, err := readRules(ctx, tx, h.eval.Keeper)
		if err != nil {
			return err
		}
		last, err := tx.LastDeliveries(ctx)
		if err != nil {
			return err
		}
		slices.SortFunc(channels, func(a, b channel) int { return strings.Compare(a.name, b.name) })
		for _, c := range channels {
			info := &dockwellv1.AlertChannelInfo{
				Id: c.id, Name: c.name, Type: alert.Type(c.spec), Target: alert.Target(c.spec),
				Channel: alert.Redacted(c.spec), Sealed: alert.Sealed(c.spec),
			}
			for _, r := range rules {
				if !r.spec.GetDisabled() && slices.ContainsFunc(channelsOf(r, channels), func(o channel) bool { return o.id == c.id }) {
					info.Rules++
				}
			}
			if d, ok := last[c.id]; ok {
				info.Last = deliveryOf(d)
			}
			res.Channels = append(res.Channels, info)
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(res), nil
}

func deliveryOf(d store.AlertDelivery) *dockwellv1.AlertDelivery {
	return &dockwellv1.AlertDelivery{Time: timestamppb.New(d.At), Ok: d.OK, Said: d.Said, About: d.About, Channel: d.ChannelName}
}

// Bounds on a page of deliveries.
const (
	defaultDeliveries = 50
	maxDeliveries     = 200
)

// ListAlertDeliveries returns the newest messages to every channel.
func (h *Handler) ListAlertDeliveries(ctx context.Context, req *connect.Request[dockwellv1.ListAlertDeliveriesRequest]) (*connect.Response[dockwellv1.ListAlertDeliveriesResponse], error) {
	size := int(req.Msg.GetPageSize())
	switch {
	case size < 0:
		return nil, authapi.Invalid(errors.New("a page size is a number from 1 to 200, or 0 for 50"))
	case size == 0:
		size = defaultDeliveries
	case size > maxDeliveries:
		size = maxDeliveries
	}
	res := &dockwellv1.ListAlertDeliveriesResponse{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		recent, err := tx.RecentDeliveries(ctx, size)
		for i := range recent {
			res.Deliveries = append(res.Deliveries, deliveryOf(recent[i]))
		}
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(res), nil
}

// TestAlertChannel sends a test message, and says what the far end answered. Each
// person or token sends TestsPerMinute at most, and each test is on Activity.
func (h *Handler) TestAlertChannel(ctx context.Context, req *connect.Request[dockwellv1.TestAlertChannelRequest]) (*connect.Response[dockwellv1.TestAlertChannelResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	client, _ := authapi.ClientFrom(ctx)
	m := req.Msg
	spec := proto.CloneOf(m.GetChannel())
	var stored *channel
	if m.GetName() != "" {
		err := h.db.View(ctx, func(tx *store.Tx) error {
			all, err := readChannels(ctx, tx, h.eval.Keeper)
			for i := range all {
				if all[i].name == m.GetName() {
					stored = &all[i]
				}
			}
			return err
		})
		if err != nil {
			return nil, h.internal(ctx, err)
		}
		if stored == nil {
			return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no alert channel %s. List them to find its name", m.GetName()), "not_found")
		}
	}
	switch {
	case spec == nil && stored == nil:
		return nil, authapi.Invalid(errors.New("give the channel to test, by name or as a form holds it"))
	case spec == nil:
		spec = proto.CloneOf(stored.spec)
	}
	if err := (alert.Channel{}).Validate(spec); err != nil {
		return nil, authapi.Invalid(err)
	}
	if stored != nil {
		if _, err := (alert.Channel{}).Keep(stored.spec, spec); err != nil {
			return nil, authapi.Invalid(err)
		}
	}
	if err := (alert.Channel{}).CheckNew(spec); err != nil {
		return nil, authapi.Invalid(err)
	}
	now := h.eval.now()
	if wait := h.tests.allow(p.Actor(), now); wait > 0 {
		err := connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("you've sent %d tests in the last minute, the most Dockwell sends. Try again in %d seconds", TestsPerMinute, int(wait.Round(time.Second)/time.Second)))
		err.Meta().Set(authapi.ReasonKey, "too_many_requests")
		return nil, err
	}
	msg := Message{Status: "test", Panel: h.eval.Panel, At: now}
	r := h.eval.Sender.Test(ctx, spec, msg)
	d := &dockwellv1.AlertDelivery{Time: timestamppb.New(now), Ok: r.OK, Said: r.Said, About: msg.About(), Channel: m.GetName()}
	// What a channel's deliveries say is how it is, so a form's test that isn't the
	// channel as it's kept, such as one with a URL that's yet to be planned, says
	// nothing of it there.
	asStored := stored != nil && proto.Equal(spec, stored.spec)
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		if asStored {
			rec := store.AlertDelivery{ID: id.New(id.Alert), ChannelID: stored.id, ChannelName: stored.name, At: now, OK: r.OK, Said: r.Said, About: msg.About()}
			if err := tx.AddAlertDelivery(ctx, rec); err != nil {
				return err
			}
		}
		return recordTest(ctx, tx, m.GetName(), spec, r, p.Actor(), client.Surface(p), now)
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.TestAlertChannelResponse{Delivery: d}), nil
}

// recordTest puts a test on Activity: who sent it, to what type of channel and
// which host, and whether it arrived.
func recordTest(ctx context.Context, tx *store.Tx, name string, spec *resourcev1.AlertChannel, r Result, actor, surface string, now time.Time) error {
	told := Told{SubjectName: name, Facts: Facts{Variant: alert.Type(spec), Where: alert.Host(spec)}, Reason: r.Said}
	if r.OK {
		told.Facts.Value = 1
	}
	detail, err := json.Marshal(told)
	if err != nil {
		return err
	}
	return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: now, Actor: actor, Action: ActionTest, Detail: string(detail), Surface: surface})
}

// ListAlertMutes returns the mutes in force.
func (h *Handler) ListAlertMutes(ctx context.Context, _ *connect.Request[dockwellv1.ListAlertMutesRequest]) (*connect.Response[dockwellv1.ListAlertMutesResponse], error) {
	now := h.eval.now()
	res := &dockwellv1.ListAlertMutesResponse{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		mutes, err := tx.AlertMutes(ctx)
		if err != nil {
			return err
		}
		servers, err := serverNames(ctx, tx)
		if err != nil {
			return err
		}
		names := actors.New().In(ctx, tx)
		for _, m := range mutes {
			if m.Until.After(now) {
				res.Mutes = append(res.Mutes, muteOf(m, servers, names))
			}
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(res), nil
}

// muteOf is a mute as the API says it: who muted it by the name Activity shows.
func muteOf(m store.AlertMute, servers map[string]string, names actors.Lookup) *dockwellv1.AlertMute {
	out := &dockwellv1.AlertMute{Until: timestamppb.New(m.Until), By: cmp.Or(names.Actor(m.Actor).GetName(), m.Actor), Reason: m.Reason}
	if r, ok := strings.CutPrefix(m.Target, "rule:"); ok {
		out.Rule = r
	}
	if s, ok := strings.CutPrefix(m.Target, "server:"); ok {
		out.ServerId, out.ServerName = s, servers[s]
	}
	return out
}

// target is what a mute names, checked: a rule that's there, or a server, by ID or
// name. It returns the mute's target, its subject's ID for the audit log, and the
// server's ID when it's a server.
func (h *Handler) target(ctx context.Context, ruleName, server string) (target, subject, serverID string, err error) {
	if (ruleName == "") == (server == "") {
		return "", "", "", authapi.Invalid(errors.New("name a rule or a server to mute, one of the two"))
	}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		if ruleName != "" {
			rules, err := readRules(ctx, tx, h.eval.Keeper)
			for _, r := range rules {
				if r.name == ruleName {
					target, subject = "rule:"+r.name, r.id
				}
			}
			return err
		}
		list, err := tx.ListServers(ctx)
		for i := range list {
			s := &list[i]
			if s.RevokedAt == nil && (s.ID == server || s.Name == server) {
				target, subject, serverID = "server:"+s.ID, s.ID, s.ID
			}
		}
		return err
	})
	if err != nil {
		return "", "", "", h.internal(ctx, err)
	}
	if target == "" {
		what := "alert rule " + ruleName
		if server != "" {
			what = "server " + server
		}
		return "", "", "", authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no %s. List them to find its name", what), "not_found")
	}
	return target, subject, serverID, nil
}

// MuteAlerts holds what a rule or a server sends until a time, on the audit log.
func (h *Handler) MuteAlerts(ctx context.Context, req *connect.Request[dockwellv1.MuteAlertsRequest]) (*connect.Response[dockwellv1.MuteAlertsResponse], error) {
	m := req.Msg
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	client, _ := authapi.ClientFrom(ctx)
	d, err := alert.ParseDuration("duration", m.GetDuration())
	switch {
	case err != nil:
		return nil, authapi.Invalid(errors.New("a mute's duration is a duration, such as 30m, 1h or 2d"))
	case d < time.Minute:
		return nil, authapi.Invalid(errors.New("a mute lasts a minute at least. Give a duration, such as 1h"))
	case d > MaxMute:
		return nil, authapi.Invalid(errors.New("a mute lasts 30 days at most, so nothing stays quiet and forgotten. Turn the rule off for longer"))
	}
	reason := strings.TrimSpace(m.GetReason())
	if strings.ContainsAny(reason, "\r\n") || utf8.RuneCountInString(reason) > maxReason {
		return nil, authapi.Invalid(fmt.Errorf("a mute's reason is one line of up to %d characters", maxReason))
	}
	target, subject, serverID, err := h.target(ctx, m.GetRule(), m.GetServer())
	if err != nil {
		return nil, err
	}
	now := h.eval.now()
	mute := store.AlertMute{Target: target, Until: now.Add(d), Actor: p.Actor(), Reason: reason, CreatedAt: now}
	var said *dockwellv1.AlertMute
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.PutAlertMute(ctx, mute); err != nil {
			return err
		}
		servers, err := serverNames(ctx, tx)
		if err != nil {
			return err
		}
		said = muteOf(mute, servers, actors.New().In(ctx, tx))
		return recordMute(ctx, tx, ActionMute, mute, subject, serverID, p.Actor(), client.Surface(p), now)
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.MuteAlertsResponse{Mute: said}), nil
}

// UnmuteAlerts ends a mute now.
func (h *Handler) UnmuteAlerts(ctx context.Context, req *connect.Request[dockwellv1.UnmuteAlertsRequest]) (*connect.Response[dockwellv1.UnmuteAlertsResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	client, _ := authapi.ClientFrom(ctx)
	target, subject, serverID, err := h.target(ctx, req.Msg.GetRule(), req.Msg.GetServer())
	if err != nil {
		return nil, err
	}
	now := h.eval.now()
	var had bool
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if had, err = tx.DeleteAlertMute(ctx, target); err != nil || !had {
			return err
		}
		return recordMute(ctx, tx, ActionUnmute, store.AlertMute{Target: target, Until: now}, subject, serverID, p.Actor(), client.Surface(p), now)
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	if !had {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("that isn't muted, so there's no mute to end"), "not_muted")
	}
	return connect.NewResponse(&dockwellv1.UnmuteAlertsResponse{}), nil
}

func recordMute(ctx context.Context, tx *store.Tx, action string, m store.AlertMute, subject, serverID, actor, surface string, now time.Time) error {
	until := m.Until
	told := Told{Until: &until, Reason: m.Reason, ServerID: serverID}
	if r, ok := strings.CutPrefix(m.Target, "rule:"); ok {
		told.Rule = r
	}
	detail, err := json.Marshal(told)
	if err != nil {
		return err
	}
	return tx.RecordEvent(ctx, store.Event{
		ID: id.New(id.Event), At: now, Actor: actor, Action: action, Subject: subject, Target: serverID, Detail: string(detail), Surface: surface,
	})
}
