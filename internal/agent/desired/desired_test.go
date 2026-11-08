// SPDX-License-Identifier: AGPL-3.0-only

package desired

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
)

var web = resource.Ref{Kind: "service", Scope: "env_01TEST", Name: "web"}

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

func TestARecordOutlivesARestart(t *testing.T) {
	dir := t.TempDir()
	if err := open(t, dir, "srv_1").Keep(record(4)); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir, "srv_1")
	if got := held(s); !reflect.DeepEqual(got, map[string]int64{"web": 4}) || s.Holds().GetDamaged() != 0 {
		t.Fatalf("after a restart it holds %v, %d damaged", got, s.Holds().GetDamaged())
	}
	got, ok, err := s.Read(web)
	if err != nil || !ok || !reflect.DeepEqual(got, record(4)) {
		t.Fatalf("read %+v, %v, %v, want %+v", got, ok, err, record(4))
	}
	if _, ok, err := s.Read(resource.Ref{Kind: "service", Scope: "env_01TEST", Name: "api"}); ok || err != nil {
		t.Errorf("read what it doesn't keep: %v, %v", ok, err)
	}
}

func TestNothingOnDiskIsInTheClear(t *testing.T) {
	dir := t.TempDir()
	if err := open(t, dir, "srv_1").Keep(record(1)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, Folder, fileName(web)))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"correct-horse-battery-staple", "nginx-spec-text", "shop.production", "sha256:aaaa", "chg_01TEST"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("the record has %q in the clear", secret)
		}
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, Folder):                0o700 | os.ModeDir,
		filepath.Join(dir, KeyFile):               0o600,
		filepath.Join(dir, Folder, fileName(web)): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode() != want {
			t.Errorf("%s is %v, %v, want %v", path, info.Mode(), err, want)
		}
	}
}

func TestOnlyALaterRevisionReplacesOne(t *testing.T) {
	s := open(t, t.TempDir(), "srv_1")
	if err := s.Keep(record(5)); err != nil {
		t.Fatal(err)
	}
	if err := s.Keep(record(3)); !errors.Is(err, ErrOlder) {
		t.Fatalf("an older revision: %v, want ErrOlder", err)
	}
	newer := record(6)
	newer.Values["DATABASE_PASSWORD"] = "rotated"
	if err := s.Keep(newer); err != nil {
		t.Fatal(err)
	}
	// A restart acts on the resource as it is, with no revision of its own.
	restarted := record(0)
	restarted.Values["DATABASE_PASSWORD"] = "rotated"
	if err := s.Keep(restarted); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Read(web)
	if err != nil || got.Revision != 6 || got.Values["DATABASE_PASSWORD"] != "rotated" {
		t.Errorf("keeps revision %d with %q, %v; want 6 with the rotated value", got.Revision, got.Values["DATABASE_PASSWORD"], err)
	}
}

func TestADeletionForgetsIt(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "srv_1")
	if err := s.Keep(record(2)); err != nil {
		t.Fatal(err)
	}
	gone := Record{Ref: web, Revision: 3}
	if err := s.Keep(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, Folder, fileName(web))); !os.IsNotExist(err) {
		t.Errorf("the record is still there: %v", err)
	}
	if got := held(open(t, dir, "srv_1")); len(got) != 0 {
		t.Errorf("after a restart it holds %v", got)
	}
}

func TestAnEmptySpecIsKept(t *testing.T) {
	s := open(t, t.TempDir(), "srv_1")
	empty := record(1)
	empty.Spec = []byte{}
	if err := s.Keep(empty); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Read(web)
	if err != nil || !ok || got.Spec == nil || len(got.Spec) != 0 {
		t.Errorf("read %v, %v, %v, want an empty spec", got.Spec, ok, err)
	}
}

// Each way a record can come to not open: it's put aside, counted, and not held.
func TestARecordThatDoesntOpenIsPutAside(t *testing.T) {
	api := resource.Ref{Kind: "service", Scope: "env_01TEST", Name: "api"}
	for name, spoil := range map[string]func(t *testing.T, dir, path string){
		"a byte changed": func(t *testing.T, _, path string) {
			b, _ := os.ReadFile(path)
			b[len(b)-3] ^= 1
			write(t, path, b)
		},
		"cut short": func(t *testing.T, _, path string) {
			b, _ := os.ReadFile(path)
			write(t, path, b[:len(b)/2])
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
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := open(t, dir, "srv_1")
			if err := s.Keep(record(1)); err != nil {
				t.Fatal(err)
			}
			spoil(t, dir, filepath.Join(dir, Folder, fileName(web)))
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
			if err := s.Keep(record(1)); err != nil {
				t.Fatal(err)
			}
			if got := held(open(t, dir, "srv_1")); got["web"] != 1 {
				t.Errorf("kept again, a restart holds %v", got)
			}
		})
	}
}

func TestAnotherServersRecordDoesntOpen(t *testing.T) {
	dir := t.TempDir()
	if err := open(t, dir, "srv_1").Keep(record(1)); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir, "srv_2")
	if got := held(s); len(got) != 0 || s.Holds().GetDamaged() != 1 {
		t.Errorf("another server holds %v, %d damaged", got, s.Holds().GetDamaged())
	}
}

func TestItNeedsTheServer(t *testing.T) {
	if _, err := Open(t.TempDir(), "", nil); err == nil {
		t.Error("opened with no server")
	}
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
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
		r.Spec = []byte(big)
		r.Values = map[string]string{"DATABASE_PASSWORD": big}
		// Sealed as Keep seals it, without its syncs, which take a while on a busy disk.
		b, err := s.seal(r)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, Folder, fileName(r.Ref)), b)
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
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc) //nolint:gosec // heap sizes fit
	t.Logf("1,000 records, %d KiB of specs and values on disk, hold %d KiB in memory", 1000*8, grew>>10)
	if grew > 1<<20 {
		t.Errorf("1,000 records hold %d KiB in memory, want under 1 MiB", grew>>10)
	}
	runtime.KeepAlive(reopened)
}
