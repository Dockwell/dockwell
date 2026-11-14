// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ProviderLink is an account at a sign-in provider that signs its person in to
// the panel, kept by the provider's issuer and the account's subject, never by an
// email address (ADR 0095).
type ProviderLink struct {
	ID         string
	UserID     string
	ProviderID string
	Issuer     string
	Subject    string
	// Account is what the provider calls the account, such as its email address,
	// for its person to tell accounts apart. Nothing else reads it.
	Account    string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

const providerLinkColumns = `id, user_id, provider_id, issuer, subject, account, created_at, last_used_at`

func scanProviderLink(row interface{ Scan(...any) error }) (ProviderLink, error) {
	var l ProviderLink
	var created int64
	var used sql.NullInt64
	if err := row.Scan(&l.ID, &l.UserID, &l.ProviderID, &l.Issuer, &l.Subject, &l.Account, &created, &used); err != nil {
		return ProviderLink{}, err
	}
	l.CreatedAt, l.LastUsedAt = fromMillis(created), timePtr(used)
	return l, nil
}

// CreateProviderLink records a link, or reports ErrConflict when the account is
// linked to someone already, or its person has an account at that provider.
func (t *Tx) CreateProviderLink(ctx context.Context, l ProviderLink) error {
	err := t.insert(ctx, `INSERT INTO provider_links (`+providerLinkColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.UserID, l.ProviderID, l.Issuer, l.Subject, l.Account, millis(l.CreatedAt), nullMillis(l.LastUsedAt))
	if t.unique(err) {
		return fmt.Errorf("provider link: %w", ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("can't record provider link: %w", err)
	}
	return nil
}

// GetProviderLinkByAccount returns the link of an account at an issuer, or
// ErrNotFound.
func (t *Tx) GetProviderLinkByAccount(ctx context.Context, issuer, subject string) (ProviderLink, error) {
	l, err := scanProviderLink(t.queryRow(ctx, `SELECT `+providerLinkColumns+` FROM provider_links WHERE issuer = ? AND subject = ?`, issuer, subject))
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderLink{}, fmt.Errorf("provider link: %w", ErrNotFound)
	}
	if err != nil {
		return ProviderLink{}, fmt.Errorf("can't read provider link: %w", err)
	}
	return l, nil
}

// GetProviderLink returns a link by ID, or ErrNotFound.
func (t *Tx) GetProviderLink(ctx context.Context, id string) (ProviderLink, error) {
	l, err := scanProviderLink(t.queryRow(ctx, `SELECT `+providerLinkColumns+` FROM provider_links WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderLink{}, fmt.Errorf("provider link %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return ProviderLink{}, fmt.Errorf("can't read provider link %s: %w", id, err)
	}
	return l, nil
}

// ListProviderLinks returns a person's links, oldest first, or everyone's for an
// empty userID.
func (t *Tx) ListProviderLinks(ctx context.Context, userID string) ([]ProviderLink, error) {
	q, args := `SELECT `+providerLinkColumns+` FROM provider_links ORDER BY id`, []any{}
	if userID != "" {
		q, args = `SELECT `+providerLinkColumns+` FROM provider_links WHERE user_id = ? ORDER BY id`, []any{userID}
	}
	rows, err := t.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("can't list provider links: %w", err)
	}
	defer rows.Close()
	var out []ProviderLink
	for rows.Next() {
		l, err := scanProviderLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UseProviderLink records that a link signed its person in.
func (t *Tx) UseProviderLink(ctx context.Context, id string, at time.Time) error {
	if _, err := t.exec(ctx, `UPDATE provider_links SET last_used_at = ? WHERE id = ?`, millis(at), id); err != nil {
		return fmt.Errorf("can't record that provider link %s signed in: %w", id, err)
	}
	return nil
}

// DeleteProviderLink deletes one of a person's links, or everyone's for an empty
// userID, and ends the sessions it began. It reports ErrNotFound for a link that
// isn't there, or isn't theirs.
func (t *Tx) DeleteProviderLink(ctx context.Context, userID, id string, at time.Time) error {
	q, args := `DELETE FROM provider_links WHERE id = ?`, []any{id}
	if userID != "" {
		q, args = `DELETE FROM provider_links WHERE id = ? AND user_id = ?`, []any{id, userID}
	}
	res, err := t.exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("can't delete provider link %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("provider link %s: %w", id, ErrNotFound)
	}
	_, err = t.EndProviderLinkSessions(ctx, id, at)
	return err
}

// EndProviderLinkSessions ends every open session a link began, and says how many.
func (t *Tx) EndProviderLinkSessions(ctx context.Context, linkID string, at time.Time) (int, error) {
	if linkID == "" {
		return 0, nil
	}
	res, err := t.exec(ctx, `UPDATE sessions SET revoked_at = ? WHERE provider_link_id = ? AND revoked_at IS NULL`, millis(at), linkID)
	if err != nil {
		return 0, fmt.Errorf("can't end the sessions provider link %s began: %w", linkID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteProviderLinksOf deletes every link to a provider, ending the sessions each
// began, and says how many it deleted.
func (t *Tx) DeleteProviderLinksOf(ctx context.Context, providerID string, at time.Time) (int, error) {
	rows, err := t.query(ctx, `SELECT id FROM provider_links WHERE provider_id = ?`, providerID)
	if err != nil {
		return 0, fmt.Errorf("can't list the links to provider %s: %w", providerID, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := t.DeleteProviderLink(ctx, "", id, at); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
