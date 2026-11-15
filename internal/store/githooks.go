// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GitHook is a service's hook, which deploys it on a push (ADR 0046). It acts for
// the person who made it, and its secret is sealed.
type GitHook struct {
	ID         string
	ResourceID string // the service's ID
	UserID     string
	Secret     []byte // sealed
	CreatedAt  time.Time
	SecretAt   time.Time
}

// What came of a push.
const (
	HookDeploying   = "deploying"
	HookNeedsPerson = "needs_person"
	HookQueued      = "queued"
	HookNothingNew  = "nothing_new"
	HookIgnored     = "ignored"
	HookFailed      = "failed"
	HookSuperseded  = "superseded"
)

// GitHookDelivery is one push a provider sent through a hook, and what came of it.
type GitHookDelivery struct {
	HookID     string
	DeliveryID string
	At         time.Time
	Provider   string
	Ref        string
	Commit     string
	Outcome    string
	ChangeID   string
	Detail     string
}

// KeepHookDeliveries is how long a delivery is kept, so one seen before does nothing.
const KeepHookDeliveries = 7 * 24 * time.Hour

const gitHookColumns = `id, resource_id, user_id, secret, created_at, secret_at`

func scanGitHook(row interface{ Scan(...any) error }) (GitHook, error) {
	var h GitHook
	var created, secretAt int64
	if err := row.Scan(&h.ID, &h.ResourceID, &h.UserID, &h.Secret, &created, &secretAt); err != nil {
		return GitHook{}, err
	}
	h.CreatedAt, h.SecretAt = fromMillis(created), fromMillis(secretAt)
	return h, nil
}

