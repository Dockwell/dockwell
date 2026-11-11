// SPDX-License-Identifier: AGPL-3.0-only

package gitfetch

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/pjbgf/sha1cd"
)

// A pack is a stranger's, and a few hundred bytes of one can stand for gigabytes:
// a delta can copy its base thousands of times, a chain of them can make each base
// again for each delta on it, and millions of empty objects can each cost the index
// more than they cost the pack. So a pack is read as git index-pack reads one, but
// within bounds:
//
//   - It holds maxObjects objects at most, as its header counts them, and each
//     costs perObject against the budget, whatever it holds.
//   - It's kept on disk, and indexed in one pass that inflates each object to
//     nothing but its name's hash, or a delta's header, and counts what it inflates.
//     The index is one slice of fixed-size entries, found by offset with a binary
//     search, as offsets only grow, and by their raw names.
//   - A delta's base comes before it, by its offset: a fetch asks for ofs-delta and
//     no thin pack, so a delta that names its base by name, which could name one
//     after it and take a pass over the pack for each, is refused.
//   - Only what a commit's folder needs is made of its deltas: the commit, the trees
//     on the path, and the folder's blobs. What's made is kept, the newest within
//     maxCached, so a chain isn't made again for each delta on it.
//   - Every object made is maxObject at most, and everything inflated or made
//     counts against one budget, so neither memory nor time grows past it.
//   - The context is checked between objects.
const (
	// maxObjects is the most objects a pack may hold: a catalog's commit is a few
	// thousand files at most, in a few hundred folders.
	maxObjects = 100_000
	// perObject is what each object costs against the budget, beyond what it
	// inflates to, for what the index keeps of it and the work of reading it.
	perObject = 1 << 10
	// maxObject is the most any object made may be: a commit, a tree, a blob, or a
	// base on the way to one.
	maxObject = 8 << 20
	// maxCached is the most of what's made that's kept.
	maxCached = 32 << 20
	// maxDepth is how deep a delta's bases may go, as Git's own limit is 4095 and
	// what it writes is 50 at most.
	maxDepth = 64
)

