// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/backup/keep"
	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	chunks "github.com/dockwell/dockwell/internal/backup/seal"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestBundleAuthenticatesEveryChunkAndTheHeader(t *testing.T) {
	now := time.Now().UTC()
	h := Header{
		Install: id.New("ins"), Bundle: id.New("cbk"), Generation: 1,
		GenerationID: id.New("gen"), Schema: store.SchemaVersion(), Taken: now, Version: "devel",
	}
	phrase := GeneratePhrase()
	r := NewRecovery(phrase, now)
	var installKey [32]byte
	_, _ = rand.Read(installKey[:])
	payload := make([]byte, chunks.KeyedChunkSize*2-keySize)
	_, _ = rand.Read(payload)
	var sealed bytes.Buffer
	if err := SealBundle(&sealed, bytes.NewReader(payload), int64(len(payload)), installKey[:], h, r); err != nil {
		t.Fatal(err)
	}
	got := &bytes.Buffer{}
	head, err := openBundle(got, bytes.NewReader(sealed.Bytes()), phrase)
	if err != nil || head.Bundle != h.Bundle || !bytes.Equal(got.Bytes(), slices.Concat(installKey[:], payload)) {
		t.Fatalf("bundle round trip: %v, %+v, %d bytes", err, head, got.Len())
	}

	reader := bytes.NewReader(sealed.Bytes())
	_, header, err := readHeader(reader)
	if err != nil {
		t.Fatal(err)
	}
	headerEnd := len(header)
	h.Bundle = id.New("cbk")
	var second bytes.Buffer
	if err := SealBundle(&second, bytes.NewReader(payload), int64(len(payload)), installKey[:], h, r); err != nil {
		t.Fatal(err)
	}
	_, otherHeader, err := readHeader(&second)
	if err != nil {
		t.Fatal(err)
	}
	original := sealed.Bytes()
	cases := map[string]func([]byte) []byte{
		"header":                     func(b []byte) []byte { b[bytes.Index(b[:headerEnd], []byte("devel"))] = 'D'; return b },
		"chunk":                      func(b []byte) []byte { b[headerEnd+128] ^= 0x80; return b },
		"truncation":                 func(b []byte) []byte { return b[:len(b)-1] },
		"cut at chunk boundary":      func(b []byte) []byte { return b[:headerEnd+chunks.KeyedSealedChunk] },
		"header from another bundle": func(b []byte) []byte { return slices.Concat(otherHeader, b[headerEnd:]) },
		"append":                     func(b []byte) []byte { return append(b, 0) },
		"reordering": func(b []byte) []byte {
			one := slices.Clone(b[headerEnd : headerEnd+chunks.KeyedSealedChunk])
			copy(b[headerEnd:], b[headerEnd+chunks.KeyedSealedChunk:headerEnd+2*chunks.KeyedSealedChunk])
			copy(b[headerEnd+chunks.KeyedSealedChunk:], one)
			return b
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := openBundle(io.Discard, bytes.NewReader(change(slices.Clone(original))), phrase); err == nil {
				t.Fatal("opened a changed bundle")
			}
		})
	}
}

func TestHeaderBoundsBeforeArgon(t *testing.T) {
	now := time.Now()
	h := Header{
		Install: id.New("ins"), Bundle: id.New("cbk"), Generation: 1, GenerationID: id.New("gen"),
		Schema: 58, Taken: now, PassphraseAt: now, KDF: KDF{Memory: memoryKiB, Passes: passes, Lanes: lanes, Salt: randomSalt()}, Salt: randomSalt(),
	}
	for name, change := range map[string]func(*Header){
		"small memory": func(h *Header) { h.KDF.Memory = 1 },
		"large memory": func(h *Header) { h.KDF.Memory = ^uint32(0) },
		"passes":       func(h *Header) { h.KDF.Passes = ^uint32(0) },
		"lanes":        func(h *Header) { h.KDF.Lanes = 5 },
		"argon salt":   func(h *Header) { h.KDF.Salt = make([]byte, 65) },
		"bundle salt":  func(h *Header) { h.Salt = make([]byte, 15) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := h
			change(&bad)
			raw, err := json.Marshal(bad)
			if err != nil {
				t.Fatal(err)
			}
			head := binary.BigEndian.AppendUint32([]byte(magic), uint32(len(raw)))
			_, err = openBundle(io.Discard, bytes.NewReader(append(head, raw...)), "anything")
			if err == nil || !strings.Contains(err.Error(), "invalid values") {
				t.Fatalf("invalid header refusal: %v", err)
			}
		})
	}
	head := binary.BigEndian.AppendUint32([]byte(magic), maxHeader+1)
	if _, _, err := readHeader(bytes.NewReader(head)); err == nil {
		t.Fatal("read an oversized header")
	}
}