// CreateGitHook records a service's hook. A service has one at most, and a second
// is ErrConflict.
func (t *Tx) CreateGitHook(ctx context.Context, h GitHook) error {
	_, err := t.exec(ctx, `INSERT INTO git_hooks (`+gitHookColumns+`) VALUES (?, ?, ?, ?, ?, ?)`,
		h.ID, h.ResourceID, h.UserID, h.Secret, millis(h.CreatedAt), millis(h.SecretAt))
	if t.unique(err) {
		return fmt.Errorf("a hook for %s: %w", h.ResourceID, ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("can't record hook %s: %w", h.ID, err)
	}
	return nil
}

// GetGitHook returns a hook by its ID, or ErrNotFound.
func (t *Tx) GetGitHook(ctx context.Context, id string) (GitHook, error) {
	h, err := scanGitHook(t.queryRow(ctx, `SELECT `+gitHookColumns+` FROM git_hooks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return GitHook{}, fmt.Errorf("hook %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return GitHook{}, fmt.Errorf("can't read hook %s: %w", id, err)
	}
	return h, nil
}

// GitHookOf returns a service's hook, or ErrNotFound.
func (t *Tx) GitHookOf(ctx context.Context, resourceID string) (GitHook, error) {
	h, err := scanGitHook(t.queryRow(ctx, `SELECT `+gitHookColumns+` FROM git_hooks WHERE resource_id = ?`, resourceID))
	if errors.Is(err, sql.ErrNoRows) {
		return GitHook{}, fmt.Errorf("the hook of %s: %w", resourceID, ErrNotFound)
	}
	if err != nil {
		return GitHook{}, fmt.Errorf("can't read the hook of %s: %w", resourceID, err)
	}
	return h, nil
}

// SetGitHookSecret gives a hook a new sealed secret.
func (t *Tx) SetGitHookSecret(ctx context.Context, id string, secret []byte, at time.Time) error {
	res, err := t.exec(ctx, `UPDATE git_hooks SET secret = ?, secret_at = ? WHERE id = ?`, secret, millis(at), id)
	if err != nil {
		return fmt.Errorf("can't give hook %s a new secret: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("hook %s: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteGitHook removes a hook and what its deliveries came to.
func (t *Tx) DeleteGitHook(ctx context.Context, id string) error {
	if _, err := t.exec(ctx, `DELETE FROM git_hook_deliveries WHERE hook_id = ?`, id); err != nil {
		return fmt.Errorf("can't forget hook %s's deliveries: %w", id, err)
	}
	res, err := t.exec(ctx, `DELETE FROM git_hooks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("can't delete hook %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("hook %s: %w", id, ErrNotFound)
	}
	return nil
}

// RecordGitHookDelivery records a delivery as it comes. One the hook has had
// before, by the provider's ID, is ErrConflict, and does nothing.
func (t *Tx) RecordGitHookDelivery(ctx context.Context, d GitHookDelivery) error {
	// One seen before inserts nothing, which leaves the transaction usable on
	// Postgres, where a failed insert wouldn't.
	res, err := t.exec(ctx, `INSERT INTO git_hook_deliveries (hook_id, delivery_id, at, provider, ref, commit_id, outcome, change_id, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (hook_id, delivery_id) DO NOTHING`,
		d.HookID, d.DeliveryID, millis(d.At), d.Provider, d.Ref, d.Commit, d.Outcome, d.ChangeID, d.Detail)
	if err != nil {
		return fmt.Errorf("can't record delivery %s: %w", d.DeliveryID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delivery %s: %w", d.DeliveryID, ErrConflict)
	}
	return nil
}

// SetGitHookDelivery records what a delivery came to.
func (t *Tx) SetGitHookDelivery(ctx context.Context, d GitHookDelivery) error {
	_, err := t.exec(ctx, `UPDATE git_hook_deliveries SET commit_id = ?, outcome = ?, change_id = ?, detail = ? WHERE hook_id = ? AND delivery_id = ?`,
		d.Commit, d.Outcome, d.ChangeID, d.Detail, d.HookID, d.DeliveryID)
	if err != nil {
		return fmt.Errorf("can't record what delivery %s came to: %w", d.DeliveryID, err)
	}
	return nil
}

// GitHookDeliveries returns a hook's newest deliveries, newest first, up to limit.
func (t *Tx) GitHookDeliveries(ctx context.Context, hookID string, limit int) ([]GitHookDelivery, error) {
	rows, err := t.query(ctx, `SELECT hook_id, delivery_id, at, provider, ref, commit_id, outcome, change_id, detail
		FROM git_hook_deliveries WHERE hook_id = ? ORDER BY at DESC, delivery_id DESC LIMIT ?`, hookID, limit)
	if err != nil {
		return nil, fmt.Errorf("can't read hook %s's deliveries: %w", hookID, err)
	}
	defer rows.Close()
	var out []GitHookDelivery
	for rows.Next() {
		var d GitHookDelivery
		var at int64
		if err := rows.Scan(&d.HookID, &d.DeliveryID, &at, &d.Provider, &d.Ref, &d.Commit, &d.Outcome, &d.ChangeID, &d.Detail); err != nil {
			return nil, err
		}
		d.At = fromMillis(at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ForgetGitHookDeliveries forgets deliveries that came before a time.
func (t *Tx) ForgetGitHookDeliveries(ctx context.Context, before time.Time) error {
	if _, err := t.exec(ctx, `DELETE FROM git_hook_deliveries WHERE at < ?`, millis(before)); err != nil {
		return fmt.Errorf("can't forget old deliveries: %w", err)
	}
	return nil
}

// EndQueuedGitHookDeliveries says what came of the deliveries that waited when the
// control plane stopped, which nothing deploys now.
func (t *Tx) EndQueuedGitHookDeliveries(ctx context.Context, outcome, detail string) error {
	if _, err := t.exec(ctx, `UPDATE git_hook_deliveries SET outcome = ?, detail = ? WHERE outcome = ?`, outcome, detail, HookQueued); err != nil {
		return fmt.Errorf("can't end the deliveries that waited: %w", err)
	}
	return nil
}
