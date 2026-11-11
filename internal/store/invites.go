// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Invite is an invitation to sign in with a role in a place: the install, or
// one project (ADR 0048, ADR 0049). Its secret is kept as a hash, and it works
// once.
type Invite struct {
	ID        string
	TokenHash []byte
	// Email is who the invite names, unverified: Dockwell sends no mail.
	Email string
	Role  string
	// ProjectID is the place: a project, or Install.
	ProjectID  string
	CreatedAt  time.Time
	CreatedBy  string // the actor who made it
	ExpiresAt  time.Time
	AcceptedAt *time.Time
	AcceptedBy string
	RevokedAt  *time.Time
	RevokedBy  string
}

// Open reports an invite that still works at now: not accepted, revoked or expired.
func (i Invite) Open(now time.Time) bool {
	return i.AcceptedAt == nil && i.RevokedAt == nil && now.Before(i.ExpiresAt)
}

// CreateInvite records an invite. Nobody is invited as owner, and an admin's
// place is the install.
func (t *Tx) CreateInvite(ctx context.Context, i Invite) error {
	switch {
	case i.Role == RoleOwner:
		return fmt.Errorf("an invite as owner: %w", ErrOwner)
	case i.Role != RoleAdmin && i.Role != RoleDeveloper && i.Role != RoleViewer:
		return fmt.Errorf("an invite as %q, which isn't a role", i.Role)
	case i.Role == RoleAdmin && i.ProjectID != Install:
		return fmt.Errorf("an admin's invite to %s: %w", i.ProjectID, ErrNotAPlace)
	}
	_, err := t.exec(ctx, `INSERT INTO invites (id, token_hash, email, role, project_id, created_at, created_by, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.TokenHash, i.Email, i.Role, i.ProjectID, millis(i.CreatedAt), i.CreatedBy, millis(i.ExpiresAt))
	if err != nil {
		return fmt.Errorf("can't record the invite for %s: %w", i.Email, err)
	}
	return nil
}

const inviteColumns = `id, token_hash, email, role, project_id, created_at, created_by, expires_at, accepted_at, accepted_by, revoked_at, revoked_by`

func scanInvite(row interface{ Scan(...any) error }) (Invite, error) {
	var i Invite
	var created, expires int64
	var accepted, revoked sql.NullInt64
	var acceptedBy sql.NullString
	if err := row.Scan(&i.ID, &i.TokenHash, &i.Email, &i.Role, &i.ProjectID, &created, &i.CreatedBy, &expires,
		&accepted, &acceptedBy, &revoked, &i.RevokedBy); err != nil {
		return Invite{}, err
	}
	i.CreatedAt, i.ExpiresAt = fromMillis(created), fromMillis(expires)
	if accepted.Valid {
		at := fromMillis(accepted.Int64)
		i.AcceptedAt = &at
	}
	i.AcceptedBy = acceptedBy.String
	if revoked.Valid {
		at := fromMillis(revoked.Int64)
		i.RevokedAt = &at
	}
	return i, nil
}

// GetInviteByHash reads the invite with a secret's hash, or ErrNotFound.
func (t *Tx) GetInviteByHash(ctx context.Context, hash []byte) (Invite, error) {
	return t.getInvite(ctx, `token_hash = ?`, hash)
}

// GetInvite reads an invite by its ID, or ErrNotFound.
func (t *Tx) GetInvite(ctx context.Context, id string) (Invite, error) {
	return t.getInvite(ctx, `id = ?`, id)
}

func (t *Tx) getInvite(ctx context.Context, where string, arg any) (Invite, error) {
	i, err := scanInvite(t.queryRow(ctx, `SELECT `+inviteColumns+` FROM invites WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, fmt.Errorf("invite: %w", ErrNotFound)
	}
	if err != nil {
		return Invite{}, fmt.Errorf("can't read an invite: %w", err)
	}
	return i, nil
}

// ListOpenInvites lists the invites that still work at now, newest first.
func (t *Tx) ListOpenInvites(ctx context.Context, now time.Time) ([]Invite, error) {
	rows, err := t.query(ctx, `SELECT `+inviteColumns+` FROM invites
		WHERE accepted_at IS NULL AND revoked_at IS NULL AND expires_at > ? ORDER BY created_at DESC, id DESC`, millis(now))
	if err != nil {
		return nil, fmt.Errorf("can't list invites: %w", err)
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read an invite: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// AcceptInvite spends an invite for the person it made, if it still works at
// at, or reports ErrNotFound: two acceptances racing can't both succeed.
func (t *Tx) AcceptInvite(ctx context.Context, id, userID string, at time.Time) error {
	res, err := t.exec(ctx, `UPDATE invites SET accepted_at = ?, accepted_by = ?
		WHERE id = ? AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > ?`, millis(at), userID, id, millis(at))
	if err != nil {
		return fmt.Errorf("can't accept invite %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("open invite %s: %w", id, ErrNotFound)
	}
	return nil
}

// RevokeInvite stops an open invite working, or reports ErrNotFound.
func (t *Tx) RevokeInvite(ctx context.Context, id, by string, at time.Time) error {
	res, err := t.exec(ctx, `UPDATE invites SET revoked_at = ?, revoked_by = ?
		WHERE id = ? AND accepted_at IS NULL AND revoked_at IS NULL`, millis(at), by, id)
	if err != nil {
		return fmt.Errorf("can't revoke invite %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("open invite %s: %w", id, ErrNotFound)
	}
	return nil
}
