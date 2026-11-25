// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestZipWriterRoundTripsWithStdlibAndStreamReader(t *testing.T) {
	tmpDir := t.TempDir()
	var buf bytes.Buffer
	zw, err := newZipWriter(&buf, tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer zw.Abort()

	mod := time.Date(2026, 11, 20, 12, 34, 56, 0, time.UTC)
	if _, err := zw.Create("site/", mod, fs.ModeDir|0o750, zipStored); err != nil {
		t.Fatal(err)
	}
	w, err := zw.Create("site/secret.env", mod, 0o600, zipDeflated)
	if err != nil {
		t.Fatal(err)
	}
	envBody := strings.Repeat("KEY=value\n", 200)
	if _, err := io.WriteString(w, envBody); err != nil {
		t.Fatal(err)
	}
	w, err = zw.Create("site/run.sh", mod, 0o755, zipDeflated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "#!/bin/sh\necho hi\n"); err != nil {
		t.Fatal(err)
	}
	w, err = zw.Create("site/中文.txt", mod, 0o644, zipDeflated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "hello world"); err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Create("site/empty.txt", mod, 0o644, zipDeflated); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	// Verify no temp files left behind.
	if leftovers, err := os.ReadDir(tmpDir); err != nil || len(leftovers) != 0 {
		t.Fatalf("temp dir leftovers = %v, %v", leftovers, err)
	}

	// Verify stdlib archive/zip reads every entry, content and mode.
	raw := buf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("archive/zip.NewReader: %v", err)
	}
	if len(zr.File) != 5 {
		t.Fatalf("entries = %d, want 5", len(zr.File))
	}
	modes := map[string]fs.FileMode{}
	contents := map[string]string{}
	for _, f := range zr.File {
		modes[f.Name] = f.Mode()
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		contents[f.Name] = string(b)
	}
	if !modes["site/"].IsDir() || modes["site/"].Perm() != 0o750 {
		t.Errorf("site/ mode = %v, want drwxr-x---", modes["site/"])
	}
	if modes["site/secret.env"].Perm() != 0o600 || contents["site/secret.env"] != envBody {
		t.Errorf("secret.env mode=%o len=%d", modes["site/secret.env"].Perm(), len(contents["site/secret.env"]))
	}
	if modes["site/run.sh"].Perm() != 0o755 || contents["site/run.sh"] != "#!/bin/sh\necho hi\n" {
		t.Errorf("run.sh mode=%o content=%q", modes["site/run.sh"].Perm(), contents["site/run.sh"])
	}
	if contents["site/中文.txt"] != "hello world" || contents["site/empty.txt"] != "" {
		t.Errorf("other contents = %v", contents)
	}

	// Verify our own streaming zip reader reads it cleanly too.
	got, err := readAll(t, raw)
	if err != nil {
		t.Fatalf("readZipDir: %v", err)
	}
	if got["site/"] != "/" || got["site/secret.env"] != envBody || got["site/中文.txt"] != "hello world" {
		t.Errorf("readAll mismatch: %v", got)
	}
}

// TestZipWriterHeapFlatAtCap proves that building a zip at maxArchived
// (100,000 entries, ~45-byte names) keeps live heap flat instead of growing by
// tens of megabytes as archive/zip.Writer does (H1).
func TestZipWriterHeapFlatAtCap(t *testing.T) {
	if testing.Short() {
		t.Skip("writes an archive at maxArchived entries")
	}
	dir := t.TempDir()
	outPath := filepath.Join(dir, "cap.zip")
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}

	zw, err := newZipWriter(outFile, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer zw.Abort()

	// Warm up the single reusable flate.Writer before taking the baseline.
	mod := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	w, err := zw.Create("assets/img/icons/warmup.svg", mod, 0o644, zipDeflated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("warmup")); err != nil {
		t.Fatal(err)
	}
	before := heap()
	var peak uint64
	payload := []byte("x")

	for i := 1; i < maxArchived; i++ {
		name := fmt.Sprintf("node_modules/pkg-%03d/lib/sub/file-%06d.js", i%1000, i)
		ew, err := zw.Create(name, mod, 0o644, zipDeflated)
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if _, err := ew.Write(payload); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if i%20_000 == 0 {
			peak = max(peak, heap())
		}
	}
	peak = max(peak, heap())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	peak = max(peak, heap())

	grew := int64(peak) - int64(before)
	t.Logf("zipWriter at %d entries: baseline=%d KiB, peak=%d KiB, delta=%d KiB", maxArchived, before/1024, peak/1024, grew/1024)
	if grew > 4<<20 {
		t.Errorf("writing %d entries grew live heap by %d bytes (> 4 MiB)", maxArchived, grew)
	}

	// Verify the zip64 output parses cleanly with readZipDir.
	st, err := outFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	d, err := readZipDir(outFile, st.Size())
	if err != nil {
		t.Fatalf("readZipDir at cap: %v", err)
	}
	if d.count != maxArchived {
		t.Fatalf("zip64 entry count = %d, want %d", d.count, maxArchived)
	}
}

