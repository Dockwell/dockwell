// SPDX-License-Identifier: AGPL-3.0-only

package seal

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"strings"
	"testing"
)

// bound is what the files here are bound to.
const bound = "env_1/data/bkp_1"

// cheap is what the files here are sealed at, as what these tests are about isn't how
// long a key takes.
const cheap = 1000

// file seals data as an upload does, chunk by chunk.
func file(t *testing.T, data []byte) []byte {
	t.Helper()
	s, err := New("a passphrase", bound, cheap)
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
		sealed := file(t, data)
		s, err := New("a passphrase", bound, cheap)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(sealed)) != s.Size(int64(size)) {
			t.Errorf("size %d: sealed to %d bytes, and Size says %d", size, len(sealed), s.Size(int64(size)))
		}
		var out bytes.Buffer
		if got, err := Open(&out, bytes.NewReader(sealed), "a passphrase", bound); err != nil || got != bound {
			t.Fatalf("size %d: %q, %v", size, got, err)
		}
		if !bytes.Equal(out.Bytes(), data) {
			t.Errorf("size %d: it came back different", size)
		}
	}
}

func TestWhatsInASealedFileDoesntShow(t *testing.T) {
	sealed := file(t, []byte(strings.Repeat("POSTGRES_PASSWORD=hunter2 ", 100)))
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Error("the plain text shows through")
	}
}

// Anything but the file as it was sealed fails: the wrong passphrase, a changed byte,
// a file cut at a chunk's edge, and chunks swapped.
func TestABadSealedFileFails(t *testing.T) {
	data := make([]byte, 3*ChunkSize)
	sealed := file(t, data)
	changed := bytes.Clone(sealed)
	changed[len(changed)/2] ^= 1
	head := fixedSize + len(bound)
	swapped := bytes.Clone(sealed)
	first, second := swapped[head:head+SealedChunk], swapped[head+SealedChunk:head+2*SealedChunk]
	held := bytes.Clone(first)
	copy(first, second)
	copy(second, held)
	for name, c := range map[string]struct {
		file       []byte
		passphrase string
	}{
		"wrong passphrase": {sealed, "another"},
		"changed":          {changed, "a passphrase"},
		"cut at a chunk":   {sealed[:head+2*SealedChunk], "a passphrase"},
		"cut in a chunk":   {sealed[:len(sealed)-100], "a passphrase"},
		"swapped":          {swapped, "a passphrase"},
		"not one":          {bytes.Repeat([]byte("x"), head+10), "a passphrase"},
		"too short":        {[]byte("x"), "a passphrase"},
	} {
		var out bytes.Buffer
		if _, err := Open(&out, bytes.NewReader(c.file), c.passphrase, bound); err == nil {
			t.Errorf("%s: it opened", name)
		}
	}
}

// A file left under another's name is refused before any of it is written, and a
// header can't be changed to pass for another or to ask for less work.
func TestAFileOpensOnlyAsWhatItIs(t *testing.T) {
	sealed := file(t, []byte("the orders"))
	var out bytes.Buffer
	if got, err := Open(&out, bytes.NewReader(sealed), "a passphrase", "env_1/data/bkp_2"); err == nil || got != bound || out.Len() != 0 {
		t.Errorf("under another's name: %q, %v, wrote %d bytes", got, err, out.Len())
	}
	other := bytes.Clone(sealed)
	copy(other[fixedSize:], "env_1/data/bkp_2")
	if _, err := Open(&out, bytes.NewReader(other), "a passphrase", "env_1/data/bkp_2"); err == nil {
		t.Error("a header changed to say it's another backup opened")
	}
	less := bytes.Clone(sealed)
	binary.BigEndian.PutUint32(less[len(magic):], 1)
	if _, err := Open(&out, bytes.NewReader(less), "a passphrase", bound); err == nil {
		t.Error("a header changed to ask for less work opened")
	}
	much := bytes.Clone(sealed)
	binary.BigEndian.PutUint32(much[len(magic):], maxIterations+1)
	if _, err := Open(&out, bytes.NewReader(much), "a passphrase", bound); err == nil || !strings.Contains(err.Error(), "more work") {
		t.Errorf("a header that asks for all the work it likes = %v", err)
	}
	if got, err := Open(&out, bytes.NewReader(sealed), "a passphrase", ""); err != nil || got != bound {
		t.Errorf("by hand, with nothing to compare: %q, %v", got, err)
	}
}

// A guess at a passphrase has to cost what opening a file costs, and a file says what
// that was, so it can grow.
func TestAFileIsSealedAtItsWork(t *testing.T) {
	s, err := New("a passphrase", bound, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(s.Header()[len(magic):]); got != 600_000 {
		t.Errorf("sealed at %d iterations", got)
	}
}

func TestSomethingSmallSealsWhole(t *testing.T) {
	for _, size := range []int{0, 300, ChunkSize, ChunkSize + 1} {
		data := bytes.Repeat([]byte("x"), size)
		sealed, err := Small(data, "a passphrase", bound, cheap)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if _, err := Open(&out, bytes.NewReader(sealed), "a passphrase", bound); err != nil || !bytes.Equal(out.Bytes(), data) {
			t.Errorf("size %d: %d bytes, %v", size, out.Len(), err)
		}
	}
}
