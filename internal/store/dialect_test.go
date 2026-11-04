// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestPlaceholdersAreRewrittenForPostgres(t *testing.T) {
	for in, want := range map[string]string{
		`SELECT a FROM t WHERE b = ? AND c = ?`:             `SELECT a FROM t WHERE b = $1 AND c = $2`,
		`VALUES (?1, 1, ?2, CASE WHEN ?3 <= 1 THEN ?2 END)`: `VALUES ($1, 1, $2, CASE WHEN $3 <= 1 THEN $2 END)`,
		`SELECT ?2, ?`: `SELECT $2, $3`,
		`SELECT '?', "a?b", ? -- what?` + "\n" + `, ?`: `SELECT '?', "a?b", $1 -- what?` + "\n" + `, $2`,
		`SELECT 'it''s ?', ? /* ? */`:                  `SELECT 'it''s ?', $1 /* ? */`,
	} {
		if got := store.RebindDollar(in); got != want {
			t.Errorf("rebind(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestPortableMigrationsMapTypesForPostgres(t *testing.T) {
	in := `-- INTEGER in a comment stays
CREATE TABLE t (
  id    TEXT PRIMARY KEY,
  n     INTEGER NOT NULL,
  b     BLOB NOT NULL DEFAULT x'',
  h     blob DEFAULT X'00ff',
  maxx  TEXT NOT NULL DEFAULT 'TEXT INTEGER',
  textual INTEGER
);`
	want := `-- INTEGER in a comment stays
CREATE TABLE t (
  id    TEXT COLLATE "C" PRIMARY KEY,
  n     BIGINT NOT NULL,
  b     BYTEA NOT NULL DEFAULT '\x',
  h     BYTEA DEFAULT '\x00ff',
  maxx  TEXT COLLATE "C" NOT NULL DEFAULT 'TEXT INTEGER',
  textual BIGINT
);`
	if got := store.PostgresSchema(in); got != want {
		t.Errorf("PostgresSchema:\n%s\nwant:\n%s", got, want)
	}
}

func TestPostgresPasswordsStayOutOfMessages(t *testing.T) {
	const secret = "s3cret-pw"
	for in, want := range map[string]string{
		"postgres://dockwell:" + secret + "@db.internal:5432/dockwell?sslmode=require": "postgres://dockwell:xxxxx@db.internal:5432/dockwell?sslmode=require",
		"postgresql://db.internal/dockwell?password=" + secret:                         "postgresql://db.internal/dockwell?password=xxxxx",
		"/var/lib/dockwell/control/state.db":                                           "/var/lib/dockwell/control/state.db",
		"postgres://dockwell:%zz" + secret + "@db/x":                                   "a postgres:// URL that doesn't parse",
	} {
		if got := store.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	// Nothing listens on port 1, so this fails to connect; the error names
	// the database without its password.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := store.Open(ctx, "postgres://dockwell:"+secret+"@127.0.0.1:1/dockwell?connect_timeout=5")
	if err == nil {
		t.Fatal("Open on a closed port succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error shows the password: %v", err)
	}
	if !strings.Contains(err.Error(), "postgres://dockwell:xxxxx@127.0.0.1:1/dockwell") || !strings.Contains(err.Error(), "Check that it is running") {
		t.Errorf("the error should name the database and say what to check: %v", err)
	}
}

// Two handles on one database stand for dockwell run and a CLI command
// beside it, or two replicas: their writes still take turns, so a read and
// a write in one transaction never lose an update.
func TestWritesQueueAcrossConnections(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		a, location := storetest.Open(t, b)
		other, err := store.Open(t.Context(), location)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		ctx := t.Context()

		var wg sync.WaitGroup
		errs := make(chan error, 40)
		for i := range 40 {
			db := a
			if i%2 == 1 {
				db = other
			}
			wg.Go(func() {
				errs <- db.Update(ctx, func(tx *store.Tx) error {
					n := 0
					if v, err := tx.GetSetting(ctx, "count"); err == nil {
						n, _ = strconv.Atoi(string(v))
					}
					return tx.PutSetting(ctx, "count", []byte(strconv.Itoa(n+1)))
				})
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		_ = a.View(ctx, func(tx *store.Tx) error {
			if v, err := tx.GetSetting(ctx, "count"); err != nil || string(v) != "40" {
				t.Errorf("count after 40 increments from two handles = %q, %v", v, err)
			}
			return nil
		})
	})
}

// On Postgres an insert that may conflict runs under a savepoint. When the
// savepoint itself fails, the error says so.
func TestASavepointThatFailsSaysSo(t *testing.T) {
	db, _ := storetest.Open(t, store.Postgres)
	ctx, cancel := context.WithCancel(t.Context())
	err := db.Update(ctx, func(tx *store.Tx) error {
		cancel()
		return tx.CreateServer(ctx, store.Server{ID: "srv_A", Name: "hz-1", CreatedAt: time.Now(), CertSerial: "1", CertNotAfter: time.Now()})
	})
	if err == nil || !strings.Contains(err.Error(), "can't set a savepoint on postgres") {
		t.Fatalf("an insert whose savepoint fails = %v, want an error naming the savepoint", err)
	}
}
