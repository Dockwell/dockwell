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

// A backup's states, as stored.
const (
	BackupRunning = "running"
	BackupDone    = "done"
	BackupFailed  = "failed"
)

// Backup is what the control plane knows of one backup. A backup is an event, not
// managed state, so this is written outside the revision store (ADR 0033).
type Backup struct {
	ID            string
	EnvironmentID string
	Volume        string // its name, which outlives it
	VolumeID      string
	ServerID      string
	State         string
	Method        string // resource.BackupDump and so on
	Database      string
	SizeBytes     int64
	SHA256        string
	FilesChanged  int64
	Actor         string
	Error         string
	StartedAt     time.Time
	FinishedAt    *time.Time
}

const backupColumns = `id, environment_id, volume, volume_id, server_id, state, method, database_name, size_bytes, sha256, files_changed, actor, error, started_at, finished_at`

func scanBackup(row interface{ Scan(...any) error }) (Backup, error) {
	var b Backup
	var started int64
	var finished sql.NullInt64
	err := row.Scan(&b.ID, &b.EnvironmentID, &b.Volume, &b.VolumeID, &b.ServerID, &b.State, &b.Method, &b.Database, &b.SizeBytes, &b.SHA256, &b.FilesChanged, &b.Actor, &b.Error, &started, &finished)
	if err != nil {
		return Backup{}, err
	}
	b.StartedAt, b.FinishedAt = fromMillis(started), timePtr(finished)
	return b, nil
}

// CreateBackup records a backup that's starting.
func (t *Tx) CreateBackup(ctx context.Context, b Backup) error {
	_, err := t.exec(ctx, `INSERT INTO backups (id, environment_id, volume, volume_id, server_id, state, actor, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.EnvironmentID, b.Volume, b.VolumeID, b.ServerID, b.State, b.Actor, millis(b.StartedAt))
	if err != nil {
		return fmt.Errorf("can't record backup %s: %w", b.ID, err)
	}
	return nil
}

// FinishBackup records how a backup ended.
func (t *Tx) FinishBackup(ctx context.Context, b Backup) error {
	_, err := t.exec(ctx, `UPDATE backups SET state = ?, method = ?, database_name = ?, size_bytes = ?, sha256 = ?, files_changed = ?, error = ?, finished_at = ? WHERE id = ?`,
		b.State, b.Method, b.Database, b.SizeBytes, b.SHA256, b.FilesChanged, b.Error, nullMillis(b.FinishedAt), b.ID)
	if err != nil {
		return fmt.Errorf("can't record how backup %s ended: %w", b.ID, err)
	}
	return nil
}

// GetBackup returns a backup, or ErrNotFound.
func (t *Tx) GetBackup(ctx context.Context, id string) (Backup, error) {
	b, err := scanBackup(t.queryRow(ctx, `SELECT `+backupColumns+` FROM backups WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Backup{}, fmt.Errorf("backup %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Backup{}, fmt.Errorf("can't read backup %s: %w", id, err)
	}
	return b, nil
}

// DeleteBackup forgets a backup.
func (t *Tx) DeleteBackup(ctx context.Context, id string) error {
	if _, err := t.exec(ctx, `DELETE FROM backups WHERE id = ?`, id); err != nil {
		return fmt.Errorf("can't forget backup %s: %w", id, err)
	}
	return nil
}

// BackupFilter narrows a list of backups.
type BackupFilter struct {
	VolumeID, EnvironmentID string
	// After is the last backup of the page before, by time and ID.
	AfterAt *time.Time
	AfterID string
	Limit   int
}

// ListBackups returns backups, newest first.
func (t *Tx) ListBackups(ctx context.Context, f BackupFilter) ([]Backup, error) {
	var where []string
	var args []any
	if f.VolumeID != "" {
		where, args = append(where, "volume_id = ?"), append(args, f.VolumeID)
	}
	if f.EnvironmentID != "" {
		where, args = append(where, "environment_id = ?"), append(args, f.EnvironmentID)
	}
	if f.AfterAt != nil {
		at := millis(*f.AfterAt)
		where, args = append(where, "(started_at < ? OR started_at = ? AND id < ?)"), append(args, at, at, f.AfterID)
	}
	query := `SELECT ` + backupColumns + ` FROM backups`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY started_at DESC, id DESC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := t.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("can't list backups: %w", err)
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read a backup: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
