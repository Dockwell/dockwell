// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dockwell/dockwell/internal/id"
)

// Change states.
const (
	ChangePlanned  = "planned"
	ChangeApplying = "applying"
	ChangeApplied  = "applied"
	ChangeFailed   = "failed"
)

// Change is one action's worth of edits to managed resources.
type Change struct {
	ID          string
	Summary     string
	Actor       string
	State       string
	Reverts     string // the change this undoes or rolls back, if any
	Fingerprint string
	Plan        []byte
	Error       string
	CreatedAt   time.Time
	AppliedAt   time.Time // zero until applied
	Items       []ChangeItem
}

// ChangeItem is what a change asks of one resource.
type ChangeItem struct {
	Kind         string
	Server       string
	Name         string
	BaseRevision int    // 0 when the resource didn't exist
	Spec         []byte // nil deletes
}

// Resource is a managed resource with its latest revision.
type Resource struct {
	ID       string
	Kind     string
	Server   string
	Name     string
	Revision int    // the latest revision's number
	Spec     []byte // the latest spec; nil once deleted
	ChangeID string // the change that made the latest revision
}

// Revision is one immutable revision of a resource.
type Revision struct {
	Resource
	CreatedAt time.Time
}

// CreateChange records a planned change and its items.
func (t *Tx) CreateChange(ctx context.Context, c Change) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO changes (id, summary, actor, state, reverts, fingerprint, plan, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Summary, c.Actor, c.State, nullString(c.Reverts), c.Fingerprint, c.Plan, millis(c.CreatedAt))
	if err != nil {
		return fmt.Errorf("can't record change %s: %w", c.ID, err)
	}
	for i, it := range c.Items {
		if _, err := t.tx.ExecContext(ctx, `INSERT INTO change_items (change_id, position, kind, server_id, name, base_revision, spec)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, c.ID, i, it.Kind, it.Server, it.Name, it.BaseRevision, it.Spec); err != nil {
			return fmt.Errorf("can't record change %s: %w", c.ID, err)
		}
	}
	return nil
}

// GetChange returns a change with its items, or ErrNotFound.
func (t *Tx) GetChange(ctx context.Context, changeID string) (Change, error) {
	var c Change
	var reverts, errText sql.NullString
	var created int64
	var applied sql.NullInt64
	err := t.tx.QueryRowContext(ctx, `SELECT id, summary, actor, state, reverts, fingerprint, plan, error, created_at, applied_at
		FROM changes WHERE id = ?`, changeID).
		Scan(&c.ID, &c.Summary, &c.Actor, &c.State, &reverts, &c.Fingerprint, &c.Plan, &errText, &created, &applied)
	if errors.Is(err, sql.ErrNoRows) {
		return Change{}, ErrNotFound
	}
	if err != nil {
		return Change{}, fmt.Errorf("can't read change %s: %w", changeID, err)
	}
	c.Reverts, c.Error, c.CreatedAt = reverts.String, errText.String, fromMillis(created)
	if applied.Valid {
		c.AppliedAt = fromMillis(applied.Int64)
	}
	rows, err := t.tx.QueryContext(ctx, `SELECT kind, server_id, name, base_revision, spec
		FROM change_items WHERE change_id = ? ORDER BY position`, changeID)
	if err != nil {
		return Change{}, fmt.Errorf("can't read change %s: %w", changeID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var it ChangeItem
		if err := rows.Scan(&it.Kind, &it.Server, &it.Name, &it.BaseRevision, &it.Spec); err != nil {
			return Change{}, fmt.Errorf("can't read change %s: %w", changeID, err)
		}
		c.Items = append(c.Items, it)
	}
	return c, rows.Err()
}

// SetChangeState moves a change on. appliedAt is recorded when set.
func (t *Tx) SetChangeState(ctx context.Context, changeID, state, errText string, appliedAt time.Time) error {
	var applied sql.NullInt64
	if !appliedAt.IsZero() {
		applied = sql.NullInt64{Int64: millis(appliedAt), Valid: true}
	}
	res, err := t.tx.ExecContext(ctx, `UPDATE changes SET state = ?, error = ?, applied_at = COALESCE(?, applied_at) WHERE id = ?`,
		state, nullString(errText), applied, changeID)
	if err != nil {
		return fmt.Errorf("can't update change %s: %w", changeID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ChangesIn returns the IDs of changes in a state, oldest first.
func (t *Tx) ChangesIn(ctx context.Context, state string) ([]string, error) {
	rows, err := t.tx.QueryContext(ctx, `SELECT id FROM changes WHERE state = ? ORDER BY created_at, id`, state)
	if err != nil {
		return nil, fmt.Errorf("can't list changes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var changeID string
		if err := rows.Scan(&changeID); err != nil {
			return nil, fmt.Errorf("can't list changes: %w", err)
		}
		out = append(out, changeID)
	}
	return out, rows.Err()
}

const resourceColumns = `r.id, r.kind, r.server_id, r.name, r.revision, v.spec, v.change_id`

func scanResource(row interface{ Scan(...any) error }) (Resource, error) {
	var r Resource
	err := row.Scan(&r.ID, &r.Kind, &r.Server, &r.Name, &r.Revision, &r.Spec, &r.ChangeID)
	return r, err
}

// GetResource returns a resource with its latest revision, or ErrNotFound
// if it was never managed.
func (t *Tx) GetResource(ctx context.Context, kind, server, name string) (Resource, error) {
	r, err := scanResource(t.tx.QueryRowContext(ctx, `SELECT `+resourceColumns+`
		FROM resources r JOIN revisions v ON v.resource_id = r.id AND v.number = r.revision
		WHERE r.kind = ? AND r.server_id = ? AND r.name = ?`, kind, server, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Resource{}, ErrNotFound
	}
	if err != nil {
		return Resource{}, fmt.Errorf("can't read %s %s: %w", kind, name, err)
	}
	return r, nil
}

// ListResources returns the resources a server should have: every one
// whose latest revision isn't a deletion, by kind and name.
func (t *Tx) ListResources(ctx context.Context, server string) ([]Resource, error) {
	rows, err := t.tx.QueryContext(ctx, `SELECT `+resourceColumns+`
		FROM resources r JOIN revisions v ON v.resource_id = r.id AND v.number = r.revision
		WHERE r.server_id = ? AND v.spec IS NOT NULL ORDER BY r.kind, r.name`, server)
	if err != nil {
		return nil, fmt.Errorf("can't list resources: %w", err)
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, fmt.Errorf("can't list resources: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddRevision writes the next revision of a resource, creating the
// resource on its first.
func (t *Tx) AddRevision(ctx context.Context, kind, server, name string, spec []byte, changeID string, at time.Time) (Revision, error) {
	r, err := t.GetResource(ctx, kind, server, name)
	switch {
	case errors.Is(err, ErrNotFound):
		r = Resource{ID: id.New(id.Resource), Kind: kind, Server: server, Name: name}
		if _, err := t.tx.ExecContext(ctx, `INSERT INTO resources (id, kind, server_id, name, revision) VALUES (?, ?, ?, ?, 0)`,
			r.ID, kind, server, name); err != nil {
			return Revision{}, fmt.Errorf("can't record %s %s: %w", kind, name, err)
		}
	case err != nil:
		return Revision{}, err
	}
	r.Revision++
	r.Spec, r.ChangeID = spec, changeID
	if _, err := t.tx.ExecContext(ctx, `INSERT INTO revisions (resource_id, number, spec, change_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		r.ID, r.Revision, spec, changeID, millis(at)); err != nil {
		return Revision{}, fmt.Errorf("can't record a revision of %s %s: %w", kind, name, err)
	}
	if _, err := t.tx.ExecContext(ctx, `UPDATE resources SET revision = ? WHERE id = ?`, r.Revision, r.ID); err != nil {
		return Revision{}, fmt.Errorf("can't record a revision of %s %s: %w", kind, name, err)
	}
	return Revision{Resource: r, CreatedAt: at}, nil
}

// GetRevision returns one revision of a resource, or ErrNotFound.
func (t *Tx) GetRevision(ctx context.Context, resourceID string, number int) (Revision, error) {
	var rev Revision
	var created int64
	err := t.tx.QueryRowContext(ctx, `SELECT r.id, r.kind, r.server_id, r.name, v.number, v.spec, v.change_id, v.created_at
		FROM revisions v JOIN resources r ON r.id = v.resource_id WHERE v.resource_id = ? AND v.number = ?`, resourceID, number).
		Scan(&rev.ID, &rev.Kind, &rev.Server, &rev.Name, &rev.Revision, &rev.Spec, &rev.ChangeID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	if err != nil {
		return Revision{}, fmt.Errorf("can't read revision %d of %s: %w", number, resourceID, err)
	}
	rev.CreatedAt = fromMillis(created)
	return rev, nil
}

// RevisionsOf returns the revisions a change made, in the order it made
// them.
func (t *Tx) RevisionsOf(ctx context.Context, changeID string) ([]Revision, error) {
	rows, err := t.tx.QueryContext(ctx, `SELECT r.id, r.kind, r.server_id, r.name, v.number, v.spec, v.change_id, v.created_at
		FROM revisions v JOIN resources r ON r.id = v.resource_id WHERE v.change_id = ? ORDER BY v.rowid`, changeID)
	if err != nil {
		return nil, fmt.Errorf("can't read the revisions of %s: %w", changeID, err)
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		var rev Revision
		var created int64
		if err := rows.Scan(&rev.ID, &rev.Kind, &rev.Server, &rev.Name, &rev.Revision, &rev.Spec, &rev.ChangeID, &created); err != nil {
			return nil, fmt.Errorf("can't read the revisions of %s: %w", changeID, err)
		}
		rev.CreatedAt = fromMillis(created)
		out = append(out, rev)
	}
	return out, rows.Err()
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
