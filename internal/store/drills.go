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

// A drill's results, as stored.
const (
	DrillRunning    = "running"
	DrillPassed     = "passed"
	DrillFailed     = "failed"
	DrillNotChecked = "not_checked"
)

// Drill is a check that a backup restores (ADR 0068). It's an event, not managed
// state, so it's written outside the revision store, and it goes with its backup.
type Drill struct {
	ID            string
	BackupID      string
	EnvironmentID string
	VolumeID      string
	ServerID      string
	Result        string
	// Why is what failed, or why it wasn't checked, in a line; Detail what the
	// database printed last, without what it quoted.
	Why, Detail   string
	RestoredBytes int64
	Tables        int64
	UnpackedOnly  bool
	Actor         string
	StartedAt     time.Time
	FinishedAt    *time.Time
	// PolicyID is the backup policy it ran for, as its schedule. Empty for one someone
	// started.
	PolicyID string
}

const drillColumns = `id, backup_id, environment_id, volume_id, server_id, result, why, detail, restored_bytes, tables_checked, unpacked_only, actor, started_at, finished_at, policy_id`

func scanDrill(row interface{ Scan(...any) error }) (Drill, error) {
	var d Drill
	var started int64
	var finished sql.NullInt64
	err := row.Scan(&d.ID, &d.BackupID, &d.EnvironmentID, &d.VolumeID, &d.ServerID, &d.Result, &d.Why, &d.Detail, &d.RestoredBytes, &d.Tables, &d.UnpackedOnly, &d.Actor, &started, &finished, &d.PolicyID)
	if err != nil {
		return Drill{}, err
	}
	d.StartedAt, d.FinishedAt = fromMillis(started), timePtr(finished)
	return d, nil
}

// CreateDrill records a drill that's starting.
func (t *Tx) CreateDrill(ctx context.Context, d Drill) error {
	_, err := t.exec(ctx, `INSERT INTO backup_drills (id, backup_id, environment_id, volume_id, server_id, result, actor, started_at, policy_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.BackupID, d.EnvironmentID, d.VolumeID, d.ServerID, d.Result, d.Actor, millis(d.StartedAt), d.PolicyID)
	if err != nil {
		return fmt.Errorf("can't record drill %s: %w", d.ID, err)
	}
	return nil
}

// FinishDrill records how a drill ended.
func (t *Tx) FinishDrill(ctx context.Context, d Drill) error {
	_, err := t.exec(ctx, `UPDATE backup_drills SET result = ?, why = ?, detail = ?, restored_bytes = ?, tables_checked = ?, unpacked_only = ?, finished_at = ? WHERE id = ?`,
		d.Result, d.Why, d.Detail, d.RestoredBytes, d.Tables, d.UnpackedOnly, nullMillis(d.FinishedAt), d.ID)
	if err != nil {
		return fmt.Errorf("can't record how drill %s ended: %w", d.ID, err)
	}
	return nil
}

// GetDrill returns a drill, or ErrNotFound.
func (t *Tx) GetDrill(ctx context.Context, id string) (Drill, error) {
	d, err := scanDrill(t.queryRow(ctx, `SELECT `+drillColumns+` FROM backup_drills WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Drill{}, fmt.Errorf("drill %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Drill{}, fmt.Errorf("can't read drill %s: %w", id, err)
	}
	return d, nil
}

// DrillFilter narrows a list of drills.
type DrillFilter struct {
	// BackupIDs are the backups whose drills to list, and VolumeID the volume's.
	BackupIDs []string
	VolumeID  string
	// PolicyID and ServerID are a schedule's drills on one server.
	PolicyID, ServerID string
	// Result is one result alone, such as DrillRunning.
	Result string
	// Since is the earliest start.
	Since *time.Time
}

// ListDrills returns drills, newest first.
func (t *Tx) ListDrills(ctx context.Context, f DrillFilter) ([]Drill, error) {
	var where []string
	var args []any
	if f.BackupIDs != nil {
		if len(f.BackupIDs) == 0 {
			return nil, nil
		}
		where = append(where, "backup_id IN ("+strings.TrimSuffix(strings.Repeat("?, ", len(f.BackupIDs)), ", ")+")")
		for _, id := range f.BackupIDs {
			args = append(args, id)
		}
	}
	if f.VolumeID != "" {
		where, args = append(where, "volume_id = ?"), append(args, f.VolumeID)
	}
	if f.PolicyID != "" {
		where, args = append(where, "policy_id = ?"), append(args, f.PolicyID)
	}
	if f.ServerID != "" {
		where, args = append(where, "server_id = ?"), append(args, f.ServerID)
	}
	if f.Result != "" {
		where, args = append(where, "result = ?"), append(args, f.Result)
	}
	if f.Since != nil {
		where, args = append(where, "started_at >= ?"), append(args, millis(*f.Since))
	}
	query := `SELECT ` + drillColumns + ` FROM backup_drills`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY started_at DESC, id DESC`
	rows, err := t.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("can't list drills: %w", err)
	}
	defer rows.Close()
	var out []Drill
	for rows.Next() {
		d, err := scanDrill(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read a drill: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// NewestDrills returns each backup's newest drill, by its backup.
func (t *Tx) NewestDrills(ctx context.Context, backupIDs []string) (map[string]Drill, error) {
	out := map[string]Drill{}
	for start := 0; start < len(backupIDs); start += 500 {
		list, err := t.ListDrills(ctx, DrillFilter{BackupIDs: backupIDs[start:min(start+500, len(backupIDs))]})
		if err != nil {
			return nil, err
		}
		for i := range list {
			if _, seen := out[list[i].BackupID]; !seen {
				out[list[i].BackupID] = list[i]
			}
		}
	}
	return out, nil
}

// DeleteDrills forgets a backup's drills, as they go with it.
func (t *Tx) DeleteDrills(ctx context.Context, backupID string) error {
	if _, err := t.exec(ctx, `DELETE FROM backup_drills WHERE backup_id = ?`, backupID); err != nil {
		return fmt.Errorf("can't forget the drills of backup %s: %w", backupID, err)
	}
	return nil
}
