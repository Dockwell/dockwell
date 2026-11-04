// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// User is a person who signs in to the panel.
type User struct {
	ID             string
	Email          string
	Name           string
	Role           string
	WebAuthnHandle []byte
	CreatedAt      time.Time
	// TOTPSecret and TOTPPending are sealed; nil when unset.
	TOTPSecret   []byte
	TOTPPending  []byte
	TOTPLastStep int64
	CodeFailures int
	CodeFailedAt *time.Time
	CodesLocked  *time.Time
}

// RoleOwner is the one role so far.
const RoleOwner = "owner"

const userColumns = `id, email, name, role, webauthn_handle, created_at, totp_secret, totp_pending, totp_last_step,
	code_failures, code_failed_at, codes_locked_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created int64
	var failedAt, lockedAt sql.NullInt64
	if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.WebAuthnHandle, &created, &u.TOTPSecret, &u.TOTPPending,
		&u.TOTPLastStep, &u.CodeFailures, &failedAt, &lockedAt); err != nil {
		return User{}, err
	}
	u.CreatedAt, u.CodeFailedAt, u.CodesLocked = fromMillis(created), timePtr(failedAt), timePtr(lockedAt)
	return u, nil
}

// CountUsers returns how many people have accounts.
func (t *Tx) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := t.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("can't count users: %w", err)
	}
	return n, nil
}

// CreateUser records a new user. A taken email, or a second owner, is
// ErrConflict.
func (t *Tx) CreateUser(ctx context.Context, u User) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO users (id, email, name, role, webauthn_handle, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.Name, u.Role, u.WebAuthnHandle, millis(u.CreatedAt))
	if isUniqueViolation(err) {
		return fmt.Errorf("user %s: %w", u.Email, ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("can't record user %s: %w", u.Email, err)
	}
	return nil
}

func (t *Tx) getUser(ctx context.Context, where string, arg any) (User, error) {
	u, err := scanUser(t.tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE `+where+` = ?`, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("user: %w", ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("can't read user: %w", err)
	}
	return u, nil
}

// GetUser returns a user by ID, or ErrNotFound.
func (t *Tx) GetUser(ctx context.Context, id string) (User, error) { return t.getUser(ctx, "id", id) }

// GetUserByEmail returns a user by lowercase email, or ErrNotFound.
func (t *Tx) GetUserByEmail(ctx context.Context, email string) (User, error) {
	return t.getUser(ctx, "email", email)
}

// GetUserByHandle returns a user by WebAuthn user handle, or ErrNotFound.
func (t *Tx) GetUserByHandle(ctx context.Context, handle []byte) (User, error) {
	return t.getUser(ctx, "webauthn_handle", handle)
}

// Owner returns the install's owner, or ErrNotFound before it is claimed.
func (t *Tx) Owner(ctx context.Context) (User, error) { return t.getUser(ctx, "role", RoleOwner) }

func (t *Tx) updateUser(ctx context.Context, id, query string, args ...any) error {
	res, err := t.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("can't update user %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("user %s: %w", id, ErrNotFound)
	}
	return nil
}

// SetTOTPPending stores a sealed secret awaiting confirmation.
func (t *Tx) SetTOTPPending(ctx context.Context, userID string, sealed []byte) error {
	return t.updateUser(ctx, userID, `UPDATE users SET totp_pending = ? WHERE id = ?`, sealed, userID)
}

// EnableTOTP makes a confirmed secret the user's, records the time step that
// confirmed it, and clears any lock.
func (t *Tx) EnableTOTP(ctx context.Context, userID string, sealed []byte, step int64) error {
	return t.updateUser(ctx, userID, `UPDATE users SET totp_secret = ?, totp_pending = NULL, totp_last_step = ?,
		code_failures = 0, code_failed_at = NULL, codes_locked_at = NULL WHERE id = ?`, sealed, step, userID)
}

// DisableTOTP deletes the user's secret and recovery codes.
func (t *Tx) DisableTOTP(ctx context.Context, userID string) error {
	if err := t.updateUser(ctx, userID, `UPDATE users SET totp_secret = NULL, totp_pending = NULL, totp_last_step = 0,
		code_failures = 0, code_failed_at = NULL, codes_locked_at = NULL WHERE id = ?`, userID); err != nil {
		return err
	}
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("can't delete recovery codes: %w", err)
	}
	return nil
}

