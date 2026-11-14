// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// DeleteSetting removes a value. Removing one that isn't there is no error.
func (t *Tx) DeleteSetting(ctx context.Context, key string) error {
	if _, err := t.exec(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("can't delete setting %s: %w", key, err)
	}
	return nil
}

// Keys of the install's own settings (ADR 0042).
const (
	// SettingInstallName is what the install is called.
	SettingInstallName = "install.name"
	// SettingTimeZone is the zone new schedules start in.
	SettingTimeZone = "install.time_zone"
	// SettingAutoUpdate is what new apps' automatic updates start as (ADR 0080),
	// as JSON.
	SettingAutoUpdate = "install.auto_update"
	// SettingStepUpFloor is the least anyone may choose for how often they're
	// asked for their passkey again (ADR 0075). Unset allows all three.
	SettingStepUpFloor = "auth.step_up_floor"
)

// SettingPublicAddress is the key of what a person set of a server's public address.
func SettingPublicAddress(server string) string { return "server.public_address." + server }

// SettingDesiredCache is the key of the ID of the cache a server's agent last
// said it keeps its desired state in (ADR 0054).
func SettingDesiredCache(server string) string { return "server.desired_cache." + server }

// SettingDesiredUnknown is the key of how many of what a server's agent keeps the
// store had no record of, as last said on the timeline (ADR 0054).
func SettingDesiredUnknown(server string) string { return "server.desired_unknown." + server }

// SettingDomainProofs is the key of what a server's certificates proved of its domains.
func SettingDomainProofs(server string) string { return "server.domain_proofs." + server }

// SettingCompromised is the key of when a server was removed as one that may be
// compromised, so what it had is revoked at once rather than once it's replaced
// (ADR 0090). It stays with the revoked server's row.
func SettingCompromised(server string) string { return "server.compromised." + server }

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
	// Labels are what the server joining with it gets (ADR 0091).
	Labels map[string]string
}

