// SPDX-License-Identifier: AGPL-3.0-only

package desired

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

var (
	web = resource.Ref{Kind: "service", Scope: "env_01TEST", Name: "web"}
	api = resource.Ref{Kind: "service", Scope: "env_01TEST", Name: "api"}
)

func record(revision int64) Record {
	return Record{
		Ref: web, ScopeName: "shop.production", Revision: revision, Change: "chg_01TEST",
		Spec:   []byte("\x0a\x05nginx-spec-text"),
		Values: map[string]string{"DATABASE_PASSWORD": "correct-horse-battery-staple"},
		Image:  resource.Image{ID: "sha256:aaaa", Digest: "nginx@sha256:bbbb"},
	}
}

func open(t *testing.T, dir, server string) *Store {
	t.Helper()
	s, err := Open(dir, server, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func held(s *Store) map[string]int64 {
	out := map[string]int64{}
	for _, h := range s.Holds().GetHeld() {
		out[h.GetResource().GetName()] = h.GetRevision()
	}
	return out
}

func keep(t *testing.T, s *Store, r Record) {
	t.Helper()
	if err := s.Keep(r); err != nil {
		t.Fatal(err)
	}
}

func remember(t *testing.T, s *Store, r Record) bool {
	t.Helper()
	kept, err := s.Remember(r)
	if err != nil {
		t.Fatal(err)
	}
	return kept
}

func revisionOf(t *testing.T, s *Store, ref resource.Ref) (int64, string) {
	t.Helper()
	r, ok, err := s.Read(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return 0, ""
	}
	return r.Revision, r.Values["DATABASE_PASSWORD"]
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func path(dir string, ref resource.Ref) string { return filepath.Join(dir, Folder, fileName(ref)) }

func TestARecordOutlivesARestart(t *testing.T) {
	dir := t.TempDir()
	first := open(t, dir, "srv_1")
	keep(t, first, record(4))
	s := open(t, dir, "srv_1")
	if got := held(s); !reflect.DeepEqual(got, map[string]int64{"web": 4}) || s.Holds().GetDamaged() != 0 {
		t.Fatalf("after a restart it holds %v, %d damaged", got, s.Holds().GetDamaged())
	}
	got, ok, err := s.Read(web)
	if err != nil || !ok || !reflect.DeepEqual(got, record(4)) {
		t.Fatalf("read %+v, %v, %v, want %+v", got, ok, err, record(4))
	}
	if _, ok, err := s.Read(api); ok || err != nil {
		t.Errorf("read what it doesn't keep: %v, %v", ok, err)
	}
	if c := s.Holds().GetCache(); c == "" || c != first.Holds().GetCache() {
		t.Errorf("the cache is %q after a restart, %q before; want one ID that stays", c, first.Holds().GetCache())
	}
}

func TestNothingOnDiskIsInTheClear(t *testing.T) {
	dir := t.TempDir()
	keep(t, open(t, dir, "srv_1"), record(1))
	b, err := os.ReadFile(path(dir, web))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"correct-horse-battery-staple", "nginx-spec-text", "shop.production", "sha256:aaaa", "chg_01TEST"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("the record has %q in the clear", secret)
		}
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, Folder):  0o700 | os.ModeDir,
		filepath.Join(dir, KeyFile): 0o600,
		path(dir, web):              0o600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode() != want {
			t.Errorf("%s is %v, %v, want %v", path, info.Mode(), err, want)
		}
	}
}

