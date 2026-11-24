// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
