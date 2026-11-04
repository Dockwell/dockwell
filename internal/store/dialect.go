// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
)

// dialect is everything the store does differently on SQLite and Postgres.
// The queries themselves are shared: they are written for SQLite, with ?
// placeholders, in SQL both databases read the same way.
type dialect struct {
	backend Backend
	// readOptions begin a read: a snapshot that sees one state throughout.
	readOptions *sql.TxOptions
	// lockWrites runs first in every write, so writes queue one at a time.
	lockWrites func(context.Context, *sql.Tx) error
	// uniqueViolation reports a UNIQUE or PRIMARY KEY constraint failure.
	uniqueViolation func(error) bool
	// survive runs a statement that may fail on a constraint so that, if
	// it does, the transaction goes on, as it does on SQLite: Postgres
	// aborts a transaction at its first failed statement unless a
	// savepoint undoes just that one.
	survive func(ctx context.Context, tx *sql.Tx, fn func() error) error
	// rebind rewrites a query's placeholders for the driver.
	rebind func(string) string
	// schema turns a portable migration into this dialect's SQL.
	schema func(string) string
}

// sqliteDialect relies on BEGIN IMMEDIATE on the single writing
// connection, which already makes writes wait for each other, and on WAL
// mode, which gives a deferred read transaction a snapshot from its first
// read.
var sqliteDialect = &dialect{
	backend:     SQLite,
	readOptions: &sql.TxOptions{ReadOnly: true},
	lockWrites:  func(context.Context, *sql.Tx) error { return nil },
	uniqueViolation: func(err error) bool {
		return strings.Contains(err.Error(), "UNIQUE constraint failed")
	},
	survive: func(_ context.Context, _ *sql.Tx, fn func() error) error { return fn() },
	rebind:  func(q string) string { return q },
	schema:  func(q string) string { return q },
}

// postgresDialect serializes writes with a transaction-level advisory lock
// for the schema, which Postgres releases at commit or rollback. Writes run
// at READ COMMITTED, so each statement sees what the writer before them
// committed; taking the lock first is what makes that equal to SQLite's
// IMMEDIATE. Reads run at REPEATABLE READ, the snapshot SQLite's readers
// get, and being read-only they never fail to serialize.
func postgresDialect(schema string) *dialect {
	h := fnv.New64a()
	_, _ = h.Write([]byte("dockwell store writes in " + schema))
	key := int64(h.Sum64()) //nolint:gosec // any 64 bits will do as a lock key
	return &dialect{
		backend:     Postgres,
		readOptions: &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true},
		lockWrites: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
				return fmt.Errorf("can't take the write lock on postgres: %w", err)
			}
			return nil
		},
		uniqueViolation: func(err error) bool {
			var pg *pgconn.PgError
			return errors.As(err, &pg) && pg.Code == "23505"
		},
		survive: savepoint,
		rebind:  cachedRebind,
		schema:  postgresSchema,
	}
}

func savepoint(ctx context.Context, tx *sql.Tx, fn func() error) error {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT dockwell_survive`); err != nil {
		return fmt.Errorf("can't set a savepoint on postgres: %w", err)
	}
	if err := fn(); err != nil {
		if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT dockwell_survive`); rerr != nil {
			return errors.Join(err, fmt.Errorf("can't roll back to the savepoint on postgres, so the transaction can't go on: %w", rerr))
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT dockwell_survive`); err != nil {
		return fmt.Errorf("can't release the savepoint on postgres: %w", err)
	}
	return nil
}

var rebinds sync.Map // query → rebound query; queries are constants, so this stays small

func cachedRebind(q string) string {
	if v, ok := rebinds.Load(q); ok {
		return v.(string) //nolint:forcetypeassert // only strings are stored
	}
	out := rebindDollar(q)
	rebinds.Store(q, out)
	return out
}

// rebindDollar rewrites SQLite's placeholders as Postgres's: ? becomes $1,
// $2… in order, and ?NNN becomes $NNN. As in SQLite, a bare ? after ?NNN
// takes the next number after the largest so far. Quoted strings,
// identifiers and comments are left alone.
func rebindDollar(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	highest := 0
	for i := 0; i < len(q); {
		if n := skipQuotedOrComment(q, i); n > i {
			b.WriteString(q[i:n])
			i = n
			continue
		}
		if q[i] != '?' {
			b.WriteByte(q[i])
			i++
			continue
		}
		j := i + 1
		for j < len(q) && q[j] >= '0' && q[j] <= '9' {
			j++
		}
		n := highest + 1
		if j > i+1 {
			n, _ = strconv.Atoi(q[i+1 : j])
		}
		highest = max(highest, n)
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
		i = j
	}
	return b.String()
}

// skipQuotedOrComment returns the end of the quoted string, quoted
// identifier or comment starting at q[i], or i if none starts there.
func skipQuotedOrComment(q string, i int) int {
	switch {
	case q[i] == '\'' || q[i] == '"':
		quote := q[i]
		for j := i + 1; j < len(q); j++ {
			if q[j] != quote {
				continue
			}
			if j+1 < len(q) && q[j+1] == quote { // a doubled quote is an escaped one
				j++
				continue
			}
			return j + 1
		}
		return len(q)
	case strings.HasPrefix(q[i:], "--"):
		if end := strings.IndexByte(q[i:], '\n'); end >= 0 {
			return i + end
		}
		return len(q)
	case strings.HasPrefix(q[i:], "/*"):
		if end := strings.Index(q[i+2:], "*/"); end >= 0 {
			return i + 2 + end + 2
		}
		return len(q)
	}
	return i
}

// postgresTypes maps the column types portable migrations use to what
// means the same in Postgres. SQLite's integers are 64-bit, and it compares
// text byte by byte, so Postgres gets BIGINT and the "C" collation: then
// ORDER BY and UNIQUE behave the same on both.
var postgresTypes = map[string]string{
	"INTEGER": "BIGINT",
	"BLOB":    "BYTEA",
	"TEXT":    `TEXT COLLATE "C"`,
}

// postgresSchema turns a portable migration, written in SQLite's dialect,
// into Postgres's: it maps the column types in postgresTypes, and blob
// literals (x'00ff') to bytea ones ('\x00ff'). Everything else in a
// portable migration must already mean the same on both; what can't goes
// in the migration's pair of dialect files.
func postgresSchema(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 64)
	for i := 0; i < len(q); {
		if (q[i] == 'x' || q[i] == 'X') && i+1 < len(q) && q[i+1] == '\'' && !wordByte(prev(q, i)) {
			end := skipQuotedOrComment(q, i+1)
			b.WriteString(`'\x`)
			b.WriteString(q[i+2 : end])
			i = end
			continue
		}
		if n := skipQuotedOrComment(q, i); n > i {
			b.WriteString(q[i:n])
			i = n
			continue
		}
		if !wordByte(q[i]) {
			b.WriteByte(q[i])
			i++
			continue
		}
		j := i
		for j < len(q) && wordByte(q[j]) {
			j++
		}
		word := q[i:j]
		if to, ok := postgresTypes[strings.ToUpper(word)]; ok {
			word = to
		}
		b.WriteString(word)
		i = j
	}
	return b.String()
}

func prev(q string, i int) byte {
	if i == 0 {
		return ' '
	}
	return q[i-1]
}

func wordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