// SpendTOTPStep records that a code for step was accepted. It fails with
// ErrConflict when that step, or a later one, was already accepted, so a
// code works once even when two requests race with it.
func (t *Tx) SpendTOTPStep(ctx context.Context, userID string, step int64) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, step, userID, step)
	if err != nil {
		return fmt.Errorf("can't record a code for user %s: %w", userID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("code for step %d: %w", step, ErrConflict)
	}
	return nil
}

// RecordCodeFailure counts a wrong code, locks code sign-in once there have
// been lockAfter in a row, and returns the count.
func (t *Tx) RecordCodeFailure(ctx context.Context, userID string, at time.Time, lockAfter int) (int, error) {
	err := t.updateUser(ctx, userID, `UPDATE users SET code_failures = code_failures + 1, code_failed_at = ?,
		codes_locked_at = CASE WHEN codes_locked_at IS NULL AND code_failures + 1 >= ? THEN ? ELSE codes_locked_at END
		WHERE id = ?`, millis(at), lockAfter, millis(at), userID)
	if err != nil {
		return 0, err
	}
	var n int
	if err := t.tx.QueryRowContext(ctx, `SELECT code_failures FROM users WHERE id = ?`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("can't read code failures: %w", err)
	}
	return n, nil
}

// ResetCodeFailures clears the wrong-code count after a good code. It leaves
// a lock in place: only UnlockCodes lifts it.
func (t *Tx) ResetCodeFailures(ctx context.Context, userID string) error {
	return t.updateUser(ctx, userID, `UPDATE users SET code_failures = 0, code_failed_at = NULL WHERE id = ?`, userID)
}

// UnlockCodes lifts a lock on code sign-in and clears the count.
func (t *Tx) UnlockCodes(ctx context.Context, userID string) error {
	return t.updateUser(ctx, userID, `UPDATE users SET code_failures = 0, code_failed_at = NULL, codes_locked_at = NULL WHERE id = ?`, userID)
}

// Passkey is a stored WebAuthn credential.
type Passkey struct {
	ID           string
	UserID       string
	CredentialID []byte
	RPID         string
	Name         string
	// Record is the WebAuthn credential record as JSON.
	Record         []byte
	CreatedAt      time.Time
	LastUsedAt     *time.Time
	CloneWarningAt *time.Time
}

const passkeyColumns = `id, user_id, credential_id, rp_id, name, record, created_at, last_used_at, clone_warning_at` //nolint:gosec // column names

func scanPasskey(row interface{ Scan(...any) error }) (Passkey, error) {
	var p Passkey
	var created int64
	var used, clone sql.NullInt64
	if err := row.Scan(&p.ID, &p.UserID, &p.CredentialID, &p.RPID, &p.Name, &p.Record, &created, &used, &clone); err != nil {
		return Passkey{}, err
	}
	p.CreatedAt, p.LastUsedAt, p.CloneWarningAt = fromMillis(created), timePtr(used), timePtr(clone)
	return p, nil
}

