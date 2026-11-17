// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// What Dockwell observed of a catalog app's owner it makes (ADR 0105).
const (
	// OwnerWaitingEmail is an install by a token that waits for the owner's email.
	OwnerWaitingEmail = "waiting_email"
	// OwnerWaiting waits for its app to run, or is being made.
	OwnerWaiting = "waiting"
	// OwnerMade is made: final.
	OwnerMade = "made"
	// OwnerFailed failed, and waits for a person to try again or open the install.
	OwnerFailed = "failed"
	// OwnerOpened was opened by a person before it was made: final.
	OwnerOpened = "opened"
)

// OwnerFinal says a state no later reading takes back: once made or opened, an
// owner never holds its install again.
func OwnerFinal(state string) bool { return state == OwnerMade || state == OwnerOpened }

// AppOwnerState is what Dockwell observed of an app.owner resource.
type AppOwnerState struct {
	ResourceID  string
	Environment string
	State       string
	// Error is what the app said where making it failed, scrubbed.
	Error     string
	Tries     int
	UpdatedAt time.Time
	// Opening is a planned change that opens the install, which makes the owner
	// opened once it applies (ADR 0105).
	Opening string
}

// AppOwnerStates reads what's observed of each app.owner resource of ids, by ID. One
// with no row isn't there: its owner isn't made.
func (t *Tx) AppOwnerStates(ctx context.Context, ids []string) (map[string]AppOwnerState, error) {
	out := map[string]AppOwnerState{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := t.query(ctx, `SELECT resource_id, environment, state, error, tries, updated_at, opening FROM app_owners WHERE resource_id IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("can't read the app owners: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s AppOwnerState
		var at int64
		if err := rows.Scan(&s.ResourceID, &s.Environment, &s.State, &s.Error, &s.Tries, &at, &s.Opening); err != nil {
			return nil, err
		}
		s.UpdatedAt = fromMillis(at)
		out[s.ResourceID] = s
	}
	return out, rows.Err()
}

// SetAppOwnerState records what's observed of an app.owner resource. A row that's
// made or opened stays as it is, whatever s says, so nothing takes an owner back
// into holding its install; it says whether it wrote s.
func (t *Tx) SetAppOwnerState(ctx context.Context, s AppOwnerState) (bool, error) {
	res, err := t.exec(ctx, `INSERT INTO app_owners (resource_id, environment, state, error, tries, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (resource_id) DO UPDATE SET state = excluded.state, error = excluded.error, tries = excluded.tries, updated_at = excluded.updated_at
		WHERE app_owners.state NOT IN ('made', 'opened')`,
		s.ResourceID, s.Environment, s.State, s.Error, s.Tries, millis(s.UpdatedAt))
	if err != nil {
		return false, fmt.Errorf("can't record app owner %s: %w", s.ResourceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetAppOwnerOpening records a planned change that opens an owner's install, which
// makes the owner opened once it applies. It keeps what's observed as it is, and
// does nothing to an owner made or opened already.
func (t *Tx) SetAppOwnerOpening(ctx context.Context, resourceID, environment, change string, at time.Time) error {
	_, err := t.exec(ctx, `INSERT INTO app_owners (resource_id, environment, state, updated_at, opening) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (resource_id) DO UPDATE SET opening = excluded.opening
		WHERE app_owners.state NOT IN ('made', 'opened')`,
		resourceID, environment, OwnerWaiting, millis(at), change)
	if err != nil {
		return fmt.Errorf("can't record the change that opens app owner %s: %w", resourceID, err)
	}
	return nil
}

// AppOwnersOpenToChange reads what's observed of every app owner not yet made or
// opened, for the control plane to make or watch.
func (t *Tx) AppOwnersOpenToChange(ctx context.Context) ([]AppOwnerState, error) {
	rows, err := t.query(ctx, `SELECT resource_id, environment, state, error, tries, updated_at, opening FROM app_owners WHERE state NOT IN ('made', 'opened') ORDER BY resource_id`)
	if err != nil {
		return nil, fmt.Errorf("can't read the app owners: %w", err)
	}
	defer rows.Close()
	var out []AppOwnerState
	for rows.Next() {
		var s AppOwnerState
		var at int64
		if err := rows.Scan(&s.ResourceID, &s.Environment, &s.State, &s.Error, &s.Tries, &at, &s.Opening); err != nil {
			return nil, err
		}
		s.UpdatedAt = fromMillis(at)
		out = append(out, s)
	}
	return out, rows.Err()
}
