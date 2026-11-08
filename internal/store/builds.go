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

// A build's states, as stored.
const (
	BuildQueued    = "queued"
	BuildBuilding  = "building"
	BuildDone      = "done"
	BuildFailed    = "failed"
	BuildCancelled = "cancelled"
	BuildLost      = "lost"
)

// What a build's log keeps: its first KeepFirst lines and its last KeepLast, and
// the logs of a service's newest KeepLogs builds (ADR 0046).
const (
	KeepFirst = 1000
	KeepLast  = 9000
	KeepLogs  = 20
)

// Build is what the control plane knows of one build, as its server says. A build is
// a step of a change, not managed state, so this is written outside the revision
// store.
type Build struct {
	ID            string
	ChangeID      string
	ResourceID    string // the service's ID
	EnvironmentID string
	Service       string // its name, which outlives it
	ServerID      string
	Revision      int64
	State         string
	URL, Ref      string
	Commit        string
	Subject       string
	Author        string
	Image         string
	Steps         int64
	Cached        int64
	Step          int64
	StepName      string
	Error         string
	FailedLine    string
	CancelledBy   string
	// Detection is what it found as it read its commit, as JSON, or empty.
	Detection string
	// Lines is how many its log has had, LeftOut how many of those it doesn't keep,
	// and Dropped how many its server dropped.
	Lines, LeftOut, Dropped int64
	CreatedAt               time.Time
	StartedAt, EndedAt      *time.Time
}

// Ended reports a build that has ended, one way or another.
func (b Build) Ended() bool {
	return b.State == BuildDone || b.State == BuildFailed || b.State == BuildCancelled || b.State == BuildLost
}

// BuildLine is a line of a build's log, as kept.
type BuildLine struct {
	Number int64
	Step   int64
	At     time.Time
	Text   string
}

const buildColumns = `id, change_id, resource_id, environment_id, service, server_id, revision, state, url, ref, commit_id, commit_subject, commit_author, image,
	steps, cached_steps, step, step_name, error, failed_line, cancelled_by, log_lines, log_left_out, log_dropped, created_at, started_at, ended_at, detection`

func scanBuild(row interface{ Scan(...any) error }) (Build, error) {
	var b Build
	var created int64
	var started, ended sql.NullInt64
	err := row.Scan(&b.ID, &b.ChangeID, &b.ResourceID, &b.EnvironmentID, &b.Service, &b.ServerID, &b.Revision, &b.State, &b.URL, &b.Ref, &b.Commit, &b.Subject, &b.Author, &b.Image,
		&b.Steps, &b.Cached, &b.Step, &b.StepName, &b.Error, &b.FailedLine, &b.CancelledBy, &b.Lines, &b.LeftOut, &b.Dropped, &created, &started, &ended, &b.Detection)
	if err != nil {
		return Build{}, err
	}
	b.CreatedAt, b.StartedAt, b.EndedAt = fromMillis(created), timePtr(started), timePtr(ended)
	return b, nil
}

// CreateBuild records a build its server has begun to tell of.
func (t *Tx) CreateBuild(ctx context.Context, b Build) error {
	_, err := t.exec(ctx, `INSERT INTO builds (id, change_id, resource_id, environment_id, service, server_id, revision, state, url, ref, commit_id, image, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.ChangeID, b.ResourceID, b.EnvironmentID, b.Service, b.ServerID, b.Revision, b.State, b.URL, b.Ref, b.Commit, b.Image, millis(b.CreatedAt))
	if err != nil {
		return fmt.Errorf("can't record build %s: %w", b.ID, err)
	}
	return nil
}

// UpdateBuild records where a build stands now.
func (t *Tx) UpdateBuild(ctx context.Context, b Build) error {
	_, err := t.exec(ctx, `UPDATE builds SET state = ?, commit_subject = ?, commit_author = ?, image = ?, steps = ?, cached_steps = ?, step = ?, step_name = ?,
		error = ?, failed_line = ?, cancelled_by = ?, log_dropped = ?, started_at = ?, ended_at = ?, detection = ? WHERE id = ?`,
		b.State, b.Subject, b.Author, b.Image, b.Steps, b.Cached, b.Step, b.StepName, b.Error, b.FailedLine, b.CancelledBy, b.Dropped,
		nullMillis(b.StartedAt), nullMillis(b.EndedAt), b.Detection, b.ID)
	if err != nil {
		return fmt.Errorf("can't record where build %s stands: %w", b.ID, err)
	}
	return nil
}

// GetBuild returns one build by ID, or ErrNotFound.
func (t *Tx) GetBuild(ctx context.Context, id string) (Build, error) {
	b, err := scanBuild(t.queryRow(ctx, `SELECT `+buildColumns+` FROM builds WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, fmt.Errorf("build %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Build{}, fmt.Errorf("can't read build %s: %w", id, err)
	}
	return b, nil
}

// BuildOf returns the build of a service for a change on a server, or ErrNotFound.
func (t *Tx) BuildOf(ctx context.Context, changeID, resourceID, serverID string) (Build, error) {
	b, err := scanBuild(t.queryRow(ctx, `SELECT `+buildColumns+` FROM builds WHERE change_id = ? AND resource_id = ? AND server_id = ?`, changeID, resourceID, serverID))
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, fmt.Errorf("build of %s for %s on %s: %w", resourceID, changeID, serverID, ErrNotFound)
	}
	if err != nil {
		return Build{}, fmt.Errorf("can't read the build of %s for %s: %w", resourceID, changeID, err)
	}
	return b, nil
}

