// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/health"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// How evaluation goes.
const (
	// Every is how often the rules are evaluated.
	Every = 15 * time.Second
	// GroupWait is how long the first of a rule's alerts waits for others, so they
	// go as one message. It counts from the oldest that hasn't gone yet, so a rule
	// whose alerts keep coming doesn't hold back the first.
	GroupWait = 30 * time.Second
	// StopGrace is how long a message on its way gets to land as the control plane
	// stops. One that doesn't stays in the outbox, and goes at the next start.
	StopGrace = 5 * time.Second
	// GiveUpAfter is how long the outbox tries a channel with one message.
	GiveUpAfter = 24 * time.Hour
	// Retention is how long alerts that closed, and messages to channels, are kept.
	Retention = 90 * 24 * time.Hour
	// pruneEvery is how often what's past Retention is forgotten.
	pruneEvery = time.Hour
	// band is how far under its threshold a disk or memory has to come for its
	// alert to resolve, in percent points, so a level that hovers there doesn't
	// open and resolve over and over.
	band = 5
	// statsGoodFor is how old a server's stats may be and still say how full it is.
	statsGoodFor = 2 * time.Minute
	// changesBack is how far back failed changes are told, after a long stop.
	changesBack = time.Hour
)

// retries are the waits before each try of a message after the first, the last of
// which repeats until GiveUpAfter.
var retries = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}

func retryAfter(tries int) time.Duration {
	return retries[min(max(tries-1, 0), len(retries)-1)]
}

// Actions on the audit log, kind ALERT on Activity.
const (
	ActionFiring   = "alert.firing"
	ActionResolved = "alert.resolved"
	ActionMute     = "alert.mute"
	ActionUnmute   = "alert.unmute"
	// ActionAcknowledge and ActionUnacknowledge are someone saying they're on an
	// alert, and taking that back.
	ActionAcknowledge   = "alert.acknowledge"
	ActionUnacknowledge = "alert.unacknowledge"
	// ActionTest is a test sent to a channel, kept or as a form held it.
	ActionTest = "alert.test"
)

// Actions are every action an alert event has.
var Actions = []string{ActionFiring, ActionResolved, ActionMute, ActionUnmute, ActionAcknowledge, ActionUnacknowledge, ActionTest}

// settingChangesTold is where the last failed change told is kept, as Unix
// milliseconds.
const settingChangesTold = "alerts.changes_told"

