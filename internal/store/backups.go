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
	// PolicyID is the policy that took it. Empty for one someone took.
	PolicyID string
	// Destination is where it goes as well, by name. CopyKey is its name there once
	// it's stored, CopyBytes what it takes there, and CopyError why it isn't.
	Destination string
	CopyKey     string
	CopyBytes   int64
	CopyError   string
	// OffServer says retention or a person removed it from its server. A server that's
	// gone or away is read from the fleet instead, so its backups come back with it.
	OffServer bool
	// SealedWith is a keyed hash of the passphrase its copy was sealed with, which says
	// whether a destination of that name still has it, and nothing of what it is.
	SealedWith string
	FetchError string
}

const backupColumns = `id, environment_id, volume, volume_id, server_id, state, method, database_name, size_bytes, sha256, files_changed, actor, error, started_at, finished_at, policy_id, destination, copy_key, copy_bytes, copy_error, off_server, sealed_with, fetch_error`

func scanBackup(row interface{ Scan(...any) error }) (Backup, error) {
	var b Backup
	var started int64
	var finished sql.NullInt64
	err := row.Scan(&b.ID, &b.EnvironmentID, &b.Volume, &b.VolumeID, &b.ServerID, &b.State, &b.Method, &b.Database, &b.SizeBytes, &b.SHA256, &b.FilesChanged, &b.Actor, &b.Error, &started, &finished, &b.PolicyID, &b.Destination, &b.CopyKey, &b.CopyBytes, &b.CopyError, &b.OffServer, &b.SealedWith, &b.FetchError)
	if err != nil {
		return Backup{}, err
	}
	b.StartedAt, b.FinishedAt = fromMillis(started), timePtr(finished)
	return b, nil
}

// CreateBackup records a backup that's starting.
func (t *Tx) CreateBackup(ctx context.Context, b Backup) error {
	_, err := t.exec(ctx, `INSERT INTO backups (id, environment_id, volume, volume_id, server_id, state, actor, started_at, policy_id, destination) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.EnvironmentID, b.Volume, b.VolumeID, b.ServerID, b.State, b.Actor, millis(b.StartedAt), b.PolicyID, b.Destination)
	if err != nil {
		return fmt.Errorf("can't record backup %s: %w", b.ID, err)
	}
	return nil
}

// FinishBackup records how a backup ended.
func (t *Tx) FinishBackup(ctx context.Context, b Backup) error {
	_, err := t.exec(ctx, `UPDATE backups SET state = ?, method = ?, database_name = ?, size_bytes = ?, sha256 = ?, files_changed = ?, error = ?, finished_at = ?,
		copy_key = ?, copy_bytes = ?, copy_error = ? WHERE id = ?`,
		b.State, b.Method, b.Database, b.SizeBytes, b.SHA256, b.FilesChanged, b.Error, nullMillis(b.FinishedAt), b.CopyKey, b.CopyBytes, b.CopyError, b.ID)
	if err != nil {
		return fmt.Errorf("can't record how backup %s ended: %w", b.ID, err)
	}
	return nil
}

// MoveBackup says which server has a backup now, as fetching it does.
func (t *Tx) MoveBackup(ctx context.Context, id, serverID string) error {
	if _, err := t.exec(ctx, `UPDATE backups SET server_id = ?, off_server = ? WHERE id = ?`, serverID, false, id); err != nil {
		return fmt.Errorf("can't record where backup %s is: %w", id, err)
	}
	return nil
}

// SetBackupCopy records what became of a backup's copy on its destination.
func (t *Tx) SetBackupCopy(ctx context.Context, b Backup) error {
	if _, err := t.exec(ctx, `UPDATE backups SET copy_key = ?, copy_bytes = ?, copy_error = ?, sealed_with = ? WHERE id = ?`,
		b.CopyKey, b.CopyBytes, b.CopyError, b.SealedWith, b.ID); err != nil {
		return fmt.Errorf("can't record where backup %s went: %w", b.ID, err)
	}
	return nil
}

// SetBackupOffServer says a backup's server no longer has it.
func (t *Tx) SetBackupOffServer(ctx context.Context, id string) error {
	if _, err := t.exec(ctx, `UPDATE backups SET off_server = ? WHERE id = ?`, true, id); err != nil {
		return fmt.Errorf("can't record that backup %s left its server: %w", id, err)
	}
	return nil
}

// SetBackupFetchError records why a backup couldn't be fetched back. Empty clears it.
func (t *Tx) SetBackupFetchError(ctx context.Context, id, why string) error {
	if _, err := t.exec(ctx, `UPDATE backups SET fetch_error = ? WHERE id = ?`, why, id); err != nil {
		return fmt.Errorf("can't record why backup %s wasn't fetched: %w", id, err)
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

// DeleteBackup forgets a backup, and its drills with it.
func (t *Tx) DeleteBackup(ctx context.Context, id string) error {
	if err := t.DeleteDrills(ctx, id); err != nil {
		return err
	}
	if _, err := t.exec(ctx, `DELETE FROM backups WHERE id = ?`, id); err != nil {
		return fmt.Errorf("can't forget backup %s: %w", id, err)
	}
	return nil
}

// BackupFilter narrows a list of backups.
type BackupFilter struct {
	VolumeID, EnvironmentID, PolicyID, ServerID string
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
	if f.PolicyID != "" {
		where, args = append(where, "policy_id = ?"), append(args, f.PolicyID)
	}
	if f.ServerID != "" {
		where, args = append(where, "server_id = ?"), append(args, f.ServerID)
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