// A step says what runs, so it's kept whatever revision the cache held: a control
// plane's store that went back numbers lower. Only what the control plane sends
// to fill the cache is held to the revision kept.
func TestAStepAlwaysWinsAndAFillDoesntGoBack(t *testing.T) {
	s := open(t, t.TempDir(), "srv_1")
	keep(t, s, record(9))
	keep(t, s, record(3))
	if rev, _ := revisionOf(t, s, web); rev != 3 {
		t.Fatalf("a step at 3 over 9 keeps %d, want 3", rev)
	}
	rotated := record(5)
	rotated.Values["DATABASE_PASSWORD"] = "rotated"
	if !remember(t, s, rotated) {
		t.Fatal("a fill at 5 over 3 wasn't kept")
	}
	if remember(t, s, record(4)) {
		t.Error("a fill at 4 over 5 was kept")
	}
	if !remember(t, s, rotated) {
		t.Error("the same fill again wasn't kept")
	}
	// A restart acts on the resource as it is, with no revision of its own.
	restarted := record(0)
	restarted.Values["DATABASE_PASSWORD"] = "rotated"
	keep(t, s, restarted)
	if rev, value := revisionOf(t, s, web); rev != 5 || value != "rotated" {
		t.Errorf("keeps revision %d with %q, want 5 with the rotated value", rev, value)
	}
	if _, err := s.Remember(Record{Ref: web, Spec: []byte{}}); err == nil {
		t.Error("a fill with no revision was taken")
	}
}

// A deletion leaves a tombstone: nothing reads it or lists it, and a fill at or
// below its revision, read before the deletion, can't bring the record back. A
// step can, and a later fill can, and a tombstone goes once it has outlived any fill.
func TestADeletionHoldsBackALateFill(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "srv_1")
	keep(t, s, record(5))
	keep(t, s, Record{Ref: web, Revision: 6})
	if _, ok, err := s.Read(web); ok || err != nil {
		t.Fatalf("a deleted resource reads %v, %v", ok, err)
	}
	if got := held(s); len(got) != 0 {
		t.Fatalf("its hello lists %v", got)
	}
	for _, late := range []int64{5, 6} {
		if remember(t, s, record(late)) {
			t.Errorf("a fill at %d after the deletion at 6 brought it back", late)
		}
	}
	b, _ := os.ReadFile(path(dir, web))
	if bytes.Contains(b, []byte("correct-horse")) {
		t.Error("the tombstone has the value in the clear")
	}
	// It outlives a restart.
	s = open(t, dir, "srv_1")
	if remember(t, s, record(6)) {
		t.Error("after a restart, a fill at 6 brought it back")
	}
	if !remember(t, s, record(7)) {
		t.Error("a fill at 7, after the deletion at 6, wasn't kept")
	}
	keep(t, s, Record{Ref: web, Revision: 8})
	keep(t, s, record(2))
	if rev, _ := revisionOf(t, s, web); rev != 2 {
		t.Errorf("a step at 2 after the deletion keeps %d, want 2", rev)
	}
	// Once it has outlived any fill, it goes.
	keep(t, s, Record{Ref: web, Revision: 9})
	s = open(t, dir, "srv_1")
	s.now = func() time.Time { return time.Now().Add(TombstoneLife + time.Hour) }
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path(dir, web)); !os.IsNotExist(err) {
		t.Errorf("a tombstone past its life is still there: %v", err)
	}
}

// What the control plane has the agent forget goes only if the agent holds it
// at the revision the control plane saw: a step since then has the say.
func TestForgetTakesOnlyWhatTheControlPlaneSaw(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "srv_1")
	keep(t, s, record(4))
	if gone, err := s.Forget(web, 3); err != nil || gone {
		t.Fatalf("forget at 3 of what's held at 4 = %v, %v", gone, err)
	}
	if gone, err := s.Forget(web, 4); err != nil || !gone {
		t.Fatalf("forget at 4 = %v, %v", gone, err)
	}
	if _, err := os.Stat(path(dir, web)); !os.IsNotExist(err) {
		t.Errorf("the record is still there: %v", err)
	}
	if gone, err := s.Forget(web, 4); err != nil || gone {
		t.Errorf("forget again = %v, %v", gone, err)
	}
	keep(t, s, Record{Ref: api, Revision: 2})
	if gone, err := s.Forget(api, 2); err != nil || gone {
		t.Errorf("forget a tombstone = %v, %v", gone, err)
	}
}

