// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Alert is one rule holding for one thing, from when it fired until it resolved.
// It is the control plane's record, written outside the revision store (ADR 0044).
type Alert struct {
	ID, RuleID, RuleName string
	// Thing is what it's about, such as a server's ID and a disk's mount point: one
	// alert of a rule is open for a thing at a time.
	Thing     string
	Condition int32
	Severity  int32
	ServerID  string
	// SubjectID and SubjectName are what else it's about: a service, a domain, a
	// volume or a change.
	SubjectID, SubjectName string
	Title, Detail, Path    string
	// Facts are what its title says, as JSON, for the panel to say in its own
	// language.
	Facts      string
	OpenedAt   time.Time
	ResolvedAt *time.Time
	// Ended says why it closed when it didn't resolve: "removed" or "disabled",
	// as its rule was.
	Ended string
	// SentAt is when its firing message was last queued for its channels.
	// ResolvedSent says its resolved message was queued, or never will be.
	SentAt       *time.Time
	ResolvedSent bool
	// AcknowledgedAt and AcknowledgedBy are when someone said they're on it, and
	// who, as an actor's ID. Unset while nobody has; a resolved alert keeps them.
	AcknowledgedAt *time.Time
	AcknowledgedBy string
}

const alertColumns = `id, rule_id, rule_name, thing, condition, severity, server_id, subject_id, subject_name, title, detail, path, facts, opened_at, resolved_at, ended, sent_at, resolved_sent, acknowledged_at, acknowledged_by`

func scanAlert(row interface{ Scan(...any) error }) (Alert, error) {
	var a Alert
	var opened int64
	var resolved, sent, acknowledged sql.NullInt64
	err := row.Scan(&a.ID, &a.RuleID, &a.RuleName, &a.Thing, &a.Condition, &a.Severity, &a.ServerID, &a.SubjectID, &a.SubjectName,
		&a.Title, &a.Detail, &a.Path, &a.Facts, &opened, &resolved, &a.Ended, &sent, &a.ResolvedSent, &acknowledged, &a.AcknowledgedBy)
	if err != nil {
		return Alert{}, err
	}
	a.OpenedAt, a.ResolvedAt, a.SentAt, a.AcknowledgedAt = fromMillis(opened), timePtr(resolved), timePtr(sent), timePtr(acknowledged)
	return a, nil
}

