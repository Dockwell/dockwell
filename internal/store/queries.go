// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetSetting returns a singleton value, or ErrNotFound.
func (t *Tx) GetSetting(ctx context.Context, key string) ([]byte, error) {
	var v []byte
	err := t.queryRow(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("can't read setting %s: %w", key, err)
	}
	return v, nil
}

// PutSetting stores a singleton value, replacing any earlier one.
func (t *Tx) PutSetting(ctx context.Context, key string, value []byte) error {
	_, err := t.exec(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, millis(time.Now()))
	if err != nil {
		return fmt.Errorf("can't store setting %s: %w", key, err)
	}
	return nil
}

// Server is a machine running the agent.
type Server struct {
	ID           string
	Name         string
	CreatedAt    time.Time
	CertSerial   string
	CertNotAfter time.Time
	// CertKeyHash is the SHA-256 of the current certificate's public key.
	CertKeyHash []byte
	// PrevCertSerial is the certificate the current one replaced.
	PrevCertSerial string
	LastSeenAt     *time.Time
	RevokedAt      *time.Time
}

const serverColumns = `id, name, created_at, cert_serial, cert_not_after, cert_key_hash, prev_cert_serial, last_seen_at, revoked_at`

func scanServer(row interface{ Scan(...any) error }) (Server, error) {
	var s Server
	var created, notAfter int64
	var lastSeen, revoked sql.NullInt64
	if err := row.Scan(&s.ID, &s.Name, &created, &s.CertSerial, &notAfter, &s.CertKeyHash, &s.PrevCertSerial, &lastSeen, &revoked); err != nil {
		return Server{}, err
	}
	s.CreatedAt, s.CertNotAfter = fromMillis(created), fromMillis(notAfter)
	s.LastSeenAt, s.RevokedAt = timePtr(lastSeen), timePtr(revoked)
	return s, nil
}

