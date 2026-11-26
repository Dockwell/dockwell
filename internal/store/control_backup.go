// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"strings"
	"time"
)

// ControlBackup is the schedule and generation of the installation's store.
type ControlBackup struct {
	Generation, Position, LastPosition                    int64
	GenerationID, Destination, DestinationPin, RecoveryID string
	Problem                                               string
	Preferred, Excluded                                   []string
	ChangedAt, FirstChangedAt, LastAttempt                time.Time
	Recovery                                              []byte
	Kept, Hourly, Requested                               bool
}

const controlBackupColumns = `generation, generation_id, position, changed_at, first_changed_at, recovery, recovery_id,
	passphrase_kept, destination, destination_pin, preferred_servers, excluded_servers, last_attempt, last_position, hourly, requested, attempt_problem`

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
		&s.DestinationPin, &preferred, &excluded, &attempt, &s.LastPosition, &hourly, &requested, &s.Problem)
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
func (t *Tx) SetControlBackupDestination(ctx context.Context, dest, pin string, now time.Time) error {
	_, err := t.exec(ctx, `UPDATE control_backup SET destination = ?, destination_pin = ?, position = position + 1,
		changed_at = ?, first_changed_at = CASE WHEN first_changed_at = 0 THEN ? ELSE first_changed_at END
		WHERE singleton = 1`, dest, pin, millis(now), millis(now))
	return err
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
	Place, Target, Problem, RecoveryID, Path, File string
	Generation                                     int64
	SentAt                                         time.Time
}

// ControlBackupPlaces reads delivery history for a generation.
func (t *Tx) ControlBackupPlaces(ctx context.Context, generation int64) ([]ControlBackupPlace, error) {
	rows, err := t.query(ctx, `SELECT place, target, sent_at, problem, recovery_id, path, file FROM control_backup_places
		WHERE generation = ? ORDER BY place`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ControlBackupPlace
	for rows.Next() {
		p := ControlBackupPlace{Generation: generation}
		var sent int64
		if err := rows.Scan(&p.Place, &p.Target, &sent, &p.Problem, &p.RecoveryID, &p.Path, &p.File); err != nil {
			return nil, err
		}
		if sent != 0 {
			p.SentAt = fromMillis(sent)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordControlBackupPlace records a delivery or refusal at one destination.
func (t *Tx) RecordControlBackupPlace(ctx context.Context, p ControlBackupPlace) error {
	var sent int64
	if !p.SentAt.IsZero() {
		sent = millis(p.SentAt)
	}
	_, err := t.exec(ctx, `INSERT INTO control_backup_places (place, generation, target, sent_at, problem, recovery_id, path, file)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (generation, place) DO UPDATE SET
		target = excluded.target, problem = excluded.problem,
		sent_at = CASE WHEN excluded.sent_at = 0
			AND control_backup_places.target = excluded.target THEN control_backup_places.sent_at ELSE excluded.sent_at END,
		recovery_id = CASE WHEN excluded.sent_at = 0
			AND control_backup_places.target = excluded.target THEN control_backup_places.recovery_id ELSE excluded.recovery_id END,
		path = CASE WHEN excluded.sent_at = 0
			AND control_backup_places.target = excluded.target THEN control_backup_places.path ELSE excluded.path END,
		file = CASE WHEN excluded.sent_at = 0
			AND control_backup_places.target = excluded.target THEN control_backup_places.file ELSE excluded.file END`,
		p.Place, p.Generation, p.Target, sent, p.Problem, p.RecoveryID, p.Path, p.File)
	return err
}
