// SPDX-License-Identifier: AGPL-3.0-only

// Package store keeps the control plane's state in SQLite (ADR 0004).
//
// SQLite allows one writer at a time, so the store keeps two pools: a
// single connection for writes, whose transactions begin IMMEDIATE so they
// never deadlock upgrading a read lock, and a pool of readers, which WAL
// mode lets run alongside the writer. Callers use View for reads and
// Update for writes; both run fn in a transaction.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// Errors callers can test for.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

// DB is an open store.
type DB struct {
	write *sql.DB
	read  *sql.DB
}

// Open opens or creates the database at path and brings its schema up to
// date. It refuses a database written by a newer version of Dockwell.
func Open(ctx context.Context, file string) (*DB, error) {
	// Create the file owner-only before SQLite does; its WAL and shared
	// memory files then take the same permissions.
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the operator's data directory
	if err != nil {
		return nil, fmt.Errorf("can't open the database at %s: %w", file, err)
	}
	_ = f.Close()
	write, err := open(file, "immediate", 1)
	if err != nil {
		return nil, err
	}
	read, err := open(file, "deferred", max(4, runtime.GOMAXPROCS(0)))
	if err != nil {
		_ = write.Close()
		return nil, err
	}
	db := &DB{write: write, read: read}
	if err := db.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func open(file, txlock string, conns int) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range []string{
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"foreign_keys(ON)",
		"busy_timeout(5000)",
	} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", txlock)
	dsn := "file:" + file + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("can't open the database at %s: %w", file, err)
	}
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	db.SetConnMaxLifetime(0)
	return db, nil
}

// Close closes both pools.
func (db *DB) Close() error {
	return errors.Join(db.write.Close(), db.read.Close())
}

// Tx is a transaction. Its methods are the store's queries.
type Tx struct {
	tx *sql.Tx
}

// View runs fn in a read-only transaction.
func (db *DB) View(ctx context.Context, fn func(*Tx) error) error {
	tx, err := db.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("can't begin a read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(&Tx{tx: tx})
}

// Update runs fn in a write transaction and commits it if fn returns nil.
func (db *DB) Update(ctx context.Context, fn func(*Tx) error) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("can't begin a write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(&Tx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("can't commit: %w", err)
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s must be named NNNN_description.sql", e.Name())
		}
		body, err := fs.ReadFile(migrations, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1, 2, 3… with no gaps; %s is out of order", m.name)
		}
	}
	return out, nil
}

// SchemaVersion returns the newest migration this binary knows.
func SchemaVersion() int {
	ms, err := loadMigrations()
	if err != nil {
		return 0
	}
	return len(ms)
}

func (db *DB) migrate(ctx context.Context) error {
	all, err := loadMigrations()
	if err != nil {
		return err
	}
	return db.Update(ctx, func(t *Tx) error {
		if _, err := t.tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
			return fmt.Errorf("can't prepare migrations: %w", err)
		}
		var current int
		if err := t.tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
			return fmt.Errorf("can't read the schema version: %w", err)
		}
		if current > len(all) {
			return fmt.Errorf("the database has schema version %d, newer than this dockwell knows (%d). "+
				"Run a newer dockwell, or restore a backup taken before the upgrade", current, len(all))
		}
		for _, m := range all[current:] {
			if _, err := t.tx.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %s failed: %w", m.name, err)
			}
			if _, err := t.tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, millis(time.Now())); err != nil {
				return err
			}
		}
		return nil
	})
}

func millis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func nullMillis(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func timePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromMillis(n.Int64)
	return &t
}

// isUniqueViolation reports a UNIQUE or PRIMARY KEY constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