func TestRecoveryInitReleasesScratchMemory(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	keyFile := filepath.Join(t.TempDir(), "key")
	key, err := seal.LoadOrCreate(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	m := New(Config{DB: db, Key: key, KeyFile: keyFile, Install: id.New("ins")})
	residentHeap := func() uint64 {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.HeapSys - stats.HeapReleased
	}
	debug.FreeOSMemory()
	before := residentHeap()
	if err := m.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Leave room for the loaded schema and allocator bookkeeping, while
	// catching Argon2's 64 MiB block if it remains resident.
	if after := residentHeap(); after > before+32<<20 {
		t.Errorf("initializing recovery left %d MiB of additional heap resident", (after-before)>>20)
	}
}

func TestControlBackupOpensAConsistentStoreAndKey(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		db, _ := storetest.Open(t, backend)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Millisecond)
		keyFile := filepath.Join(t.TempDir(), "key")
		key, err := seal.LoadOrCreate(keyFile)
		if err != nil {
			t.Fatal(err)
		}
		install := id.New("ins")
		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.PutSetting(ctx, "install.id", []byte(install)); err != nil {
				return err
			}
			if err := tx.PutSetting(ctx, "test.variable", []byte("example.com")); err != nil {
				return err
			}
			if err := tx.AddHistoryRows(ctx, []store.HistoryRow{{
				ServerID: "server", Start: now, Tier: store.HistoryMinute, CoveredMs: 60_000,
			}}); err != nil {
				return err
			}
			return tx.CreateUser(ctx, store.User{
				ID: id.New(id.User), Name: "Owner", Email: "owner@example.com",
				Role: store.RoleOwner, WebAuthnHandle: []byte("owner"), CreatedAt: now,
			})
		}); err != nil {
			t.Fatal(err)
		}
		m := New(Config{DB: db, Key: key, KeyFile: keyFile, Install: install, Now: func() time.Time { return now }})
		if err := m.Init(ctx); err != nil {
			t.Fatal(err)
		}
		b, err := m.take(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		defer b.close()
		r, err := m.openRecovery(b.state)
		if err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(t.TempDir(), "recovered")
		h, err := Extract(ctx, io.NewSectionReader(b.file, 0, b.size), r.Phrase, root)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(keyFile)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(root, "key"))
		if err != nil || !bytes.Equal(want, got) || h.Schema != store.SchemaVersion() || h.Install != install || h.Generation != 1 {
			t.Fatalf("recovered key and header: key matches %t, header %+v, error %v", bytes.Equal(want, got), h, err)
		}
		restored, err := sql.Open("sqlite", filepath.Join(root, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		var check string
		if err := restored.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
			t.Fatalf("quick_check = %q, %v", check, err)
		}
		var history int
		if err := restored.QueryRowContext(ctx, "SELECT COUNT(*) FROM server_history").Scan(&history); err != nil || history != 0 {
			t.Fatalf("recovered bundle contains %d metric rows, %v", history, err)
		}
		var owner string
		if err := restored.QueryRowContext(ctx, "SELECT email FROM users").Scan(&owner); err != nil || owner != "owner@example.com" {
			t.Fatalf("recovered owner = %q, %v", owner, err)
		}
	})
}

