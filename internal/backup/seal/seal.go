// SPDX-License-Identifier: AGPL-3.0-only

// Package seal encrypts a backup on its server before it leaves, so what's in a
// bucket means nothing without its passphrase, which a storage provider never has.
//
// A sealed file is a header, then chunks of up to ChunkSize bytes, each sealed with
// AES-256-GCM under a key derived from the passphrase and the file's own salt. Each
// chunk's nonce is its number, and says whether it's the last, so a chunk can't be
// moved, dropped or added, and a file can't be cut short. Chunks are sealed on their
// own, so a part of an upload can be made again from its place in the file.
//
// dockwell open-backup opens one, with nothing else of Dockwell's (ADR 0033).
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// magic starts a sealed file, with its format's version.
const magic = "dockwell-backup-1\n"

// ChunkSize is how much of a file each chunk holds.
const ChunkSize = 1 << 20

const (
	saltSize  = 32
	nonceSize = 12
	tagSize   = 16
	// HeaderSize is how long a header is.
	HeaderSize = len(magic) + saltSize
	// SealedChunk is how long a whole chunk is, sealed.
	SealedChunk = ChunkSize + tagSize
)

// Sealer seals one file, chunk by chunk, in any order.
type Sealer struct {
	aead cipher.AEAD
	salt []byte
}

func keyed(passphrase string, salt []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, []byte(passphrase), salt, "dockwell backup", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// New starts a sealed file, with a salt of its own.
func New(passphrase string) (*Sealer, error) {
	if passphrase == "" {
		return nil, errors.New("a sealed backup needs a passphrase")
	}
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := keyed(passphrase, salt)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead, salt: salt}, nil
}

// Header is what a sealed file starts with.
func (s *Sealer) Header() []byte { return append([]byte(magic), s.salt...) }

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
	return s.aead.Seal(nil, nonce(n, last), chunk, nil)
}

// Size is how long the sealed file of size bytes is.
func Size(size int64) int64 {
	chunks := size / ChunkSize
	if size%ChunkSize != 0 || size == 0 {
		chunks++
	}
	return int64(HeaderSize) + size + chunks*tagSize
}

// Open reads a sealed file r, and writes what it holds to w. Nothing it wrote is to
// be trusted until it returns nil: a file cut short or changed fails, here or at its
// last chunk.
func Open(w io.Writer, r io.Reader, passphrase string) error {
	head := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, head); err != nil {
		return fmt.Errorf("the file is too short to be a sealed backup: %w", err)
	}
	if string(head[:len(magic)]) != magic {
		return errors.New("the file isn't a sealed backup")
	}
	aead, err := keyed(passphrase, head[len(magic):])
	if err != nil {
		return err
	}
	// A chunk is the last one when nothing follows it, so one more is read ahead.
	this := make([]byte, SealedChunk)
	next := make([]byte, SealedChunk)
	n, err := io.ReadFull(r, this)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	for chunk := uint64(0); ; chunk++ {
		m, err := io.ReadFull(r, next)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		last := m == 0
		plain, err := aead.Open(nil, nonce(chunk, last), this[:n], nil)
		if err != nil {
			return errors.New("the backup can't be opened: the passphrase is wrong, or the file was changed or cut short")
		}
		if _, err := w.Write(plain); err != nil {
			return err
		}
		if last {
			return nil
		}
		this, next, n = next, this, m
	}
}
