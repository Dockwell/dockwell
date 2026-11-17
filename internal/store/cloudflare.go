// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// What Dockwell wrote at Cloudflare for routes, or took on there (ADR 0104). A step
// records each write in its own transaction, so a control plane stopped midway finds
// what it wrote on its next try.

// What a published thing was found to be, as last read.
const (
	CloudflarePublished = "published"
	CloudflareGone      = "gone"
	CloudflareChanged   = "changed"
	// CloudflareTunnelDeleted is a rule whose tunnel was deleted in Cloudflare.
	CloudflareTunnelDeleted = "tunnel_deleted"
	// CloudflareUnreadable is what Dockwell can't read: its provider isn't connected
	// any more, or Cloudflare refuses the token.
	CloudflareUnreadable = "unreadable"
)

// CloudflareRecord is a DNS record Dockwell wrote or took on, as it wrote it.
type CloudflareRecord struct {
	ID, Provider, ZoneID, Zone, Name, Type, Content string
	Proxied                                         bool
	TTL                                             int
	Comment, RouteID, ChangeID                      string
	// TakenOnAt is when a record made by hand was taken on; nil for one written.
	TakenOnAt *time.Time
	// Replaced is the record a write replaced, whole, as JSON, for undo to write
	// back; empty when it replaced none.
	Replaced  string
	State     string
	CheckedAt *time.Time
	CreatedAt time.Time
}

const cloudflareRecordColumns = `id, provider, zone_id, zone, name, type, content, proxied, ttl, comment, route_id, change_id, taken_on_at, replaced, state, checked_at, created_at`

func scanCloudflareRecord(row interface{ Scan(...any) error }) (CloudflareRecord, error) {
	var r CloudflareRecord
	var proxied int
	var taken, checked sql.NullInt64
	var created int64
	if err := row.Scan(&r.ID, &r.Provider, &r.ZoneID, &r.Zone, &r.Name, &r.Type, &r.Content, &proxied, &r.TTL, &r.Comment, &r.RouteID, &r.ChangeID, &taken, &r.Replaced, &r.State, &checked, &created); err != nil {
		return CloudflareRecord{}, err
	}
	r.Proxied, r.TakenOnAt, r.CheckedAt, r.CreatedAt = proxied != 0, timePtr(taken), timePtr(checked), fromMillis(created)
	return r, nil
}

