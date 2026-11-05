// SPDX-License-Identifier: AGPL-3.0-only

// Package store keeps the control plane's state, in SQLite by default and
// in Postgres for high availability (ADR 0004). Both run the same queries
// behind one API; what differs between them is in dialect.go. See
// docs/adr/0014-object-model-and-store.md.
//
// Writes are serialized on both. SQLite allows one writer at a time, so
// the store keeps two pools: a single connection for writes, whose
// transactions begin IMMEDIATE so they never deadlock upgrading a read
// lock, and a pool of readers, which WAL mode lets run alongside the
// writer. Postgres gets the same shape: one writing connection per
// process, and a transaction-level advisory lock that every write takes
// first, so writers in other processes and replicas queue as they do on
// SQLite. Reads run in a snapshot on both.
//
// Callers use View for reads and Update for writes; both run fn in a
// transaction. Managed state (changes, revisions and deployments) is
// written only in Revise, which only the revision store calls.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for Postgres
	_ "modernc.org/sqlite"             // registers the pure-Go "sqlite" driver
)

// Errors callers can test for.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

// DB is an open store.
type DB struct {
	write   *sql.DB
	read    *sql.DB
	dialect *dialect
	// where is the store's location for messages, without secrets.
	where string
	// recorded fires when a write that recorded an audit event commits.
	recorded signal
}

// Backend names a kind of database the store runs on.
type Backend string

// Backends.
const (
	SQLite   Backend = "sqlite"
	Postgres Backend = "postgres"
)

// IsPostgres reports whether location names a Postgres database rather
// than a SQLite file.
func IsPostgres(location string) bool {
	scheme, _, ok := strings.Cut(location, "://")
	return ok && (scheme == "postgres" || scheme == "postgresql")
}

// Redact returns location fit for logs and messages: a Postgres URL
// without its password, or a SQLite path as it is.
func Redact(location string) string {
	if !IsPostgres(location) {
		return location
	}
	u, err := url.Parse(location)
	if err != nil {
		// Never echo a URL that doesn't parse: it may hold a password
		// anywhere.
		return "a postgres:// URL that doesn't parse"
	}
	q := u.Query()
	for k := range q {
		if strings.Contains(strings.ToLower(k), "password") {
			q.Set(k, "xxxxx")
		}
	}
	u.RawQuery = q.Encode()
	return u.Redacted()
}

// Open opens the store at location and brings its schema up to date. A
// postgres:// or postgresql:// URL opens that Postgres database; anything
// else is the path of a SQLite file, created if need be. Open refuses a
// database written by a newer version of Dockwell.
func Open(ctx context.Context, location string) (*DB, error) {
	db, err := open(ctx, location)
	if err != nil {
		return nil, err
	}
	if err := db.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// OpenExisting opens the store at location as it is, for a command that
// runs beside the control plane that keeps it, such as the CLI's local
// access: it never creates the database and never migrates it, since a
// migration under a running control plane of another version would change
// its schema beneath it. It refuses a database whose schema version isn't
// this binary's with a *SchemaError, as on a server whose dockwell was
// upgraded before the control plane restarted onto it.
func OpenExisting(ctx context.Context, location string) (*DB, error) {
	if !IsPostgres(location) {
		if _, err := os.Stat(location); err != nil {
			return nil, fmt.Errorf("can't open the database at %s: %w", location, err)
		}
	}
	db, err := open(ctx, location)
	if err != nil {
		return nil, err
	}
	var have int
	if err := db.read.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&have); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("can't read the schema version of the database at %s: %w. Start the control plane on it with dockwell run, which sets it up, then try again",
			db.where, scrub(err, location))
	}
	if want := SchemaVersion(); have != want {
		_ = db.Close()
		return nil, &SchemaError{Where: db.where, Database: have, Binary: want}
	}
	return db, nil
}

// SchemaError is OpenExisting's refusal of a database whose schema version
// isn't the one this binary knows.
type SchemaError struct {
	// Where is the database, without secrets.
	Where string
	// Database is the schema version the database has, and Binary the one
	// this binary knows.
	Database, Binary int
}

func (e *SchemaError) Error() string {
	if e.Database < e.Binary {
		return fmt.Sprintf("the database at %s has schema version %d, older than this dockwell's %d, so the control plane on it runs an older dockwell. "+
			"Restart the control plane with this one, which brings the database up to date, then try again", e.Where, e.Database, e.Binary)
	}
	return fmt.Sprintf("the database at %s has schema version %d, newer than this dockwell knows (%d). Run the dockwell the control plane runs",
		e.Where, e.Database, e.Binary)
}

// open opens the store at location without migrating it.
func open(ctx context.Context, location string) (*DB, error) {
	if IsPostgres(location) {
		return openPostgres(ctx, location)
	}
	return openSQLite(location)
}

func openSQLite(file string) (*DB, error) {
	// Create the file owner-only before SQLite does; its WAL and shared
	// memory files then take the same permissions.
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the operator's data directory
	if err != nil {
		return nil, fmt.Errorf("can't open the database at %s: %w", file, err)
	}
	_ = f.Close()
	open := func(txlock string, conns int) (*sql.DB, error) {
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
		db, err := sql.Open("sqlite", "file:"+file+"?"+q.Encode())
		if err != nil {
			return nil, fmt.Errorf("can't open the database at %s: %w", file, err)
		}
		db.SetMaxOpenConns(conns)
		db.SetMaxIdleConns(conns)
		db.SetConnMaxLifetime(0)
		return db, nil
	}
	write, err := open("immediate", 1)
	if err != nil {
		return nil, err
	}
	read, err := open("deferred", readers())
	if err != nil {
		_ = write.Close()
		return nil, err
	}
	return &DB{write: write, read: read, dialect: sqliteDialect, where: file}, nil
}

