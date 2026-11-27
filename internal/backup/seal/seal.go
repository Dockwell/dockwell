// SPDX-License-Identifier: AGPL-3.0-only

// Package seal encrypts a backup on its server before it leaves, so what's in a
// bucket means nothing without its passphrase, which a storage provider never has.
//
// A sealed file is a header, then chunks of up to the size named by its format, each sealed with
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
	"bufio"
	"bytes"
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
	"strconv"
	"strings"
)

// ArchiveVersion is the format used for new passphrase-sealed backups.
const ArchiveVersion = 3

// LegacyArchiveVersion is the one-MiB archive format used by older readers.
const LegacyArchiveVersion = 2

// magic starts a sealed file, with its format's version.
const (
	legacyMagic     = "dockwell-backup-2\n"
	magic           = "dockwell-backup-3\n"
	legacyChunkSize = 1 << 20
	// KeyedChunkSize is the fixed chunk size for callers that supply their own header.
	KeyedChunkSize = legacyChunkSize
	// KeyedSealedChunk is a full keyed chunk including its tag.
	KeyedSealedChunk = KeyedChunkSize + TagSize
	archiveChunkSize = 64 << 10
)

// Iterations is the work factor of a file sealed now. It costs a guess at a passphrase
// about what opening a file does, which is what a chosen phrase needs. Each file says
// its own, so this can grow.
const Iterations = 600_000

// maxIterations bounds what a header can ask of whoever opens it.
const maxIterations = 100_000_000

const (
	saltSize  = 32
	nonceSize = 12
	// TagSize is the overhead of sealing one chunk.
	TagSize   = 16
	fixedSize = len(magic) + 4 + saltSize + 2
)

// Sealer seals one file, chunk by chunk, in any order.
type Sealer struct {
	aead      cipher.AEAD
	header    []byte
	chunkSize int
}

func keyed(passphrase string, salt []byte, iterations int) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, 32)
	if err != nil {
		return nil, err
	}
	return gcm(key)
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// New starts a sealed file, with a salt of its own. binding says what it is, so it
// can be told from any other sealed with the same passphrase. iterations is what its
// key takes, and zero means Iterations: only a test gives any other, as a key takes
// over a second under the race detector.
func New(passphrase, binding string, iterations int) (*Sealer, error) {
	return newSealer(passphrase, binding, iterations, magic, archiveChunkSize)
}

func newSealer(passphrase, binding string, iterations int, version string, chunkSize int) (*Sealer, error) {
	if passphrase == "" {
		return nil, errors.New("a sealed backup needs a passphrase")
	}
	if iterations == 0 {
		iterations = Iterations
	}
	if len(binding) > 1<<16-1 {
		return nil, errors.New("what a sealed file is bound to is too long")
	}
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := keyed(passphrase, salt, iterations)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, fixedSize+4+len(binding))
	header = append(header, version...)
	header = binary.BigEndian.AppendUint32(header, uint32(iterations))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint16(header, uint16(len(binding))) //nolint:gosec // checked above
	if version == magic {
		header = binary.BigEndian.AppendUint32(header, uint32(chunkSize)) //nolint:gosec // fixed archive chunk size
	}
	header = append(header, binding...)
	return &Sealer{aead: aead, header: header, chunkSize: chunkSize}, nil
}

// WithKey reuses the backup chunk format with a separately derived key and header.
func WithKey(key, header []byte) (*Sealer, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead, header: header, chunkSize: KeyedChunkSize}, nil
}

// Write streams exactly size bytes with the header and an authenticated last chunk.
func (s *Sealer) Write(w io.Writer, r io.Reader, size int64) error {
	if _, err := w.Write(s.header); err != nil {
		return err
	}
	buf := make([]byte, min(size, int64(s.chunkSize)), min(size, int64(s.chunkSize))+TagSize)
	for n := uint64(0); ; n++ {
		part := buf[:min(size, int64(s.chunkSize))]
		if _, err := io.ReadFull(r, part); err != nil {
			return err
		}
		size -= int64(len(part))
		if _, err := w.Write(s.aead.Seal(part[:0], nonce(n, size == 0), part, s.header)); err != nil {
			return err
		}
		if size == 0 {
			return nil
		}
	}
}

// Header is what a sealed file starts with.
func (s *Sealer) Header() []byte { return s.header }

// ChunkSize returns the plaintext bytes in a full chunk.
func (s *Sealer) ChunkSize() int64 { return int64(s.chunkSize) }

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

