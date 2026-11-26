// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// ControlSnapshot identifies the captured installation, schema, and backup settings.
type ControlSnapshot struct {
	Schema  int
	Install string
	Backup  ControlBackup
}

// SnapshotControl fills an empty private file with a consistent SQLite copy.
func (db *DB) SnapshotControl(ctx context.Context, path string) (ControlSnapshot, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // private backup staging directory
	if err != nil {
		return ControlSnapshot{}, err
	}
	if err := f.Close(); err != nil {
		return ControlSnapshot{}, err
	}
	if db.Backend() == Postgres {
		if err := db.copyControlToSQLite(ctx, path); err != nil {
			return ControlSnapshot{}, err
		}
	} else {
		if _, err := db.read.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
			return ControlSnapshot{}, fmt.Errorf("copy the SQLite store: %w", err)
		}
		copyDB, err := openControlSnapshot(path, false)
		if err != nil {
			return ControlSnapshot{}, err
		}
		defer copyDB.Close()
		if _, err := copyDB.ExecContext(ctx, `PRAGMA secure_delete = ON;
			DELETE FROM history_chunks; DELETE FROM history_series; DELETE FROM server_history; VACUUM`); err != nil {
			return ControlSnapshot{}, fmt.Errorf("remove history from the snapshot: %w", err)
		}
	}
	return VerifyControlSnapshot(ctx, path)
}

func openControlSnapshot(path string, readOnly bool) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	if readOnly {
		u.RawQuery = "mode=ro"
	}
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

// VerifyControlSnapshot checks a snapshot without opening or migrating it as a live store.
func VerifyControlSnapshot(ctx context.Context, path string) (ControlSnapshot, error) {
	db, err := openControlSnapshot(path, true)
	if err != nil {
		return ControlSnapshot{}, err
	}
	defer db.Close()
	var check string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return ControlSnapshot{}, err
	}
	if check != "ok" {
		return ControlSnapshot{}, fmt.Errorf("snapshot failed the SQLite integrity check: %s", check)
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return ControlSnapshot{}, err
	}
	if rows.Next() {
		_ = rows.Close()
		return ControlSnapshot{}, errors.New("snapshot failed the SQLite foreign key check")
	}
	if err := rows.Err(); err != nil {
		return ControlSnapshot{}, err
	}
	var s ControlSnapshot
	if err := db.QueryRowContext(ctx, `SELECT MAX(version),
		COALESCE((SELECT CAST(value AS TEXT) FROM settings WHERE key = 'install.id'), '') FROM schema_migrations`).Scan(&s.Schema, &s.Install); err != nil {
		return s, err
	}
	s.Backup, err = scanControlBackup(db.QueryRowContext(ctx, "SELECT "+controlBackupColumns+" FROM control_backup WHERE singleton = 1"))
	return s, err
}

func (db *DB) copyControlToSQLite(ctx context.Context, path string) error {
	dst, err := openControlSnapshot(path, false)
	if err != nil {
		return err
	}
	defer dst.Close()
	return db.View(ctx, func(src *Tx) error {
		var version int
		if err := src.queryRow(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
			return err
		}
		all, err := loadMigrations()
		if err != nil {
			return err
		}
		if version < 58 || version > len(all) {
			return fmt.Errorf("can't snapshot store schema %d", version)
		}
		tx, err := dst.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, "CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
			return err
		}
		for _, m := range all[:version] {
			if _, err := tx.ExecContext(ctx, m.sql(sqliteDialect)); err != nil {
				return fmt.Errorf("build snapshot schema at %s: %w", m.name, err)
			}
		}
		// Foreign keys stay off while copying: revisions and resources refer
		// to one another. VerifyControlSnapshot checks every key afterwards.
		rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
			ORDER BY (name = 'control_backup'), name`)
		if err != nil {
			return err
		}
		var tables []string
		for rows.Next() {
			var table string
			if err := rows.Scan(&table); err != nil {
				_ = rows.Close()
				return err
			}
			tables = append(tables, table)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, table := range tables {
			switch table {
			case "history_chunks", "history_series", "server_history":
				continue
			}
			if table == "control_backup" {
				if _, err := tx.ExecContext(ctx, "DELETE FROM control_backup"); err != nil {
					return err
				}
			}
			if err := copyControlTable(ctx, src, tx, table); err != nil {
				return fmt.Errorf("copy snapshot table %s: %w", table, err)
			}
		}
		return tx.Commit()
	})
}

func identifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func copyControlTable(ctx context.Context, src *Tx, dst *sql.Tx, table string) error {
	// Read the SQLite schema's columns so the copy never depends on the order
	// Postgres and SQLite chose while migrating a table.
	rows, err := dst.QueryContext(ctx, "SELECT * FROM "+identifier(table)+" LIMIT 0") //nolint:gosec // quoted name from the snapshot schema
	if err != nil {
		return err
	}
	columns, err := rows.Columns()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for i := range columns {
		columns[i] = identifier(columns[i])
	}
	names := strings.Join(columns, ", ")
	rows, err = src.query(ctx, "SELECT "+names+" FROM "+identifier(table))
	if err != nil {
		return err
	}
	defer rows.Close()
	write, err := dst.PrepareContext(ctx, "INSERT INTO "+identifier(table)+" ("+names+") VALUES (?"+strings.Repeat(", ?", len(columns)-1)+")") //nolint:gosec // only quoted schema identifiers are joined
	if err != nil {
		return err
	}
	defer write.Close()
	values, into := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		into[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(into...); err != nil {
			return err
		}
		if _, err := write.ExecContext(ctx, values...); err != nil {
			return err
		}
	}
	return rows.Err()
}