// BuildFilter narrows a list of builds.
type BuildFilter struct {
	// ResourceID is the service's ID.
	ResourceID string
	// ChangeIDs keeps those of these changes alone.
	ChangeIDs []string
	// Before starts after this build, newest first.
	BeforeAt time.Time
	BeforeID string
	Limit    int
}

// ListBuilds lists builds, newest first.
func (t *Tx) ListBuilds(ctx context.Context, f BuildFilter) ([]Build, error) {
	var where []string
	var args []any
	if f.ResourceID != "" {
		where = append(where, `resource_id = ?`)
		args = append(args, f.ResourceID)
	}
	if len(f.ChangeIDs) > 0 {
		where = append(where, `change_id IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(f.ChangeIDs)), ", ")+`)`)
		for _, id := range f.ChangeIDs {
			args = append(args, id)
		}
	}
	if f.BeforeID != "" {
		where = append(where, `(created_at < ? OR created_at = ? AND id < ?)`)
		args = append(args, millis(f.BeforeAt), millis(f.BeforeAt), f.BeforeID)
	}
	query := `SELECT ` + buildColumns + ` FROM builds`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY created_at DESC, id DESC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := t.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("can't list builds: %w", err)
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read a build: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AddBuildLines keeps lines a build's log gained, numbered on from those it has, and
// leaves out what is past its first KeepFirst and before its last KeepLast. It says
// the build's counts as they stand.
func (t *Tx) AddBuildLines(ctx context.Context, id string, lines []BuildLine) (Build, error) {
	b, err := t.GetBuild(ctx, id)
	if err != nil || len(lines) == 0 {
		return b, err
	}
	for i := range lines {
		b.Lines++
		lines[i].Number = b.Lines
		if _, err := t.exec(ctx, `INSERT INTO build_lines (build_id, number, step, at, line) VALUES (?, ?, ?, ?, ?)`,
			id, b.Lines, lines[i].Step, millis(lines[i].At), lines[i].Text); err != nil {
			return b, fmt.Errorf("can't keep a line of build %s: %w", id, err)
		}
	}
	if over := b.Lines - KeepFirst - KeepLast; over > 0 {
		if _, err := t.exec(ctx, `DELETE FROM build_lines WHERE build_id = ? AND number > ? AND number <= ?`, id, KeepFirst, KeepFirst+over); err != nil {
			return b, fmt.Errorf("can't leave out the middle of build %s's log: %w", id, err)
		}
		b.LeftOut = over
	}
	if _, err := t.exec(ctx, `UPDATE builds SET log_lines = ?, log_left_out = ? WHERE id = ?`, b.Lines, b.LeftOut, id); err != nil {
		return b, fmt.Errorf("can't count build %s's log: %w", id, err)
	}
	return b, nil
}

// BuildLines returns the lines a build's log keeps after the line numbered after,
// oldest first, at most limit of them.
func (t *Tx) BuildLines(ctx context.Context, id string, after int64, limit int) ([]BuildLine, error) {
	rows, err := t.query(ctx, `SELECT number, step, at, line FROM build_lines WHERE build_id = ? AND number > ? ORDER BY number LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, fmt.Errorf("can't read build %s's log: %w", id, err)
	}
	defer rows.Close()
	var out []BuildLine
	for rows.Next() {
		var l BuildLine
		var at int64
		if err := rows.Scan(&l.Number, &l.Step, &at, &l.Text); err != nil {
			return nil, fmt.Errorf("can't read a line of build %s's log: %w", id, err)
		}
		l.At = fromMillis(at)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ForgetOldBuildLogs drops the logs of a service's builds past its newest keep,
// whose records stay.
func (t *Tx) ForgetOldBuildLogs(ctx context.Context, resourceID string, keep int) error {
	builds, err := t.ListBuilds(ctx, BuildFilter{ResourceID: resourceID})
	if err != nil || len(builds) <= keep {
		return err
	}
	for i := range builds[keep:] {
		b := &builds[keep+i]
		if _, err := t.exec(ctx, `DELETE FROM build_lines WHERE build_id = ?`, b.ID); err != nil {
			return fmt.Errorf("can't drop build %s's log: %w", b.ID, err)
		}
	}
	return nil
}