// CreatePasskey records a passkey. A credential ID already stored is
// ErrConflict.
func (t *Tx) CreatePasskey(ctx context.Context, p Passkey) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO passkeys (`+passkeyColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.UserID, p.CredentialID, p.RPID, p.Name, p.Record, millis(p.CreatedAt), nullMillis(p.LastUsedAt), nullMillis(p.CloneWarningAt))
	if isUniqueViolation(err) {
		return fmt.Errorf("passkey: %w", ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("can't record passkey: %w", err)
	}
	return nil
}

// ListPasskeys returns a user's passkeys, oldest first.
func (t *Tx) ListPasskeys(ctx context.Context, userID string) ([]Passkey, error) {
	rows, err := t.tx.QueryContext(ctx, `SELECT `+passkeyColumns+` FROM passkeys WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("can't list passkeys: %w", err)
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPasskeyByCredentialID returns a passkey by its WebAuthn credential ID,
// or ErrNotFound.
func (t *Tx) GetPasskeyByCredentialID(ctx context.Context, credentialID []byte) (Passkey, error) {
	p, err := scanPasskey(t.tx.QueryRowContext(ctx, `SELECT `+passkeyColumns+` FROM passkeys WHERE credential_id = ?`, credentialID))
	if errors.Is(err, sql.ErrNoRows) {
		return Passkey{}, fmt.Errorf("passkey: %w", ErrNotFound)
	}
	if err != nil {
		return Passkey{}, fmt.Errorf("can't read passkey: %w", err)
	}
	return p, nil
}

// UsePasskey stores a passkey's record after a sign-in, with its new sign
// count.
func (t *Tx) UsePasskey(ctx context.Context, id string, record []byte, at time.Time) error {
	return t.updatePasskey(ctx, id, `UPDATE passkeys SET record = ?, last_used_at = ? WHERE id = ?`, record, millis(at), id)
}

// FlagPasskeyClone marks a passkey whose sign count went backwards.
func (t *Tx) FlagPasskeyClone(ctx context.Context, id string, at time.Time) error {
	return t.updatePasskey(ctx, id, `UPDATE passkeys SET clone_warning_at = ? WHERE id = ? AND clone_warning_at IS NULL`, millis(at), id)
}

// DeletePasskey removes one of a user's passkeys, or reports ErrNotFound.
func (t *Tx) DeletePasskey(ctx context.Context, userID, id string) error {
	return t.updatePasskey(ctx, id, `DELETE FROM passkeys WHERE id = ? AND user_id = ?`, id, userID)
}

func (t *Tx) updatePasskey(ctx context.Context, id, query string, args ...any) error {
	res, err := t.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("can't update passkey %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("passkey %s: %w", id, ErrNotFound)
	}
	return nil
}

// ReplaceRecoveryCodes deletes a user's recovery codes and stores new hashes.
func (t *Tx) ReplaceRecoveryCodes(ctx context.Context, userID string, hashes [][]byte, at time.Time) error {
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("can't delete recovery codes: %w", err)
	}
	for _, h := range hashes {
		if _, err := t.tx.ExecContext(ctx, `INSERT INTO recovery_codes (user_id, code_hash, created_at) VALUES (?, ?, ?)`,
			userID, h, millis(at)); err != nil {
			return fmt.Errorf("can't store recovery codes: %w", err)
		}
	}
	return nil
}

// SpendRecoveryCode marks an unused recovery code used, or reports
// ErrNotFound when there is no such unused code.
func (t *Tx) SpendRecoveryCode(ctx context.Context, userID string, hash []byte, at time.Time) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
		millis(at), userID, hash)
	if err != nil {
		return fmt.Errorf("can't spend a recovery code: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("recovery code: %w", ErrNotFound)
	}
	return nil
}

// CountRecoveryCodes returns how many of a user's recovery codes are unused.
func (t *Tx) CountRecoveryCodes(ctx context.Context, userID string) (int, error) {
	var n int
	if err := t.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("can't count recovery codes: %w", err)
	}
	return n, nil
}

// Session is a signed-in browser.
type Session struct {
	ID         string
	TokenHash  []byte
	UserID     string
	RPID       string
	Method     string
	UserAgent  string
	CreatedIP  string
	LastIP     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
}

const sessionColumns = `id, token_hash, user_id, rp_id, method, user_agent, created_ip, last_ip, created_at, last_seen_at, expires_at, revoked_at`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var created, seen, expires int64
	var revoked sql.NullInt64
	if err := row.Scan(&s.ID, &s.TokenHash, &s.UserID, &s.RPID, &s.Method, &s.UserAgent, &s.CreatedIP, &s.LastIP,
		&created, &seen, &expires, &revoked); err != nil {
		return Session{}, err
	}
	s.CreatedAt, s.LastSeenAt, s.ExpiresAt, s.RevokedAt = fromMillis(created), fromMillis(seen), fromMillis(expires), timePtr(revoked)
	return s, nil
}

// CreateSession records a new session.
func (t *Tx) CreateSession(ctx context.Context, s Session) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO sessions (`+sessionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.TokenHash, s.UserID, s.RPID, s.Method, s.UserAgent, s.CreatedIP, s.LastIP,
		millis(s.CreatedAt), millis(s.LastSeenAt), millis(s.ExpiresAt), nullMillis(s.RevokedAt))
	if err != nil {
		return fmt.Errorf("can't record session: %w", err)
	}
	return nil
}

// GetSessionByTokenHash returns a session by the hash of its token, or
// ErrNotFound. Expired and revoked sessions are returned too; callers check.
func (t *Tx) GetSessionByTokenHash(ctx context.Context, hash []byte) (Session, error) {
	s, err := scanSession(t.tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE token_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("session: %w", ErrNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("can't read session: %w", err)
	}
	return s, nil
}

// GetSession returns a session by ID, or ErrNotFound.
func (t *Tx) GetSession(ctx context.Context, id string) (Session, error) {
	s, err := scanSession(t.tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("can't read session %s: %w", id, err)
	}
	return s, nil
}

// TouchSession records that a session made a request.
func (t *Tx) TouchSession(ctx context.Context, id string, at time.Time, ip string) error {
	_, err := t.tx.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, last_ip = ? WHERE id = ? AND revoked_at IS NULL`,
		millis(at), ip, id)
	if err != nil {
		return fmt.Errorf("can't update session %s: %w", id, err)
	}
	return nil
}