const (
	objCommit   = 1
	objTree     = 2
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

var kindNames = map[int]string{objCommit: "commit", objTree: "tree", objBlob: "blob", objTag: "tag"}

// kindOf is an object type's number, by its name.
var kindOf = map[string]int8{"commit": objCommit, "tree": objTree, "blob": objBlob, "tag": objTag}

var (
	// errMissing is a name that isn't in the pack as an object of the type asked for.
	errMissing = errors.New("not in the pack")
	// errCollision is an object made to share another's name.
	errCollision = errors.New("an object is made to share another's SHA-1, which Git refuses")
)

// namer hashes as Git names objects, by SHA-1 with the collision detection Git's
// own SHA-1DC has, so an object made to share another's name, as a chosen-prefix
// collision would, is refused rather than taken for the commit's.
func namer() sha1cd.CollisionResistantHash {
	return sha1cd.New().(sha1cd.CollisionResistantHash) //nolint:forcetypeassert // what sha1cd.New returns
}

// nameOf is what a namer hashed, or errCollision.
func nameOf(h sha1cd.CollisionResistantHash) ([20]byte, error) {
	var name [20]byte
	sum, collides := h.CollisionResistantSum(nil)
	if collides {
		return name, errCollision
	}
	copy(name[:], sum)
	return name, nil
}

// tooBig is an object over what's asked of it.
type tooBig struct{ size int64 }

func (e tooBig) Error() string {
	return fmt.Sprintf("an object is %d bytes, more than a source reads", e.size)
}

// packEntry is one object of a pack, as indexed: 64 bytes, kept by value.
type packEntry struct {
	offset int64    // where it starts in the pack
	at     int64    // where its zlib stream starts
	size   int64    // what its stream inflates to: the object, or the delta
	base   int64    // a delta's base's offset
	made   int64    // a delta's result's size, as its header says
	name   [20]byte // its name, once named
	kind   int8     // what the pack says it is: a type, or a delta
	typ    int8     // its type, once known: at once for a whole object
	named  bool
}

func (e *packEntry) delta() bool { return e.kind == objOfsDelta }

type pack struct {
	ctx      context.Context
	r        io.ReaderAt
	entries  []packEntry
	byName   map[[20]byte]int32
	budget   int64
	cache    map[int64][]byte
	order    []int64       // what's kept, oldest first
	zr       io.ReadCloser // the zlib reader the index reuses
	filtered bool          // the host left out blobs over a limit
	cached   int64
	named    map[int8]bool
}

// counter counts what's read through it, so the index knows where each object
// starts. It's a flate.Reader, so zlib reads no further than a stream's end.
type counter struct {
	r *bufio.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *counter) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// head keeps the first bytes written to it, a delta's header, and drops the rest.
type head struct{ b []byte }

func (h *head) Write(p []byte) (int, error) {
	if room := 20 - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// indexPack reads a pack of size bytes from f once: it checks its checksum, and
// indexes each object, naming each whole one. budget bounds what it may inflate and
// make, then and later.
func indexPack(ctx context.Context, f *os.File, size, budget int64) (*pack, error) {
	if size < 32 {
		return nil, errors.New("what it sent isn't a pack")
	}
	var header [12]byte
	if _, err := f.ReadAt(header[:], 0); err != nil || string(header[:4]) != "PACK" {
		return nil, errors.New("what it sent isn't a pack")
	}
	if v := binary.BigEndian.Uint32(header[4:8]); v != 2 && v != 3 {
		return nil, fmt.Errorf("the pack is version %d, where Git's are 2 or 3", v)
	}
	// Each object takes at least two bytes of a pack, so a count past what the pack
	// could hold is a lie, and one past maxObjects is more than a source reads.
	count := binary.BigEndian.Uint32(header[8:12])
	switch {
	case count > maxObjects:
		return nil, fmt.Errorf("the pack holds %d objects, more than the %d a source reads. Keep the catalog in a repository of its own", count, maxObjects)
	case int64(count)*2 > size:
		return nil, errors.New("the pack ends inside an object")
	}
	p := &pack{ctx: ctx, r: f, budget: budget, cache: map[int64][]byte{}, named: map[int8]bool{}}
	if err := p.spend(int64(count) * perObject); err != nil {
		return nil, err
	}
	h := namer()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, size-20)); err != nil {
		return nil, err
	}
	sum, err := nameOf(h)
	if err != nil {
		return nil, err
	}
	var trailer [20]byte
	if _, err := f.ReadAt(trailer[:], size-20); err != nil || sum != trailer {
		return nil, errors.New("the pack's checksum doesn't match it")
	}
	p.entries = make([]packEntry, 0, count)
	p.byName = make(map[[20]byte]int32, count)
	c := &counter{r: bufio.NewReaderSize(io.NewSectionReader(f, 12, size-32), 64<<10), n: 12}
	for range count {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e, err := p.indexOne(c)
		if err != nil {
			return nil, err
		}
		p.entries = append(p.entries, e)
		if !e.delta() {
			if _, had := p.byName[e.name]; !had {
				p.byName[e.name] = int32(len(p.entries) - 1) //nolint:gosec // maxObjects fits
			}
		}
	}
	if c.n != size-20 {
		return nil, errors.New("the pack holds more than its header counts")
	}
	return p, nil
}

