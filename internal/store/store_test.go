// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Re-exported so the tests read as they did inside the package.
type (
	DB         = store.DB
	Tx         = store.Tx
	Server     = store.Server
	CertUpdate = store.CertUpdate
	JoinToken  = store.JoinToken
	Event      = store.Event
)

var (
	ErrNotFound = store.ErrNotFound
	ErrConflict = store.ErrConflict
)

func TestDatabaseFilesAreOwnerOnly(t *testing.T) {
	db, path := storetest.Open(t, store.SQLite)
	if err := db.Update(t.Context(), func(tx *Tx) error { return tx.PutSetting(t.Context(), "k", []byte("v")) }); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(f), info.Mode().Perm())
		}
	}
}

func TestOpenMigratesOnceAndReopens(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, location := storetest.Open(t, b)
		if db.Backend() != b {
			t.Errorf("Backend = %s, want %s", db.Backend(), b)
		}
		if err := db.Update(t.Context(), func(tx *Tx) error {
			return tx.PutSetting(t.Context(), "install.id", []byte("ins_1"))
		}); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()

		again, err := store.Open(t.Context(), location)
		if err != nil {
			t.Fatalf("reopening: %v", err)
		}
		defer again.Close()
		var got []byte
		if err := again.View(t.Context(), func(tx *Tx) (err error) {
			got, err = tx.GetSetting(t.Context(), "install.id")
			return err
		}); err != nil || string(got) != "ins_1" {
			t.Fatalf("setting after reopen = %q, %v", got, err)
		}
		if store.SchemaVersion() < 6 {
			t.Error("SchemaVersion should count the embedded migrations")
		}
	})
}

func TestRefusesNewerSchema(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, location := storetest.Open(t, b)
		_ = db.Close()
		storetest.Exec(t, location, `INSERT INTO schema_migrations (version, applied_at) VALUES (999, 0)`)
		_, err := store.Open(t.Context(), location)
		if err == nil || !strings.Contains(err.Error(), "newer than this dockwell knows") {
			t.Fatalf("Open on a newer schema = %v", err)
		}
	})
}

// A control plane from before a command could name a value has no check of an
// older agent, and runs one itself, which would read its commands with their
// values written in. So none opens a database a later one has used: its schema
// version is past all 18 they knew, though that migration changes nothing.
func TestNoControlPlaneFromBeforeReferencesOpensALaterDatabase(t *testing.T) {
	const beforeReferences = 18
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, location := storetest.Open(t, b)
		_ = db.Close()
		older, err := store.OpenKnowing(t.Context(), location, beforeReferences)
		if err == nil {
			_ = older.Close()
			t.Fatal("a dockwell from before a command could name a value opened a database a later one had used")
		}
		if !strings.Contains(err.Error(), "newer than this dockwell knows (18)") {
			t.Errorf("an older dockwell's refusal = %v", err)
		}
	})
}

// A code setup left waiting by an older dockwell has no session that could
// finish it, so upgrading forgets it, and keeps the secret already in use.
func TestUpgradeForgetsACodeSetupLeftWaiting(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		location := storetest.New(t, b)
		old, err := store.OpenAt(t.Context(), location, 15)
		if err != nil {
			t.Fatal(err)
		}
		_ = old.Close()
		blob := "X'0102'"
		if b == store.Postgres {
			blob = `'\x0102'::bytea`
		}
		storetest.Exec(t, location, `INSERT INTO users (id, email, name, role, webauthn_handle, created_at, totp_secret, totp_pending)
			VALUES ('usr_1', 'mira@acme.dev', 'Mira Chen', 'owner', `+blob+`, 0, `+blob+`, `+blob+`)`)
		db, err := store.Open(t.Context(), location)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		_ = db.View(t.Context(), func(tx *store.Tx) error {
			u, err := tx.GetUser(t.Context(), "usr_1")
			if err != nil || u.TOTPPending != nil || u.TOTPSecret == nil {
				t.Errorf("after upgrading, the user = %+v, %v; want no pending secret, and the secret in use", u, err)
			}
			return nil
		})
	})
}

