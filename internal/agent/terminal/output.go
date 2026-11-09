// SPDX-License-Identifier: AGPL-3.0-only

package terminal

// output is what a terminal printed last, numbered by byte from its start: a
// ring that grows as it fills, to max bytes, and then keeps the newest of them.
type output struct {
	buf   []byte
	max   int
	start uint64 // the first byte kept
	end   uint64 // one past the last byte printed
}

func newOutput(maxBytes int) *output {
	return &output{max: maxBytes}
}

// write keeps p, dropping what's oldest past max.
//
//nolint:gosec // lengths here are at most max, a few hundred KiB, and offsets only grow from 0
func (o *output) write(p []byte) {
	if len(p) > o.max {
		// Only its end fits, and what was kept goes.
		o.end += uint64(len(p) - o.max)
		o.start = o.end
		p = p[len(p)-o.max:]
	}
	if need := int(o.end-o.start) + len(p); need > len(o.buf) && len(o.buf) < o.max {
		o.grow(min(max(need, 2*len(o.buf), 4096), o.max))
	}
	for len(p) > 0 {
		at := int(o.end % uint64(len(o.buf)))
		n := copy(o.buf[at:], p)
		p = p[n:]
		o.end += uint64(n)
	}
	if o.end-o.start > uint64(len(o.buf)) {
		o.start = o.end - uint64(len(o.buf))
	}
}

// grow makes the ring size bytes long, keeping what it holds where it reads.
//
//nolint:gosec // as write
func (o *output) grow(size int) {
	if size <= len(o.buf) {
		return
	}
	kept := o.read(o.start, int(o.end-o.start))
	o.buf = make([]byte, size)
	for i, b := range kept {
		o.buf[int((o.start+uint64(i))%uint64(size))] = b
	}
}

// read returns up to n bytes from off, which is clamped to what's kept, and a
// copy, so it can be sent with no lock held.
//
//nolint:gosec // as write
func (o *output) read(off uint64, n int) []byte {
	if off < o.start {
		off = o.start
	}
	if off >= o.end || n <= 0 || len(o.buf) == 0 {
		return nil
	}
	n = min(n, int(o.end-off))
	out := make([]byte, n)
	at := int(off % uint64(len(o.buf)))
	c := copy(out, o.buf[at:])
	copy(out[c:], o.buf)
	return out
}