// N4: general-purpose bit 11 (UTF-8) must be set only for valid non-ASCII UTF-8
// names, never for raw non-UTF-8 bytes (such as Latin-1 caf\xe9.txt), in both
// the local header and the central directory record.
func TestZipWriterUTF8FlagOnlyForValidUTF8(t *testing.T) {
	var buf bytes.Buffer
	zw, err := newZipWriter(&buf, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer zw.Abort()

	mod := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	latin1 := "site/caf\xe9.txt"
	cases := []struct {
		name     string
		body     string
		wantUTF8 bool
	}{
		{"site/plain.txt", "ascii", false},
		{"site/中文.txt", "utf8", true},
		{latin1, "latin1", false},
	}
	for _, tc := range cases {
		w, err := zw.Create(tc.name, mod, 0o644, zipDeflated)
		if err != nil {
			t.Fatalf("Create(%q): %v", tc.name, err)
		}
		if _, err := io.WriteString(w, tc.body); err != nil {
			t.Fatalf("Write(%q): %v", tc.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	raw := buf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	d, err := readZipDir(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("readZipDir: %v", err)
	}
	localFlags := map[string]uint16{}
	cdFlags := map[string]uint16{}
	if err := d.each(func(e *zipEntry) error {
		cdFlags[e.name] = e.flags
		localFlags[e.name] = binary.LittleEndian.Uint16(raw[e.local+6 : e.local+8])
		return nil
	}); err != nil {
		t.Fatalf("zipDir.each: %v", err)
	}

	byName := map[string]*zip.File{}
	for _, f := range zr.File {
		byName[f.Name] = f
	}
	for _, tc := range cases {
		f, ok := byName[tc.name]
		if !ok {
			t.Fatalf("missing entry %q in archive/zip", tc.name)
		}
		if got := (f.Flags & zipFlagUTF8) != 0; got != tc.wantUTF8 {
			t.Errorf("%q archive/zip UTF-8 flag = %v, want %v", tc.name, got, tc.wantUTF8)
		}
		if got := (cdFlags[tc.name] & zipFlagUTF8) != 0; got != tc.wantUTF8 {
			t.Errorf("%q central dir UTF-8 flag = %v, want %v", tc.name, got, tc.wantUTF8)
		}
		if got := (localFlags[tc.name] & zipFlagUTF8) != 0; got != tc.wantUTF8 {
			t.Errorf("%q local header UTF-8 flag = %v, want %v", tc.name, got, tc.wantUTF8)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %q: %v", tc.name, err)
		}
		gotBody, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || string(gotBody) != tc.body {
			t.Errorf("%q body = %q, %v; want %q", tc.name, gotBody, err, tc.body)
		}
	}
}

func TestZipWriterComment(t *testing.T) {
	var buf bytes.Buffer
	zw, err := newZipWriter(&buf, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer zw.Abort()

	w, err := zw.Create("ok.txt", time.Unix(1_700_000_000, 0), 0o644, zipDeflated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "hello"); err != nil {
		t.Fatal(err)
	}
	const want = "Left out (2): .env, backup.tar"
	zw.SetComment(want)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	if zr.Comment != want {
		t.Errorf("zip comment = %q, want %q", zr.Comment, want)
	}
}

func TestZipWriterCommentTruncatesOnUTF8Boundary(t *testing.T) {
	var buf bytes.Buffer
	zw, err := newZipWriter(&buf, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer zw.Abort()

	if _, err := zw.Create("ok.txt", time.Unix(1_700_000_000, 0), 0o644, zipDeflated); err != nil {
		t.Fatal(err)
	}
	// Make a comment whose byte 65535 lands in the middle of a 3-byte UTF-8 rune.
	huge := "a" + strings.Repeat("中", 25_000)
	zw.SetComment(huge)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	if len(zr.Comment) > zip16 || len(zr.Comment) < zip16-4 {
		t.Errorf("truncated comment len = %d, want near %d", len(zr.Comment), zip16)
	}
	if !utf8.ValidString(zr.Comment) {
		t.Error("truncated zip comment split a UTF-8 character")
	}
}
