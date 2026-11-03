// SPDX-License-Identifier: AGPL-3.0-only

// Package id makes the identifiers Dockwell gives its objects: a short type
// prefix and a ULID, such as "srv_01JD6Z3R8M9QF2K7T4XW5BNC0V".
//
// The prefix says what an ID names wherever it appears (logs, URLs, the CLI);
// the ULID sorts by creation time, which keeps database indexes compact.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Prefixes for each kind of object.
const (
	Server    = "srv"
	JoinToken = "jtk"
	Event     = "evt"
	User      = "usr"
	Session   = "ses"
)

// crockford is Crockford's base32 alphabet: no I, L, O or U, so IDs survive
// being read aloud or copied by hand.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const ulidLen = 26

// New returns a new ID with the given prefix, using the current time.
func New(prefix string) string {
	return newAt(prefix, time.Now())
}

func newAt(prefix string, t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	// The 48-bit timestamp is split across 2 and 4 bytes; truncation is the point.
	binary.BigEndian.PutUint16(b[0:2], uint16(ms>>32)) //nolint:gosec // see above
	binary.BigEndian.PutUint32(b[2:6], uint32(ms))     //nolint:gosec // see above
	// crypto/rand.Read never fails on supported platforms (Go 1.24+).
	_, _ = rand.Read(b[6:])
	return prefix + "_" + encode(b)
}

// encode writes 128 bits as 26 base32 characters, most significant first.
func encode(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	var out [ulidLen]byte
	for i := ulidLen - 1; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// ErrMalformed reports an ID that isn't prefix_ULID.
var ErrMalformed = errors.New("malformed ID")

// Parse checks that s is an ID with the given prefix and returns the time
// it was created.
func Parse(prefix, s string) (time.Time, error) {
	p, ulid, ok := strings.Cut(s, "_")
	if !ok || p != prefix || len(ulid) != ulidLen {
		return time.Time{}, fmt.Errorf("%w: %q is not a %s_ ID", ErrMalformed, s, prefix)
	}
	// The first character carries only 3 bits of the 130 encoded, so it
	// can't exceed 7.
	if strings.IndexByte(crockford[:8], ulid[0]) < 0 {
		return time.Time{}, fmt.Errorf("%w: %q is out of range", ErrMalformed, s)
	}
	var ms uint64
	for i := range 10 {
		v := strings.IndexByte(crockford, ulid[i])
		if v < 0 {
			return time.Time{}, fmt.Errorf("%w: %q has a character outside base32", ErrMalformed, s)
		}
		ms = ms<<5 | uint64(v)
	}
	for i := 10; i < ulidLen; i++ {
		if strings.IndexByte(crockford, ulid[i]) < 0 {
			return time.Time{}, fmt.Errorf("%w: %q has a character outside base32", ErrMalformed, s)
		}
	}
	return time.UnixMilli(int64(ms)).UTC(), nil
}
