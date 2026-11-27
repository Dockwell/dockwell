// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ControlBackup is the schedule and generation of the installation's store.
type ControlBackup struct {
	Generation, Position, LastPosition                    int64
	GenerationID, Destination, DestinationPin, RecoveryID string
	DestinationSelection, DestinationMarker               string
	Problem                                               string
	Preferred, Excluded                                   []string
	ChangedAt, FirstChangedAt, LastAttempt                time.Time
	Recovery                                              []byte
	Kept, Hourly, Requested                               bool
}

const controlBackupColumns = `generation, generation_id, position, changed_at, first_changed_at, recovery, recovery_id,
	passphrase_kept, destination, destination_pin, preferred_servers, excluded_servers, last_attempt, last_position, hourly, requested, attempt_problem, destination_selection, destination_marker`

// ControlBackup reads the recovery configuration and schedule.
func (t *Tx) ControlBackup(ctx context.Context) (ControlBackup, error) {
	return scanControlBackup(t.queryRow(ctx, "SELECT "+controlBackupColumns+" FROM control_backup WHERE singleton = 1"))
}

func scanControlBackup(row interface{ Scan(...any) error }) (ControlBackup, error) {
	var s ControlBackup
	var at, first, attempt int64
	var kept, hourly, requested int
	var preferred, excluded string
	err := row.Scan(&s.Generation, &s.GenerationID, &s.Position, &at, &first, &s.Recovery, &s.RecoveryID, &kept, &s.Destination,
		&s.DestinationPin, &preferred, &excluded, &attempt, &s.LastPosition, &hourly, &requested, &s.Problem, &s.DestinationSelection, &s.DestinationMarker)
	s.ChangedAt, s.FirstChangedAt, s.LastAttempt = fromMillis(at), fromMillis(first), fromMillis(attempt)
	s.Kept, s.Hourly, s.Requested = kept != 0, hourly != 0, requested != 0
	if preferred != "" {
		s.Preferred = strings.Split(preferred, ",")
	}
	if excluded != "" {
		s.Excluded = strings.Split(excluded, ",")
	}
	return s, err
}

// InitControlBackup stores recovery material when this install has none.
func (t *Tx) InitControlBackup(ctx context.Context, generationID, recoveryID string, recovery []byte) error {
	_, err := t.exec(ctx, `UPDATE control_backup SET generation_id = ?, recovery = ?, recovery_id = ?
		WHERE singleton = 1 AND generation_id = ''`, generationID, recovery, recoveryID)
	return err
}

// SetControlBackupPassphrase stores a new sealed phrase and queues a backup.
func (t *Tx) SetControlBackupPassphrase(ctx context.Context, recoveryID string, recovery []byte, now time.Time) error {
	_, err := t.exec(ctx, `UPDATE control_backup SET recovery = ?, recovery_id = ?, passphrase_kept = 0, requested = 1,
		position = position + 1, changed_at = ?, first_changed_at = CASE WHEN first_changed_at = 0 THEN ? ELSE first_changed_at END
		WHERE singleton = 1`, recovery, recoveryID, millis(now), millis(now))
	return err
}

// ConfirmControlBackupPassphrase records that the current phrase was kept.
func (t *Tx) ConfirmControlBackupPassphrase(ctx context.Context, now time.Time) error {
	_, err := t.exec(ctx, `UPDATE control_backup SET passphrase_kept = 1, position = position + 1,
		changed_at = ?, first_changed_at = CASE WHEN first_changed_at = 0 THEN ? ELSE first_changed_at END
		WHERE singleton = 1`, millis(now), millis(now))
	return err
}

// SetControlBackupDestination stores the selected destination and its location pin.
func (t *Tx) SetControlBackupDestination(ctx context.Context, dest, pin, selection string, now time.Time) error {
	_, err := t.exec(ctx, `UPDATE control_backup SET destination = ?, destination_pin = ?, destination_selection = ?,
		destination_marker = '', position = position + 1,
		changed_at = ?, first_changed_at = CASE WHEN first_changed_at = 0 THEN ? ELSE first_changed_at END
		WHERE singleton = 1`, dest, pin, selection, millis(now), millis(now))
	if err != nil || pin == "" {
		return err
	}
	_, err = t.exec(ctx, `UPDATE control_backup_places SET dismissed = 0 WHERE place = ?
		AND generation = (SELECT generation FROM control_backup WHERE singleton = 1)`, "offsite:"+pin)
	return err
}

