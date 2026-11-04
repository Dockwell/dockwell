// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// APIToken is a secret for scripts, the CLI and integrations, acting for
// the user who made it within its scope.
type APIToken struct {
	ID        string
	TokenHash []byte
	UserID    string
	Name      string
	// Hint is the prefix and the first and last 4 of the secret's random
	// digits, such as "dwt_4f9K…x9Ta".
	Hint       string
	Scope      string
	CreatedAt  time.Time
	ExpiresAt  *time.Time // nil: it doesn't expire
	LastUsedAt *time.Time
	LastIP     string
	RevokedAt  *time.Time
}

// Token scopes, as stored.
const (
	ScopeReadOnly  = "read_only"
	ScopeReadWrite = "read_write"
)

// MaxTokenAddresses bounds the addresses kept for each token.
const MaxTokenAddresses = 256

const apiTokenColumns = `id, token_hash, user_id, name, hint, scope, created_at, expires_at, last_used_at, last_ip, revoked_at` //nolint:gosec // column names

func scanAPIToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var created int64
	var expires, used, revoked sql.NullInt64
	if err := row.Scan(&t.ID, &t.TokenHash, &t.UserID, &t.Name, &t.Hint, &t.Scope, &created, &expires, &used, &t.LastIP, &revoked); err != nil {
		return APIToken{}, err
	}
	t.CreatedAt, t.ExpiresAt, t.LastUsedAt, t.RevokedAt = fromMillis(created), timePtr(expires), timePtr(used), timePtr(revoked)
	return t, nil
}

// CreateAPIToken records a new token.
func (t *Tx) CreateAPIToken(ctx context.Context, tok APIToken) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO api_tokens (id, token_hash, user_id, name, hint, scope, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tok.ID, tok.TokenHash, tok.UserID, tok.Name, tok.Hint, tok.Scope, millis(tok.CreatedAt), nullMillis(tok.ExpiresAt))
	if err != nil {
		return fmt.Errorf("can't record API token %s: %w", tok.ID, err)
	}
	return nil
}

func (t *Tx) getAPIToken(ctx context.Context, where string, arg any) (APIToken, error) {
	tok, err := scanAPIToken(t.tx.QueryRowContext(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens WHERE `+where+` = ?`, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, fmt.Errorf("API token: %w", ErrNotFound)
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("can't read API token: %w", err)
	}
	return tok, nil
}

// GetAPITokenByHash returns a token by the hash of its secret, or
// ErrNotFound. Revoked and expired tokens are returned too; callers check.
func (t *Tx) GetAPITokenByHash(ctx context.Context, hash []byte) (APIToken, error) {
	return t.getAPIToken(ctx, "token_hash", hash)
}

// GetAPIToken returns a token by ID, or ErrNotFound.
func (t *Tx) GetAPIToken(ctx context.Context, id string) (APIToken, error) {
	return t.getAPIToken(ctx, "id", id)
}

// ListLiveAPITokens returns a user's tokens that are neither revoked nor
// expired at now, newest first.
func (t *Tx) ListLiveAPITokens(ctx context.Context, userID string, now time.Time) ([]APIToken, error) {
	rows, err := t.tx.QueryContext(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens
		WHERE user_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) ORDER BY id DESC`, userID, millis(now))
	if err != nil {
		return nil, fmt.Errorf("can't list API tokens: %w", err)
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		tok, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// CountLiveAPITokens counts a user's tokens that are neither revoked nor
// expired at now.
func (t *Tx) CountLiveAPITokens(ctx context.Context, userID string, now time.Time) (int, error) {
	var n int
	if err := t.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens
		WHERE user_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`, userID, millis(now)).Scan(&n); err != nil {
		return 0, fmt.Errorf("can't count API tokens: %w", err)
	}
	return n, nil
}

// RevokeAPIToken stops one of a user's tokens working, or reports
// ErrNotFound when the user has no such token still unrevoked.
func (t *Tx) RevokeAPIToken(ctx context.Context, userID, id string, at time.Time) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		millis(at), id, userID)
	if err != nil {
		return fmt.Errorf("can't revoke API token %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("API token %s: %w", id, ErrNotFound)
	}
	return nil
}

// TouchAPIToken records that a token was used at at from ip.
func (t *Tx) TouchAPIToken(ctx context.Context, id string, at time.Time, ip string) error {
	if _, err := t.tx.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ?, last_ip = ? WHERE id = ? AND revoked_at IS NULL`,
		millis(at), ip, id); err != nil {
		return fmt.Errorf("can't update API token %s: %w", id, err)
	}
	return nil
}

// SeeAPITokenAddress records that a token was used from address, and
// reports whether that was the first time. It keeps MaxTokenAddresses for
// each token, forgetting the least recently seen.
func (t *Tx) SeeAPITokenAddress(ctx context.Context, id, address string, at time.Time) (bool, error) {
	res, err := t.tx.ExecContext(ctx, `INSERT INTO api_token_addresses (token_id, address, first_seen, last_seen) VALUES (?, ?, ?, ?)
		ON CONFLICT (token_id, address) DO NOTHING`, id, address, millis(at), millis(at))
	if err != nil {
		return false, fmt.Errorf("can't record an address for API token %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := t.tx.ExecContext(ctx, `UPDATE api_token_addresses SET last_seen = ? WHERE token_id = ? AND address = ?`,
			millis(at), id, address); err != nil {
			return false, fmt.Errorf("can't record an address for API token %s: %w", id, err)
		}
		return false, nil
	}
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM api_token_addresses WHERE token_id = ?1 AND address NOT IN (
		SELECT address FROM api_token_addresses WHERE token_id = ?1 ORDER BY last_seen DESC, address LIMIT ?2)`,
		id, MaxTokenAddresses); err != nil {
		return false, fmt.Errorf("can't forget old addresses for API token %s: %w", id, err)
	}
	return true, nil
}

// PruneAPITokens deletes tokens revoked or expired before cutoff, with the
// addresses they were used from.
func (t *Tx) PruneAPITokens(ctx context.Context, cutoff time.Time) error {
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE revoked_at < ?1 OR expires_at < ?1`, millis(cutoff)); err != nil {
		return fmt.Errorf("can't prune API tokens: %w", err)
	}
	return nil
}
