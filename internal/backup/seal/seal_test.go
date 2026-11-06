// SPDX-License-Identifier: AGPL-3.0-only

package seal

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

// file seals data as an upload does, chunk by chunk.
func file(t *testing.T, data []byte, passphrase string) []byte {
	t.Helper()
	s, err := New(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	out := bytes.Clone(s.Header())
	n := uint64(0)
	for {
		size := min(len(data), ChunkSize)
		last := size == len(data)
		out = append(out, s.Seal(n, last, data[:size])...)
		if last {
			return out
		}
		data = data[size:]
		n++
	}
}

func TestASealedFileOpens(t *testing.T) {
	for _, size := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3*ChunkSize + 17} {
		data := make([]byte, size)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		sealed := file(t, data, "a passphrase")
		if int64(len(sealed)) != Size(int64(size)) {
			t.Errorf("size %d: sealed to %d bytes, and Size says %d", size, len(sealed), Size(int64(size)))
		}
		var out bytes.Buffer
		if err := Open(&out, bytes.NewReader(sealed), "a passphrase"); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(out.Bytes(), data) {
			t.Errorf("size %d: it came back different", size)
		}
	}
}

func TestWhatsInASealedFileDoesntShow(t *testing.T) {
	sealed := file(t, []byte(strings.Repeat("POSTGRES_PASSWORD=hunter2 ", 100)), "a passphrase")
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Error("the plain text shows through")
	}
}

// Anything but the file as it was sealed fails: the wrong passphrase, a changed byte,
// a file cut at a chunk's edge, and chunks swapped.
func TestABadSealedFileFails(t *testing.T) {
	data := make([]byte, 3*ChunkSize)
	sealed := file(t, data, "a passphrase")
	changed := bytes.Clone(sealed)
	changed[len(changed)/2] ^= 1
	swapped := bytes.Clone(sealed)
	first, second := swapped[HeaderSize:HeaderSize+SealedChunk], swapped[HeaderSize+SealedChunk:HeaderSize+2*SealedChunk]
	held := bytes.Clone(first)
	copy(first, second)
	copy(second, held)
	for name, c := range map[string]struct {
		file       []byte
		passphrase string
	}{
		"wrong passphrase": {sealed, "another"},
		"changed":          {changed, "a passphrase"},
		"cut at a chunk":   {sealed[:HeaderSize+2*SealedChunk], "a passphrase"},
		"cut in a chunk":   {sealed[:len(sealed)-100], "a passphrase"},
		"swapped":          {swapped, "a passphrase"},
		"not one":          {bytes.Repeat([]byte("x"), HeaderSize+10), "a passphrase"},
		"too short":        {[]byte("x"), "a passphrase"},
	} {
		var out bytes.Buffer
		if err := Open(&out, bytes.NewReader(c.file), c.passphrase); err == nil {
			t.Errorf("%s: it opened", name)
		}
	}
}
