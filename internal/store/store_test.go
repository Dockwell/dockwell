// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestDatabaseFilesAreOwnerOnly(t *testing.T) {
	db, path := openTest(t)
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
	db, path := openTest(t)
	if err := db.Update(t.Context(), func(tx *Tx) error {
		return tx.PutSetting(t.Context(), "install.id", []byte("ins_1"))
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	again, err := Open(t.Context(), path)
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
	if SchemaVersion() < 1 {
		t.Error("SchemaVersion should count the embedded migrations")
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	db, path := openTest(t)
	if err := db.Update(t.Context(), func(tx *Tx) error {
		_, err := tx.tx.ExecContext(t.Context(), `INSERT INTO schema_migrations (version, applied_at) VALUES (999, 0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	_, err := Open(t.Context(), path)
	if err == nil || !strings.Contains(err.Error(), "newer than this dockwell knows") {
		t.Fatalf("Open on a newer schema = %v", err)
	}
}

func TestSettings(t *testing.T) {
	db, _ := openTest(t)
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
}

func TestServers(t *testing.T) {
	db, _ := openTest(t)
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
}

func TestJoinTokensAreSingleUse(t *testing.T) {
	db, _ := openTest(t)
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
}

func TestEventsNewestFirst(t *testing.T) {
	db, _ := openTest(t)
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
}

func TestReadsDontWaitForWrites(t *testing.T) {
	db, _ := openTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	holding := make(chan struct{})
	release := make(chan struct{})
	go func() {
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
	// A write transaction is open; WAL lets a read run meanwhile.
	err := db.View(ctx, func(tx *Tx) error {
		_, err := tx.GetSetting(ctx, "k")
		return err
	})
	close(release)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("read during an open write = %v; it should see the last committed state", err)
	}
}