// CreateJoinToken records a new join token.
func (t *Tx) CreateJoinToken(ctx context.Context, j JoinToken) error {
	labels, err := labelsColumn(j.Labels)
	if err != nil {
		return err
	}
	_, err = t.exec(ctx, `INSERT INTO join_tokens (id, secret_hash, created_at, created_by, expires_at, labels)
		VALUES (?, ?, ?, ?, ?, ?)`, j.ID, j.SecretHash, millis(j.CreatedAt), j.CreatedBy, millis(j.ExpiresAt), labels)
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
	var labels string
	err := t.queryRow(ctx, `SELECT id, secret_hash, created_at, created_by, expires_at, used_at, used_by, labels
		FROM join_tokens WHERE id = ?`, id).Scan(&j.ID, &j.SecretHash, &created, &j.CreatedBy, &expires, &used, &usedBy, &labels)
	if errors.Is(err, sql.ErrNoRows) {
		return JoinToken{}, fmt.Errorf("join token %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return JoinToken{}, fmt.Errorf("can't read join token %s: %w", id, err)
	}
	j.CreatedAt, j.ExpiresAt, j.UsedAt, j.UsedBy = fromMillis(created), fromMillis(expires), timePtr(used), usedBy.String
	if j.Labels, err = labelsOf(labels); err != nil {
		return JoinToken{}, fmt.Errorf("can't read the labels of join token %s: %w", id, err)
	}
	return j, nil
}

// JoinLabels are the labels server joined with, as its join token had them, or
// none (ADR 0091).
func (t *Tx) JoinLabels(ctx context.Context, server string) (map[string]string, error) {
	rows, err := t.query(ctx, `SELECT labels FROM join_tokens WHERE used_by = ? AND labels <> ''`, server)
	if err != nil {
		return nil, fmt.Errorf("can't read the labels %s joined with: %w", server, err)
	}
	defer rows.Close()
	var out map[string]string
	for rows.Next() {
		var labels string
		if err := rows.Scan(&labels); err != nil {
			return nil, fmt.Errorf("can't read the labels %s joined with: %w", server, err)
		}
		if out, err = labelsOf(labels); err != nil {
			return nil, fmt.Errorf("can't read the labels %s joined with: %w", server, err)
		}
	}
	return out, rows.Err()
}

// labelsColumn is labels as the column keeps them: JSON, or empty for none.
func labelsColumn(labels map[string]string) (string, error) {
	if len(labels) == 0 {
		return "", nil
	}
	b, err := json.Marshal(labels)
	return string(b), err
}

func labelsOf(column string) (map[string]string, error) {
	if column == "" {
		return nil, nil
	}
	var out map[string]string
	err := json.Unmarshal([]byte(column), &out)
	return out, err
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
	// Detail is free text for people, which may hold addresses. Nothing
	// reads it to find what an event is about: Target says that.
	Detail string
	// Surface is where it came from: SurfacePanel, SurfaceAPI, SurfaceCLI
	// or SurfaceAutomatic. SurfaceOf gives an actor's own.
	Surface string
	// Target is who or what the event acted on, other than its actor, by
	// ID: the server that joined or was removed, the API token or passkey
	// made or removed, or the person a sign-in link is for. Empty when
	// there is nothing more to name, such as signing in.
	Target string
}

// Where an audit event or a change came from.
const (
	SurfacePanel     = "panel"
	SurfaceAPI       = "api"
	SurfaceCLI       = "cli" // the dockwell CLI: calling the API with a token, or run on a server, such as dockwell join or dockwell recover
	SurfaceAutomatic = "automatic"
)

// SurfaceOf is where what an actor does on its own behalf comes from: a
// person's session from the panel, a shell account from the CLI, and
// Dockwell on its own, automatically. A writer that knows better says so:
// a server joins with dockwell join, from the CLI, and a call with an API
// token is from the CLI or the API as the call says, which
// auth.Client.Surface decides. Knowing only the actor, SurfaceOf says a
// token's is the API's.
func SurfaceOf(actor string) string {
	kind, _, _ := strings.Cut(actor, ":")
	switch kind {
	case "user", "anonymous":
		return SurfacePanel
	case "token":
		return SurfaceAPI
	case "cli":
		return SurfaceCLI
	}
	return SurfaceAutomatic
}

// RecordEvent appends to the audit log.
func (t *Tx) RecordEvent(ctx context.Context, e Event) error {
	_, err := t.exec(ctx, `INSERT INTO audit_events (id, at, actor, action, subject, detail, surface, target) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, millis(e.At), e.Actor, e.Action, e.Subject, e.Detail, e.Surface, e.Target)
	if err != nil {
		return fmt.Errorf("can't record audit event %s: %w", e.Action, err)
	}
	if t.recorded != nil {
		*t.recorded = true
	}
	return nil
}

// RecordEventOnce records e unless an event of its ID is there, as for one a server
// sends again, and says whether it recorded it.
func (t *Tx) RecordEventOnce(ctx context.Context, e Event) (bool, error) {
	res, err := t.exec(ctx, `INSERT INTO audit_events (id, at, actor, action, subject, detail, surface, target) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		e.ID, millis(e.At), e.Actor, e.Action, e.Subject, e.Detail, e.Surface, e.Target)
	if err != nil {
		return false, fmt.Errorf("can't record audit event %s: %w", e.Action, err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if t.recorded != nil {
		*t.recorded = true
	}
	return true, nil
}

// UpdateEvent rewrites an event that tells of something still going on, as a
// service that keeps changing: its time and what it says, so one event stands for
// all of it rather than one each few minutes. It changes nothing else, and only an
// event of e's action.
func (t *Tx) UpdateEvent(ctx context.Context, e Event) error {
	res, err := t.exec(ctx, `UPDATE audit_events SET at = ?, detail = ? WHERE id = ? AND action = ?`, millis(e.At), e.Detail, e.ID, e.Action)
	if err != nil {
		return fmt.Errorf("can't update audit event %s: %w", e.Action, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("can't update audit event %s: %w", e.Action, err)
	} else if n == 0 {
		return fmt.Errorf("can't update audit event %s %s: %w", e.Action, e.ID, ErrNotFound)
	}
	if t.recorded != nil {
		*t.recorded = true
	}
	return nil
}

// LatestEvent reads the newest audit event of an action, about a subject and a
// target, and whether there is one.
func (t *Tx) LatestEvent(ctx context.Context, action, subject, target string) (Event, bool, error) {
	row := t.queryRow(ctx, `SELECT `+eventColumns+` FROM audit_events WHERE action = ? AND subject = ? AND target = ? ORDER BY at DESC, id DESC LIMIT 1`, action, subject, target)
	e, err := scanEvent(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Event{}, false, nil
	case err != nil:
		return Event{}, false, fmt.Errorf("can't read an audit event: %w", err)
	}
	return e, true, nil
}

// GetEvent reads one audit event by its ID, or ErrNotFound.
func (t *Tx) GetEvent(ctx context.Context, id string) (Event, error) {
	e, err := scanEvent(t.queryRow(ctx, `SELECT `+eventColumns+` FROM audit_events WHERE id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Event{}, fmt.Errorf("audit event %s: %w", id, ErrNotFound)
	case err != nil:
		return Event{}, fmt.Errorf("can't read an audit event: %w", err)
	}
	return e, nil
}

const eventColumns = `id, at, actor, action, subject, detail, surface, target`

func scanEvent(row interface{ Scan(...any) error }) (Event, error) {
	var e Event
	var at int64
	err := row.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Subject, &e.Detail, &e.Surface, &e.Target)
	e.At = fromMillis(at)
	return e, err
}

// ListEvents returns the newest audit events first, at most limit of them.
func (t *Tx) ListEvents(ctx context.Context, limit int) ([]Event, error) {
	rows, err := t.query(ctx, `SELECT `+eventColumns+` FROM audit_events ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("can't list audit events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
