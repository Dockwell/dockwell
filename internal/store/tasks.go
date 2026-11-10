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

// KeptTaskRuns is how many of a task's runs on each server the control plane keeps
// (ADR 0053).
const KeptTaskRuns = 100

// TaskRun is one run of a scheduled task on one server, as its agent said.
type TaskRun struct {
	ID       string
	TaskID   string
	ServerID string
	// State is a dockwell.v1.TaskRunState.
	State     int32
	StartedAt time.Time
	// EndedAt is zero while it runs.
	EndedAt time.Time
	// Run is the dockwell.v1.TaskRun, marshalled, with its attempts and output.
	Run []byte
}

// PutTaskRun keeps a run as it is now: a running one, and again when it ends, in
// its place. An agent may send a run twice, which keeps it once. A run that ended
// stays ended, whatever arrives after: Run now's answer and the agent's word that
// the run ended can come in either order.
func (t *Tx) PutTaskRun(ctx context.Context, r TaskRun) error {
	var ended any
	if !r.EndedAt.IsZero() {
		ended = millis(r.EndedAt)
	}
	_, err := t.exec(ctx, `INSERT INTO task_runs (id, task_id, server_id, state, started_at, ended_at, run) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET state = excluded.state, ended_at = excluded.ended_at, run = excluded.run
		WHERE task_runs.ended_at IS NULL`,
		r.ID, r.TaskID, r.ServerID, r.State, millis(r.StartedAt), ended, r.Run)
	if err != nil {
		return fmt.Errorf("can't record the task run %s: %w", r.ID, err)
	}
	return nil
}

// PruneTaskRuns lets go of a task's runs on a server past the newest KeptTaskRuns.
func (t *Tx) PruneTaskRuns(ctx context.Context, taskID, serverID string) error {
	_, err := t.exec(ctx, `DELETE FROM task_runs WHERE task_id = ? AND server_id = ? AND id NOT IN (
		SELECT id FROM task_runs WHERE task_id = ? AND server_id = ? ORDER BY started_at DESC, id DESC LIMIT ?)`,
		taskID, serverID, taskID, serverID, KeptTaskRuns)
	if err != nil {
		return fmt.Errorf("can't let go of old runs of %s: %w", taskID, err)
	}
	return nil
}

// TaskRunFilter says which runs ListTaskRuns returns.
type TaskRunFilter struct {
	TaskID   string
	ServerID string
	// After a run, by when it started and its ID, for the next page.
	AfterAt *time.Time
	AfterID string
	Limit   int
}

// ListTaskRuns returns a task's runs, newest first.
func (t *Tx) ListTaskRuns(ctx context.Context, f TaskRunFilter) ([]TaskRun, error) {
	where, args := []string{"task_id = ?"}, []any{f.TaskID}
	if f.ServerID != "" {
		where, args = append(where, "server_id = ?"), append(args, f.ServerID)
	}
	if f.AfterAt != nil {
		at := millis(*f.AfterAt)
		where, args = append(where, "(started_at < ? OR started_at = ? AND id < ?)"), append(args, at, at, f.AfterID)
	}
	query := `SELECT id, task_id, server_id, state, started_at, ended_at, run FROM task_runs WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY started_at DESC, id DESC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := t.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("can't list the runs of %s: %w", f.TaskID, err)
	}
	defer rows.Close()
	var out []TaskRun
	for rows.Next() {
		var r TaskRun
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&r.ID, &r.TaskID, &r.ServerID, &r.State, &started, &ended, &r.Run); err != nil {
			return nil, fmt.Errorf("can't read a task run: %w", err)
		}
		r.StartedAt = fromMillis(started)
		if ended.Valid {
			r.EndedAt = fromMillis(ended.Int64)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneGoneTaskRuns lets go of the runs of each task deleted before before. Until
// then they stay, as undoing the delete brings the task back under its own ID.
func (t *Tx) PruneGoneTaskRuns(ctx context.Context, kind string, before time.Time) error {
	_, err := t.exec(ctx, `DELETE FROM task_runs WHERE task_id NOT IN (SELECT id FROM resources WHERE kind = ?)
		OR task_id IN (SELECT r.id FROM resources r JOIN revisions v ON v.resource_id = r.id AND v.number = r.revision
			WHERE r.kind = ? AND v.spec IS NULL AND v.created_at < ?)`, kind, kind, millis(before))
	if err != nil {
		return fmt.Errorf("can't let go of the runs of deleted tasks: %w", err)
	}
	return nil
}

// GetTaskRun returns one run.
func (t *Tx) GetTaskRun(ctx context.Context, id string) (TaskRun, error) {
	var r TaskRun
	var started int64
	var ended sql.NullInt64
	err := t.queryRow(ctx, `SELECT id, task_id, server_id, state, started_at, ended_at, run FROM task_runs WHERE id = ?`, id).
		Scan(&r.ID, &r.TaskID, &r.ServerID, &r.State, &started, &ended, &r.Run)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskRun{}, ErrNotFound
	}
	if err != nil {
		return TaskRun{}, fmt.Errorf("can't read the task run %s: %w", id, err)
	}
	r.StartedAt = fromMillis(started)
	if ended.Valid {
		r.EndedAt = fromMillis(ended.Int64)
	}
	return r, nil
}
