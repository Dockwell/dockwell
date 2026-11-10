// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"
)

// The roles a person may hold, beside RoleOwner (ADR 0049).
const (
	RoleAdmin     = "admin"
	RoleDeveloper = "developer"
	RoleViewer    = "viewer"
)

// Install is the place a grant on the whole install has, which covers every
// project.
const Install = ""

// Grant is a role a person holds in a place: the install, or one project.
type Grant struct {
	UserID string
	// ProjectID is the project's resource ID, or Install.
	ProjectID string
	Role      string
	CreatedAt time.Time
	// CreatedBy is the actor who granted it, as the audit log names actors.
	CreatedBy string
}

// ErrOwner is a change to the owner's grant that isn't a transfer of ownership:
// the owner's role is given and taken only by TransferOwnership (ADR 0049).
var ErrOwner = errors.New("the owner's role changes only when ownership is transferred")

// ErrNotAPlace is a grant of the owner's or an admin's role on one project:
// those hold the install only.
var ErrNotAPlace = errors.New("the owner and admins hold the whole install, not one project")

// RoleRank orders roles by what they may do: a higher one may do all a lower
// one may. An unknown role ranks below a viewer.
func RoleRank(role string) int {
	switch role {
	case RoleViewer:
		return 1
	case RoleDeveloper:
		return 2
	case RoleAdmin:
		return 3
	case RoleOwner:
		return 4
	}
	return 0
}

const grantColumns = `user_id, project_id, role, created_at, created_by`

func scanGrants(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
},
) ([]Grant, error) {
	var out []Grant
	for rows.Next() {
		var g Grant
		var created int64
		if err := rows.Scan(&g.UserID, &g.ProjectID, &g.Role, &created, &g.CreatedBy); err != nil {
			return nil, err
		}
		g.CreatedAt = fromMillis(created)
		out = append(out, g)
	}
	return out, rows.Err()
}

// Grants returns what a person holds, the install first, then by project.
func (t *Tx) Grants(ctx context.Context, userID string) ([]Grant, error) {
	rows, err := t.query(ctx, `SELECT `+grantColumns+` FROM grants WHERE user_id = ? ORDER BY project_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("can't read what %s may do: %w", userID, err)
	}
	defer rows.Close()
	return scanGrants(rows)
}

// ListGrants returns every grant, by person, the install first.
func (t *Tx) ListGrants(ctx context.Context) ([]Grant, error) {
	rows, err := t.query(ctx, `SELECT `+grantColumns+` FROM grants ORDER BY user_id, project_id`)
	if err != nil {
		return nil, fmt.Errorf("can't read who may do what: %w", err)
	}
	defer rows.Close()
	return scanGrants(rows)
}

// SetGrant gives a person a role in a place, in place of the one they held
// there. It returns ErrOwner for the owner's role, or a change to what the owner
// holds on the install, which only TransferOwnership makes; and ErrNotAPlace
// for an admin's role on one project.
func (t *Tx) SetGrant(ctx context.Context, g Grant) error {
	if g.Role == RoleOwner {
		return ErrOwner
	}
	if g.ProjectID == Install {
		if owner, err := t.isOwner(ctx, g.UserID); err != nil || owner {
			return cmp.Or(err, ErrOwner)
		}
	}
	return t.setGrant(ctx, g)
}

// isOwner reports the person who holds the owner's grant.
func (t *Tx) isOwner(ctx context.Context, userID string) (bool, error) {
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM grants WHERE user_id = ? AND role = ?`, userID, RoleOwner).Scan(&n)
	return n > 0, err
}

// TransferOwnership makes to the owner, in place of whatever they held on the
// install. The owner before, if there was one, becomes an admin (ADR 0049). It's
// the only way the owner's grant changes.
func (t *Tx) TransferOwnership(ctx context.Context, to string, at time.Time, by string) error {
	if _, err := t.GetUser(ctx, to); err != nil {
		return err
	}
	if _, err := t.exec(ctx, `UPDATE grants SET role = ?, created_at = ?, created_by = ? WHERE role = ? AND user_id <> ?`,
		RoleAdmin, millis(at), by, RoleOwner, to); err != nil {
		return fmt.Errorf("can't make the owner an admin: %w", err)
	}
	return t.setGrant(ctx, Grant{UserID: to, ProjectID: Install, Role: RoleOwner, CreatedAt: at, CreatedBy: by})
}

// setGrant gives a role in a place, the owner's included.
func (t *Tx) setGrant(ctx context.Context, g Grant) error {
	if (g.Role == RoleOwner || g.Role == RoleAdmin) && g.ProjectID != Install {
		return ErrNotAPlace
	}
	if RoleRank(g.Role) == 0 {
		return fmt.Errorf("%q isn't a role. A role is owner, admin, developer or viewer", g.Role)
	}
	_, err := t.exec(ctx, `INSERT INTO grants (`+grantColumns+`) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, project_id) DO UPDATE SET role = excluded.role, created_at = excluded.created_at, created_by = excluded.created_by`,
		g.UserID, g.ProjectID, g.Role, millis(g.CreatedAt), g.CreatedBy)
	if t.unique(err) {
		return fmt.Errorf("there's an owner already, and only ever one: %w", ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("can't grant %s in %s: %w", g.Role, placeName(g.ProjectID), err)
	}
	return nil
}

// RemoveGrant takes back what a person holds in a place, or returns ErrNotFound
// when they hold nothing there.
func (t *Tx) RemoveGrant(ctx context.Context, userID, projectID string) error {
	if projectID == Install {
		if owner, err := t.isOwner(ctx, userID); err != nil || owner {
			return cmp.Or(err, ErrOwner)
		}
	}
	res, err := t.exec(ctx, `DELETE FROM grants WHERE user_id = ? AND project_id = ?`, userID, projectID)
	if err != nil {
		return fmt.Errorf("can't take back what %s holds in %s: %w", userID, placeName(projectID), err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%s holds nothing in %s: %w", userID, placeName(projectID), ErrNotFound)
	}
	return nil
}

func placeName(projectID string) string {
	if projectID == Install {
		return "the install"
	}
	return projectID
}
