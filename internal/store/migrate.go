// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migrations live in migrations/, one version each:
//
//	NNNN_name.sql           portable SQL, run on both databases
//	NNNN_name.sqlite.sql    run on SQLite only, after the portable part
//	NNNN_name.postgres.sql  run on Postgres only, after the portable part
//
// Portable SQL is written for SQLite with the column types INTEGER, BLOB
// and TEXT, which Postgres gets as BIGINT, BYTEA and TEXT COLLATE "C" (see
// postgresSchema). What can't be said once, such as a trigger, goes in the
// dialect files, which come in pairs so neither database is forgotten.
//
//go:embed migrations/*.sql
var migrations embed.FS

type migration struct {
	version  int
	name     string // NNNN_name
	portable string
	sqlite   string
	postgres string
	// pair counts the dialect files seen, which must be zero or two.
	pair int
}

// sql returns what the migration runs on d.
func (m migration) sql(d *dialect) string {
	out := d.schema(m.portable)
	if d.backend == Postgres {
		return out + "\n" + m.postgres
	}
	return out + "\n" + m.sqlite
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("can't list the migrations built into dockwell: %w", err)
	}
	byVersion := map[int]*migration{}
	for _, e := range entries {
		file := e.Name()
		base, dialectName := strings.TrimSuffix(file, ".sql"), ""
		for _, d := range []string{".sqlite", ".postgres"} {
			if strings.HasSuffix(base, d) {
				base, dialectName = strings.TrimSuffix(base, d), d[1:]
			}
		}
		prefix, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 || !strings.HasSuffix(file, ".sql") {
			return nil, fmt.Errorf("migration %s must be named NNNN_description.sql, .sqlite.sql or .postgres.sql", file)
		}
		body, err := fs.ReadFile(migrations, path.Join("migrations", file))
		if err != nil {
			return nil, fmt.Errorf("can't read migration %s: %w", file, err)
		}
		m := byVersion[v]
		if m == nil {
			m = &migration{version: v, name: base}
			byVersion[v] = m
		}
		if m.name != base {
			return nil, fmt.Errorf("migrations %s and %s share version %d; give one the next number", m.name, base, v)
		}
		switch dialectName {
		case "sqlite":
			m.sqlite, m.pair = string(body), m.pair+1
		case "postgres":
			m.postgres, m.pair = string(body), m.pair+1
		default:
			m.portable = string(body)
		}
	}
	out := make([]migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.pair == 1 {
			return nil, fmt.Errorf("migration %s has a file for one database only; add the other's, even if it only says why it has nothing to do", m.name)
		}
		out = append(out, *m)
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
	return db.migrateTo(ctx, all, len(all))
}

// migrateTo applies migrations up to version target, in one transaction.
func (db *DB) migrateTo(ctx context.Context, all []migration, target int) error {
	d := db.dialect
	return db.update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, d.schema(`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`)); err != nil {
			return fmt.Errorf("can't prepare migrations on %s: %w", d.backend, err)
		}
		var current int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
			return fmt.Errorf("can't read the schema version on %s: %w", d.backend, err)
		}
		if current > len(all) {
			return fmt.Errorf("the database has schema version %d, newer than this dockwell knows (%d). "+
				"Run a newer dockwell, or restore a backup taken before the upgrade", current, len(all))
		}
		for _, m := range all[current:target] {
			if _, err := tx.ExecContext(ctx, m.sql(d)); err != nil {
				return fmt.Errorf("migration %s failed on %s: %w", m.name, d.backend, err)
			}
			if _, err := tx.ExecContext(ctx, d.rebind(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`),
				m.version, millis(time.Now())); err != nil {
				return fmt.Errorf("migration %s ran on %s, but can't be recorded as done: %w", m.name, d.backend, err)
			}
		}
		return nil
	})
}