// PinControlBackupMarker trusts the marker only for the approval that read it.
func (t *Tx) PinControlBackupMarker(ctx context.Context, generation int64, selection, marker string) (bool, error) {
	r, err := t.exec(ctx, `UPDATE control_backup SET destination_marker = ? WHERE singleton = 1
		AND generation = ? AND destination_selection = ? AND destination_marker = ''`, marker, generation, selection)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

// DismissControlBackupPlace hides an earlier destination after owner approval.
func (t *Tx) DismissControlBackupPlace(ctx context.Context, pin string, now time.Time) (bool, error) {
	r, err := t.exec(ctx, `UPDATE control_backup_places SET dismissed = 1 WHERE place = ?
		AND dismissed = 0 AND sent_at > 0 AND generation = (SELECT generation FROM control_backup WHERE singleton = 1)
		AND target <> (SELECT destination_pin FROM control_backup WHERE singleton = 1)`, "offsite:"+pin)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	_, err = t.exec(ctx, `UPDATE control_backup SET position = position + 1, changed_at = ?,
		first_changed_at = CASE WHEN first_changed_at = 0 THEN ? ELSE first_changed_at END WHERE singleton = 1`, millis(now), millis(now))
	return err == nil, err
}

// SetControlBackupServers stores preferred and excluded server IDs.
func (t *Tx) SetControlBackupServers(ctx context.Context, preferred, excluded []string, now time.Time) error {
	_, err := t.exec(ctx, `UPDATE control_backup SET preferred_servers = ?, excluded_servers = ?, position = position + 1,
		changed_at = ?, first_changed_at = CASE WHEN first_changed_at = 0 THEN ? ELSE first_changed_at END
		WHERE singleton = 1`, strings.Join(preferred, ","), strings.Join(excluded, ","), millis(now), millis(now))
	return err
}

// RequestControlBackup queues a backup within its normal time limits.
func (t *Tx) RequestControlBackup(ctx context.Context) error {
	_, err := t.exec(ctx, `UPDATE control_backup SET requested = 1 WHERE singleton = 1`)
	return err
}

// BeginControlBackupAttempt records a start and consumes any queued request.
func (t *Tx) BeginControlBackupAttempt(ctx context.Context, at time.Time) error {
	_, err := t.exec(ctx, `UPDATE control_backup SET last_attempt = ?, requested = 0 WHERE singleton = 1`, millis(at))
	return err
}

// RecordControlBackupAttempt advances the schedule to the snapshot position.
func (t *Tx) RecordControlBackupAttempt(ctx context.Context, position int64, at time.Time, hourly bool) error {
	_, err := t.exec(ctx, `UPDATE control_backup SET last_attempt = ?, last_position = ?, hourly = ?, attempt_problem = '',
		first_changed_at = CASE WHEN position = ? THEN 0 ELSE first_changed_at END WHERE singleton = 1`,
		millis(at), position, boolInt(hourly), position)
	return err
}

// RecordControlBackupProblem records why the last snapshot could not be sealed.
func (t *Tx) RecordControlBackupProblem(ctx context.Context, problem string) error {
	_, err := t.exec(ctx, `UPDATE control_backup SET attempt_problem = ? WHERE singleton = 1`, problem)
	return err
}

// ControlBackupPlace records each place independently. A failed send preserves
// the time of its latest complete bundle at the same location.
type ControlBackupPlace struct {
	Place, Target, Problem, RecoveryID, Path, File, Name string
	RetentionState                                       string
	PreviousPending                                      bool
	Generation                                           int64
	SentAt                                               time.Time
}

// ControlBackupPlaces reads delivery history for a generation.
func (t *Tx) ControlBackupPlaces(ctx context.Context, generation int64) ([]ControlBackupPlace, error) {
	rows, err := t.query(ctx, `SELECT place, target, sent_at, problem, recovery_id, path, file, name, previous_pending FROM control_backup_places
		WHERE generation = ? AND dismissed = 0 ORDER BY place`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ControlBackupPlace
	for rows.Next() {
		p := ControlBackupPlace{Generation: generation}
		var sent int64
		var pending int
		if err := rows.Scan(&p.Place, &p.Target, &sent, &p.Problem, &p.RecoveryID, &p.Path, &p.File, &p.Name, &pending); err != nil {
			return nil, err
		}
		if sent != 0 {
			p.SentAt = fromMillis(sent)
		}
		p.PreviousPending = pending != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// ControlBackupRetention reads the retry state for a single destination and generation.
func (t *Tx) ControlBackupRetention(ctx context.Context, generation int64, place string) (state, recoveryID string, previous bool, err error) {
	var pending int
	err = t.queryRow(ctx, `SELECT retention_state, recovery_id, previous_pending FROM control_backup_places
		WHERE generation = ? AND place = ?`, generation, place).Scan(&state, &recoveryID, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return state, recoveryID, pending != 0, err
}

// RecordControlBackupPlace records a delivery or refusal at one destination.
func (t *Tx) RecordControlBackupPlace(ctx context.Context, p ControlBackupPlace) error {
	var sent int64
	if !p.SentAt.IsZero() {
		sent = millis(p.SentAt)
	}
	_, err := t.exec(ctx, `INSERT INTO control_backup_places (place, generation, target, sent_at, problem, recovery_id, path, file, name, previous_pending, retention_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (generation, place) DO UPDATE SET
		target = excluded.target, problem = excluded.problem, name = excluded.name,
		previous_pending = CASE WHEN excluded.sent_at = 0 AND control_backup_places.target = excluded.target
			THEN control_backup_places.previous_pending ELSE excluded.previous_pending END,
		retention_state = CASE WHEN excluded.sent_at = 0 AND control_backup_places.target = excluded.target
			THEN control_backup_places.retention_state ELSE excluded.retention_state END,
		sent_at = CASE WHEN excluded.sent_at = 0
			AND control_backup_places.target = excluded.target THEN control_backup_places.sent_at ELSE excluded.sent_at END,
		recovery_id = CASE WHEN excluded.sent_at = 0
			AND control_backup_places.target = excluded.target THEN control_backup_places.recovery_id ELSE excluded.recovery_id END,
		path = CASE WHEN excluded.sent_at = 0
			AND control_backup_places.target = excluded.target THEN control_backup_places.path ELSE excluded.path END,
		file = CASE WHEN excluded.sent_at = 0
			AND control_backup_places.target = excluded.target THEN control_backup_places.file ELSE excluded.file END`,
		p.Place, p.Generation, p.Target, sent, p.Problem, p.RecoveryID, p.Path, p.File, p.Name, boolInt(p.PreviousPending), p.RetentionState)
	return err
}