// PutCloudflareRecord records r, or what's recorded of its ID again: a step run again
// after a crash writes the same row.
func (t *Tx) PutCloudflareRecord(ctx context.Context, r CloudflareRecord) error {
	if r.State == "" {
		r.State = CloudflarePublished
	}
	_, err := t.exec(ctx, `INSERT INTO cloudflare_records (`+cloudflareRecordColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET provider = excluded.provider, zone_id = excluded.zone_id, zone = excluded.zone, name = excluded.name,
		type = excluded.type, content = excluded.content, proxied = excluded.proxied, ttl = excluded.ttl, comment = excluded.comment,
		route_id = excluded.route_id, change_id = excluded.change_id, taken_on_at = excluded.taken_on_at, replaced = excluded.replaced,
		state = excluded.state, checked_at = excluded.checked_at`,
		r.ID, r.Provider, r.ZoneID, r.Zone, strings.ToLower(r.Name), r.Type, r.Content, boolInt(r.Proxied), r.TTL, r.Comment, r.RouteID, r.ChangeID,
		nullMillis(r.TakenOnAt), r.Replaced, r.State, nullMillis(r.CheckedAt), millis(r.CreatedAt))
	if t.unique(err) {
		return fmt.Errorf("a %s record at %s is kept already under another ID, so this one can't be kept beside it, and the change stops here. Plan it again, and if it stops the same way, report it: %w", r.Type, r.Name, ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("can't record the DNS record at %s: %w", r.Name, err)
	}
	return nil
}

// CloudflarePendingType is the type of a row that keeps a record made by hand,
// whole, before a replace deletes it: no record at Cloudflare has its ID, and no
// listing of records holds it.
const CloudflarePendingType = "pending"

// CloudflareRecordsOf lists the records written or taken on for a route.
func (t *Tx) CloudflareRecordsOf(ctx context.Context, routeID string) ([]CloudflareRecord, error) {
	return t.cloudflareRecords(ctx, `WHERE route_id = ? AND type <> 'pending' ORDER BY name, type`, routeID)
}

// CloudflareRecordsIn lists the records Dockwell holds in a zone.
func (t *Tx) CloudflareRecordsIn(ctx context.Context, zoneID string) ([]CloudflareRecord, error) {
	return t.cloudflareRecords(ctx, `WHERE zone_id = ? AND type <> 'pending' ORDER BY name, type`, zoneID)
}

// CloudflareRecordsAll lists every record Dockwell holds, for the drift timer.
func (t *Tx) CloudflareRecordsAll(ctx context.Context) ([]CloudflareRecord, error) {
	return t.cloudflareRecords(ctx, `WHERE type <> 'pending' ORDER BY zone_id, name, type`)
}

// CloudflarePendingOf lists what a route's replaces kept of records made by hand,
// before their deletes, whose writes haven't landed yet.
func (t *Tx) CloudflarePendingOf(ctx context.Context, routeID string) ([]CloudflareRecord, error) {
	return t.cloudflareRecords(ctx, `WHERE route_id = ? AND type = 'pending' ORDER BY name`, routeID)
}

// CloudflareRecordByID returns the row of a record by its ID at Cloudflare, or
// ErrNotFound.
func (t *Tx) CloudflareRecordByID(ctx context.Context, id string) (CloudflareRecord, error) {
	r, err := scanCloudflareRecord(t.queryRow(ctx, `SELECT `+cloudflareRecordColumns+` FROM cloudflare_records WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return CloudflareRecord{}, fmt.Errorf("the record %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return CloudflareRecord{}, fmt.Errorf("can't read the record %s: %w", id, err)
	}
	return r, nil
}

// CloudflareRecordAt returns the record Dockwell holds of a type at a name, or
// ErrNotFound.
func (t *Tx) CloudflareRecordAt(ctx context.Context, zoneID, name, typ string) (CloudflareRecord, error) {
	r, err := scanCloudflareRecord(t.queryRow(ctx, `SELECT `+cloudflareRecordColumns+` FROM cloudflare_records WHERE zone_id = ? AND name = ? AND type = ?`, zoneID, strings.ToLower(name), typ))
	if errors.Is(err, sql.ErrNoRows) {
		return CloudflareRecord{}, fmt.Errorf("a record of %s at %s: %w", typ, name, ErrNotFound)
	}
	if err != nil {
		return CloudflareRecord{}, fmt.Errorf("can't read the record at %s: %w", name, err)
	}
	return r, nil
}

func (t *Tx) cloudflareRecords(ctx context.Context, where string, args ...any) ([]CloudflareRecord, error) {
	rows, err := t.query(ctx, `SELECT `+cloudflareRecordColumns+` FROM cloudflare_records `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("can't list DNS records written at Cloudflare: %w", err)
	}
	defer rows.Close()
	var out []CloudflareRecord
	for rows.Next() {
		r, err := scanCloudflareRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read a DNS record written at Cloudflare: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteCloudflareRecord forgets a record, once it's deleted at Cloudflare or let go.
func (t *Tx) DeleteCloudflareRecord(ctx context.Context, id string) error {
	if _, err := t.exec(ctx, `DELETE FROM cloudflare_records WHERE id = ?`, id); err != nil {
		return fmt.Errorf("can't forget the DNS record %s: %w", id, err)
	}
	return nil
}

// MarkCloudflareRecord says what the drift timer found of a record.
func (t *Tx) MarkCloudflareRecord(ctx context.Context, id, state string, at time.Time) error {
	if _, err := t.exec(ctx, `UPDATE cloudflare_records SET state = ?, checked_at = ? WHERE id = ?`, state, millis(at), id); err != nil {
		return fmt.Errorf("can't mark the DNS record %s: %w", id, err)
	}
	return nil
}

// CloudflareRule is a tunnel's rule Dockwell wrote or took on.
type CloudflareRule struct {
	TunnelID, Hostname, Path, Account, Provider, Zone, Service string
	// ReadDoc and WrittenDoc are the tunnel's document as read before the write and
	// as written, for the record (ADR 0104, section 4).
	ReadDoc, WrittenDoc string
	Version             int
	ChangeID            string
	TakenOnAt           *time.Time
	// Replaced is the rule as it was before Move it to Dockwell, as JSON.
	Replaced  string
	State     string
	CheckedAt *time.Time
	CreatedAt time.Time
	// ServerID is the server whose proxy the rule sends to: empty for one written
	// before it was kept.
	ServerID string
}

const cloudflareRuleColumns = `tunnel_id, hostname, path, account, provider, zone, service, read_doc, written_doc, version, change_id, taken_on_at, replaced, state, checked_at, created_at, server_id`

func scanCloudflareRule(row interface{ Scan(...any) error }) (CloudflareRule, error) {
	var r CloudflareRule
	var taken, checked sql.NullInt64
	var created int64
	if err := row.Scan(&r.TunnelID, &r.Hostname, &r.Path, &r.Account, &r.Provider, &r.Zone, &r.Service, &r.ReadDoc, &r.WrittenDoc, &r.Version, &r.ChangeID, &taken, &r.Replaced, &r.State, &checked, &created, &r.ServerID); err != nil {
		return CloudflareRule{}, err
	}
	r.TakenOnAt, r.CheckedAt, r.CreatedAt = timePtr(taken), timePtr(checked), fromMillis(created)
	return r, nil
}

// PutCloudflareRule records r, or what's recorded of its tunnel, hostname and path
// again.
func (t *Tx) PutCloudflareRule(ctx context.Context, r CloudflareRule) error {
	if r.State == "" {
		r.State = CloudflarePublished
	}
	_, err := t.exec(ctx, `INSERT INTO cloudflare_rules (`+cloudflareRuleColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tunnel_id, hostname, path) DO UPDATE SET account = excluded.account, provider = excluded.provider, zone = excluded.zone,
		service = excluded.service, read_doc = excluded.read_doc, written_doc = excluded.written_doc, version = excluded.version,
		change_id = excluded.change_id, taken_on_at = excluded.taken_on_at, replaced = excluded.replaced, state = excluded.state,
		checked_at = excluded.checked_at, server_id = excluded.server_id`,
		r.TunnelID, strings.ToLower(r.Hostname), r.Path, r.Account, r.Provider, r.Zone, r.Service, r.ReadDoc, r.WrittenDoc, r.Version,
		r.ChangeID, nullMillis(r.TakenOnAt), r.Replaced, r.State, nullMillis(r.CheckedAt), millis(r.CreatedAt), r.ServerID)
	if err != nil {
		return fmt.Errorf("can't record the rule %s in the tunnel: %w", r.Hostname, err)
	}
	return nil
}

// CloudflareRulesOf lists the rules Dockwell holds in a tunnel.
func (t *Tx) CloudflareRulesOf(ctx context.Context, tunnelID string) ([]CloudflareRule, error) {
	return t.cloudflareRules(ctx, `WHERE tunnel_id = ? ORDER BY hostname, path`, tunnelID)
}

// CloudflareRulesAll lists every rule Dockwell holds, for the drift timer.
func (t *Tx) CloudflareRulesAll(ctx context.Context) ([]CloudflareRule, error) {
	return t.cloudflareRules(ctx, `ORDER BY tunnel_id, hostname, path`)
}

// CloudflareRuleAt returns the rule Dockwell holds in a tunnel for hostname and
// path, or ErrNotFound.
func (t *Tx) CloudflareRuleAt(ctx context.Context, tunnelID, hostname, path string) (CloudflareRule, error) {
	r, err := scanCloudflareRule(t.queryRow(ctx, `SELECT `+cloudflareRuleColumns+` FROM cloudflare_rules WHERE tunnel_id = ? AND hostname = ? AND path = ?`, tunnelID, strings.ToLower(hostname), path))
	if errors.Is(err, sql.ErrNoRows) {
		return CloudflareRule{}, fmt.Errorf("a rule %s in the tunnel: %w", hostname, ErrNotFound)
	}
	if err != nil {
		return CloudflareRule{}, fmt.Errorf("can't read the rule %s in the tunnel: %w", hostname, err)
	}
	return r, nil
}

func (t *Tx) cloudflareRules(ctx context.Context, where string, args ...any) ([]CloudflareRule, error) {
	rows, err := t.query(ctx, `SELECT `+cloudflareRuleColumns+` FROM cloudflare_rules `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("can't list tunnel rules written at Cloudflare: %w", err)
	}
	defer rows.Close()
	var out []CloudflareRule
	for rows.Next() {
		r, err := scanCloudflareRule(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read a tunnel rule written at Cloudflare: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteCloudflareRule forgets a rule, once it's out of the tunnel or let go.
func (t *Tx) DeleteCloudflareRule(ctx context.Context, tunnelID, hostname, path string) error {
	if _, err := t.exec(ctx, `DELETE FROM cloudflare_rules WHERE tunnel_id = ? AND hostname = ? AND path = ?`, tunnelID, strings.ToLower(hostname), path); err != nil {
		return fmt.Errorf("can't forget the rule %s: %w", hostname, err)
	}
	return nil
}

// MarkCloudflareRule says what the drift timer found of a rule.
func (t *Tx) MarkCloudflareRule(ctx context.Context, tunnelID, hostname, path, state string, at time.Time) error {
	if _, err := t.exec(ctx, `UPDATE cloudflare_rules SET state = ?, checked_at = ? WHERE tunnel_id = ? AND hostname = ? AND path = ?`,
		state, millis(at), tunnelID, strings.ToLower(hostname), path); err != nil {
		return fmt.Errorf("can't mark the rule %s: %w", hostname, err)
	}
	return nil
}

// CloudflareZoneMode is a zone Dockwell set to Full (strict) for an admin, with the
// mode it had before.
type CloudflareZoneMode struct {
	ZoneID, Provider, Zone, Was, ChangeID string
	SetAt                                 time.Time
}

// PutCloudflareZoneMode records that Dockwell sets a zone's mode, and what it was, as
// a step does before it writes, so a crash between the two loses nothing.
func (t *Tx) PutCloudflareZoneMode(ctx context.Context, m CloudflareZoneMode) error {
	_, err := t.exec(ctx, `INSERT INTO cloudflare_zone_modes (zone_id, provider, zone, was, change_id, set_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (zone_id) DO UPDATE SET provider = excluded.provider, zone = excluded.zone, was = excluded.was, change_id = excluded.change_id, set_at = excluded.set_at`,
		m.ZoneID, m.Provider, m.Zone, m.Was, m.ChangeID, millis(m.SetAt))
	if err != nil {
		return fmt.Errorf("can't record %s's mode: %w", m.Zone, err)
	}
	return nil
}

// CloudflareZoneModeOf returns what's recorded of a zone's mode, or ErrNotFound.
func (t *Tx) CloudflareZoneModeOf(ctx context.Context, zoneID string) (CloudflareZoneMode, error) {
	var m CloudflareZoneMode
	var at int64
	err := t.queryRow(ctx, `SELECT zone_id, provider, zone, was, change_id, set_at FROM cloudflare_zone_modes WHERE zone_id = ?`, zoneID).
		Scan(&m.ZoneID, &m.Provider, &m.Zone, &m.Was, &m.ChangeID, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return CloudflareZoneMode{}, fmt.Errorf("the zone %s's mode: %w", zoneID, ErrNotFound)
	}
	if err != nil {
		return CloudflareZoneMode{}, fmt.Errorf("can't read the zone %s's mode: %w", zoneID, err)
	}
	m.SetAt = fromMillis(at)
	return m, nil
}

// DeleteCloudflareZoneMode forgets a zone's mode, once it's put back or let go.
func (t *Tx) DeleteCloudflareZoneMode(ctx context.Context, zoneID string) error {
	if _, err := t.exec(ctx, `DELETE FROM cloudflare_zone_modes WHERE zone_id = ?`, zoneID); err != nil {
		return fmt.Errorf("can't forget the zone %s's mode: %w", zoneID, err)
	}
	return nil
}

// CloudflareTunnel is a tunnel Dockwell writes to: the version of its document it
// last read.
type CloudflareTunnel struct {
	TunnelID, Account string
	SeenVersion       int
	CheckedAt         *time.Time
}

// SawTunnel records the version of a tunnel's document last read.
func (t *Tx) SawTunnel(ctx context.Context, tunnelID, account string, version int, at time.Time) error {
	_, err := t.exec(ctx, `INSERT INTO cloudflare_tunnels (tunnel_id, account, seen_version, checked_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (tunnel_id) DO UPDATE SET account = excluded.account, seen_version = excluded.seen_version, checked_at = excluded.checked_at`,
		tunnelID, account, version, millis(at))
	if err != nil {
		return fmt.Errorf("can't record the tunnel's version: %w", err)
	}
	return nil
}

// CloudflareTunnelOf returns what's recorded of a tunnel, or ErrNotFound.
func (t *Tx) CloudflareTunnelOf(ctx context.Context, tunnelID string) (CloudflareTunnel, error) {
	var c CloudflareTunnel
	var checked sql.NullInt64
	err := t.queryRow(ctx, `SELECT tunnel_id, account, seen_version, checked_at FROM cloudflare_tunnels WHERE tunnel_id = ?`, tunnelID).
		Scan(&c.TunnelID, &c.Account, &c.SeenVersion, &checked)
	if errors.Is(err, sql.ErrNoRows) {
		return CloudflareTunnel{}, fmt.Errorf("the tunnel %s: %w", tunnelID, ErrNotFound)
	}
	if err != nil {
		return CloudflareTunnel{}, fmt.Errorf("can't read the tunnel %s: %w", tunnelID, err)
	}
	c.CheckedAt = timePtr(checked)
	return c, nil
}

// LockTunnel makes holder the one writer of a tunnel until until, and says whether it
// is: a lock another holds, and hasn't run out by now, stays theirs. A holder that
// has it already takes it again, its lease longer.
func (t *Tx) LockTunnel(ctx context.Context, tunnelID, holder string, now, until time.Time) (bool, error) {
	if holder == "" {
		return false, errors.New("a tunnel's lock needs a holder")
	}
	_, err := t.exec(ctx, `INSERT INTO cloudflare_tunnels (tunnel_id, lock_holder, lock_until) VALUES (?, ?, ?)
		ON CONFLICT (tunnel_id) DO UPDATE SET lock_holder = excluded.lock_holder, lock_until = excluded.lock_until
		WHERE cloudflare_tunnels.lock_holder = '' OR cloudflare_tunnels.lock_holder = excluded.lock_holder OR cloudflare_tunnels.lock_until < ?`,
		tunnelID, holder, millis(until), millis(now))
	if err != nil {
		return false, fmt.Errorf("can't lock the tunnel %s: %w", tunnelID, err)
	}
	var got string
	if err := t.queryRow(ctx, `SELECT lock_holder FROM cloudflare_tunnels WHERE tunnel_id = ?`, tunnelID).Scan(&got); err != nil {
		return false, fmt.Errorf("can't read the tunnel %s's lock: %w", tunnelID, err)
	}
	return got == holder, nil
}

// UnlockTunnel lets a tunnel go, if holder holds it.
func (t *Tx) UnlockTunnel(ctx context.Context, tunnelID, holder string) error {
	if _, err := t.exec(ctx, `UPDATE cloudflare_tunnels SET lock_holder = '', lock_until = 0 WHERE tunnel_id = ? AND lock_holder = ?`, tunnelID, holder); err != nil {
		return fmt.Errorf("can't unlock the tunnel %s: %w", tunnelID, err)
	}
	return nil
}
