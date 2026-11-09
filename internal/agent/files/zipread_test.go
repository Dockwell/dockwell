// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// readAll reads each entry of the archive in data as readZipDir does, as name to
// what it holds, a folder as "/".
func readAll(t *testing.T, data []byte) (map[string]string, error) {
	t.Helper()
	d, err := readZipDir(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	got := map[string]string{}
	err = d.each(func(e *zipEntry) error {
		if e.mode.IsDir() {
			got[e.name] = "/"
			return nil
		}
		r, err := d.open(e)
		if err != nil {
			return err
		}
		text, err := io.ReadAll(r)
		got[e.name] = string(text)
		return err
	})
	return got, err
}

// It reads what archive/zip writes: stored and deflated, folders, a comment, data
// descriptors, what's before the archive, and zip64.
func TestReadingAZipARecordAtATime(t *testing.T) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	want := map[string]string{"a.txt": strings.Repeat("deflated ", 1000), "dir/": "/", "dir/b.bin": "stored"}
	w, _ := z.Create("a.txt")
	_, _ = w.Write([]byte(want["a.txt"]))
	_, _ = z.CreateHeader(&zip.FileHeader{Name: "dir/"})
	w, _ = z.CreateHeader(&zip.FileHeader{Name: "dir/b.bin", Method: zip.Store})
	_, _ = w.Write([]byte("stored"))
	_ = z.SetComment("made by a test")
	_ = z.Close()

	for name, data := range map[string][]byte{
		"plain":           b.Bytes(),
		"self-extracting": append([]byte(strings.Repeat("#!/bin/sh\n", 50)), b.Bytes()...),
	} {
		got, err := readAll(t, data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: read %v", name, got)
		}
	}

	// More entries than 16 bits count, so archive/zip writes a zip64 end.
	var many bytes.Buffer
	z = zip.NewWriter(&many)
	for i := range 70_000 {
		_, _ = z.CreateHeader(&zip.FileHeader{Name: strconv.Itoa(i), Method: zip.Store})
	}
	_ = z.Close()
	d, err := readZipDir(bytes.NewReader(many.Bytes()), int64(many.Len()))
	if err != nil || d.count != 70_000 {
		t.Fatalf("zip64: %v, %v", d, err)
	}
	n := 0
	if err := d.each(func(*zipEntry) error { n++; return nil }); err != nil || n != 70_000 {
		t.Errorf("zip64: read %d, %v", n, err)
	}
}

// What doesn't read as it says, or can't be read, is refused.
func TestAZipThatDoesntReadAsItSays(t *testing.T) {
	good := zipOf(t, map[string]string{"a.txt": "hello"})
	corrupt := func(change func([]byte)) []byte {
		data := bytes.Clone(good)
		change(data)
		return data
	}
	dirAt := bytes.LastIndex(good, []byte{0x50, 0x4b, 0x01, 0x02})
	for name, data := range map[string][]byte{
		"not a zip":    []byte("hello, this isn't a zip archive at all"),
		"short":        good[:10],
		"no end":       good[:len(good)-zipEndLen],
		"bad checksum": corrupt(func(d []byte) { binary.LittleEndian.PutUint32(d[dirAt+16:], 1) }),
		"wrong size":   corrupt(func(d []byte) { binary.LittleEndian.PutUint32(d[dirAt+24:], 4) }),
		"encrypted":    corrupt(func(d []byte) { d[dirAt+8] |= zipEncrypted }),
		"other method": corrupt(func(d []byte) { binary.LittleEndian.PutUint16(d[dirAt+10:], 12) }),
		"says it holds more": corrupt(func(d []byte) {
			end := bytes.LastIndex(d, []byte{0x50, 0x4b, 0x05, 0x06})
			binary.LittleEndian.PutUint16(d[end+10:], 2)
		}),
	} {
		if got, err := readAll(t, data); err == nil {
			t.Errorf("%s: read %v", name, got)
		}
	}
}

// A false count must not turn a partial extraction into a successful unpack.
func TestUnpackingAZipWithAnUnderstatedCount(t *testing.T) {
	good := zipOf(t, map[string]string{"first.txt": "first", "later.txt": "later"})
	for _, count := range []uint16{0, 1} {
		t.Run(strconv.Itoa(int(count)), func(t *testing.T) {
			data := bytes.Clone(good)
			end := bytes.LastIndex(data, []byte{0x50, 0x4b, 0x05, 0x06})
			binary.LittleEndian.PutUint16(data[end+8:], count)
			binary.LittleEndian.PutUint16(data[end+10:], count)
			if got, err := readAll(t, data); err == nil {
				t.Errorf("a directory claiming %d entries read successfully: %v", count, got)
			}

			d := newDisk(t)
			d.noAccounts()
			path := filepath.Join(d.root, "site", "partial.zip")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			out := d.upload(t, path, data, dockwellv1.Unpack_UNPACK_ZIP)
			if out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UNPACK_REFUSED || out.GetFinished() == nil {
				t.Errorf("an archive claiming %d entries finished as %v", count, out)
			}
			if there(filepath.Join(d.root, "site", "partial")) {
				t.Error("a refused unpack left a partial output folder")
			}
			if kept, err := os.ReadFile(path); err != nil || !bytes.Equal(kept, data) {
				t.Errorf("a refused unpack changed its uploaded archive: %v", err)
			}
		})
	}
}

// A directory can end with its optional signature, but no unaccounted records.
func TestAZipsDirectoryTail(t *testing.T) {
	good := zipOf(t, map[string]string{"first.txt": "first"})
	end := bytes.LastIndex(good, []byte{0x50, 0x4b, 0x05, 0x06})
	signature := []byte{0x50, 0x4b, 0x05, 0x05, 3, 0, 's', 'i', 'g'}
	for _, tc := range []struct {
		name string
		tail []byte
		ok   bool
	}{
		{"empty", nil, true},
		{"signature", signature, true},
		{"cut signature", signature[:len(signature)-1], false},
		{"after signature", append(bytes.Clone(signature), 0), false},
		{"unknown record", []byte("unaccounted directory bytes"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append(bytes.Clone(good[:end]), tc.tail...)
			data = append(data, good[end:]...)
			size := binary.LittleEndian.Uint32(good[end+12:]) + uint32(len(tc.tail))
			binary.LittleEndian.PutUint32(data[end+len(tc.tail)+12:], size)
			got, err := readAll(t, data)
			if (err == nil) != tc.ok || (tc.ok && got["first.txt"] != "first") {
				t.Errorf("read %v, %v; want readable %v", got, err, tc.ok)
			}
		})
	}
}

// Its directory is read a record at a time, so the agent holds about as much for
// an archive of 99,000 entries as for one of a few. archive/zip held each entry,
// which took an agent past its budget.
func TestAZipsDirectoryIsntHeldWhole(t *testing.T) {
	if testing.Short() {
		t.Skip("writes an archive of 99,000 entries")
	}
	path := filepath.Join(t.TempDir(), "many.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for i := range 99_000 {
		if _, err := z.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("assets/img/icons/%06d.svg", i), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()

	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := heap()
	d, err := readZipDir(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	var most uint64
	n := 0
	if err := d.each(func(*zipEntry) error {
		n++
		if n%11_000 == 0 {
			most = max(most, heap())
		}
		return nil
	}); err != nil || n != 99_000 {
		t.Fatalf("read %d, %v", n, err)
	}
	if grew := int64(most) - int64(before); grew > 2<<20 {
		t.Errorf("reading the directory held %d bytes more", grew)
	}
}