func (t *Tx) alerts(ctx context.Context, query string, args ...any) ([]Alert, error) {
	rows, err := t.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("can't read alerts: %w", err)
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read alerts: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// OpenAlert records an alert that fired. ErrConflict says one of its rule is open
// for its thing already.
func (t *Tx) OpenAlert(ctx context.Context, a Alert) error {
	err := t.insert(ctx, `INSERT INTO alerts (`+alertColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.RuleID, a.RuleName, a.Thing, a.Condition, a.Severity, a.ServerID, a.SubjectID, a.SubjectName,
		a.Title, a.Detail, a.Path, a.Facts, millis(a.OpenedAt), nullMillis(a.ResolvedAt), a.Ended, nullMillis(a.SentAt), a.ResolvedSent,
		nullMillis(a.AcknowledgedAt), a.AcknowledgedBy)
	if t.unique(err) {
		return fmt.Errorf("an alert of %s is open for %s already: %w", a.RuleName, a.Thing, ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("can't record alert %s: %w", a.ID, err)
	}
	return nil
}

// ResolveAlert records that an alert resolved, or ended for why, as its rule was
// removed. quiet says no resolved message is owed. One whose firing message was
// never queued owes none either.
func (t *Tx) ResolveAlert(ctx context.Context, id string, at time.Time, ended string, quiet bool) error {
	if _, err := t.exec(ctx, `UPDATE alerts SET resolved_at = ?, ended = ?, resolved_sent = (resolved_sent OR ? OR sent_at IS NULL) WHERE id = ? AND resolved_at IS NULL`,
		millis(at), ended, quiet, id); err != nil {
		return fmt.Errorf("can't resolve alert %s: %w", id, err)
	}
	return nil
}

// MarkAlertSent records that an alert's firing message was queued for its channels.
func (t *Tx) MarkAlertSent(ctx context.Context, id string, at time.Time) error {
	if _, err := t.exec(ctx, `UPDATE alerts SET sent_at = ? WHERE id = ?`, millis(at), id); err != nil {
		return fmt.Errorf("can't record that alert %s was sent: %w", id, err)
	}
	return nil
}

// MarkResolvedSent records that an alert's resolved message was queued, or never will be.
func (t *Tx) MarkResolvedSent(ctx context.Context, id string) error {
	if _, err := t.exec(ctx, `UPDATE alerts SET resolved_sent = ? WHERE id = ?`, true, id); err != nil {
		return fmt.Errorf("can't record that alert %s was sent as resolved: %w", id, err)
	}
	return nil
}

// AcknowledgeAlert records who said they're on an open alert, and when, over
// whoever had it. It reports whether the alert was there and open: one that
// resolved meanwhile isn't touched.
func (t *Tx) AcknowledgeAlert(ctx context.Context, id string, at time.Time, actor string) (bool, error) {
	res, err := t.exec(ctx, `UPDATE alerts SET acknowledged_at = ?, acknowledged_by = ? WHERE id = ? AND resolved_at IS NULL`, millis(at), actor, id)
	if err != nil {
		return false, fmt.Errorf("can't acknowledge alert %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("can't acknowledge alert %s: %w", id, err)
	}
	return n > 0, nil
}

// UnacknowledgeAlert takes back an open alert's acknowledgement. It reports
// whether there was one to take: an alert nobody has, or one that resolved
// meanwhile, isn't touched.
func (t *Tx) UnacknowledgeAlert(ctx context.Context, id string) (bool, error) {
	res, err := t.exec(ctx, `UPDATE alerts SET acknowledged_at = NULL, acknowledged_by = '' WHERE id = ? AND resolved_at IS NULL AND acknowledged_at IS NOT NULL`, id)
	if err != nil {
		return false, fmt.Errorf("can't take back the acknowledgement of alert %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("can't take back the acknowledgement of alert %s: %w", id, err)
	}
	return n > 0, nil
}

// OpenAlerts returns every alert that's firing, oldest first.
func (t *Tx) OpenAlerts(ctx context.Context) ([]Alert, error) {
	return t.alerts(ctx, `SELECT `+alertColumns+` FROM alerts WHERE resolved_at IS NULL ORDER BY opened_at, id`)
}

// OwedResolved returns the alerts that resolved and still owe their resolved
// message, oldest first.
func (t *Tx) OwedResolved(ctx context.Context) ([]Alert, error) {
	return t.alerts(ctx, `SELECT `+alertColumns+` FROM alerts WHERE resolved_at IS NOT NULL AND resolved_sent = ? ORDER BY resolved_at, id`, false)
}

// GetAlert returns an alert, or ErrNotFound.
func (t *Tx) GetAlert(ctx context.Context, id string) (Alert, error) {
	a, err := scanAlert(t.queryRow(ctx, `SELECT `+alertColumns+` FROM alerts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Alert{}, fmt.Errorf("alert %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Alert{}, fmt.Errorf("can't read alert %s: %w", id, err)
	}
	return a, nil
}

// AlertTold reports whether a rule ever had an alert for a thing, open or not, as a
// failed change is told once.
func (t *Tx) AlertTold(ctx context.Context, ruleID, thing string) (bool, error) {
	var n int
	if err := t.queryRow(ctx, `SELECT COUNT(*) FROM alerts WHERE rule_id = ? AND thing = ?`, ruleID, thing).Scan(&n); err != nil {
		return false, fmt.Errorf("can't read alerts of %s: %w", ruleID, err)
	}
	return n > 0, nil
}

// ResolvedAlerts returns a page of alerts that resolved, newest first by when they
// fired, after the one at afterAt and afterID when those are set.
func (t *Tx) ResolvedAlerts(ctx context.Context, afterAt *time.Time, afterID string, limit int) ([]Alert, error) {
	if afterAt == nil {
		return t.alerts(ctx, `SELECT `+alertColumns+` FROM alerts WHERE resolved_at IS NOT NULL ORDER BY opened_at DESC, id DESC LIMIT ?`, limit)
	}
	at := millis(*afterAt)
	return t.alerts(ctx, `SELECT `+alertColumns+` FROM alerts WHERE resolved_at IS NOT NULL AND (opened_at < ? OR (opened_at = ? AND id < ?))
		ORDER BY opened_at DESC, id DESC LIMIT ?`, at, at, afterID, limit)
}

// RuleFired is how a rule has fired: how many of its alerts are open, and when it
// last fired.
type RuleFired struct {
	Open int
	Last time.Time
}

// RulesFired returns how each rule that ever fired has, by rule ID.
func (t *Tx) RulesFired(ctx context.Context) (map[string]RuleFired, error) {
	rows, err := t.query(ctx, `SELECT rule_id, SUM(CASE WHEN resolved_at IS NULL THEN 1 ELSE 0 END), MAX(opened_at) FROM alerts GROUP BY rule_id`)
	if err != nil {
		return nil, fmt.Errorf("can't read how rules fired: %w", err)
	}
	defer rows.Close()
	out := map[string]RuleFired{}
	for rows.Next() {
		var id string
		var open, last int64
		if err := rows.Scan(&id, &open, &last); err != nil {
			return nil, fmt.Errorf("can't read how rules fired: %w", err)
		}
		out[id] = RuleFired{Open: int(open), Last: fromMillis(last)}
	}
	return out, rows.Err()
}

// AlertDelivery is one message to a channel, and what the far end answered.
type AlertDelivery struct {
	ID, ChannelID, ChannelName string
	At                         time.Time
	OK                         bool
	Said, About                string
}

// AddAlertDelivery records a message to a channel.
func (t *Tx) AddAlertDelivery(ctx context.Context, d AlertDelivery) error {
	if _, err := t.exec(ctx, `INSERT INTO alert_deliveries (id, channel_id, channel_name, at, ok, said, about) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.ChannelID, d.ChannelName, millis(d.At), d.OK, d.Said, d.About); err != nil {
		return fmt.Errorf("can't record a message to %s: %w", d.ChannelName, err)
	}
	return nil
}

// LastDeliveries returns each channel's newest message, by channel ID.
func (t *Tx) LastDeliveries(ctx context.Context) (map[string]AlertDelivery, error) {
	rows, err := t.query(ctx, `SELECT d.id, d.channel_id, d.channel_name, d.at, d.ok, d.said, d.about FROM alert_deliveries d
		WHERE d.at = (SELECT MAX(at) FROM alert_deliveries WHERE channel_id = d.channel_id) ORDER BY d.channel_id, d.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("can't read the channels' messages: %w", err)
	}
	defer rows.Close()
	out := map[string]AlertDelivery{}
	for rows.Next() {
		var d AlertDelivery
		var at int64
		if err := rows.Scan(&d.ID, &d.ChannelID, &d.ChannelName, &at, &d.OK, &d.Said, &d.About); err != nil {
			return nil, fmt.Errorf("can't read the channels' messages: %w", err)
		}
		d.At = fromMillis(at)
		if _, seen := out[d.ChannelID]; !seen {
			out[d.ChannelID] = d
		}
	}
	return out, rows.Err()
}

// RecentDeliveries returns the newest messages to every channel, newest first.
func (t *Tx) RecentDeliveries(ctx context.Context, limit int) ([]AlertDelivery, error) {
	rows, err := t.query(ctx, `SELECT id, channel_id, channel_name, at, ok, said, about FROM alert_deliveries ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("can't read the channels' messages: %w", err)
	}
	defer rows.Close()
	var out []AlertDelivery
	for rows.Next() {
		var d AlertDelivery
		var at int64
		if err := rows.Scan(&d.ID, &d.ChannelID, &d.ChannelName, &at, &d.OK, &d.Said, &d.About); err != nil {
			return nil, fmt.Errorf("can't read the channels' messages: %w", err)
		}
		d.At = fromMillis(at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// AlertMute is what a mute holds, until a time: a rule, as "rule:<name>", or a
// server, as "server:<id>".
type AlertMute struct {
	Target    string
	Until     time.Time
	Actor     string
	Reason    string
	CreatedAt time.Time
}

// PutAlertMute mutes a target, or moves the end of its mute.
func (t *Tx) PutAlertMute(ctx context.Context, m AlertMute) error {
	_, err := t.exec(ctx, `INSERT INTO alert_mutes (target, until_at, actor, reason, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (target) DO UPDATE SET until_at = excluded.until_at, actor = excluded.actor, reason = excluded.reason, created_at = excluded.created_at`,
		m.Target, millis(m.Until), m.Actor, m.Reason, millis(m.CreatedAt))
	if err != nil {
		return fmt.Errorf("can't mute %s: %w", m.Target, err)
	}
	return nil
}

// DeleteAlertMute ends a target's mute, and reports whether it had one.
func (t *Tx) DeleteAlertMute(ctx context.Context, target string) (bool, error) {
	res, err := t.exec(ctx, `DELETE FROM alert_mutes WHERE target = ?`, target)
	if err != nil {
		return false, fmt.Errorf("can't end the mute of %s: %w", target, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AlertMutes returns every mute, ended ones too until something deletes them,
// soonest to end first.
func (t *Tx) AlertMutes(ctx context.Context) ([]AlertMute, error) {
	rows, err := t.query(ctx, `SELECT target, until_at, actor, reason, created_at FROM alert_mutes ORDER BY until_at, target`)
	if err != nil {
		return nil, fmt.Errorf("can't read the alert mutes: %w", err)
	}
	defer rows.Close()
	var out []AlertMute
	for rows.Next() {
		var m AlertMute
		var until, created int64
		if err := rows.Scan(&m.Target, &until, &m.Actor, &m.Reason, &created); err != nil {
			return nil, fmt.Errorf("can't read the alert mutes: %w", err)
		}
		m.Until, m.CreatedAt = fromMillis(until), fromMillis(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// EventsSince returns the audit log's events of an action after a time, oldest
// first, at most limit of them.
func (t *Tx) EventsSince(ctx context.Context, action string, after time.Time, limit int) ([]Event, error) {
	rows, err := t.query(ctx, `SELECT id, at, actor, action, subject, detail, surface, target FROM audit_events
		WHERE at > ? AND action = ? ORDER BY at, id LIMIT ?`, millis(after), action, limit)
	if err != nil {
		return nil, fmt.Errorf("can't read the audit log: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Subject, &e.Detail, &e.Surface, &e.Target); err != nil {
			return nil, fmt.Errorf("can't read the audit log: %w", err)
		}
		e.At = fromMillis(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestBackups returns each volume's newest backup that started after a time, by
// volume ID.
func (t *Tx) LatestBackups(ctx context.Context, after time.Time) (map[string]Backup, error) {
	rows, err := t.query(ctx, `SELECT `+backupColumns+` FROM backups b WHERE b.started_at > ?
		AND b.started_at = (SELECT MAX(started_at) FROM backups WHERE volume_id = b.volume_id) ORDER BY b.volume_id, b.id DESC`, millis(after))
	if err != nil {
		return nil, fmt.Errorf("can't read the newest backups: %w", err)
	}
	defer rows.Close()
	out := map[string]Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read the newest backups: %w", err)
		}
		if _, seen := out[b.VolumeID]; !seen {
			out[b.VolumeID] = b
		}
	}
	return out, rows.Err()
}

// AlertSend is a message an alert owes a channel, in the outbox until the far end
// takes it or Dockwell gives up.
type AlertSend struct {
	ID, AlertID, RuleID, ChannelID string
	// Status is "firing" or "resolved".
	Status   string
	QueuedAt time.Time
	DueAt    time.Time
	Tries    int
	DoneAt   *time.Time
	OK       bool
	Said     string
}

const sendColumns = `id, alert_id, rule_id, channel_id, status, queued_at, due_at, tries, done_at, ok, said`

func (t *Tx) sends(ctx context.Context, query string, args ...any) ([]AlertSend, error) {
	rows, err := t.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("can't read the alerts' outbox: %w", err)
	}
	defer rows.Close()
	var out []AlertSend
	for rows.Next() {
		var s AlertSend
		var queued, due int64
		var done sql.NullInt64
		if err := rows.Scan(&s.ID, &s.AlertID, &s.RuleID, &s.ChannelID, &s.Status, &queued, &due, &s.Tries, &done, &s.OK, &s.Said); err != nil {
			return nil, fmt.Errorf("can't read the alerts' outbox: %w", err)
		}
		s.QueuedAt, s.DueAt, s.DoneAt = fromMillis(queued), fromMillis(due), timePtr(done)
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueueSend puts a message in the outbox. ErrConflict says the alert owes the
// channel one of its status already.
func (t *Tx) QueueSend(ctx context.Context, s AlertSend) error {
	err := t.insert(ctx, `INSERT INTO alert_sends (`+sendColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.AlertID, s.RuleID, s.ChannelID, s.Status, millis(s.QueuedAt), millis(s.DueAt), s.Tries, nullMillis(s.DoneAt), s.OK, s.Said)
	if t.unique(err) {
		return fmt.Errorf("alert %s owes %s a %s message already: %w", s.AlertID, s.ChannelID, s.Status, ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("can't queue a message of alert %s: %w", s.AlertID, err)
	}
	return nil
}

// DueSends returns what's in the outbox and due by now, soonest first.
func (t *Tx) DueSends(ctx context.Context, now time.Time, limit int) ([]AlertSend, error) {
	return t.sends(ctx, `SELECT `+sendColumns+` FROM alert_sends WHERE done_at IS NULL AND due_at <= ? ORDER BY due_at, id LIMIT ?`, millis(now), limit)
}

// SendsOf returns every message alerts owe or owed, by alert ID.
func (t *Tx) SendsOf(ctx context.Context, alertIDs []string) (map[string][]AlertSend, error) {
	out := map[string][]AlertSend{}
	for len(alertIDs) > 0 {
		chunk := alertIDs[:min(len(alertIDs), 500)]
		alertIDs = alertIDs[len(chunk):]
		list, err := t.sends(ctx, `SELECT `+sendColumns+` FROM alert_sends WHERE alert_id IN (`+marks(len(chunk))+`) ORDER BY queued_at, id`, anys(chunk)...)
		if err != nil {
			return nil, err
		}
		for i := range list {
			out[list[i].AlertID] = append(out[list[i].AlertID], list[i])
		}
	}
	return out, nil
}

// FinishSend records that a message was taken, or that Dockwell gave up on it.
func (t *Tx) FinishSend(ctx context.Context, id string, at time.Time, ok bool, said string) error {
	if _, err := t.exec(ctx, `UPDATE alert_sends SET done_at = ?, ok = ?, said = ?, tries = tries + 1 WHERE id = ? AND done_at IS NULL`, millis(at), ok, said, id); err != nil {
		return fmt.Errorf("can't record how a message went: %w", err)
	}
	return nil
}

// RetrySend records a try that failed, and when the next goes.
func (t *Tx) RetrySend(ctx context.Context, id string, due time.Time, said string) error {
	if _, err := t.exec(ctx, `UPDATE alert_sends SET due_at = ?, said = ?, tries = tries + 1 WHERE id = ? AND done_at IS NULL`, millis(due), said, id); err != nil {
		return fmt.Errorf("can't record how a message went: %w", err)
	}
	return nil
}

// PostponeSend puts a message off until due, as a far end that's limiting asks,
// which doesn't count as a try.
func (t *Tx) PostponeSend(ctx context.Context, id string, due time.Time, said string) error {
	if _, err := t.exec(ctx, `UPDATE alert_sends SET due_at = ?, said = ? WHERE id = ? AND done_at IS NULL`, millis(due), said, id); err != nil {
		return fmt.Errorf("can't record how a message went: %w", err)
	}
	return nil
}

// AlertsByID returns the alerts with these IDs, in no order.
func (t *Tx) AlertsByID(ctx context.Context, ids []string) ([]Alert, error) {
	var out []Alert
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		list, err := t.alerts(ctx, `SELECT `+alertColumns+` FROM alerts WHERE id IN (`+marks(len(chunk))+`)`, anys(chunk)...)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

// PruneAlerts forgets alerts that closed before a time, with what they sent, and
// messages to channels from before it. It says how many alerts it forgot.
func (t *Tx) PruneAlerts(ctx context.Context, before time.Time) (int64, error) {
	at := millis(before)
	if _, err := t.exec(ctx, `DELETE FROM alert_sends WHERE done_at IS NOT NULL AND done_at < ?`, at); err != nil {
		return 0, fmt.Errorf("can't forget old messages of alerts: %w", err)
	}
	res, err := t.exec(ctx, `DELETE FROM alerts WHERE resolved_at IS NOT NULL AND resolved_at < ?
		AND NOT EXISTS (SELECT 1 FROM alert_sends s WHERE s.alert_id = alerts.id AND s.done_at IS NULL)`, at)
	if err != nil {
		return 0, fmt.Errorf("can't forget old alerts: %w", err)
	}
	if _, err := t.exec(ctx, `DELETE FROM alert_deliveries WHERE at < ?`, at); err != nil {
		return 0, fmt.Errorf("can't forget old messages to channels: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