// A step that passed and can't be kept leaves nothing that says what ran
// before: the older record goes, or, if it can't, nothing reads it.
func TestAFailedKeepForgetsTheOlderRecord(t *testing.T) {
	t.Run("the record can go", func(t *testing.T) {
		dir := t.TempDir()
		s := open(t, dir, "srv_1")
		keep(t, s, record(1))
		// Nothing can be renamed over a folder, which can be removed.
		if err := os.Remove(path(dir, web)); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path(dir, web), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := s.Keep(record(2)); err == nil || !strings.Contains(err.Error(), "so the agent forgot what it kept of it") {
			t.Fatalf("Keep = %v, want it failed, and the older record forgotten", err)
		}
		if _, ok, _ := s.Read(web); ok || len(held(s)) != 0 {
			t.Errorf("still reads it, or lists %v", held(s))
		}
	})
	t.Run("it can't", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root writes anything")
		}
		dir := t.TempDir()
		s := open(t, dir, "srv_1")
		keep(t, s, record(1))
		if err := os.Chmod(filepath.Join(dir, Folder), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, Folder), 0o700) })
		if err := s.Keep(record(2)); err == nil || !strings.Contains(err.Error(), "so the agent reads it no more") {
			t.Fatalf("Keep = %v, want it failed, and the older record unread", err)
		}
		if _, ok, _ := s.Read(web); ok || len(held(s)) != 0 {
			t.Errorf("still reads it, or lists %v", held(s))
		}
		if _, err := s.Remember(record(2)); err == nil {
			t.Error("a fill into a read-only folder said it kept it")
		}
	})
}

func TestAnEmptySpecIsKept(t *testing.T) {
	s := open(t, t.TempDir(), "srv_1")
	empty := record(1)
	empty.Spec = []byte{}
	keep(t, s, empty)
	got, ok, err := s.Read(web)
	if err != nil || !ok || got.Spec == nil || len(got.Spec) != 0 {
		t.Errorf("read %v, %v, %v, want an empty spec", got.Spec, ok, err)
	}
}

// Each way a record can come to not open: it's put aside, counted, and not held.
func TestARecordThatDoesntOpenIsPutAside(t *testing.T) {
	spoil := map[string]func(t *testing.T, dir, path string){
		"a byte changed": func(t *testing.T, _, path string) {
			b, _ := os.ReadFile(path)
			b[len(b)-3] ^= 1
			write(t, path, b)
		},
		"a byte of its resource changed": func(t *testing.T, _, path string) {
			b, _ := os.ReadFile(path)
			b[bytes.Index(b, []byte("web"))+2] = 'c'
			write(t, path, b)
		},
		"another format": func(t *testing.T, _, path string) {
			b, _ := os.ReadFile(path)
			b[3] = 2
			write(t, path, b)
		},
		"under another resource's name": func(t *testing.T, dir, path string) {
			if err := os.Rename(path, filepath.Join(dir, Folder, fileName(api))); err != nil {
				t.Fatal(err)
			}
		},
		"sealed with another key": func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, KeyFile), bytes.Repeat([]byte{7}, keySize))
		},
		"a key that isn't one": func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, KeyFile), []byte("short"))
		},
		"the key gone": func(t *testing.T, dir, _ string) {
			if err := os.Remove(filepath.Join(dir, KeyFile)); err != nil {
				t.Fatal(err)
			}
		},
	}
	for _, n := range []int{0, 1, 4, 5, 20} {
		spoil[fmt.Sprintf("cut to %d bytes", n)] = func(t *testing.T, _, path string) {
			b, _ := os.ReadFile(path)
			write(t, path, b[:n])
		}
	}
	for name, spoil := range spoil {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := open(t, dir, "srv_1")
			keep(t, s, record(1))
			spoil(t, dir, path(dir, web))
			s = open(t, dir, "srv_1")
			if got := held(s); len(got) != 0 || s.Holds().GetDamaged() != 1 {
				t.Fatalf("holds %v with %d damaged, want nothing held and 1 damaged", got, s.Holds().GetDamaged())
			}
			if left, _ := filepath.Glob(filepath.Join(dir, Folder, "*"+suffix)); len(left) != 0 {
				t.Errorf("left %v where records are read", left)
			}
			if aside, _ := filepath.Glob(filepath.Join(dir, Folder, Damaged, "*"+suffix)); len(aside) != 1 {
				t.Errorf("put aside %v, want the record", aside)
			}
			// It keeps again, with whatever key it has now.
			keep(t, s, record(1))
			if got := held(open(t, dir, "srv_1")); got["web"] != 1 {
				t.Errorf("kept again, a restart holds %v", got)
			}
		})
	}
	t.Run("two records swapped", func(t *testing.T) {
		dir := t.TempDir()
		s := open(t, dir, "srv_1")
		keep(t, s, record(1))
		other := record(1)
		other.Ref = api
		keep(t, s, other)
		a, b := path(dir, web), path(dir, api)
		if err := os.Rename(a, a+".x"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(b, a); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(a+".x", b); err != nil {
			t.Fatal(err)
		}
		s = open(t, dir, "srv_1")
		if got := held(s); len(got) != 0 || s.Holds().GetDamaged() != 2 {
			t.Errorf("holds %v with %d damaged, want both put aside", got, s.Holds().GetDamaged())
		}
	})
}

