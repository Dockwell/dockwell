// SPDX-License-Identifier: AGPL-3.0-only

// Package seal encrypts a backup on its server before it leaves, so what's in a
// bucket means nothing without its passphrase, which a storage provider never has.
//
// A sealed file is a header, then chunks of up to ChunkSize bytes, each sealed with
// AES-256-GCM under a key derived from the passphrase and the file's own salt, with
// PBKDF2-SHA-256 as a passphrase can be weak. Each chunk's nonce is its number, and
// says whether it's the last, so a chunk can't be moved, dropped or added, and a file
// can't be cut short. Chunks are sealed on their own, so a part of an upload can be
// made again from its place in the file.
//
// The header names what the file is, its binding, in the clear, and every chunk
// authenticates the whole header. So an older file left under a newer name is refused
// by whoever asks for the newer one, and a header can't be changed, its work factor
// among it, without the file failing to open.
//
// dockwell open-backup opens one, with nothing else of Dockwell's (ADR 0033).
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
)

// magic starts a sealed file, with its format's version.
const magic = "dockwell-backup-2\n"

// Iterations is the work factor of a file sealed now. It costs a guess at a passphrase
// about what opening a file does, which is what a chosen phrase needs. Each file says
// its own, so this can grow.
const Iterations = 600_000

// maxIterations bounds what a header can ask of whoever opens it.
const maxIterations = 100_000_000

// work is what a file is sealed at, which a test of something else can lower.
var work = Iterations

// ChunkSize is how much of a file each chunk holds.
const ChunkSize = 1 << 20

const (
	saltSize  = 32
	nonceSize = 12
	tagSize   = 16
	// fixedSize is the part of a header every file has: the magic, the iterations, the
	// salt, and how long the binding is.
	fixedSize = len(magic) + 4 + saltSize + 2
	// SealedChunk is how long a whole chunk is, sealed.
	SealedChunk = ChunkSize + tagSize
)

// Sealer seals one file, chunk by chunk, in any order.
type Sealer struct {
	aead   cipher.AEAD
	header []byte
}

func keyed(passphrase string, salt []byte, iterations int) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// New starts a sealed file, with a salt of its own. binding says what it is, so it
// can be told from any other sealed with the same passphrase.
func New(passphrase, binding string) (*Sealer, error) {
	if passphrase == "" {
		return nil, errors.New("a sealed backup needs a passphrase")
	}
	if len(binding) > 1<<16-1 {
		return nil, errors.New("what a sealed file is bound to is too long")
	}
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := keyed(passphrase, salt, work)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, fixedSize+len(binding))
	header = append(header, magic...)
	header = binary.BigEndian.AppendUint32(header, uint32(work)) //nolint:gosec // a constant, and a small one
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint16(header, uint16(len(binding))) //nolint:gosec // checked above
	header = append(header, binding...)
	return &Sealer{aead: aead, header: header}, nil
}

// Header is what a sealed file starts with.
func (s *Sealer) Header() []byte { return s.header }

// nonce is a chunk's: its number, and whether it's the last.
func nonce(n uint64, last bool) []byte {
	out := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(out[3:11], n)
	if last {
		out[11] = 1
	}
	return out
}

// Seal seals chunk number n.
func (s *Sealer) Seal(n uint64, last bool, chunk []byte) []byte {
	return s.aead.Seal(nil, nonce(n, last), chunk, s.header)
}

// Size is how long the sealed file of size bytes is.
func (s *Sealer) Size(size int64) int64 {
	chunks := size / ChunkSize
	if size%ChunkSize != 0 || size == 0 {
		chunks++
	}
	return int64(len(s.header)) + size + chunks*tagSize
}

// Small seals something small whole.
func Small(plain []byte, passphrase, binding string) ([]byte, error) {
	s, err := New(passphrase, binding)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(s.Header())
	for n, at := uint64(0), 0; ; n, at = n+1, at+ChunkSize {
		end := min(at+ChunkSize, len(plain))
		last := end == len(plain)
		out = append(out, s.Seal(n, last, plain[at:end])...)
		if last {
			return out, nil
		}
	}
}

// Open reads a sealed file r, and writes what it holds to w. Nothing it wrote is to
// be trusted until it returns nil: a file cut short or changed fails, here or at its
// last chunk. With want set, a file bound to anything else is refused before any of it
// is written. It says what the file is bound to.
func Open(w io.Writer, r io.Reader, passphrase, want string) (string, error) {
	head := make([]byte, fixedSize)
	if _, err := io.ReadFull(r, head); err != nil {
		return "", fmt.Errorf("the file is too short to be a sealed backup: %w", err)
	}
	if string(head[:len(magic)]) != magic {
		return "", errors.New("the file isn't a sealed backup of this version")
	}
	iterations := binary.BigEndian.Uint32(head[len(magic):])
	if iterations == 0 || iterations > maxIterations {
		return "", errors.New("the file can't be opened: it asks for more work than any sealed backup does")
	}
	salt := head[len(magic)+4 : len(magic)+4+saltSize]
	bound := make([]byte, binary.BigEndian.Uint16(head[fixedSize-2:]))
	if _, err := io.ReadFull(r, bound); err != nil {
		return "", fmt.Errorf("the file is too short to be a sealed backup: %w", err)
	}
	binding := string(bound)
	if want != "" && binding != want {
		return binding, fmt.Errorf("this file holds %s, not %s", binding, want)
	}
	aead, err := keyed(passphrase, salt, int(iterations))
	if err != nil {
		return binding, err
	}
	header := slices.Concat(head, bound)
	// A chunk is the last one when nothing follows it, so one more is read ahead.
	this := make([]byte, SealedChunk)
	next := make([]byte, SealedChunk)
	n, err := io.ReadFull(r, this)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return binding, err
	}
	for chunk := uint64(0); ; chunk++ {
		m, err := io.ReadFull(r, next)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return binding, err
		}
		last := m == 0
		plain, err := aead.Open(nil, nonce(chunk, last), this[:n], header)
		if err != nil {
			return binding, errors.New("the backup can't be opened: the passphrase is wrong, or the file was changed or cut short")
		}
		if _, err := w.Write(plain); err != nil {
			return binding, err
		}
		if last {
			return binding, nil
		}
		this, next, n = next, this, m
	}
}