// OpenExisting opens a store only as it is: it never migrates one an older
// dockwell left, never creates one, and refuses a schema it doesn't know.
func TestOpenExistingNeverMigrates(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		location := storetest.New(t, b)
		current := store.SchemaVersion()
		old, err := store.OpenAt(t.Context(), location, current-1)
		if err != nil {
			t.Fatal(err)
		}
		_ = old.Close()

		_, err = store.OpenExisting(t.Context(), location)
		var schema *store.SchemaError
		if !errors.As(err, &schema) || schema.Database != current-1 || schema.Binary != current ||
			!strings.Contains(err.Error(), "older than this dockwell's") || !strings.Contains(err.Error(), "Restart the control plane") {
			t.Fatalf("OpenExisting on an older schema = %v, want a *SchemaError that says to restart", err)
		}
		// It left the schema as it was: opening at the old version again
		// finds nothing newer than it.
		again, err := store.OpenAt(t.Context(), location, current-1)
		if err != nil {
			t.Fatalf("the schema moved on: %v", err)
		}
		var newest int
		if err := again.View(t.Context(), func(tx *Tx) error {
			return store.QueryIntForTest(t.Context(), tx, `SELECT MAX(version) FROM schema_migrations`, &newest)
		}); err != nil {
			t.Fatal(err)
		}
		_ = again.Close()
		if newest != current-1 {
			t.Errorf("after OpenExisting the schema version is %d, want %d", newest, current-1)
		}

		up, err := store.Open(t.Context(), location)
		if err != nil {
			t.Fatal(err)
		}
		_ = up.Close()
		db, err := store.OpenExisting(t.Context(), location)
		if err != nil {
			t.Fatalf("OpenExisting on the current schema = %v", err)
		}
		_ = db.Close()

		storetest.Exec(t, location, fmt.Sprintf(`INSERT INTO schema_migrations (version, applied_at) VALUES (%d, 0)`, current+1))
		if _, err := store.OpenExisting(t.Context(), location); !errors.As(err, &schema) || !strings.Contains(err.Error(), "newer than this dockwell knows") {
			t.Errorf("OpenExisting on a newer schema = %v", err)
		}
	})

	missing := filepath.Join(t.TempDir(), "state.db")
	if _, err := store.OpenExisting(t.Context(), missing); err == nil {
		t.Error("OpenExisting opened a database that isn't there")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenExisting created %s: %v", missing, err)
	}
}