// CreateServer records a new server. A name already used by an active
// server is ErrConflict.
func (t *Tx) CreateServer(ctx context.Context, s Server) error {
	if s.CertKeyHash == nil {
		s.CertKeyHash = []byte{}
	}
	err := t.insert(ctx, `INSERT INTO servers (`+serverColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Name, millis(s.CreatedAt), s.CertSerial, millis(s.CertNotAfter), s.CertKeyHash, s.PrevCertSerial,
		nullMillis(s.LastSeenAt), nullMillis(s.RevokedAt))
	if t.unique(err) {
		return fmt.Errorf("a server named %s %w", s.Name, ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("can't record server %s: %w", s.Name, err)
	}
	return nil
}

// GetServer returns one server by ID, or ErrNotFound.
func (t *Tx) GetServer(ctx context.Context, id string) (Server, error) {
	s, err := scanServer(t.queryRow(ctx, `SELECT `+serverColumns+` FROM servers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Server{}, fmt.Errorf("server %s: %w", id, ErrNotFound)
	}
	return s, err
}

// ListServers returns the servers that aren't revoked, oldest first.
func (t *Tx) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := t.query(ctx, `SELECT `+serverColumns+` FROM servers WHERE revoked_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("can't list servers: %w", err)
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		s, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CertUpdate is a renewed certificate.
type CertUpdate struct {
	Serial     string
	KeyHash    []byte
	NotAfter   time.Time
	PrevSerial string
}

// UpdateServerCert records a renewed certificate.
func (t *Tx) UpdateServerCert(ctx context.Context, id string, c CertUpdate) error {
	return t.updateServer(ctx, id, `UPDATE servers SET cert_serial = ?, cert_key_hash = ?, cert_not_after = ?, prev_cert_serial = ? WHERE id = ?`,
		c.Serial, c.KeyHash, millis(c.NotAfter), c.PrevSerial, id)
}

// ServerReport is what a server's agent last said about it, encoded.
type ServerReport struct {
	Inventory  []byte
	AgentBuild []byte
}

// SaveServerReport stores what a server's agent reported, and when.
func (t *Tx) SaveServerReport(ctx context.Context, id string, r ServerReport, at time.Time) error {
	return t.updateServer(ctx, id, `UPDATE servers SET inventory = ?, agent_build = ?, last_seen_at = ? WHERE id = ?`,
		r.Inventory, r.AgentBuild, millis(at), id)
}

// ServerReports returns the stored reports of active servers, by ID.
func (t *Tx) ServerReports(ctx context.Context) (map[string]ServerReport, error) {
	rows, err := t.query(ctx, `SELECT id, inventory, agent_build FROM servers WHERE revoked_at IS NULL AND inventory IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("can't read server reports: %w", err)
	}
	defer rows.Close()
	out := map[string]ServerReport{}
	for rows.Next() {
		var id string
		var r ServerReport
		if err := rows.Scan(&id, &r.Inventory, &r.AgentBuild); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// TouchServer records that a server was heard from.
func (t *Tx) TouchServer(ctx context.Context, id string, at time.Time) error {
	return t.updateServer(ctx, id, `UPDATE servers SET last_seen_at = ? WHERE id = ?`, millis(at), id)
}

// RevokeServer stops a server from connecting again.
func (t *Tx) RevokeServer(ctx context.Context, id string, at time.Time) error {
	return t.updateServer(ctx, id, `UPDATE servers SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, millis(at), id)
}

func (t *Tx) updateServer(ctx context.Context, id, query string, args ...any) error {
	res, err := t.exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("can't update server %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("server %s: %w", id, ErrNotFound)
	}
	return nil
}

// JoinToken lets one server join, once, before it expires.
type JoinToken struct {
	ID         string
	SecretHash []byte
	CreatedAt  time.Time
	CreatedBy  string
	ExpiresAt  time.Time
	UsedAt     *time.Time
	UsedBy     string
}

// CreateJoinToken records a new join token.
func (t *Tx) CreateJoinToken(ctx context.Context, j JoinToken) error {
	_, err := t.exec(ctx, `INSERT INTO join_tokens (id, secret_hash, created_at, created_by, expires_at)
		VALUES (?, ?, ?, ?, ?)`, j.ID, j.SecretHash, millis(j.CreatedAt), j.CreatedBy, millis(j.ExpiresAt))
	if err != nil {
		return fmt.Errorf("can't record join token: %w", err)
	}
	return nil
}

// GetJoinToken returns a join token by ID, or ErrNotFound.
func (t *Tx) GetJoinToken(ctx context.Context, id string) (JoinToken, error) {
	var j JoinToken
	var created, expires int64
	var used sql.NullInt64
	var usedBy sql.NullString
	err := t.queryRow(ctx, `SELECT id, secret_hash, created_at, created_by, expires_at, used_at, used_by
		FROM join_tokens WHERE id = ?`, id).Scan(&j.ID, &j.SecretHash, &created, &j.CreatedBy, &expires, &used, &usedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return JoinToken{}, fmt.Errorf("join token %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return JoinToken{}, fmt.Errorf("can't read join token %s: %w", id, err)
	}
	j.CreatedAt, j.ExpiresAt, j.UsedAt, j.UsedBy = fromMillis(created), fromMillis(expires), timePtr(used), usedBy.String
	return j, nil
}

// MarkJoinTokenUsed spends a token. It fails with ErrNotFound if the token
// was already used, so two servers can't race to join with one token.
func (t *Tx) MarkJoinTokenUsed(ctx context.Context, id, serverID string, at time.Time) error {
	res, err := t.exec(ctx, `UPDATE join_tokens SET used_at = ?, used_by = ? WHERE id = ? AND used_at IS NULL`,
		millis(at), serverID, id)
	if err != nil {
		return fmt.Errorf("can't spend join token %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unused join token %s: %w", id, ErrNotFound)
	}
	return nil
}

// Event is one entry in the audit log.
type Event struct {
	ID      string
	At      time.Time
	Actor   string
	Action  string
	Subject string
	Detail  string
}

// RecordEvent appends to the audit log.
func (t *Tx) RecordEvent(ctx context.Context, e Event) error {
	_, err := t.exec(ctx, `INSERT INTO audit_events (id, at, actor, action, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, millis(e.At), e.Actor, e.Action, e.Subject, e.Detail)
	if err != nil {
		return fmt.Errorf("can't record audit event %s: %w", e.Action, err)
	}
	return nil
}

// ListEvents returns the newest audit events first, at most limit of them.
func (t *Tx) ListEvents(ctx context.Context, limit int) ([]Event, error) {
	rows, err := t.query(ctx, `SELECT id, at, actor, action, subject, detail FROM audit_events ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("can't list audit events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Subject, &e.Detail); err != nil {
			return nil, err
		}
		e.At = fromMillis(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