// A record that can't be read now, as for a disk error, isn't damage: it's left
// where it is, and the cache doesn't open, so the agent says why.
func TestARecordItCantReadNowIsntDamage(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads anything")
	}
	dir := t.TempDir()
	keep(t, open(t, dir, "srv_1"), record(1))
	if err := os.Chmod(path(dir, web), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "srv_1", nil); err == nil || !strings.Contains(err.Error(), "Check the disk") {
		t.Fatalf("Open = %v, want it to fail and say why", err)
	}
	if aside, _ := filepath.Glob(filepath.Join(dir, Folder, Damaged, "*")); len(aside) != 0 {
		t.Errorf("put aside %v", aside)
	}
	if err := os.Chmod(path(dir, web), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := held(open(t, dir, "srv_1")); got["web"] != 1 {
		t.Errorf("once it reads again, holds %v", got)
	}
}

func TestAnotherServersRecordDoesntOpen(t *testing.T) {
	dir := t.TempDir()
	keep(t, open(t, dir, "srv_1"), record(1))
	s := open(t, dir, "srv_2")
	if got := held(s); len(got) != 0 || s.Holds().GetDamaged() != 1 {
		t.Errorf("another server holds %v, %d damaged", got, s.Holds().GetDamaged())
	}
}

// What the sealing doesn't do, as the threat model says: a copy of the whole
// data folder, key and all, opens anywhere for the same server, and an older
// copy of a record from this server opens, at its own revision.
func TestWhatTheSealingDoesntStop(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "srv_1")
	keep(t, s, record(3))
	older, _ := os.ReadFile(path(dir, web))
	keep(t, s, record(5))

	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, Folder), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{KeyFile, filepath.Join(Folder, fileName(web))} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(elsewhere, f), b)
	}
	if rev, value := revisionOf(t, open(t, elsewhere, "srv_1"), web); rev != 5 || value != "correct-horse-battery-staple" {
		t.Errorf("the folder copied elsewhere reads revision %d with %q", rev, value)
	}

	write(t, path(dir, web), older)
	if rev, _ := revisionOf(t, open(t, dir, "srv_1"), web); rev != 3 {
		t.Errorf("an older copy put back reads revision %d, want 3", rev)
	}
}

// A key others can read is made the agent's alone again, and the log says so.
func TestTheKeyIsTheAgentsAlone(t *testing.T) {
	dir := t.TempDir()
	keep(t, open(t, dir, "srv_1"), record(1))
	if err := os.Chmod(filepath.Join(dir, KeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	s, err := Open(dir, "srv_1", slog.New(slog.NewTextHandler(io.MultiWriter(&log), nil)))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, KeyFile))
	if err != nil || info.Mode().Perm() != 0o600 || !strings.Contains(log.String(), "could be read by others") || held(s)["web"] != 1 {
		t.Errorf("the key is %v after, %v, holding %v, and the log says %q", info.Mode(), err, held(s), log.String())
	}
}

