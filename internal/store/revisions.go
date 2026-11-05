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

// The revision store: the organization, every change, the resources it
// touched, each immutable revision of their specs, and where each revision
// is deployed. Anyone can read it; only a RevisionTx writes it, and only
// internal/revision opens one. See docs/adr/0009-resources-revisions-and-plans.md
// and docs/adr/0014-object-model-and-store.md.

// Organization is the identity, billing and policy boundary. An install
// has one, made on its first start.
type Organization struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// Organization returns the install's organization, or ErrNotFound before
// its first start has made it.
func (t *Tx) Organization(ctx context.Context) (Organization, error) {
	var o Organization
	var created int64
	err := t.queryRow(ctx, `SELECT id, name, created_at FROM organizations ORDER BY created_at, id LIMIT 1`).
		Scan(&o.ID, &o.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Organization{}, fmt.Errorf("organization: %w", ErrNotFound)
	}
	if err != nil {
		return Organization{}, fmt.Errorf("can't read the organization: %w", err)
	}
	o.CreatedAt = fromMillis(created)
	return o, nil
}

// CreateOrganization records the install's organization.
func (t *RevisionTx) CreateOrganization(ctx context.Context, o Organization) error {
	if _, err := t.exec(ctx, `INSERT INTO organizations (id, name, created_at) VALUES (?, ?, ?)`,
		o.ID, o.Name, millis(o.CreatedAt)); err != nil {
		return fmt.Errorf("can't record the organization: %w", err)
	}
	return nil
}

// Change states.
const (
	ChangePlanned   = "planned"
	ChangeApplying  = "applying"
	ChangeApplied   = "applied"
	ChangeFailed    = "failed"
	ChangeDiscarded = "discarded"
)

// Change is one action's worth of edits to managed resources.
type Change struct {
	ID          string
	Summary     string
	Actor       string
	State       string
	Reverts     string // the change this undoes or rolls back, if any
	Fingerprint string
	// FingerprintScheme says how Fingerprint was made, as the revision
	// store numbers its schemes. A change recorded before schemes were
	// recorded has 1.
	FingerprintScheme int
	Plan              []byte
	Error             string
	CreatedAt         time.Time
	AppliedAt         time.Time // zero until applied
	// ProposedVia is the surface it was proposed from, such as "panel".
	ProposedVia string
	// AppliedBy and AppliedVia are who applied it and from where; empty
	// until it is applied.
	AppliedBy, AppliedVia string
	// Progress is each step of an applying change and how it went, as the
	// revision store writes it; nil until it applies.
	Progress []byte
	Items    []ChangeItem
	// Servers are the servers its plan touches, removed ones included, by
	// ID. CreateChange records them; reads leave them out, and
	// ChangeServers has them.
	Servers []string
}

// ChangeItem is what a change asks of one resource.
type ChangeItem struct {
	// ResourceID names the resource, and is given to a new one when the
	// change is planned, so another item can live in it.
	ResourceID   string
	Kind         string
	Scope        string // the ID of what it lives in
	Name         string
	BaseRevision int    // 0 when the resource didn't exist
	Spec         []byte // nil deletes
	// Servers is where the new revision is placed, by ID: its server for a
	// host-level kind, and none for a deletion or a kind that runs on no
	// server.
	Servers []string
	// Rotates is true when the edit sets a secret's value, which plans as
	// changed whatever it holds (ADR 0022).
	Rotates bool
}