// A migration that can't be recorded as done says which one, and on which
// database.
func TestAMigrationThatCantBeRecordedSaysWhich(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		location := storetest.New(t, b)
		storetest.Exec(t, location, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at BIGINT NOT NULL, note TEXT NOT NULL)`)
		_, err := store.Open(t.Context(), location)
		if want := "migration 0001_init ran on " + string(b) + ", but can't be recorded as done"; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Open = %v, want an error saying %q", err, want)
		}
	})
}

func TestSettings(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		err := db.View(ctx, func(tx *Tx) error {
			_, err := tx.GetSetting(ctx, "missing")
			return err
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing setting: %v", err)
		}
		for _, v := range []string{"one", "two"} {
			if err := db.Update(ctx, func(tx *Tx) error { return tx.PutSetting(ctx, "k", []byte(v)) }); err != nil {
				t.Fatal(err)
			}
		}
		_ = db.View(ctx, func(tx *Tx) error {
			v, err := tx.GetSetting(ctx, "k")
			if err != nil || string(v) != "two" {
				t.Errorf("PutSetting should replace: got %q, %v", v, err)
			}
			return nil
		})
	})
}

func TestServers(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Millisecond)
		hz1 := Server{ID: "srv_1", Name: "hz-1", CreatedAt: now, CertSerial: "01", CertNotAfter: now.Add(720 * time.Hour)}

		if err := db.Update(ctx, func(tx *Tx) error { return tx.CreateServer(ctx, hz1) }); err != nil {
			t.Fatal(err)
		}
		err := db.Update(ctx, func(tx *Tx) error {
			return tx.CreateServer(ctx, Server{ID: "srv_2", Name: "hz-1", CreatedAt: now, CertSerial: "02", CertNotAfter: now})
		})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("a second active hz-1: %v, want ErrConflict", err)
		}
		// A conflict leaves the transaction usable on both databases, so
		// the caller can try another name in it.
		if err := db.Update(ctx, func(tx *Tx) error {
			if err := tx.CreateServer(ctx, Server{ID: "srv_2", Name: "hz-1", CreatedAt: now, CertSerial: "02", CertNotAfter: now}); !errors.Is(err, ErrConflict) {
				t.Errorf("hz-1 again: %v, want ErrConflict", err)
			}
			if err := tx.CreateServer(ctx, Server{ID: "srv_2", Name: "hz-1-2", CreatedAt: now, CertSerial: "02", CertNotAfter: now}); err != nil {
				return err
			}
			return tx.RevokeServer(ctx, "srv_2", now)
		}); err != nil {
			t.Fatalf("another name after a conflict, in the same transaction: %v", err)
		}

		seen := now.Add(time.Minute)
		if err := db.Update(ctx, func(tx *Tx) error {
			if err := tx.TouchServer(ctx, "srv_1", seen); err != nil {
				return err
			}
			return tx.UpdateServerCert(ctx, "srv_1", CertUpdate{Serial: "03", KeyHash: []byte{3}, NotAfter: now.Add(1440 * time.Hour), PrevSerial: "01"})
		}); err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *Tx) error {
			got, err := tx.GetServer(ctx, "srv_1")
			if err != nil {
				t.Fatal(err)
			}
			if got.LastSeenAt == nil || !got.LastSeenAt.Equal(seen) || got.CertSerial != "03" || got.PrevCertSerial != "01" || len(got.CertKeyHash) != 1 {
				t.Errorf("server after updates = %+v", got)
			}
			return nil
		})

		if err := db.Update(ctx, func(tx *Tx) error { return tx.RevokeServer(ctx, "srv_1", now) }); err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *Tx) error {
			list, err := tx.ListServers(ctx)
			if err != nil || len(list) != 0 {
				t.Errorf("revoked servers must not be listed: %v, %v", list, err)
			}
			return nil
		})
		// The name is free again once the old server is revoked.
		if err := db.Update(ctx, func(tx *Tx) error {
			return tx.CreateServer(ctx, Server{ID: "srv_3", Name: "hz-1", CreatedAt: now, CertSerial: "04", CertNotAfter: now})
		}); err != nil {
			t.Fatalf("reusing a revoked server's name: %v", err)
		}
		err = db.Update(ctx, func(tx *Tx) error { return tx.TouchServer(ctx, "srv_nope", now) })
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("touching a missing server: %v", err)
		}
	})
}

func TestJoinTokensAreSingleUse(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Now()
		if err := db.Update(ctx, func(tx *Tx) error {
			if err := tx.CreateJoinToken(ctx, JoinToken{ID: "jtk_1", SecretHash: []byte{1, 2}, CreatedAt: now, CreatedBy: "cli", ExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
			return tx.CreateServer(ctx, Server{ID: "srv_1", Name: "a", CreatedAt: now, CertSerial: "1", CertNotAfter: now})
		}); err != nil {
			t.Fatal(err)
		}

		// Two joins race for one token: exactly one wins.
		var wg sync.WaitGroup
		results := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				results <- db.Update(ctx, func(tx *Tx) error { return tx.MarkJoinTokenUsed(ctx, "jtk_1", "srv_1", time.Now()) })
			})
		}
		wg.Wait()
		close(results)
		var ok int
		for err := range results {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, ErrNotFound):
				t.Errorf("unexpected error: %v", err)
			}
		}
		if ok != 1 {
			t.Fatalf("%d joins spent the same token, want exactly 1", ok)
		}
		_ = db.View(ctx, func(tx *Tx) error {
			j, err := tx.GetJoinToken(ctx, "jtk_1")
			if err != nil || j.UsedAt == nil || j.UsedBy != "srv_1" {
				t.Errorf("spent token = %+v, %v", j, err)
			}
			return nil
		})
	})
}

func TestGetEvent(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		if err := db.Update(ctx, func(tx *Tx) error {
			return tx.RecordEvent(ctx, Event{ID: "evt_1", At: time.Now(), Actor: "user:a", Action: "file.saving", Subject: "/a", Target: "srv_A"})
		}); err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *Tx) error {
			if e, err := tx.GetEvent(ctx, "evt_1"); err != nil || e.Action != "file.saving" || e.Actor != "user:a" || e.Target != "srv_A" {
				t.Errorf("GetEvent = %+v, %v", e, err)
			}
			if _, err := tx.GetEvent(ctx, "evt_2"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("an event that isn't there = %v", err)
			}
			return nil
		})
	})
}

func TestEventsNewestFirst(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		base := time.Now()
		if err := db.Update(ctx, func(tx *Tx) error {
			for i, action := range []string{"server.join", "join_token.create", "server.revoke"} {
				if err := tx.RecordEvent(ctx, Event{ID: string(rune('a' + i)), At: base.Add(time.Duration(i) * time.Second), Actor: "cli", Action: action, Subject: "x"}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *Tx) error {
			events, err := tx.ListEvents(ctx, 2)
			if err != nil || len(events) != 2 || events[0].Action != "server.revoke" || events[1].Action != "join_token.create" {
				t.Errorf("ListEvents = %+v, %v", events, err)
			}
			return nil
		})
	})
}

func TestReadsDontWaitForWrites(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		holding := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = db.Update(ctx, func(tx *Tx) error {
				if err := tx.PutSetting(ctx, "k", []byte("v")); err != nil {
					return err
				}
				close(holding)
				<-release
				return nil
			})
		}()
		<-holding
		// A write transaction is open; reads run meanwhile.
		err := db.View(ctx, func(tx *Tx) error {
			_, err := tx.GetSetting(ctx, "k")
			return err
		})
		close(release)
		<-done
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("read during an open write = %v; it should see the last committed state", err)
		}
	})
}

func TestServerReports(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Millisecond)
		if err := db.Update(ctx, func(tx *Tx) error {
			for _, id := range []string{"srv_1", "srv_2"} {
				if err := tx.CreateServer(ctx, Server{ID: id, Name: id, CreatedAt: now, CertSerial: id, CertNotAfter: now}); err != nil {
					return err
				}
			}
			return tx.SaveServerReport(ctx, "srv_1", store.ServerReport{Inventory: []byte("inv"), AgentBuild: []byte("build")}, now)
		}); err != nil {
			t.Fatal(err)
		}
		_ = db.View(ctx, func(tx *Tx) error {
			reports, err := tx.ServerReports(ctx)
			if err != nil || len(reports) != 1 || string(reports["srv_1"].Inventory) != "inv" || string(reports["srv_1"].AgentBuild) != "build" {
				t.Errorf("ServerReports = %v, %v", reports, err)
			}
			s, err := tx.GetServer(ctx, "srv_1")
			if err != nil || s.LastSeenAt == nil || !s.LastSeenAt.Equal(now) {
				t.Errorf("SaveServerReport should record when: %+v, %v", s, err)
			}
			return nil
		})
	})
}
