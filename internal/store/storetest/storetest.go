// SPDX-License-Identifier: AGPL-3.0-only

// Package storetest opens stores for tests, on SQLite and on Postgres.
//
// Tests that run on both call Each, which runs them once per backend. SQLite
// needs nothing; Postgres needs a server, named by DOCKWELL_TEST_POSTGRES,
// such as postgres://postgres@127.0.0.1:5432/postgres. Each test gets a
// schema of its own there, dropped when it ends. Without the variable, the
// Postgres runs skip and say how to turn them on. CI sets it.
package storetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dockwell/dockwell/internal/store"
)

// EnvPostgres names the Postgres server tests run against.
const EnvPostgres = "DOCKWELL_TEST_POSTGRES"

// Backends are the backends Each runs on, in order.
var Backends = []store.Backend{store.SQLite, store.Postgres}

// Each runs fn once on each backend, as subtests named "sqlite" and
// "postgres". The Postgres one skips, saying why, unless
// DOCKWELL_TEST_POSTGRES names a server.
func Each(t *testing.T, fn func(t *testing.T, b store.Backend)) {
	t.Helper()
	for _, b := range Backends {
		t.Run(string(b), func(t *testing.T) {
			Require(t, b)
			fn(t, b)
		})
	}
}

// Require skips the test unless backend b is available.
func Require(t testing.TB, b store.Backend) {
	t.Helper()
	if b == store.Postgres && os.Getenv(EnvPostgres) == "" {
		t.Skipf("%s isn't set, so this runs on SQLite only. To run it on Postgres too, set %s to a server's URL, "+
			"such as postgres://postgres@127.0.0.1:5432/postgres", EnvPostgres, EnvPostgres)
	}
}

// Open returns a new, empty store on b, closed when the test ends, and
// its location, which store.Open takes to open it again.
func Open(t testing.TB, b store.Backend) (*store.DB, string) {
	t.Helper()
	location := New(t, b)
	db, err := store.Open(context.Background(), location)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, location
}

// New returns the location of a new, empty database on b, without opening
// it: a file in a temporary directory, or a fresh schema on the Postgres
// server. Either goes away when the test ends.
func New(t testing.TB, b store.Backend) string {
	t.Helper()
	Require(t, b)
	if b == store.SQLite {
		return filepath.Join(t.TempDir(), "state.db")
	}
	base, err := url.Parse(os.Getenv(EnvPostgres))
	if err != nil || !store.IsPostgres(base.String()) {
		t.Fatalf("%s must be a postgres:// URL, such as postgres://postgres@127.0.0.1:5432/postgres", EnvPostgres)
	}
	var b8 [8]byte
	_, _ = rand.Read(b8[:])
	schema := "dwtest_" + hex.EncodeToString(b8[:])
	admin := server(t, base.String())
	if _, err := admin.ExecContext(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("can't make a schema for the test on %s: %v", store.Redact(base.String()), err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("can't drop the test's schema %s: %v", schema, err)
		}
	})
	q := base.Query()
	q.Set("search_path", schema)
	base.RawQuery = q.Encode()
	return base.String()
}

var (
	serversMu sync.Mutex
	servers   = map[string]*sql.DB{}
)

// server returns a connection pool for the Postgres server, shared by the
// tests in this process.
func server(t testing.TB, location string) *sql.DB {
	t.Helper()
	serversMu.Lock()
	defer serversMu.Unlock()
	if db, ok := servers[location]; ok {
		return db
	}
	db, err := sql.Open("pgx", location)
	if err != nil {
		t.Fatalf("can't reach %s: %v", store.Redact(location), err)
	}
	db.SetMaxOpenConns(2)
	servers[location] = db
	return db
}

// Exec runs a statement on the database at location from outside the
// store, as something else writing to it would.
func Exec(t testing.TB, location, query string) {
	t.Helper()
	var db *sql.DB
	var err error
	if store.IsPostgres(location) {
		db, err = sql.Open("pgx", location)
	} else {
		db, err = sql.Open("sqlite", "file:"+location+"?_pragma=busy_timeout(5000)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// FailUpdates makes every UPDATE that sets column in table fail, as a full
// disk would, until the returned func runs.
func FailUpdates(t testing.TB, location, table, column string) (restore func()) {
	t.Helper()
	name := "storetest_fail_" + table + "_" + column
	if !store.IsPostgres(location) {
		Exec(t, location, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE OF %s ON %s BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`,
			name, column, table))
		return func() { Exec(t, location, `DROP TRIGGER `+name) }
	}
	Exec(t, location, `CREATE OR REPLACE FUNCTION storetest_fail() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'disk I/O error'; END $$`)
	Exec(t, location, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE OF %s ON %s FOR EACH ROW EXECUTE FUNCTION storetest_fail()`,
		name, column, table))
	return func() { Exec(t, location, fmt.Sprintf(`DROP TRIGGER %s ON %s`, name, table)) }
}
