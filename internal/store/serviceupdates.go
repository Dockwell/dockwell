// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ServiceUpdate is what Dockwell last found newer than what a service runs, and
// what a person set aside of it (ADR 0079). Found and Skipped are JSON the
// control plane reads; the store keeps them as they come.
type ServiceUpdate struct {
	ServiceID string
	// ServerID is the server it was last checked through, and Image the reference
	// it was checked for.
	ServerID, Image string
	// CheckedAt is the last check that answered, TriedAt the last check, and
	// NextAt when it's checked next. Each is zero until it's set.
	CheckedAt, TriedAt, NextAt time.Time
	// State and Problem are dockwellv1.UpdateCheck and dockwellv1.UpdateProblem.
	State, Problem int32
	Detail         string
	Found          string
	// Skipped and the snooze are a person's, which a check never writes.
	Skipped     string
	SnoozeUntil time.Time
	SnoozedBy   string
}

// PutServiceUpdateCheck writes what a check of a service found, leaving what a
// person skipped or snoozed as it was.
func (t *Tx) PutServiceUpdateCheck(ctx context.Context, u ServiceUpdate) error {
	_, err := t.exec(ctx, `INSERT INTO service_updates (service_id, server_id, image, checked_at, tried_at, next_at, state, problem, detail, found)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (service_id) DO UPDATE SET server_id = excluded.server_id, image = excluded.image, checked_at = excluded.checked_at,
		tried_at = excluded.tried_at, next_at = excluded.next_at, state = excluded.state, problem = excluded.problem,
		detail = excluded.detail, found = excluded.found`,
		u.ServiceID, u.ServerID, u.Image, zeroNull(u.CheckedAt), zeroNull(u.TriedAt), zeroNull(u.NextAt), u.State, u.Problem, u.Detail, u.Found)
	if err != nil {
		return fmt.Errorf("can't keep what the check of %s found: %w", u.ServiceID, err)
	}
	return nil
}

// SetServiceUpdateSkipped writes the versions a person skipped of a service's
// updates, leaving what a check found as it was.
func (t *Tx) SetServiceUpdateSkipped(ctx context.Context, serviceID, skipped string) error {
	_, err := t.exec(ctx, `INSERT INTO service_updates (service_id, skipped) VALUES (?, ?)
		ON CONFLICT (service_id) DO UPDATE SET skipped = excluded.skipped`, serviceID, skipped)
	if err != nil {
		return fmt.Errorf("can't keep what was skipped of %s's updates: %w", serviceID, err)
	}
	return nil
}

// SetServiceUpdateSnooze sets a service's updates aside until a time, by whom,
// or brings them back with a zero time.
func (t *Tx) SetServiceUpdateSnooze(ctx context.Context, serviceID string, until time.Time, by string) error {
	_, err := t.exec(ctx, `INSERT INTO service_updates (service_id, snooze_until, snoozed_by) VALUES (?, ?, ?)
		ON CONFLICT (service_id) DO UPDATE SET snooze_until = excluded.snooze_until, snoozed_by = excluded.snoozed_by`,
		serviceID, zeroNull(until), by)
	if err != nil {
		return fmt.Errorf("can't keep the snooze of %s's updates: %w", serviceID, err)
	}
	return nil
}

const serviceUpdateColumns = `service_id, server_id, image, checked_at, tried_at, next_at, state, problem, detail, found, skipped, snooze_until, snoozed_by`

// ServiceUpdates returns what's kept of each service's updates, by service ID:
// every service's when ids is empty.
func (t *Tx) ServiceUpdates(ctx context.Context, ids []string) (map[string]ServiceUpdate, error) {
	q := `SELECT ` + serviceUpdateColumns + ` FROM service_updates`
	args := make([]any, 0, len(ids))
	if len(ids) > 0 {
		q += ` WHERE service_id IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	rows, err := t.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("can't read the services' updates: %w", err)
	}
	defer rows.Close()
	out := map[string]ServiceUpdate{}
	for rows.Next() {
		var u ServiceUpdate
		var checked, tried, next, snooze sql.NullInt64
		if err := rows.Scan(&u.ServiceID, &u.ServerID, &u.Image, &checked, &tried, &next, &u.State, &u.Problem, &u.Detail, &u.Found, &u.Skipped, &snooze, &u.SnoozedBy); err != nil {
			return nil, fmt.Errorf("can't read the services' updates: %w", err)
		}
		u.CheckedAt, u.TriedAt, u.NextAt, u.SnoozeUntil = fromNullMillis(checked), fromNullMillis(tried), fromNullMillis(next), fromNullMillis(snooze)
		out[u.ServiceID] = u
	}
	return out, rows.Err()
}

// DeleteServiceUpdates forgets what's kept of services that are gone, and
// reports how many it forgot.
func (t *Tx) DeleteServiceUpdates(ctx context.Context, ids []string) (int, error) {
	n := 0
	for _, id := range ids {
		res, err := t.exec(ctx, `DELETE FROM service_updates WHERE service_id = ?`, id)
		if err != nil {
			return n, fmt.Errorf("can't forget %s's updates: %w", id, err)
		}
		k, _ := res.RowsAffected()
		n += int(k)
	}
	return n, nil
}

// zeroNull is a time to keep, with the zero time as none.
func zeroNull(at time.Time) sql.NullInt64 {
	if at.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: millis(at), Valid: true}
}

func fromNullMillis(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromMillis(v.Int64)
}