// ListActiveSessions returns a user's sessions that are neither revoked nor
// past their absolute limit, newest first. Callers apply the idle limit.
func (t *Tx) ListActiveSessions(ctx context.Context, userID string, now time.Time) ([]Session, error) {
	rows, err := t.tx.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions
		WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ? ORDER BY id DESC`, userID, millis(now))
	if err != nil {
		return nil, fmt.Errorf("can't list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeSession ends one of a user's sessions, or reports ErrNotFound when
// the user has no such session still open.
func (t *Tx) RevokeSession(ctx context.Context, userID, id string, at time.Time) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE sessions SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		millis(at), id, userID)
	if err != nil {
		return fmt.Errorf("can't revoke session %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	return nil
}

// PruneSessions deletes sessions that ended before cutoff.
func (t *Tx) PruneSessions(ctx context.Context, cutoff time.Time) error {
	_, err := t.tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ? OR revoked_at < ?`, millis(cutoff), millis(cutoff))
	if err != nil {
		return fmt.Errorf("can't prune sessions: %w", err)
	}
	return nil
}

// LinkToken is a one-time link: a claim link, or a sign-in link.
type LinkToken struct {
	ID        string
	Purpose   string
	TokenHash []byte
	UserID    string // empty for a claim link
	CreatedAt time.Time
	CreatedBy string
	ExpiresAt time.Time
	UsedAt    *time.Time
	RevokedAt *time.Time
}

// Link purposes.
const (
	LinkClaim   = "claim"
	LinkRecover = "recover"
)

// CreateLinkToken records a new link and revokes older unused links for the
// same purpose and user, so only the newest one works.
func (t *Tx) CreateLinkToken(ctx context.Context, l LinkToken) error {
	if _, err := t.tx.ExecContext(ctx, `UPDATE link_tokens SET revoked_at = ?
		WHERE purpose = ? AND COALESCE(user_id, '') = ? AND used_at IS NULL AND revoked_at IS NULL`,
		millis(l.CreatedAt), l.Purpose, l.UserID); err != nil {
		return fmt.Errorf("can't replace older links: %w", err)
	}
	var user sql.NullString
	if l.UserID != "" {
		user = sql.NullString{String: l.UserID, Valid: true}
	}
	_, err := t.tx.ExecContext(ctx, `INSERT INTO link_tokens (id, purpose, token_hash, user_id, created_at, created_by, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, l.ID, l.Purpose, l.TokenHash, user, millis(l.CreatedAt), l.CreatedBy, millis(l.ExpiresAt))
	if err != nil {
		return fmt.Errorf("can't record link: %w", err)
	}
	return nil
}

// GetLinkTokenByHash returns a link by the hash of its token, or
// ErrNotFound. Used, revoked and expired links are returned too; callers
// check.
func (t *Tx) GetLinkTokenByHash(ctx context.Context, hash []byte) (LinkToken, error) {
	var l LinkToken
	var created, expires int64
	var used, revoked sql.NullInt64
	var user sql.NullString
	err := t.tx.QueryRowContext(ctx, `SELECT id, purpose, token_hash, user_id, created_at, created_by, expires_at, used_at, revoked_at
		FROM link_tokens WHERE token_hash = ?`, hash).Scan(&l.ID, &l.Purpose, &l.TokenHash, &user, &created, &l.CreatedBy, &expires, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return LinkToken{}, fmt.Errorf("link: %w", ErrNotFound)
	}
	if err != nil {
		return LinkToken{}, fmt.Errorf("can't read link: %w", err)
	}
	l.UserID, l.CreatedAt, l.ExpiresAt, l.UsedAt, l.RevokedAt = user.String, fromMillis(created), fromMillis(expires), timePtr(used), timePtr(revoked)
	return l, nil
}

// SpendLinkToken marks a link used. It fails with ErrNotFound when the link
// was already used or revoked, so two requests can't both spend it.
func (t *Tx) SpendLinkToken(ctx context.Context, id string, at time.Time) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE link_tokens SET used_at = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL`,
		millis(at), id)
	if err != nil {
		return fmt.Errorf("can't spend link %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unused link %s: %w", id, ErrNotFound)
	}
	return nil
}