// Resource is a managed resource with its latest revision.
type Resource struct {
	ID       string
	Kind     string
	Scope    string // the ID of the organization, project, environment or server it lives in
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

// Deployment states. A deployment is pending until its server has it, then
// live or failed. A live or pending one is superseded once a later revision
// of its resource, or a deletion, takes its place on the server.
const (
	DeploymentPending    = "pending"
	DeploymentLive       = "live"
	DeploymentFailed     = "failed"
	DeploymentSuperseded = "superseded"
)

// Deployment is a revision placed on a server, with its rollout state.
type Deployment struct {
	ResourceID string
	Revision   int
	Server     string
	State      string
	Error      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// CreateChange records a planned change and its items.
func (t *RevisionTx) CreateChange(ctx context.Context, c Change) error {
	scheme := c.FingerprintScheme
	if scheme == 0 {
		scheme = 1
	}
	_, err := t.exec(ctx, `INSERT INTO changes (id, summary, actor, state, reverts, fingerprint, fingerprint_scheme, plan, created_at, proposed_via)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Summary, c.Actor, c.State, nullString(c.Reverts), c.Fingerprint, scheme, c.Plan, millis(c.CreatedAt), c.ProposedVia)
	if err != nil {
		return fmt.Errorf("can't record change %s: %w", c.ID, err)
	}
	for i := range c.Items {
		it := &c.Items[i]
		if _, err := t.exec(ctx, `INSERT INTO change_items (change_id, position, resource_id, kind, scope, name, base_revision, spec, servers, rotates)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.ID, i, it.ResourceID, it.Kind, it.Scope, it.Name, it.BaseRevision, it.Spec,
			strings.Join(it.Servers, " "), boolInt(it.Rotates)); err != nil {
			return fmt.Errorf("can't record change %s: %w", c.ID, err)
		}
	}
	for _, server := range c.Servers {
		if _, err := t.exec(ctx, `INSERT INTO change_servers (change_id, server_id) VALUES (?, ?)`, c.ID, server); err != nil {
			return fmt.Errorf("can't record change %s: %w", c.ID, err)
		}
	}
	return nil
}

const changeColumns = `id, summary, actor, state, reverts, fingerprint, fingerprint_scheme, plan, error, created_at, applied_at,
	proposed_via, applied_by, applied_via, progress`

func scanChange(row interface{ Scan(...any) error }) (Change, error) {
	var c Change
	var reverts, errText, appliedBy, appliedVia sql.NullString
	var created int64
	var applied sql.NullInt64
	err := row.Scan(&c.ID, &c.Summary, &c.Actor, &c.State, &reverts, &c.Fingerprint, &c.FingerprintScheme, &c.Plan, &errText, &created, &applied,
		&c.ProposedVia, &appliedBy, &appliedVia, &c.Progress)
	c.Reverts, c.Error, c.CreatedAt = reverts.String, errText.String, fromMillis(created)
	c.AppliedBy, c.AppliedVia = appliedBy.String, appliedVia.String
	if applied.Valid {
		c.AppliedAt = fromMillis(applied.Int64)
	}
	return c, err
}

// GetChange returns a change with its items, or ErrNotFound.
func (t *Tx) GetChange(ctx context.Context, changeID string) (Change, error) {
	c, err := scanChange(t.queryRow(ctx, `SELECT `+changeColumns+` FROM changes WHERE id = ?`, changeID))
	if errors.Is(err, sql.ErrNoRows) {
		return Change{}, ErrNotFound
	}
	if err != nil {
		return Change{}, fmt.Errorf("can't read change %s: %w", changeID, err)
	}
	rows, err := t.query(ctx, `SELECT resource_id, kind, scope, name, base_revision, spec, spec IS NULL, servers, rotates
		FROM change_items WHERE change_id = ? ORDER BY position`, changeID)
	if err != nil {
		return Change{}, fmt.Errorf("can't read change %s: %w", changeID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var it ChangeItem
		var servers string
		var deleted bool
		var rotates int64
		if err := rows.Scan(&it.ResourceID, &it.Kind, &it.Scope, &it.Name, &it.BaseRevision, &it.Spec, &deleted, &servers, &rotates); err != nil {
			return Change{}, fmt.Errorf("can't read change %s: %w", changeID, err)
		}
		it.Rotates = rotates != 0
		it.Spec = spec(it.Spec, deleted)
		if servers != "" {
			it.Servers = strings.Fields(servers)
		}
		c.Items = append(c.Items, it)
	}
	return c, rows.Err()
}

// ChangePage names where a page of changes starts: after the change with
// this creation time and ID, going back in time. The zero value starts with
// the newest.
type ChangePage struct {
	CreatedAt time.Time
	ID        string
}

// ListChanges returns at most limit changes, newest first, without their
// items, from where page says.
func (t *Tx) ListChanges(ctx context.Context, page ChangePage, limit int) ([]Change, error) {
	query := `SELECT ` + changeColumns + ` FROM changes`
	args := []any{}
	if page.ID != "" {
		query += ` WHERE created_at < ? OR created_at = ? AND id < ?`
		at := millis(page.CreatedAt)
		args = append(args, at, at, page.ID)
	}
	rows, err := t.query(ctx, query+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("can't list changes: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, fmt.Errorf("can't list changes: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Reverting returns the changes that undo or roll back any of changeIDs,
// oldest first, in one query. Each has only its ID, actor, state and what
// it reverts.
func (t *Tx) Reverting(ctx context.Context, changeIDs []string) ([]Change, error) {
	if len(changeIDs) == 0 {
		return nil, nil
	}
	args := make([]any, len(changeIDs))
	for i, id := range changeIDs {
		args[i] = id
	}
	marks := strings.Repeat("?, ", len(changeIDs)-1) + "?"
	rows, err := t.query(ctx, `SELECT id, actor, state, reverts FROM changes WHERE reverts IN (`+marks+`) ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("can't read the changes that revert others: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.ID, &c.Actor, &c.State, &c.Reverts); err != nil {
			return nil, fmt.Errorf("can't read the changes that revert others: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// StartApplying moves a planned change to applying, recording who applied
// it, from where, and when, with its steps' progress. It fails with
// ErrNotFound unless the change is still planned.
func (t *RevisionTx) StartApplying(ctx context.Context, changeID, by, via string, progress []byte, at time.Time) error {
	res, err := t.exec(ctx, `UPDATE changes SET state = ?, applied_at = ?, applied_by = ?, applied_via = ?, progress = ?
		WHERE id = ? AND state = ?`, ChangeApplying, millis(at), by, via, progress, changeID, ChangePlanned)
	if err != nil {
		return fmt.Errorf("can't start applying change %s: %w", changeID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("planned change %s: %w", changeID, ErrNotFound)
	}
	return nil
}

// SetChangeProgress records how an applying change's steps are going.
func (t *RevisionTx) SetChangeProgress(ctx context.Context, changeID string, progress []byte) error {
	if _, err := t.exec(ctx, `UPDATE changes SET progress = ? WHERE id = ?`, progress, changeID); err != nil {
		return fmt.Errorf("can't record the progress of change %s: %w", changeID, err)
	}
	return nil
}

// SetChangeState moves a change on. appliedAt is recorded when set.
func (t *RevisionTx) SetChangeState(ctx context.Context, changeID, state, errText string, appliedAt time.Time) error {
	var applied sql.NullInt64
	if !appliedAt.IsZero() {
		applied = sql.NullInt64{Int64: millis(appliedAt), Valid: true}
	}
	res, err := t.exec(ctx, `UPDATE changes SET state = ?, error = ?, applied_at = COALESCE(?, applied_at) WHERE id = ?`,
		state, nullString(errText), applied, changeID)
	if err != nil {
		return fmt.Errorf("can't update change %s: %w", changeID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// changesIn lists the changes in a state, oldest first. The index
// changes_by_state answers it, as Resume runs it on every start, every
// connect and every wakeup.
const changesIn = `SELECT id FROM changes WHERE state = ? ORDER BY created_at, id`

// ChangesIn returns the IDs of changes in a state, oldest first.
func (t *Tx) ChangesIn(ctx context.Context, state string) ([]string, error) {
	rows, err := t.query(ctx, changesIn, state)
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

const resourceColumns = `r.id, r.kind, r.scope, r.name, r.revision, v.spec, v.spec IS NULL, v.change_id`

const latest = ` FROM resources r JOIN revisions v ON v.resource_id = r.id AND v.number = r.revision`

func scanResource(row interface{ Scan(...any) error }) (Resource, error) {
	var r Resource
	var deleted bool
	err := row.Scan(&r.ID, &r.Kind, &r.Scope, &r.Name, &r.Revision, &r.Spec, &deleted, &r.ChangeID)
	r.Spec = spec(r.Spec, deleted)
	return r, err
}

// spec returns a stored spec: nil only for a deletion. An empty message
// encodes to no bytes, which a driver may read back as nil.
func spec(b []byte, deleted bool) []byte {
	switch {
	case deleted:
		return nil
	case b == nil:
		return []byte{}
	}
	return b
}

func (t *Tx) getResource(ctx context.Context, what, where string, args ...any) (Resource, error) {
	r, err := scanResource(t.queryRow(ctx, `SELECT `+resourceColumns+latest+` WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Resource{}, ErrNotFound
	}
	if err != nil {
		return Resource{}, fmt.Errorf("can't read %s: %w", what, err)
	}
	return r, nil
}

// GetResource returns a resource with its latest revision, or ErrNotFound
// if it was never managed.
func (t *Tx) GetResource(ctx context.Context, kind, scope, name string) (Resource, error) {
	return t.getResource(ctx, kind+" "+name, `r.kind = ? AND r.scope = ? AND r.name = ?`, kind, scope, name)
}

// GetResourceByID returns a resource with its latest revision, or
// ErrNotFound.
func (t *Tx) GetResourceByID(ctx context.Context, id string) (Resource, error) {
	return t.getResource(ctx, id, `r.id = ?`, id)
}

func (t *Tx) listResources(ctx context.Context, query string, args ...any) ([]Resource, error) {
	rows, err := t.query(ctx, query, args...)
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

// ListResources returns the resources that live in a scope, of one kind or
// of every kind when kind is empty, by kind and name. Deleted ones are
// left out.
func (t *Tx) ListResources(ctx context.Context, scope, kind string) ([]Resource, error) {
	return t.listResources(ctx, `SELECT `+resourceColumns+latest+`
		WHERE r.scope = ? AND (r.kind = ? OR ? = '') AND v.spec IS NOT NULL ORDER BY r.kind, r.name`, scope, kind, kind)
}

// ListPlaced returns the resources a server should have: every one whose
// latest revision is deployed there, by kind, scope and name.
func (t *Tx) ListPlaced(ctx context.Context, server string) ([]Resource, error) {
	return t.listResources(ctx, `SELECT `+resourceColumns+latest+`
		JOIN deployments d ON d.resource_id = r.id AND d.revision = r.revision
		WHERE d.server_id = ? AND v.spec IS NOT NULL ORDER BY r.kind, r.scope, r.name`, server)
}

// SpecAt says where a stored spec lives: in a revision of a resource, by
// its number, or in an item of a change, by its position. The secrets
// keeper binds a sealed spec to it, so one copied anywhere else, even to
// another revision of the same resource, doesn't open (ADR 0022).
type SpecAt struct {
	ResourceID string
	Kind       string
	// Revision is the revision's number, for a spec in a revision.
	Revision int
	// Change and Position name the change and the item's place in it, for
	// a spec a change asks for.
	Change   string
	Position int
}

// InRevision is where a revision keeps its spec.
func InRevision(resourceID, kind string, number int) SpecAt {
	return SpecAt{ResourceID: resourceID, Kind: kind, Revision: number}
}

// InChange is where a change keeps the spec its item at position asks
// for.
func InChange(resourceID, kind, changeID string, position int) SpecAt {
	return SpecAt{ResourceID: resourceID, Kind: kind, Change: changeID, Position: position}
}

// NewRevision is the next revision of a resource.
type NewRevision struct {
	Kind, Scope, Name string
	// ID names the resource if this is its first revision.
	ID string
	// Number is the number the revision must get, which a sealed spec is
	// bound to; 0 takes the next, whatever it is.
	Number   int
	Spec     []byte // nil deletes
	Change   string
	Position int // of the change item asking for it
	At       time.Time
	// Uses is what the spec uses of others in its scope, by reference,
	// kept in the clear beside it.
	Uses []Use
}

// Use is one resource that a revision uses in its scope, such as a secret
// a service uses, by reference.
type Use struct {
	As       string // the name the user sees it as, such as DATABASE_URL
	Kind     string
	Name     string
	Revision int // the revision it is pinned to; 0 when it isn't
}

// AddRevision writes the next revision of a resource, creating the
// resource on its first.
func (t *RevisionTx) AddRevision(ctx context.Context, n NewRevision) (Revision, error) {
	r, err := t.GetResource(ctx, n.Kind, n.Scope, n.Name)
	switch {
	case errors.Is(err, ErrNotFound):
		r = Resource{ID: n.ID, Kind: n.Kind, Scope: n.Scope, Name: n.Name}
		if _, err := t.exec(ctx, `INSERT INTO resources (id, kind, scope, name, revision) VALUES (?, ?, ?, ?, 0)`,
			r.ID, n.Kind, n.Scope, n.Name); err != nil {
			return Revision{}, fmt.Errorf("can't record %s %s: %w", n.Kind, n.Name, err)
		}
	case err != nil:
		return Revision{}, err
	}
	r.Revision++
	if n.Number != 0 && n.Number != r.Revision {
		return Revision{}, fmt.Errorf("%s %s is at revision %d, so its next is %d, not %d: %w", n.Kind, n.Name, r.Revision-1, r.Revision, n.Number, ErrConflict)
	}
	r.Spec, r.ChangeID = n.Spec, n.Change
	if _, err := t.exec(ctx, `INSERT INTO revisions (resource_id, number, spec, change_id, position, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.Revision, n.Spec, n.Change, n.Position, millis(n.At)); err != nil {
		return Revision{}, fmt.Errorf("can't record a revision of %s %s: %w", n.Kind, n.Name, err)
	}
	if _, err := t.exec(ctx, `UPDATE resources SET revision = ? WHERE id = ?`, r.Revision, r.ID); err != nil {
		return Revision{}, fmt.Errorf("can't record a revision of %s %s: %w", n.Kind, n.Name, err)
	}
	for _, u := range n.Uses {
		if _, err := t.exec(ctx, `INSERT INTO revision_uses (resource_id, revision, scope, used_as, used_kind, used_name, used_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, r.ID, r.Revision, n.Scope, u.As, u.Kind, u.Name, u.Revision); err != nil {
			return Revision{}, fmt.Errorf("can't record what %s %s uses: %w", n.Kind, n.Name, err)
		}
	}
	return Revision{Resource: r, CreatedAt: n.At}, nil
}

// Using is a resource whose latest revision uses another in its scope,
// and what it uses.
type Using struct {
	ID, Kind, Scope, Name string
	Revision              int // the user's latest revision
	Uses                  Use
}

// UsersOf returns what the resources in scope use, at their latest
// revisions, by the user's kind and name, then the name each use is seen
// as. A deleted resource uses nothing. With kind set, it returns only the
// uses of that kind; with names set too, only of those names.
func (t *Tx) UsersOf(ctx context.Context, scope, kind string, names ...string) ([]Using, error) {
	query := `SELECT r.id, r.kind, r.scope, r.name, r.revision, u.used_as, u.used_kind, u.used_name, u.used_revision
		FROM revision_uses u JOIN resources r ON r.id = u.resource_id AND r.revision = u.revision
		WHERE u.scope = ? AND (u.used_kind = ? OR ? = '')`
	args := []any{scope, kind, kind}
	if len(names) > 0 {
		query += ` AND u.used_name IN (` + marks(len(names)) + `)`
		for _, n := range names {
			args = append(args, n)
		}
	}
	rows, err := t.query(ctx, query+` ORDER BY r.kind, r.name, u.used_as`, args...)
	if err != nil {
		return nil, fmt.Errorf("can't read what the resources of %s use: %w", scope, err)
	}
	defer rows.Close()
	var out []Using
	for rows.Next() {
		var u Using
		if err := rows.Scan(&u.ID, &u.Kind, &u.Scope, &u.Name, &u.Revision, &u.Uses.As, &u.Uses.Kind, &u.Uses.Name, &u.Uses.Revision); err != nil {
			return nil, fmt.Errorf("can't read what the resources of %s use: %w", scope, err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

const revisionColumns = `r.id, r.kind, r.scope, r.name, v.number, v.spec, v.spec IS NULL, v.change_id, v.created_at`

func scanRevision(row interface{ Scan(...any) error }) (Revision, error) {
	var rev Revision
	var created int64
	var deleted bool
	err := row.Scan(&rev.ID, &rev.Kind, &rev.Scope, &rev.Name, &rev.Revision, &rev.Spec, &deleted, &rev.ChangeID, &created)
	rev.Spec, rev.CreatedAt = spec(rev.Spec, deleted), fromMillis(created)
	return rev, err
}

// GetRevision returns one revision of a resource, or ErrNotFound.
func (t *Tx) GetRevision(ctx context.Context, resourceID string, number int) (Revision, error) {
	rev, err := scanRevision(t.queryRow(ctx, `SELECT `+revisionColumns+`
		FROM revisions v JOIN resources r ON r.id = v.resource_id WHERE v.resource_id = ? AND v.number = ?`, resourceID, number))
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	if err != nil {
		return Revision{}, fmt.Errorf("can't read revision %d of %s: %w", number, resourceID, err)
	}
	return rev, nil
}

// RevisionsOf returns the revisions a change made, in the order it asked
// for them.
func (t *Tx) RevisionsOf(ctx context.Context, changeID string) ([]Revision, error) {
	rows, err := t.query(ctx, `SELECT `+revisionColumns+`
		FROM revisions v JOIN resources r ON r.id = v.resource_id WHERE v.change_id = ? ORDER BY v.position`, changeID)
	if err != nil {
		return nil, fmt.Errorf("can't read the revisions of %s: %w", changeID, err)
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		rev, err := scanRevision(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read the revisions of %s: %w", changeID, err)
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}

// PlaceRevision deploys a revision on servers, pending until each has it.
func (t *RevisionTx) PlaceRevision(ctx context.Context, resourceID string, revision int, servers []string, at time.Time) error {
	for _, s := range servers {
		if _, err := t.exec(ctx, `INSERT INTO deployments (resource_id, revision, server_id, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, resourceID, revision, s, DeploymentPending, millis(at), millis(at)); err != nil {
			return fmt.Errorf("can't place revision %d of %s on %s: %w", revision, resourceID, s, err)
		}
	}
	return nil
}

// SetDeploymentState records how a deployment's rollout went. Once one is
// live, the earlier revisions it replaces on that server are superseded.
// ErrNotFound means the revision was never placed on the server.
func (t *RevisionTx) SetDeploymentState(ctx context.Context, d Deployment) error {
	res, err := t.exec(ctx, `UPDATE deployments SET state = ?, error = ?, updated_at = ?
		WHERE resource_id = ? AND revision = ? AND server_id = ?`,
		d.State, nullString(d.Error), millis(d.UpdatedAt), d.ResourceID, d.Revision, d.Server)
	if err != nil {
		return fmt.Errorf("can't update the deployment of %s on %s: %w", d.ResourceID, d.Server, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("revision %d of %s on %s: %w", d.Revision, d.ResourceID, d.Server, ErrNotFound)
	}
	if d.State == DeploymentLive {
		return t.Supersede(ctx, d.ResourceID, d.Server, d.Revision, d.UpdatedAt)
	}
	return nil
}

// Supersede marks a resource's live and pending deployments on a server
// from before revision as superseded: something later, or a deletion,
// has taken their place there.
func (t *RevisionTx) Supersede(ctx context.Context, resourceID, server string, revision int, at time.Time) error {
	if _, err := t.exec(ctx, `UPDATE deployments SET state = ?, updated_at = ?
		WHERE resource_id = ? AND server_id = ? AND revision < ? AND state IN (?, ?)`,
		DeploymentSuperseded, millis(at), resourceID, server, revision, DeploymentLive, DeploymentPending); err != nil {
		return fmt.Errorf("can't update the deployments of %s on %s: %w", resourceID, server, err)
	}
	return nil
}

// Placement returns the servers a revision was placed on, by ID.
func (t *Tx) Placement(ctx context.Context, resourceID string, revision int) ([]string, error) {
	deps, err := t.Deployments(ctx, resourceID, revision)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(deps))
	for i, d := range deps {
		out[i] = d.Server
	}
	return out, nil
}

// Deployments returns where a revision of a resource was placed, by
// server ID, or every revision's when revision is 0, newest first.
func (t *Tx) Deployments(ctx context.Context, resourceID string, revision int) ([]Deployment, error) {
	rows, err := t.query(ctx, `SELECT resource_id, revision, server_id, state, error, created_at, updated_at
		FROM deployments WHERE resource_id = ? AND (revision = ? OR ? = 0) ORDER BY revision DESC, server_id`,
		resourceID, revision, revision)
	if err != nil {
		return nil, fmt.Errorf("can't read the deployments of %s: %w", resourceID, err)
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var errText sql.NullString
		var created, updated int64
		if err := rows.Scan(&d.ResourceID, &d.Revision, &d.Server, &d.State, &errText, &created, &updated); err != nil {
			return nil, fmt.Errorf("can't read the deployments of %s: %w", resourceID, err)
		}
		d.Error, d.CreatedAt, d.UpdatedAt = errText.String, fromMillis(created), fromMillis(updated)
		out = append(out, d)
	}
	return out, rows.Err()
}