func TestOffsiteCopyAndRetentionStayInTheirGeneration(t *testing.T) {
	fake := s3test.New(t)
	d := destination{prefix: "backup", bucket: s3.Bucket{Endpoint: fake.URL(), Name: "bucket", AccessKey: "key", SecretKey: "secret", Client: fake.Client()}}
	now := time.Now().UTC()
	file, err := os.CreateTemp(t.TempDir(), "bundle")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	install, gen, phrase := id.New("ins"), id.New("gen"), id.New("phrase")
	h := Header{Install: install, Generation: 1, GenerationID: gen}
	data := []byte("a sealed object from a separately verified bundle")
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	prefix := "/bucket/" + d.key("dockwell-control/"+install+"/")
	old := keep.Name(now.AddDate(0, 0, -20), id.New("cbk"), phrase)
	fake.Set(prefix+"1/"+old, []byte("old"))
	for i := 1; i <= 23; i++ {
		fake.Set(prefix+"1/"+keep.Name(now.Add(-time.Duration(i)*time.Minute), id.New("cbk"), phrase), []byte("recent"))
	}
	fake.Set(prefix+"2/"+old, []byte("a different generation"))
	b := takenBundle{header: h, name: keep.Name(now, id.New("cbk"), phrase), file: file, size: int64(len(data)), sum: sum[:]}
	selection := id.New("cbs")
	trusted, err := d.ensureMarker(t.Context(), h, selection, "", time.Now())
	if err != nil || trusted != selection {
		t.Fatalf("marker: %q, %v", trusted, err)
	}
	if problem, old, err := d.store(t.Context(), b, &retentionState{}, now); problem != "" || old || err != nil {
		t.Fatalf("copy: %q, old=%v, %v", problem, old, err)
	}
	objects := fake.Objects()
	if !bytes.Equal(objects[prefix+"1/"+b.name], data) || objects[prefix+"1/"+old] != nil || objects[prefix+"2/"+old] == nil ||
		objects[prefix+"current.json"] == nil {
		t.Fatalf("wrong objects after backup: %v", objects)
	}
	// A later marker makes a stale sender leave every object alone.
	fake.Set(prefix+"current.json", []byte(`{"generation":2}`))
	b.name = keep.Name(now.Add(time.Hour), id.New("cbk"), phrase)
	if _, err := d.ensureMarker(t.Context(), h, selection, trusted, time.Now()); err == nil || fake.Objects()[prefix+"1/"+b.name] != nil {
		t.Fatal("sent a bundle past a different generation marker")
	}
}

func TestScheduleDebouncesAgainstAFakeClock(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	s := store.ControlBackup{
		LastAttempt: now.Add(-20 * time.Minute), LastPosition: 3, Position: 4,
		FirstChangedAt: now.Add(-25 * time.Second), ChangedAt: now.Add(-10 * time.Second),
	}
	check := func(want time.Time) {
		t.Helper()
		if got := due(s, now); !got.Equal(want) {
			t.Fatalf("due at %v, want %v", got, want)
		}
	}
	check(now.Add(50 * time.Second))
	now = now.Add(20 * time.Second)
	s.ChangedAt = now
	check(now.Add(time.Minute))
	now = now.Add(10 * time.Minute)
	s.ChangedAt = now
	check(s.FirstChangedAt.Add(10 * time.Minute))
	s.LastAttempt = now.Add(-5 * time.Minute)
	check(now.Add(5 * time.Minute))
	s.LastPosition = s.Position
	check(now.Add(55 * time.Minute))
	s.Requested = true
	check(now.Add(5 * time.Minute))
	s.Hourly = true
	check(now.Add(55 * time.Minute))
}