// indexOne indexes the object c is at, and leaves c at the next.
func (p *pack) indexOne(c *counter) (packEntry, error) {
	e := packEntry{offset: c.n}
	b, err := c.ReadByte()
	if err != nil {
		return e, errors.New("the pack ends inside an object")
	}
	e.kind = int8(b>>4) & 7
	e.size = int64(b & 15)
	for shift := 4; b&0x80 != 0; shift += 7 {
		if b, err = c.ReadByte(); err != nil || shift > 56 {
			return e, errors.New("an object's size can't be read")
		}
		e.size |= int64(b&0x7f) << shift
	}
	switch e.kind {
	case objOfsDelta:
		b, err := c.ReadByte()
		if err != nil {
			return e, errors.New("a delta's base can't be read")
		}
		back := int64(b & 0x7f)
		for b&0x80 != 0 {
			if b, err = c.ReadByte(); err != nil || back > 1<<48 {
				return e, errors.New("a delta's base can't be read")
			}
			back = (back+1)<<7 | int64(b&0x7f)
		}
		// A base comes before its delta, so no chain of offsets goes round.
		if back <= 0 || back > e.offset {
			return e, errors.New("a delta's base isn't before it in the pack")
		}
		e.base = e.offset - back
	case objRefDelta:
		return e, errors.New("the pack has a delta that names its base, which a fetch that asks for ofs-delta and no thin pack never gets")
	case objCommit, objTree, objBlob, objTag:
		e.typ = e.kind
	default:
		return e, fmt.Errorf("an object is of type %d, which Git doesn't have", e.kind)
	}
	if err := p.spend(e.size); err != nil {
		return e, err
	}
	e.at = c.n
	if p.zr == nil {
		if p.zr, err = zlib.NewReader(c); err != nil {
			return e, fmt.Errorf("an object can't be read: %w", err)
		}
	} else if err := p.zr.(zlib.Resetter).Reset(c, nil); err != nil { //nolint:forcetypeassert // what zlib.NewReader returns
		return e, fmt.Errorf("an object can't be read: %w", err)
	}
	var to io.Writer
	var named sha1cd.CollisionResistantHash
	var top head
	if e.typ != 0 {
		named = namer()
		fmt.Fprintf(named, "%s %d\x00", kindNames[int(e.typ)], e.size)
		to = named
	} else {
		to = &top
	}
	n, err := io.CopyN(to, p.zr, e.size)
	if err != nil && !errors.Is(err, io.EOF) {
		return e, fmt.Errorf("an object can't be read: %w", err)
	}
	if n != e.size {
		return e, errors.New("an object holds less than its size says")
	}
	// Its stream ends where its size says, which reading on to the end checks, with
	// the stream's own checksum, and leaves c at the next object.
	if more, err := io.CopyN(io.Discard, p.zr, 1); more != 0 || !errors.Is(err, io.EOF) {
		return e, errors.New("an object holds more than its size says")
	}
	if named != nil {
		if e.name, err = nameOf(named); err != nil {
			return e, err
		}
		e.named = true
		return e, nil
	}
	r := bytes.NewReader(top.b)
	if _, err := varint(r); err != nil {
		return e, errors.New("a delta can't be read")
	}
	if e.made, err = varint(r); err != nil {
		return e, errors.New("a delta can't be read")
	}
	return e, nil
}

// varint reads a delta header's size: seven bits a byte, least first.
func varint(r io.ByteReader) (int64, error) {
	var n int64
	for shift := 0; ; shift += 7 {
		c, err := r.ReadByte()
		if err != nil || shift > 56 {
			return 0, errors.New("a delta can't be read")
		}
		n |= int64(c&0x7f) << shift
		if c&0x80 == 0 {
			return n, nil
		}
	}
}

// spend counts n bytes inflated or made against the budget.
func (p *pack) spend(n int64) error {
	if p.budget -= n; p.budget < 0 {
		return errors.New("the pack's objects make more than a source reads. Name a repository of entries alone")
	}
	return nil
}

// at is the index of the entry at an offset, or -1.
func (p *pack) at(offset int64) int {
	i := sort.Search(len(p.entries), func(i int) bool { return p.entries[i].offset >= offset })
	if i < len(p.entries) && p.entries[i].offset == offset {
		return i
	}
	return -1
}

// object returns the object of a name and type, if it's most bytes at most: a whole
// one by its name, or a delta's, once the deltas of its type are named.
func (p *pack) object(hexName, typ string, most int64) ([]byte, error) {
	var name [20]byte
	if raw, err := hex.DecodeString(hexName); err != nil || len(raw) != 20 {
		return nil, errMissing
	} else {
		copy(name[:], raw)
	}
	want := kindOf[typ]
	for {
		if i, ok := p.byName[name]; ok {
			e := &p.entries[i]
			if e.typ != want {
				return nil, errMissing
			}
			size := e.size
			if e.delta() {
				size = e.made
			}
			if size > most {
				return nil, tooBig{size}
			}
			return p.data(int(i))
		}
		if p.named[want] {
			return nil, errMissing
		}
		if err := p.nameDeltas(want); err != nil {
			return nil, err
		}
		p.named[want] = true
	}
}