// SealInPlace replaces a chunk with its sealed bytes when it has room for the tag.
func (s *Sealer) SealInPlace(n uint64, last bool, chunk []byte) []byte {
	return s.aead.Seal(chunk[:0], nonce(n, last), chunk, s.header)
}

// Size is how long the sealed file of size bytes is.
func (s *Sealer) Size(size int64) int64 {
	chunks := size / s.ChunkSize()
	if size%s.ChunkSize() != 0 || size == 0 {
		chunks++
	}
	return int64(len(s.header)) + size + chunks*TagSize
}

// Small seals something small whole, as New does.
func Small(plain []byte, passphrase, binding string, iterations int) ([]byte, error) {
	s, err := New(passphrase, binding, iterations)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(s.Header())
	for n, at := uint64(0), 0; ; n, at = n+1, at+s.chunkSize {
		end := min(at+s.chunkSize, len(plain))
		last := end == len(plain)
		out = append(out, s.Seal(n, last, plain[at:end])...)
		if last {
			return out, nil
		}
	}
}

// NewerArchiveError reports the format claimed by an unrecognized archive header.
type NewerArchiveError struct {
	Version uint64
}

func (e *NewerArchiveError) Error() string {
	return fmt.Sprintf("this backup uses sealed format v%d. Upgrade to a Dockwell version that supports it, then open the backup again", e.Version)
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
	version := string(head[:len(magic)])
	if version != legacyMagic && version != magic {
		line, _, hasLine := strings.Cut(string(head), "\n")
		number, hasPrefix := strings.CutPrefix(line, "dockwell-backup-")
		if hasLine && hasPrefix {
			n, err := strconv.ParseUint(number, 10, 64)
			if err == nil && n > ArchiveVersion {
				return "", &NewerArchiveError{Version: n}
			}
		}
		return "", errors.New("the file isn't a sealed backup of this version")
	}
	iterations := binary.BigEndian.Uint32(head[len(magic):])
	if iterations == 0 || iterations > maxIterations {
		return "", errors.New("the file can't be opened: it asks for more work than any sealed backup does")
	}
	salt := head[len(magic)+4 : len(magic)+4+saltSize]
	chunkSize := legacyChunkSize
	if version == magic {
		var size [4]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return "", fmt.Errorf("the file is too short to be a sealed backup: %w", err)
		}
		decoded := binary.BigEndian.Uint32(size[:])
		if decoded < archiveChunkSize || decoded > legacyChunkSize {
			return "", errors.New("the sealed backup has an unsupported chunk size")
		}
		chunkSize = int(decoded)
		head = append(head, size[:]...)
	}
	bound := make([]byte, binary.BigEndian.Uint16(head[fixedSize-2:fixedSize]))
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
	s := &Sealer{aead: aead, header: slices.Concat(head, bound), chunkSize: chunkSize}
	return binding, s.Read(w, r)
}

// Read opens the chunks after a header. Output must be staged until Read succeeds.
func (s *Sealer) Read(w io.Writer, r io.Reader) error {
	return s.ReadWithFallback(w, r, nil)
}

// ReadWithFallback can try a legacy key if the first chunk fails authentication.
func (s *Sealer) ReadWithFallback(w io.Writer, r io.Reader, fallback func() (*Sealer, error)) error {
	// A chunk is last if nothing follows it. Look ahead by one byte so
	// opening a large backup needs only one chunk buffer.
	chunkSize := s.chunkSize + TagSize
	if small, ok := r.(*bytes.Reader); ok {
		chunkSize = min(chunkSize, small.Len())
	}
	body := bufio.NewReader(r)
	this := make([]byte, chunkSize)
	for chunk := uint64(0); ; chunk++ {
		n, err := io.ReadFull(body, this)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		_, err = body.Peek(1)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		last := errors.Is(err, io.EOF)
		dest := this[:0]
		if chunk == 0 && fallback != nil {
			dest = nil // A failed Open may overwrite its destination.
		}
		plain, err := s.aead.Open(dest, nonce(chunk, last), this[:n], s.header)
		if err != nil && chunk == 0 && fallback != nil {
			s, err = fallback()
			if err != nil {
				return err
			}
			plain, err = s.aead.Open(this[:0], nonce(chunk, last), this[:n], s.header)
		}
		if err != nil {
			return errors.New("the backup can't be opened: the passphrase is wrong, or the file was changed or cut short")
		}
		if _, err := w.Write(plain); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}