func TestRecoveryPhraseRules(t *testing.T) {
	generated := GeneratePhrase()
	if len(generated) != 39 || strings.Trim(strings.ReplaceAll(generated, "-", ""), id.CrockfordAlphabet) != "" {
		t.Fatalf("generated phrase has the wrong format")
	}
	if err := validatePhrase(generated); err != nil {
		t.Fatal(err)
	}
	if err := validatePhrase("passwordpasswordpasswordpassword"); err == nil {
		t.Fatal("accepted a low-entropy phrase")
	}
	for _, phrase := range []string{"cQ8#nRsW4@kH9zE2tM7w", "gravel optic cedar paper lattice"} {
		if err := validatePhrase(phrase); err != nil {
			t.Errorf("rejected a strong phrase: %v", err)
		}
	}
}

func TestAttemptFailurePreservesPriorPlace(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	m := New(Config{DB: db, Logger: slog.New(slog.DiscardHandler)})
	now := time.Now().UTC().Truncate(time.Millisecond)
	m.result(t.Context(), "bundle", store.ControlBackupPlace{Generation: 1, Place: "offsite", Target: "location", RecoveryID: "phrase", SentAt: now}, ActionSent)
	m.result(t.Context(), "bundle", store.ControlBackupPlace{Generation: 1, Place: "offsite", Target: "location", RecoveryID: "rotated", Problem: "upload failed"}, ActionFailed)
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		p, err := tx.ControlBackupPlaces(t.Context(), 1)
		if err != nil {
			return err
		}
		if len(p) != 1 || !p[0].SentAt.Equal(now) || p[0].RecoveryID != "phrase" || p[0].Problem != "upload failed" {
			t.Fatalf("wrong place: %+v", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyManagedWritesAdvanceTheBackupPosition(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		db, _ := storetest.Open(t, backend)
		ctx := t.Context()
		now := time.Now()
		update := func(fn func(*store.Tx) error) {
			t.Helper()
			if err := db.Update(ctx, fn); err != nil {
				t.Fatal(err)
			}
		}
		get := func() store.ControlBackup {
			t.Helper()
			var got store.ControlBackup
			if err := db.View(ctx, func(tx *store.Tx) error {
				var err error
				got, err = tx.ControlBackup(ctx)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return got
		}
		user := id.New(id.User)
		session, token := id.New(id.Session), id.New(id.Token)
		update(func(tx *store.Tx) error {
			if err := tx.CreateUser(ctx, store.User{
				ID: user, Name: "Owner", Email: "owner@example.com",
				Role: store.RoleOwner, WebAuthnHandle: []byte("owner"), CreatedAt: now,
			}); err != nil {
				return err
			}
			if err := tx.CreateSession(ctx, store.Session{
				ID: session, UserID: user, TokenHash: []byte("session"),
				Method: "claim", RPID: "localhost", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
			}); err != nil {
				return err
			}
			return tx.CreateAPIToken(ctx, store.APIToken{
				ID: token, UserID: user, TokenHash: []byte("token"),
				Name: "Read", Scope: store.ScopeReadOnly, CreatedAt: now,
			})
		})
		managed := get()
		if managed.Position == 0 || managed.FirstChangedAt.UnixMilli() == 0 {
			t.Fatal("managed writes did not advance the position")
		}
		update(func(tx *store.Tx) error {
			if err := tx.TouchSession(ctx, session, now.Add(time.Minute), "192.0.2.1"); err != nil {
				return err
			}
			if err := tx.TouchAPIToken(ctx, token, now.Add(time.Minute), "192.0.2.1"); err != nil {
				return err
			}
			_, err := tx.SeeAPITokenAddress(ctx, token, "192.0.2.1", now.Add(time.Minute))
			return err
		})
		afterTouch := get()
		if managed.Position != afterTouch.Position || !managed.ChangedAt.Equal(afterTouch.ChangedAt) {
			t.Fatal("a last-use write scheduled a backup")
		}
		update(func(tx *store.Tx) error { return tx.RevokeAPIToken(ctx, user, token, now.Add(time.Minute)) })
		if got := get(); got.Position != managed.Position+1 || !got.FirstChangedAt.Equal(managed.FirstChangedAt) {
			t.Fatalf("token revocation did not advance the debounce window: %+v", got)
		}
	})
}

func BenchmarkControlBackupRecoveryDerivation(b *testing.B) {
	k := KDF{Memory: memoryKiB, Passes: passes, Lanes: lanes, Salt: randomSalt()}
	for b.Loop() {
		clear(k.derive("a-generated-recovery-passphrase"))
	}
}

func TestOpeningCrockfordRecoveryPhraseVariants(t *testing.T) {
	now := time.Now()
	canonical := "011A-BCDE-FGHJ-KMNP-QRST-VWXY-Z234-5678"
	r := NewRecovery(canonical, now)
	h := Header{Install: id.New("ins"), Bundle: id.New("cbk"), Generation: 1, GenerationID: id.New("gen"), Schema: 58, Taken: now}
	var sealed bytes.Buffer
	if err := SealBundle(&sealed, strings.NewReader("state"), 5, make([]byte, keySize), h, r); err != nil {
		t.Fatal(err)
	}
	for _, typed := range []string{strings.ToLower(canonical), strings.ReplaceAll(canonical, "-", ""), strings.ReplaceAll(canonical, "-", " "), "  oilA_BCDE_FGHJ_KMNP_QRST_VWXY_Z234_5678  "} {
		if canonicalPhrase(typed) != canonical {
			t.Errorf("did not normalize %q", typed)
		}
		if _, err := openBundle(io.Discard, bytes.NewReader(sealed.Bytes()), typed); err != nil {
			t.Errorf("could not open with %q: %v", typed, err)
		}
	}
	// A custom phrase which looked like a code used its literal spelling before round 2.
	legacy := strings.ToLower(canonical)
	r.Phrase, r.Base = legacy, r.KDF.derive(legacy)
	sealed.Reset()
	if err := SealBundle(&sealed, strings.NewReader("state"), 5, make([]byte, keySize), h, r); err != nil {
		t.Fatal(err)
	}
	if _, err := openBundle(io.Discard, bytes.NewReader(sealed.Bytes()), legacy); err != nil {
		t.Fatal(err)
	}
}

func TestSweepRemovesOnlyAbandonedControlBackupWork(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for _, entry := range []struct {
		name string
		age  time.Duration
	}{
		{".control-backup-stale", 2 * time.Hour},
		{".control-backup-fresh", 15 * time.Minute},
		{"other", 2 * time.Hour},
	} {
		path := filepath.Join(root, entry.name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-entry.age), now.Add(-entry.age)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sweepBackups(root, now); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != ".control-backup-fresh" || entries[1].Name() != "other" {
		t.Fatalf("wrong remaining directories: %v", entries)
	}
}

func TestRecoveryPhraseRejectsWeakSegments(t *testing.T) {
	for _, phrase := range []string{
		"password1234567890ab", "Password1234567890ab", "1qaz2wsx3edc4rfv5tgb", "qwertyuiop1234567890",
		"1234567890qwertyuiop", "zxcvbnmasdfghjklqwer", "aaaaaaaaaaaaaaaaaaab", "iloveyouiloveyou2026",
		"Summer2026!Summer2026", "P@ssw0rd!P@ssw0rd!12", "monkeymonkeymonkey12", "letmein1234567890abc",
		"abcdefghij1234567890", "qwertyuiopasdfghjklz", "asdfghjkl;asdfghjkl;", "trustno1trustno1trust",
		"correcthorsebatterystaple1", "cQ8#nRsW4@\tkH9zE2tM7w",
	} {
		if err := validatePhrase(phrase); err == nil {
			t.Errorf("accepted a weak or invalid test phrase %q", phrase)
		}
	}
	for _, phrase := range []string{"cQ8#nRsW4@kH9zE2tM7w", "q7#Vx9!mT2pL$wR4zK8n", "k3vq9xw2mzr8tp4ld7hy"} {
		if err := validatePhrase(phrase); err != nil {
			t.Errorf("rejected a random test phrase %q: %v", phrase, err)
		}
	}
}