func openPostgres(ctx context.Context, location string) (*DB, error) {
	where := Redact(location)
	open := func(conns int) (*sql.DB, error) {
		db, err := sql.Open("pgx", location)
		if err != nil {
			return nil, fmt.Errorf("can't use the Postgres database at %s: %w", where, scrub(err, location))
		}
		db.SetMaxOpenConns(conns)
		db.SetMaxIdleConns(conns)
		// Postgres, or a proxy in front of it, may close idle connections.
		db.SetConnMaxIdleTime(5 * time.Minute)
		return db, nil
	}
	write, err := open(1)
	if err != nil {
		return nil, err
	}
	read, err := open(readers())
	if err != nil {
		_ = write.Close()
		return nil, err
	}
	db := &DB{write: write, read: read, where: where}
	// The write lock belongs to the schema the store lives in, so two
	// installs sharing a database, in different schemas, don't queue
	// behind each other.
	var schema string
	if err := write.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("can't reach the Postgres database at %s: %w. Check that it is running and that the URL, user and password are right",
			where, scrub(err, location))
	}
	db.dialect = postgresDialect(schema)
	return db, nil
}

// readers is the size of the read pool.
func readers() int { return max(4, runtime.GOMAXPROCS(0)) }

// scrub removes location from an error's text, in case a driver quoted it,
// password and all.
func scrub(err error, location string) error {
	if err == nil || !strings.Contains(err.Error(), location) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), location, Redact(location)))
}

// Backend reports which kind of database the store runs on.
func (db *DB) Backend() Backend { return db.dialect.backend }

// Where reports the store's location, without secrets.
func (db *DB) Where() string { return db.where }

// Close closes both pools.
func (db *DB) Close() error {
	return errors.Join(db.write.Close(), db.read.Close())
}

// Tx is a transaction. Its methods are the store's queries.
type Tx struct {
	tx      *sql.Tx
	dialect *dialect
	// recorded is set once a write records an audit event; nil in a read.
	recorded *bool
}

func (t *Tx) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.dialect.rebind(query), args...)
}

func (t *Tx) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	counted(ctx)
	return t.tx.QueryContext(ctx, t.dialect.rebind(query), args...)
}

func (t *Tx) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	counted(ctx)
	return t.tx.QueryRowContext(ctx, t.dialect.rebind(query), args...)
}

type queryCounter struct{}

// CountingQueries returns a context whose reads count their queries in n, for
// a test that holds down what a screen reads each time a change moves.
func CountingQueries(ctx context.Context, n *atomic.Int64) context.Context {
	return context.WithValue(ctx, queryCounter{}, n)
}

func counted(ctx context.Context) {
	if n, ok := ctx.Value(queryCounter{}).(*atomic.Int64); ok {
		n.Add(1)
	}
}

// unique reports a UNIQUE or PRIMARY KEY constraint failure.
func (t *Tx) unique(err error) bool { return err != nil && t.dialect.uniqueViolation(err) }

// insert runs an INSERT that may break a UNIQUE constraint. If it does,
// the transaction stays usable on both databases, so a caller that gets
// ErrConflict can try again, such as with another name.
func (t *Tx) insert(ctx context.Context, query string, args ...any) error {
	return t.dialect.survive(ctx, t.tx, func() error {
		_, err := t.exec(ctx, query, args...)
		return err
	})
}

// View runs fn in a read-only transaction, which sees one snapshot of the
// store throughout.
func (db *DB) View(ctx context.Context, fn func(*Tx) error) error {
	tx, err := db.read.BeginTx(ctx, db.dialect.readOptions)
	if err != nil {
		return fmt.Errorf("can't begin a read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(&Tx{tx: tx, dialect: db.dialect})
}

// Update runs fn in a write transaction and commits it if fn returns nil.
// Write transactions run one at a time, across every process using the
// store.
func (db *DB) Update(ctx context.Context, fn func(*Tx) error) error {
	return db.writeTx(ctx, fn)
}

// writeTx runs fn in a write transaction, as Update and Revise do, and
// fires Recorded once it commits if fn recorded an audit event.
func (db *DB) writeTx(ctx context.Context, fn func(*Tx) error) error {
	var recorded bool
	err := db.update(ctx, func(tx *sql.Tx) error { return fn(&Tx{tx: tx, dialect: db.dialect, recorded: &recorded}) })
	if err == nil && recorded {
		db.recorded.fire()
	}
	return err
}

func (db *DB) update(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("can't begin a write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := db.dialect.lockWrites(ctx, tx); err != nil {
		return fmt.Errorf("can't begin a write: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("can't commit: %w", err)
	}
	return nil
}

// RevisionTx is a write transaction that may also write managed state:
// changes, revisions and deployments. Only Revise makes one.
type RevisionTx struct {
	*Tx
}

// Revise runs fn in a write transaction that may write managed state, and
// commits it if fn returns nil. Only the revision store (internal/revision)
// calls it, so every change to managed state goes through a plan; a test
// fails if anything else does (PRD principle 2, ADR 0009).
func (db *DB) Revise(ctx context.Context, fn func(*RevisionTx) error) error {
	return db.writeTx(ctx, func(tx *Tx) error { return fn(&RevisionTx{Tx: tx}) })
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

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// boolInt stores a bool as the INTEGER column both databases have.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