// Starts on one folder at once settle on one key, so none seals with a key that lost.
func TestStartsAtOnceAgreeOnAKey(t *testing.T) {
	for round := range 20 {
		dir := t.TempDir()
		stores := make([]*Store, 4)
		var wg sync.WaitGroup
		for i := range stores {
			wg.Go(func() {
				s, err := Open(dir, "srv_1", nil)
				if err != nil {
					t.Error(err)
					return
				}
				stores[i] = s
			})
		}
		wg.Wait()
		for i, s := range stores {
			if s == nil {
				t.FailNow()
			}
			r := record(1)
			r.Ref.Name = fmt.Sprintf("web-%d", i)
			keep(t, s, r)
		}
		if s := open(t, dir, "srv_1"); s.Holds().GetDamaged() != 0 || len(held(s)) != 4 {
			t.Fatalf("round %d: %d records sealed with a key that lost, %v held", round, s.Holds().GetDamaged(), held(s))
		}
	}
}

// What a power cut leaves of a write goes as the cache opens.
func TestAPowerCutsLeftoversGo(t *testing.T) {
	dir := t.TempDir()
	keep(t, open(t, dir, "srv_1"), record(1))
	write(t, filepath.Join(dir, Folder, "."+fileName(web)+".123456"), []byte("sealed bytes"))
	write(t, filepath.Join(dir, "."+KeyFile+".987"), bytes.Repeat([]byte{1}, keySize))
	s := open(t, dir, "srv_1")
	left, _ := filepath.Glob(filepath.Join(dir, Folder, ".*"))
	keys, _ := filepath.Glob(filepath.Join(dir, "."+KeyFile+".*"))
	if len(left) != 0 || len(keys) != 0 || held(s)["web"] != 1 {
		t.Errorf("left %v and %v, holding %v", left, keys, held(s))
	}
}

// The cache's ID changes with its key, so the control plane can tell a new cache.
func TestANewKeyIsANewCache(t *testing.T) {
	dir := t.TempDir()
	before := open(t, dir, "srv_1").Holds().GetCache()
	if err := os.Remove(filepath.Join(dir, KeyFile)); err != nil {
		t.Fatal(err)
	}
	if after := open(t, dir, "srv_1").Holds().GetCache(); after == before || len(after) != 16 {
		t.Errorf("the cache was %q, and is %q with a new key", before, after)
	}
}

func TestItNeedsTheServer(t *testing.T) {
	if _, err := Open(t.TempDir(), "", nil); err == nil {
		t.Error("opened with no server")
	}
}

// What the cache holds in memory is an index, not the records: 1,000 resources,
// each with a spec and a value of a few KiB, cost the agent well under a megabyte
// once opened (ADR 0054). The records are read only as reconciling needs one.
func TestTheIndexIsAllThatStaysInMemory(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "srv_1")
	big := strings.Repeat("x", 4<<10)
	for i := range 1000 {
		r := record(1)
		r.Ref.Name = fmt.Sprintf("web-%04d", i)
		// Sealed as Keep seals it, without its syncs, which take a while on a busy disk.
		b, err := s.seal(r.Ref, sealed{Revision: 1, Spec: []byte(big), Values: map[string]string{"DATABASE_PASSWORD": big}})
		if err != nil {
			t.Fatal(err)
		}
		write(t, path(dir, r.Ref), b)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	reopened := open(t, dir, "srv_1")
	runtime.GC()
	runtime.ReadMemStats(&after)
	if n := len(reopened.Holds().GetHeld()); n != 1000 {
		t.Fatalf("holds %d", n)
	}
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("1,000 records, %d KiB of specs and values on disk, hold %d KiB in memory", 1000*8, grew>>10)
	if grew > 1<<20 {
		t.Errorf("1,000 records hold %d KiB in memory, want under 1 MiB", grew>>10)
	}
	runtime.KeepAlive(reopened)
}
