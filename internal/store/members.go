// SPDX-License-Identifier: AGPL-3.0-only

package store

// Managing members: what removing someone, or lowering what they hold, ends
// (docs/adr/0049-roles-and-invites-by-project.md, "Managing members").

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Ended counts what ending someone's access took with it.
type Ended struct {
	Sessions, Tokens int
	// Links are the accounts at sign-in providers deleted with someone removed,
	// another way they signed in (ADR 0095).
	Links int
}

// RevokeSessions ends every session a person has open, and says how many.
func (t *Tx) RevokeSessions(ctx context.Context, userID string, at time.Time) (int, error) {
	res, err := t.exec(ctx, `UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ?`,
		millis(at), userID, millis(at))
	if err != nil {
		return 0, fmt.Errorf("can't end the sessions of %s: %w", userID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RevokeAPITokens stops every API token a person made, the CLI's local ones
// too, and says how many: a token acts for its maker, and none outlives what
// they held (ADR 0049).
func (t *Tx) RevokeAPITokens(ctx context.Context, userID string, at time.Time) (int, error) {
	res, err := t.exec(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, millis(at), userID)
	if err != nil {
		return 0, fmt.Errorf("can't revoke the API tokens of %s: %w", userID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// EndAccess ends a person's sessions and revokes their API tokens.
func (t *Tx) EndAccess(ctx context.Context, userID string, at time.Time) (Ended, error) {
	sessions, err := t.RevokeSessions(ctx, userID, at)
	if err != nil {
		return Ended{}, err
	}
	tokens, err := t.RevokeAPITokens(ctx, userID, at)
	return Ended{Sessions: sessions, Tokens: tokens}, err
}

// RemoveUser removes a person: their sign-in is off, with their passkeys, codes,
// links at sign-in providers and sessions gone and their API tokens revoked, and
// they hold nothing. Their
// row stays, marked removed, so what they did keeps their name, and their email
// is free again. It returns ErrOwner for the owner, who transfers ownership
// first, and ErrNotFound for nobody, or someone removed already.
func (t *Tx) RemoveUser(ctx context.Context, userID, by string, at time.Time) (Ended, error) {
	if owner, err := t.isOwner(ctx, userID); err != nil || owner {
		return Ended{}, errors.Join(err, ownerOr(owner))
	}
	res, err := t.exec(ctx, `UPDATE users SET removed_at = ?, removed_by = ?, totp_secret = NULL, totp_pending = NULL,
		totp_pending_session = NULL, totp_pending_expires_at = NULL WHERE id = ? AND removed_at IS NULL`, millis(at), by, userID)
	if err != nil {
		return Ended{}, fmt.Errorf("can't remove %s: %w", userID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Ended{}, fmt.Errorf("user %s: %w", userID, ErrNotFound)
	}
	for _, q := range []string{
		`DELETE FROM passkeys WHERE user_id = ?`,
		`DELETE FROM recovery_codes WHERE user_id = ?`,
		// A one-time sign-in link made for them before would sign them in after.
		`DELETE FROM link_tokens WHERE user_id = ?`,
		`DELETE FROM grants WHERE user_id = ?`,
	} {
		if _, err := t.exec(ctx, q, userID); err != nil {
			return Ended{}, fmt.Errorf("can't remove what %s signed in with and held: %w", userID, err)
		}
	}
	// An account linked at a provider would sign them in again (ADR 0095).
	res, err = t.exec(ctx, `DELETE FROM provider_links WHERE user_id = ?`, userID)
	if err != nil {
		return Ended{}, fmt.Errorf("can't delete the provider links of %s: %w", userID, err)
	}
	links, _ := res.RowsAffected()
	ended, err := t.EndAccess(ctx, userID, at)
	ended.Links = int(links)
	return ended, err
}

// ownerOr is ErrOwner when owner.
func ownerOr(owner bool) error {
	if owner {
		return ErrOwner
	}
	return nil
}

// AcceptedInvite is the invite a person joined with, or ErrNotFound for
// someone who claimed the install or was there before invites.
func (t *Tx) AcceptedInvite(ctx context.Context, userID string) (Invite, error) {
	i, err := scanInvite(t.queryRow(ctx, `SELECT `+inviteColumns+` FROM invites WHERE accepted_by = ? ORDER BY accepted_at DESC LIMIT 1`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, fmt.Errorf("the invite of %s: %w", userID, ErrNotFound)
	}
	if err != nil {
		return Invite{}, fmt.Errorf("can't read the invite of %s: %w", userID, err)
	}
	return i, nil
}

// PlannedChangesBy lists the changes an actor planned that are still planned,
// newest first, without their items.
func (t *Tx) PlannedChangesBy(ctx context.Context, actor string) ([]Change, error) {
	rows, err := t.query(ctx, `SELECT `+changeColumns+` FROM changes WHERE state = ? AND actor = ? ORDER BY created_at DESC, id DESC`,
		ChangePlanned, actor)
	if err != nil {
		return nil, fmt.Errorf("can't list the changes %s planned: %w", actor, err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read a change: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