// nameDeltas makes each delta whose object is of type typ, to name it, in one pass in
// the pack's order: each delta's base comes before it, so its type is known by then.
func (p *pack) nameDeltas(typ int8) error {
	for i := range p.entries {
		e := &p.entries[i]
		if !e.delta() || e.named || e.made > maxObject {
			continue
		}
		if err := p.ctx.Err(); err != nil {
			return err
		}
		root, err := p.rootType(i)
		if err != nil {
			return err
		}
		if root != typ {
			continue
		}
		data, err := p.data(i)
		if err != nil {
			return err
		}
		h := namer()
		fmt.Fprintf(h, "%s %d\x00", kindNames[int(typ)], len(data))
		_, _ = h.Write(data) // a hash never fails to write
		if e.name, err = nameOf(h); err != nil {
			return err
		}
		e.named = true
		if _, had := p.byName[e.name]; !had {
			p.byName[e.name] = int32(i)
		}
	}
	return nil
}

// rootType is the type of the object at the root of an entry's chain of bases.
func (p *pack) rootType(i int) (int8, error) {
	chain, err := p.chain(i)
	if err != nil {
		return 0, err
	}
	return p.entries[chain[len(chain)-1]].typ, nil
}

// chain is an entry and its bases, by index, down to a whole object or a base
// already made.
func (p *pack) chain(i int) ([]int, error) {
	out := []int{i}
	for cur := i; p.entries[cur].delta(); {
		if _, ok := p.cache[p.entries[cur].offset]; ok && cur != i {
			break
		}
		if len(out) > maxDepth {
			return nil, errors.New("a delta's bases go deeper than Git's do")
		}
		base := p.at(p.entries[cur].base)
		if base < 0 {
			return nil, errors.New("a delta's base isn't in the pack")
		}
		out = append(out, base)
		cur = base
	}
	return out, nil
}

// data makes an entry's object: its base's, then each delta's on the way, keeping
// what it makes while there's room.
func (p *pack) data(i int) ([]byte, error) {
	if data, ok := p.cache[p.entries[i].offset]; ok {
		return data, nil
	}
	chain, err := p.chain(i)
	if err != nil {
		return nil, err
	}
	k := len(chain) - 1
	root := &p.entries[chain[k]]
	data, ok := p.cache[root.offset]
	if !ok {
		if root.delta() {
			return nil, errors.New("a delta's base isn't in the pack")
		}
		if data, err = p.inflate(root, maxObject); err != nil {
			return nil, err
		}
	}
	for ; k > 0; k-- {
		if err := p.ctx.Err(); err != nil {
			return nil, err
		}
		p.keep(p.entries[chain[k]].offset, data)
		d := &p.entries[chain[k-1]]
		if d.made > maxObject {
			return nil, tooBig{d.made}
		}
		delta, err := p.inflate(d, 2*maxObject)
		if err != nil {
			return nil, err
		}
		if err := p.spend(d.made); err != nil {
			return nil, err
		}
		if data, err = applyDelta(data, delta, maxObject); err != nil {
			return nil, err
		}
		d.typ = root.typ
	}
	if p.entries[i].delta() {
		p.keep(p.entries[i].offset, data)
	}
	return data, nil
}

// inflate reads an entry's stream, if it's most bytes at most.
func (p *pack) inflate(e *packEntry, most int64) ([]byte, error) {
	if e.size > most {
		return nil, tooBig{e.size}
	}
	if err := p.spend(e.size); err != nil {
		return nil, err
	}
	zr, err := zlib.NewReader(io.NewSectionReader(p.r, e.at, 1<<62))
	if err != nil {
		return nil, fmt.Errorf("an object can't be read: %w", err)
	}
	defer zr.Close()
	out := make([]byte, e.size)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, fmt.Errorf("an object can't be read: %w", err)
	}
	return out, nil
}

// keep keeps what's made, dropping the oldest to make room, so a chain isn't made
// again for each delta on it.
func (p *pack) keep(offset int64, data []byte) {
	if _, ok := p.cache[offset]; ok || int64(len(data)) > maxCached {
		return
	}
	for p.cached+int64(len(data)) > maxCached && len(p.order) > 0 {
		p.cached -= int64(len(p.cache[p.order[0]]))
		delete(p.cache, p.order[0])
		p.order = p.order[1:]
	}
	p.cache[offset] = data
	p.order = append(p.order, offset)
	p.cached += int64(len(data))
}