// Told is what an alert event says, as the audit log keeps it in its detail: the
// one kind of detail that leaves the store, as this package is its whole writer.
type Told struct {
	ID          string     `json:"id,omitempty"`
	Rule        string     `json:"rule,omitempty"`
	Severity    int32      `json:"severity,omitempty"`
	Condition   int32      `json:"condition,omitempty"`
	Title       string     `json:"title,omitempty"`
	SubjectName string     `json:"subject_name,omitempty"`
	Facts       Facts      `json:"facts,omitzero"`
	ServerID    string     `json:"server_id,omitempty"`
	Path        string     `json:"path,omitempty"`
	OpenedAt    *time.Time `json:"opened_at,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	// TakenFrom is who had the alert before, as an actor, on an acknowledgement
	// that took it over, so the event says both without the one before it.
	TakenFrom string `json:"taken_from,omitempty"`
}

// Facts are what an alert's title says, apart, so the panel can say it in its own
// language. The title, in English, goes in messages.
type Facts struct {
	// Variant is which of a condition's titles it has: for an app that's down,
	// "unhealthy", "not_running", "restarting" or "flapping"; for a backup,
	// "failed" or "copy".
	Variant string `json:"variant,omitempty"`
	// Server is the name of the server it's on, when it's on one.
	Server string `json:"server,omitempty"`
	// Where is a disk's mount point, or a backup's destination.
	Where string `json:"where,omitempty"`
	// Value is how full a disk or memory is, in percent, or how many days a
	// certificate has left.
	Value int32 `json:"value,omitempty"`
}

// ReadFacts reads what an alert's title says, as the store keeps it.
func ReadFacts(raw string) Facts {
	var f Facts
	_ = json.Unmarshal([]byte(raw), &f)
	return f
}

func (f Facts) json() string {
	if f == (Facts{}) {
		return ""
	}
	b, _ := json.Marshal(f)
	return string(b)
}

// ReadTold reads what an alert event says.
func ReadTold(detail string) (Told, bool) {
	var t Told
	if json.Unmarshal([]byte(detail), &t) != nil {
		return Told{}, false
	}
	return t, true
}

// Fleet is the servers, as the control plane last heard of each.
type Fleet interface {
	List() []*dockwellv1.Server
}

// Domains is what each server's ingress last said of a domain.
type Domains interface {
	Domain(server, domain string) (status *dockwellv1.DomainStatus, connected bool)
}

// Health is how every service is on each server it's placed on.
type Health interface {
	Placed(ctx context.Context) ([]*dockwellv1.ServiceHealth, error)
}

// Standings say how a service stands on a server as the health recorder folds
// it, which is what app-down judges by (ADR 0027, ADR 0044).
type Standings interface {
	Standing(server, project, service string) health.Standing
}

// Evaluator decides, from what the control plane holds already, what each rule
// holds for, opens and resolves alerts, and sends what's due (ADR 0044).
type Evaluator struct {
	DB      *store.DB
	Keeper  *secrets.Keeper
	Engine  *revision.Engine
	Fleet   Fleet
	Domains Domains
	Health  Health
	// Standings say whether an app is down. Nil judges by Health's states alone.
	Standings Standings
	Sender    *Sender
	// Panel is where the panel answers, for links in messages. Empty puts none.
	Panel  string
	Logger *slog.Logger
	// Now is the time. Nil is time.Now.
	Now func() time.Time

	once    sync.Once
	started time.Time
	mu      sync.Mutex
	// pending is since when a rule's condition has held for a thing that hasn't
	// fired yet, by rule ID and thing, as far as its signal doesn't say itself.
	pending map[string]time.Time
	// clearing is since when an open alert's condition has been clear, by rule ID
	// and thing: it resolves once that has lasted the rule's duration.
	clearing map[string]time.Time
	// sending are the outbox's messages on their way, by ID, which the next look
	// leaves alone.
	sending map[string]bool
	// busy are the channels a message is on its way to, by ID: one goes at a time,
	// so a channel that limits how fast it takes them isn't sent several at once.
	busy map[string]bool
	// held are the channels that asked Dockwell to slow down, by ID, until when.
	held map[string]time.Time
	// sendCtx is what messages go with: it outlives Run's by StopGrace.
	sendCtx context.Context
	// pruned is when what's past Retention was last forgotten.
	pruned time.Time
	// sends are the messages on their way.
	sends sync.WaitGroup
}

func (e *Evaluator) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Evaluator) init() {
	e.once.Do(func() {
		e.started = e.now()
		e.pending = map[string]time.Time{}
		e.clearing = map[string]time.Time{}
		e.sending = map[string]bool{}
		e.busy = map[string]bool{}
		e.held = map[string]time.Time{}
		if e.Logger == nil {
			e.Logger = slog.Default()
		}
		if e.Sender == nil {
			e.Sender = &Sender{}
		}
	})
}

// Run evaluates every Every until ctx ends, then gives what's on its way StopGrace
// to land. What doesn't stays in the outbox for the next start. The install's
// first rules are MakeDefaults', which the control plane calls first.
func (e *Evaluator) Run(ctx context.Context) {
	e.init()
	sendCtx, stopSends := context.WithCancel(context.WithoutCancel(ctx))
	e.mu.Lock()
	e.sendCtx = sendCtx
	e.mu.Unlock()
	defer func() {
		late := time.AfterFunc(StopGrace, stopSends)
		e.sends.Wait()
		late.Stop()
		stopSends()
	}()
	t := time.NewTicker(Every)
	defer t.Stop()
	for {
		if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
			e.Logger.ErrorContext(ctx, "can't evaluate the alert rules", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Wait waits for the messages on their way, for a test.
func (e *Evaluator) Wait() { e.sends.Wait() }

// verdict is whether a rule holds for a thing.
type verdict int

const (
	fine verdict = iota
	holds
	// unknown is a thing whose signal can't be read now: an open alert stays open,
	// and a pending one pending.
	unknown
	// nearly is a level under its rule's threshold by less than the band: an open
	// alert isn't clear of it yet, so it stays open, and a pending one isn't over it,
	// so it starts again. The band is for resolving alone.
	nearly
)

// finding is what a rule's signal says of one thing.
type finding struct {
	thing   string
	verdict verdict
	// since is when it began to hold, where the signal says. Zero is when it was
	// first seen to.
	since time.Time
	// prefix says thing is the start of every thing it's about, which are unknown
	// alike, as a server's disks are while its stats can't be read.
	prefix                                 bool
	title, detail                          string
	serverID, subjectID, subjectName, path string
	// facts are what title says, apart, for the panel.
	facts Facts
}

// rule is a rule as the store has it.
type rule struct {
	id, name string
	spec     *resourcev1.AlertRule
}

// channel is a channel as the store has it, opened.
type channel struct {
	id, name string
	spec     *resourcev1.AlertChannel
}

// state is what one tick reads of the store.
type state struct {
	rules    []rule
	channels []channel
	open     []store.Alert
	owed     []store.Alert
	mutes    []store.AlertMute
}

func (e *Evaluator) read(ctx context.Context) (state, error) {
	var st state
	err := e.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		if st.rules, err = readRules(ctx, tx, e.Keeper); err != nil {
			return err
		}
		if st.channels, err = readChannels(ctx, tx, e.Keeper); err != nil {
			return err
		}
		if st.open, err = tx.OpenAlerts(ctx); err != nil {
			return err
		}
		if st.owed, err = tx.OwedResolved(ctx); err != nil {
			return err
		}
		st.mutes, err = tx.AlertMutes(ctx)
		return err
	})
	return st, err
}

func readRules(ctx context.Context, tx *store.Tx, keeper *secrets.Keeper) ([]rule, error) {
	stored, err := tx.ListKind(ctx, string(alert.RuleKind))
	if err != nil {
		return nil, err
	}
	out := make([]rule, 0, len(stored))
	for i := range stored {
		r := &stored[i]
		spec, err := keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, fmt.Errorf("can't read the alert rule %s: %w", r.Name, err)
		}
		out = append(out, rule{id: r.ID, name: r.Name, spec: spec.(*resourcev1.AlertRule)})
	}
	slices.SortFunc(out, func(a, b rule) int {
		return cmp.Or(cmp.Compare(a.spec.GetCondition(), b.spec.GetCondition()), cmp.Compare(a.name, b.name))
	})
	return out, nil
}

func readChannels(ctx context.Context, tx *store.Tx, keeper *secrets.Keeper) ([]channel, error) {
	stored, err := tx.ListKind(ctx, string(alert.ChannelKind))
	if err != nil {
		return nil, err
	}
	out := make([]channel, 0, len(stored))
	for i := range stored {
		r := &stored[i]
		spec, err := keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, fmt.Errorf("can't read the alert channel %s: %w", r.Name, err)
		}
		out = append(out, channel{id: r.ID, name: r.Name, spec: spec.(*resourcev1.AlertChannel)})
	}
	return out, nil
}

// channelsOf are where a rule sends: those it names, or every one.
func channelsOf(r rule, all []channel) []channel {
	if len(r.spec.GetChannels()) == 0 {
		return all
	}
	var out []channel
	for _, c := range all {
		if slices.Contains(r.spec.GetChannels(), c.name) {
			out = append(out, c)
		}
	}
	return out
}

// muted says whether a mute holds what an alert of a rule sends now.
func muted(mutes []store.AlertMute, ruleName, serverID string, now time.Time) bool {
	for _, m := range mutes {
		if m.Until.After(now) && (m.Target == "rule:"+ruleName || (serverID != "" && m.Target == "server:"+serverID)) {
			return true
		}
	}
	return false
}

// Tick evaluates every rule once, queues what's due in the outbox, and sends what
// the outbox has due.
func (e *Evaluator) Tick(ctx context.Context) error {
	e.init()
	now := e.now()
	st, err := e.read(ctx)
	if err != nil {
		return err
	}
	sig := &signals{e: e, now: now}
	byRule := map[string][]store.Alert{}
	for i := range st.open {
		byRule[st.open[i].RuleID] = append(byRule[st.open[i].RuleID], st.open[i])
	}
	live := map[string]bool{}
	removed := map[string]string{}
	for _, r := range st.rules {
		live[r.id] = !r.spec.GetDisabled()
		if r.spec.GetDisabled() {
			removed[r.id] = "disabled"
			continue
		}
		found, err := sig.of(ctx, r)
		if err != nil {
			e.Logger.WarnContext(ctx, "can't read what an alert rule watches", slog.String("rule", r.name), slog.String("error", err.Error()))
			continue
		}
		notice := r.spec.GetCondition() == resourcev1.AlertRule_CONDITION_CHANGE_FAILED
		opens, resolves := e.judge(r, found, byRule[r.id], now, notice)
		if notice {
			// What's queued, or has nowhere to go, is over: a failed change is. The
			// outbox still delivers what was queued.
			mine := byRule[r.id]
			for i := range mine {
				if mine[i].SentAt != nil || len(channelsOf(r, st.channels)) == 0 || muted(st.mutes, r.name, mine[i].ServerID, now) {
					resolves = append(resolves, resolution{alert: mine[i], quiet: true})
				}
			}
		}
		if err := e.write(ctx, opens, resolves, now); err != nil {
			return err
		}
		if notice {
			if err := e.advanceChanges(ctx, sig.lastChange); err != nil {
				return err
			}
		}
	}
	// A rule that went, or was turned off, closes its alerts. One whose firing
	// message was queued gets a resolved one that says why.
	var gone []resolution
	for ruleID, alerts := range byRule {
		if live[ruleID] {
			continue
		}
		why := cmp.Or(removed[ruleID], "removed")
		for i := range alerts {
			gone = append(gone, resolution{alert: alerts[i], ended: why})
		}
	}
	if len(gone) > 0 {
		if err := e.write(ctx, nil, gone, now); err != nil {
			return err
		}
	}
	e.forget(live)
	if err := e.queue(ctx, now); err != nil {
		return err
	}
	if err := e.deliver(ctx, now); err != nil {
		return err
	}
	return e.prune(ctx, now)
}

// forget drops what's held for rules that went, or were turned off, so one that
// comes back starts its clocks again.
func (e *Evaluator) forget(live map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, held := range []map[string]time.Time{e.pending, e.clearing} {
		for key := range held {
			ruleID, _, _ := strings.Cut(key, "|")
			if !live[ruleID] {
				delete(held, key)
			}
		}
	}
}

// resolution is an alert to close: one that resolved, or, with ended, one whose
// rule was removed or turned off. quiet says nothing is owed of it.
type resolution struct {
	alert store.Alert
	ended string
	quiet bool
}

// judge decides, for one rule, which alerts open and which resolve, from what its
// signal found and what's open of it, and keeps what's pending. An open alert
// resolves once its condition has been clear for the rule's duration, so one that
// dips and comes back stays one alert. A notice's alerts are never gone, as its
// signal finds each once.
func (e *Evaluator) judge(r rule, found []finding, open []store.Alert, now time.Time, notice bool) (opens []store.Alert, resolves []resolution) {
	e.mu.Lock()
	defer e.mu.Unlock()
	byThing := make(map[string]*store.Alert, len(open))
	for i := range open {
		byThing[open[i].Thing] = &open[i]
	}
	seen := make(map[string]bool, len(found))
	var dark []string
	wait := alert.Duration(r.spec)
	for i := range found {
		f := &found[i]
		if f.prefix {
			dark = append(dark, f.thing)
			continue
		}
		seen[f.thing] = true
		key := r.id + "|" + f.thing
		a, isOpen := byThing[f.thing]
		switch f.verdict {
		case holds:
			delete(e.clearing, key)
		case unknown:
			continue
		case nearly:
			if isOpen {
				delete(e.clearing, key)
			} else {
				delete(e.pending, key)
			}
			continue
		case fine:
			delete(e.pending, key)
			if !isOpen {
				continue
			}
			cleared, ok := e.clearing[key]
			if !ok {
				cleared = now
				e.clearing[key] = cleared
			}
			if now.Sub(cleared) >= wait {
				delete(e.clearing, key)
				resolves = append(resolves, resolution{alert: *a})
			}
			continue
		}
		if isOpen {
			continue
		}
		since := f.since
		if since.IsZero() {
			p, ok := e.pending[key]
			if !ok {
				p = now
				e.pending[key] = p
			}
			since = p
		}
		if now.Sub(since) < wait {
			if _, ok := e.pending[key]; !ok && f.since.IsZero() {
				e.pending[key] = since
			}
			continue
		}
		delete(e.pending, key)
		opens = append(opens, store.Alert{
			ID: id.New(id.Alert), RuleID: r.id, RuleName: r.name, Thing: f.thing,
			Condition: int32(r.spec.GetCondition()), Severity: int32(r.spec.GetSeverity()),
			ServerID: f.serverID, SubjectID: f.subjectID, SubjectName: f.subjectName,
			Title: f.title, Detail: f.detail, Path: f.path, Facts: f.facts.json(), OpenedAt: now,
		})
	}
	// What's gone, such as a service deleted or a server removed, resolves: there's
	// nothing left to hold for.
	inDark := func(thing string) bool {
		return slices.ContainsFunc(dark, func(p string) bool { return strings.HasPrefix(thing, p) })
	}
	for thing, a := range byThing {
		if !notice && !seen[thing] && !inDark(thing) {
			delete(e.clearing, r.id+"|"+thing)
			resolves = append(resolves, resolution{alert: *a})
		}
	}
	prefix := r.id + "|"
	for _, held := range []map[string]time.Time{e.pending, e.clearing} {
		for key := range held {
			thing := strings.TrimPrefix(key, prefix)
			if strings.HasPrefix(key, prefix) && !seen[thing] && !inDark(thing) {
				delete(held, key)
			}
		}
	}
	return opens, resolves
}

// write records what opened and closed, each that resolved on Activity.
func (e *Evaluator) write(ctx context.Context, opens []store.Alert, resolves []resolution, now time.Time) error {
	if len(opens) == 0 && len(resolves) == 0 {
		return nil
	}
	return e.DB.Update(ctx, func(tx *store.Tx) error {
		for i := range opens {
			a := &opens[i]
			if err := tx.OpenAlert(ctx, *a); err != nil {
				if errors.Is(err, store.ErrConflict) {
					continue // another look opened it already
				}
				return err
			}
			if err := record(ctx, tx, ActionFiring, *a, now); err != nil {
				return err
			}
		}
		for i := range resolves {
			res := &resolves[i]
			if err := tx.ResolveAlert(ctx, res.alert.ID, now, res.ended, res.quiet); err != nil {
				return err
			}
			if res.quiet || res.ended != "" {
				// A notice that was told, or an alert its rule took with it, didn't
				// resolve: what's on Activity already says what happened.
				continue
			}
			a := res.alert
			a.ResolvedAt = &now
			if err := record(ctx, tx, ActionResolved, a, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// record puts an alert that fired or resolved on the audit log, and so on Activity.
func record(ctx context.Context, tx *store.Tx, action string, a store.Alert, now time.Time) error {
	opened := a.OpenedAt
	told := Told{
		ID: a.ID, Rule: a.RuleName, Severity: a.Severity, Condition: a.Condition, Title: a.Title,
		SubjectName: a.SubjectName, Facts: ReadFacts(a.Facts), ServerID: a.ServerID, Path: a.Path,
	}
	if action == ActionResolved {
		told.OpenedAt = &opened
	} else {
		told.Detail = a.Detail
	}
	detail, err := json.Marshal(told)
	if err != nil {
		return err
	}
	return tx.RecordEvent(ctx, store.Event{
		ID: id.New(id.Event), At: now, Actor: revision.SystemActor, Action: action,
		Subject: cmp.Or(a.SubjectID, a.ServerID), Target: a.ServerID, Detail: string(detail), Surface: store.SurfaceAutomatic,
	})
}

// advanceChanges keeps where the failed changes told have got to, so a restart
// tells none again.
func (e *Evaluator) advanceChanges(ctx context.Context, last time.Time) error {
	if last.IsZero() {
		return nil
	}
	return e.DB.Update(ctx, func(tx *store.Tx) error {
		return tx.PutSetting(ctx, settingChangesTold, []byte(strconv.FormatInt(last.UnixMilli(), 10)))
	})
}

// queue puts in the outbox what's due: each rule's alerts that fired, once the
// oldest that hasn't gone has waited GroupWait for others, those due to repeat,
// and a resolved message for each that resolved after its firing one was queued,
// to every channel that one went to. A mute holds what fires, never what
// resolves (ADR 0044).
func (e *Evaluator) queue(ctx context.Context, now time.Time) error {
	st, err := e.read(ctx)
	if err != nil {
		return err
	}
	byID := map[string]rule{}
	for _, r := range st.rules {
		byID[r.id] = r
	}
	byChannel := map[string]channel{}
	for _, c := range st.channels {
		byChannel[c.id] = c
	}
	var sends []store.AlertSend
	var fired []string
	unsent := map[string][]store.Alert{}
	for i := range st.open {
		a := st.open[i]
		r, ok := byID[a.RuleID]
		if !ok || r.spec.GetDisabled() || len(channelsOf(r, st.channels)) == 0 || muted(st.mutes, r.name, a.ServerID, now) {
			continue
		}
		if a.SentAt == nil {
			unsent[r.id] = append(unsent[r.id], a)
			continue
		}
		if repeat := alert.Repeat(r.spec); repeat > 0 && now.Sub(*a.SentAt) >= repeat {
			sends = append(sends, sendsTo(a, channelsOf(r, st.channels), "firing", now)...)
			fired = append(fired, a.ID)
		}
	}
	for ruleID, list := range unsent {
		r := byID[ruleID]
		oldest := list[0].OpenedAt
		for i := range list {
			if list[i].OpenedAt.Before(oldest) {
				oldest = list[i].OpenedAt
			}
		}
		if r.spec.GetCondition() != resourcev1.AlertRule_CONDITION_CHANGE_FAILED && now.Sub(oldest) < GroupWait {
			continue // the first waits for others, so an outage goes as one message
		}
		for i := range list {
			sends = append(sends, sendsTo(list[i], channelsOf(r, st.channels), "firing", now)...)
			fired = append(fired, list[i].ID)
		}
	}
	owed := make([]string, len(st.owed))
	for i := range st.owed {
		owed[i] = st.owed[i].ID
	}
	var cancel []string
	err = e.DB.Update(ctx, func(tx *store.Tx) error {
		queued, err := tx.SendsOf(ctx, owed)
		if err != nil {
			return err
		}
		for i := range st.owed {
			a := st.owed[i]
			to := map[string]bool{}
			for j := range queued[a.ID] {
				s := &queued[a.ID][j]
				if s.Status != "firing" {
					continue
				}
				if s.DoneAt == nil {
					// It resolved before its firing message landed: the resolved one,
					// which says how long it held, stands for both.
					cancel = append(cancel, s.ID)
				}
				if _, ok := byChannel[s.ChannelID]; ok && (s.DoneAt == nil || s.OK) {
					to[s.ChannelID] = true
				}
			}
			for channelID := range to {
				sends = append(sends, store.AlertSend{ID: id.New(id.Alert), AlertID: a.ID, RuleID: a.RuleID, ChannelID: channelID, Status: "resolved", QueuedAt: now, DueAt: now})
			}
			if err := tx.MarkResolvedSent(ctx, a.ID); err != nil {
				return err
			}
		}
		for _, sid := range cancel {
			if err := tx.FinishSend(ctx, sid, now, false, "it resolved before this went"); err != nil {
				return err
			}
		}
		for i := range sends {
			if err := tx.QueueSend(ctx, sends[i]); err != nil && !errors.Is(err, store.ErrConflict) {
				return err
			}
		}
		for _, aid := range fired {
			if err := tx.MarkAlertSent(ctx, aid, now); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// sendsTo is a message of an alert to each of its channels, due now.
func sendsTo(a store.Alert, to []channel, status string, now time.Time) []store.AlertSend {
	out := make([]store.AlertSend, len(to))
	for i, c := range to {
		out[i] = store.AlertSend{ID: id.New(id.Alert), AlertID: a.ID, RuleID: a.RuleID, ChannelID: c.id, Status: status, QueuedAt: now, DueAt: now}
	}
	return out
}

// outgoing is one message to one channel: a rule's sends of one status that are
// due together.
type outgoing struct {
	c     channel
	m     Message
	sends []store.AlertSend
}

// deliver sends what the outbox has due, a rule's sends to a channel as one
// message, each channel on its own so nothing waits on one that's down, and one
// message at a time to each, none to one that asked to slow down until it said.
func (e *Evaluator) deliver(ctx context.Context, now time.Time) error {
	e.mu.Lock()
	base := e.sendCtx
	e.mu.Unlock()
	if base == nil {
		base = context.WithoutCancel(ctx)
	}
	var due []store.AlertSend
	var alerts []store.Alert
	st, err := e.read(ctx)
	if err != nil {
		return err
	}
	err = e.DB.View(ctx, func(tx *store.Tx) error {
		list, err := tx.DueSends(ctx, now, 1000)
		if err != nil {
			return err
		}
		e.mu.Lock()
		for i := range list {
			if !e.sending[list[i].ID] {
				due = append(due, list[i])
			}
		}
		e.mu.Unlock()
		ids := make([]string, 0, len(due))
		for i := range due {
			ids = append(ids, due[i].AlertID)
		}
		alerts, err = tx.AlertsByID(ctx, ids)
		return err
	})
	if err != nil || len(due) == 0 {
		return err
	}
	byAlert := map[string]store.Alert{}
	for i := range alerts {
		byAlert[alerts[i].ID] = alerts[i]
	}
	rules := map[string]rule{}
	for _, r := range st.rules {
		rules[r.id] = r
	}
	channels := map[string]channel{}
	for _, c := range st.channels {
		channels[c.id] = c
	}
	groups := map[string]*outgoing{}
	var order []string
	var dropped []store.AlertSend
	for i := range due {
		s := &due[i]
		c, ok := channels[s.ChannelID]
		a, known := byAlert[s.AlertID]
		if !ok || !known {
			dropped = append(dropped, *s)
			continue
		}
		k := s.ChannelID + "|" + s.RuleID + "|" + s.Status
		g := groups[k]
		if g == nil {
			g = &outgoing{c: c, m: Message{Status: s.Status, Rule: rules[s.RuleID].spec, RuleName: a.RuleName, Panel: e.Panel, At: now}}
			groups[k] = g
			order = append(order, k)
		}
		g.m.Alerts = append(g.m.Alerts, a)
		g.sends = append(g.sends, *s)
	}
	if len(dropped) > 0 {
		// A channel that was removed, or an alert that was forgotten, has no one left
		// to tell.
		if err := e.DB.Update(ctx, func(tx *store.Tx) error {
			for i := range dropped {
				if err := tx.FinishSend(ctx, dropped[i].ID, now, false, "the channel was removed"); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	slices.Sort(order)
	byChannel := map[string][]*outgoing{}
	var channelOrder []string
	for _, k := range order {
		g := groups[k]
		if _, ok := byChannel[g.c.id]; !ok {
			channelOrder = append(channelOrder, g.c.id)
		}
		byChannel[g.c.id] = append(byChannel[g.c.id], g)
	}
	for _, cid := range channelOrder {
		gs := byChannel[cid]
		e.mu.Lock()
		if e.busy[cid] || now.Before(e.held[cid]) {
			e.mu.Unlock()
			continue // what's due stays due, for when it's free
		}
		e.busy[cid] = true
		for _, g := range gs {
			for i := range g.sends {
				e.sending[g.sends[i].ID] = true
			}
		}
		e.mu.Unlock()
		e.sends.Go(func() {
			defer func() {
				e.mu.Lock()
				delete(e.busy, cid)
				e.mu.Unlock()
			}()
			for i, g := range gs {
				if e.send(base, g) {
					// It asked to slow down: the rest wait with it.
					for _, rest := range gs[i+1:] {
						e.release(rest)
					}
					return
				}
			}
		})
	}
	return nil
}

// release lets the next look have a message that wasn't sent.
func (e *Evaluator) release(g *outgoing) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range g.sends {
		delete(e.sending, g.sends[i].ID)
	}
}

// send sends one message, and records how it went: done when the far end took
// it, and otherwise tried again later, until GiveUpAfter. A stop that cuts it
// short leaves it in the outbox, for the next start. A far end that asks Dockwell
// to slow down holds its channel for as long as it asks, or the schedule's next
// wait if longer, which isn't a failure: it reports that.
func (e *Evaluator) send(ctx context.Context, g *outgoing) (limited bool) {
	defer e.release(g)
	r := e.Sender.Once(ctx, g.c.spec, g.m)
	if !r.OK && ctx.Err() != nil {
		return false // stopping: it goes at the next start
	}
	at := e.now()
	ctx = context.WithoutCancel(ctx)
	first := g.sends[0]
	for i := range g.sends {
		if g.sends[i].QueuedAt.Before(first.QueuedAt) {
			first = g.sends[i]
		}
	}
	gaveUp := !r.OK && at.Sub(first.QueuedAt) >= GiveUpAfter
	if r.Limited && !gaveUp {
		until := at.Add(max(r.RetryAfter, retryAfter(first.Tries+1)))
		e.mu.Lock()
		e.held[g.c.id] = until
		e.mu.Unlock()
		err := e.DB.Update(ctx, func(tx *store.Tx) error {
			for i := range g.sends {
				if err := tx.PostponeSend(ctx, g.sends[i].ID, until, r.Said); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			e.Logger.WarnContext(ctx, "can't record that an alert channel asked to slow down; it may go again sooner", slog.String("channel", g.c.name), slog.String("error", err.Error()))
		}
		e.Logger.InfoContext(ctx, "an alert channel asked Dockwell to slow down; it waits, then goes again", slog.String("channel", g.c.name), slog.Time("until", until))
		return true
	}
	said := r.Said
	if gaveUp {
		said = clip("Dockwell gave up after " + humanDuration(at.Sub(first.QueuedAt)) + ": " + r.Said)
	}
	err := e.DB.Update(ctx, func(tx *store.Tx) error {
		for i := range g.sends {
			s := &g.sends[i]
			var err error
			if r.OK || gaveUp {
				err = tx.FinishSend(ctx, s.ID, at, r.OK, said)
			} else {
				err = tx.RetrySend(ctx, s.ID, at.Add(retryAfter(s.Tries+1)), r.Said)
			}
			if err != nil {
				return err
			}
		}
		// A message that went, its first failure, and giving up are what a channel's
		// deliveries show; not every try between.
		if r.OK || gaveUp || first.Tries == 0 {
			return tx.AddAlertDelivery(ctx, store.AlertDelivery{
				ID: id.New(id.Alert), ChannelID: g.c.id, ChannelName: g.c.name, At: at, OK: r.OK, Said: said, About: g.m.About(),
			})
		}
		return nil
	})
	if err != nil {
		e.Logger.WarnContext(ctx, "can't record how a message to an alert channel went; it may go again", slog.String("channel", g.c.name), slog.String("error", err.Error()))
	}
	if !r.OK {
		e.Logger.WarnContext(ctx, "an alert channel didn't take a message; Dockwell tries again", slog.String("channel", g.c.name), slog.String("said", r.Said), slog.Bool("gave_up", gaveUp))
	}
	return false
}

// prune forgets, once an hour, alerts that closed and messages to channels from
// before Retention.
func (e *Evaluator) prune(ctx context.Context, now time.Time) error {
	if now.Sub(e.pruned) < pruneEvery {
		return nil
	}
	e.pruned = now
	return e.DB.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.PruneAlerts(ctx, now.Add(-Retention))
		return err
	})
}
