// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KeyEncryptionKey wraps data keys. It is kept sealed with the install's
// key, which is never in the database. See
// docs/adr/0022-secrets-encrypted-at-rest.md.
type KeyEncryptionKey struct {
	ID        string
	Sealed    []byte // empty once retired
	CreatedAt time.Time
	RetiredAt *time.Time
}

// DataKey seals one stored spec. It is kept wrapped under a key-encryption
// key.
type DataKey struct {
	ID        string
	KEKID     string
	Wrapped   []byte
	CreatedAt time.Time
}

func scanKEK(row interface{ Scan(...any) error }) (KeyEncryptionKey, error) {
	var k KeyEncryptionKey
	var created int64
	var retired sql.NullInt64
	if err := row.Scan(&k.ID, &k.Sealed, &created, &retired); err != nil {
		return KeyEncryptionKey{}, err
	}
	k.CreatedAt, k.RetiredAt = fromMillis(created), timePtr(retired)
	return k, nil
}

// CreateKEK records a new key-encryption key, which wraps new data keys
// from now on.
func (t *Tx) CreateKEK(ctx context.Context, k KeyEncryptionKey) error {
	if _, err := t.exec(ctx, `INSERT INTO key_encryption_keys (id, sealed, created_at) VALUES (?, ?, ?)`,
		k.ID, k.Sealed, millis(k.CreatedAt)); err != nil {
		return fmt.Errorf("can't record key-encryption key %s: %w", k.ID, err)
	}
	return nil
}

// ActiveKEK returns the key-encryption key that wraps new data keys: the
// newest one that isn't retired. ErrNotFound means there is none yet.
func (t *Tx) ActiveKEK(ctx context.Context) (KeyEncryptionKey, error) {
	k, err := scanKEK(t.queryRow(ctx, `SELECT id, sealed, created_at, retired_at FROM key_encryption_keys
		WHERE retired_at IS NULL ORDER BY created_at DESC, id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return KeyEncryptionKey{}, ErrNotFound
	}
	if err != nil {
		return KeyEncryptionKey{}, fmt.Errorf("can't read the key-encryption key: %w", err)
	}
	return k, nil
}

// LiveKEKs returns the key-encryption keys that aren't retired, oldest
// first. There is one, but for an instant during a rotation.
func (t *Tx) LiveKEKs(ctx context.Context) ([]KeyEncryptionKey, error) {
	rows, err := t.query(ctx, `SELECT id, sealed, created_at, retired_at FROM key_encryption_keys
		WHERE retired_at IS NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("can't list the key-encryption keys: %w", err)
	}
	defer rows.Close()
	var out []KeyEncryptionKey
	for rows.Next() {
		k, err := scanKEK(rows)
		if err != nil {
			return nil, fmt.Errorf("can't list the key-encryption keys: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetKEK returns a key-encryption key, or ErrNotFound.
func (t *Tx) GetKEK(ctx context.Context, id string) (KeyEncryptionKey, error) {
	k, err := scanKEK(t.queryRow(ctx, `SELECT id, sealed, created_at, retired_at FROM key_encryption_keys WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return KeyEncryptionKey{}, ErrNotFound
	}
	if err != nil {
		return KeyEncryptionKey{}, fmt.Errorf("can't read key-encryption key %s: %w", id, err)
	}
	return k, nil
}

// RetireKEK retires a key-encryption key once no data key is wrapped under
// it, and forgets it: its sealed copy is emptied.
func (t *Tx) RetireKEK(ctx context.Context, id string, at time.Time) error {
	var left int
	if err := t.queryRow(ctx, `SELECT COUNT(*) FROM data_keys WHERE kek_id = ?`, id).Scan(&left); err != nil {
		return fmt.Errorf("can't count the data keys under %s: %w", id, err)
	}
	if left > 0 {
		return fmt.Errorf("can't retire key-encryption key %s: %d data keys are still wrapped under it", id, left)
	}
	res, err := t.exec(ctx, `UPDATE key_encryption_keys SET sealed = ?, retired_at = ? WHERE id = ? AND retired_at IS NULL`,
		[]byte{}, millis(at), id)
	if err != nil {
		return fmt.Errorf("can't retire key-encryption key %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("can't retire key-encryption key %s: %w", id, ErrNotFound)
	}
	return nil
}

// CreateDataKey records a data key, wrapped.
func (t *Tx) CreateDataKey(ctx context.Context, d DataKey) error {
	if _, err := t.exec(ctx, `INSERT INTO data_keys (id, kek_id, wrapped, created_at) VALUES (?, ?, ?, ?)`,
		d.ID, d.KEKID, d.Wrapped, millis(d.CreatedAt)); err != nil {
		return fmt.Errorf("can't record data key %s: %w", d.ID, err)
	}
	return nil
}

func scanDataKey(row interface{ Scan(...any) error }) (DataKey, error) {
	var d DataKey
	var created int64
	if err := row.Scan(&d.ID, &d.KEKID, &d.Wrapped, &created); err != nil {
		return DataKey{}, err
	}
	d.CreatedAt = fromMillis(created)
	return d, nil
}

// GetDataKey returns a data key, or ErrNotFound.
func (t *Tx) GetDataKey(ctx context.Context, id string) (DataKey, error) {
	d, err := scanDataKey(t.queryRow(ctx, `SELECT id, kek_id, wrapped, created_at FROM data_keys WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return DataKey{}, ErrNotFound
	}
	if err != nil {
		return DataKey{}, fmt.Errorf("can't read data key %s: %w", id, err)
	}
	return d, nil
}

// DataKeysUnder returns up to limit data keys wrapped under a
// key-encryption key, by ID, after the ID given.
func (t *Tx) DataKeysUnder(ctx context.Context, kekID, after string, limit int) ([]DataKey, error) {
	rows, err := t.query(ctx, `SELECT id, kek_id, wrapped, created_at FROM data_keys WHERE kek_id = ? AND id > ? ORDER BY id LIMIT ?`,
		kekID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("can't list the data keys under %s: %w", kekID, err)
	}
	defer rows.Close()
	var out []DataKey
	for rows.Next() {
		d, err := scanDataKey(rows)
		if err != nil {
			return nil, fmt.Errorf("can't list the data keys under %s: %w", kekID, err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RewrapDataKey stores a data key wrapped under another key-encryption
// key. The data key itself, and so everything it sealed, stays the same.
func (t *Tx) RewrapDataKey(ctx context.Context, id, kekID string, wrapped []byte) error {
	res, err := t.exec(ctx, `UPDATE data_keys SET kek_id = ?, wrapped = ? WHERE id = ?`, kekID, wrapped, id)
	if err != nil {
		return fmt.Errorf("can't wrap data key %s again: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("can't wrap data key %s again: %w", id, ErrNotFound)
	}
	return nil
}
