// SPDX-License-Identifier: AGPL-3.0-only

// Package seal encrypts secrets the control plane keeps at rest, such as
// the CA's private key, under one key-encryption key held in a file only
// root can read.
//
// Each sealed value is AES-256-GCM with a random nonce, bound to a purpose
// string (as associated data), so a value sealed for one use can't be
// opened as another.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	keySize = 32
	version = 1
)

// ErrOpen reports a sealed value that is corrupt, was sealed with another
// key, or was sealed for another purpose.
var ErrOpen = errors.New("can't open sealed value")

// Key seals and opens values.
type Key struct {
	aead cipher.AEAD
}

// LoadOrCreate reads the key at path, creating it with 0600 permissions if
// it doesn't exist. It refuses a key file that others can read.
func LoadOrCreate(path string) (*Key, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own data directory
	switch {
	case errors.Is(err, fs.ErrNotExist):
		raw = make([]byte, keySize)
		_, _ = rand.Read(raw)
		if err := writeKey(path, raw); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("can't read the key at %s: %w", path, err)
	default:
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("the key at %s can be read by other users (mode %s); run chmod 600 %s", path, info.Mode().Perm(), path)
		}
	}
	if len(raw) != keySize {
		return nil, fmt.Errorf("the key at %s is %d bytes, not %d; restore it from backup", path, len(raw), keySize)
	}
	return newKey(raw)
}

// Load reads the key at path without ever creating one, for commands that
// must use the control plane's existing key.
func Load(path string) (*Key, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("can't find the key at %s: %w. Run this on the control server, as the user that runs dockwell", path, err)
	}
	return LoadOrCreate(path)
}

// Commit moves the key at from to path once what it seals is on record,
// and syncs the directory so the move survives a crash. The move is
// atomic: path has the whole key or none.
func Commit(from, path string) error {
	if err := os.Rename(from, path); err != nil {
		return fmt.Errorf("can't move the key from %s to %s: %w", from, path, err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("can't sync %s: %w", filepath.Dir(path), err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("can't sync %s: %w", filepath.Dir(path), err)
	}
	return nil
}

func writeKey(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("can't create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator's data directory
	if err != nil {
		return fmt.Errorf("can't create the key at %s: %w", path, err)
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func newKey(raw []byte) (*Key, error) {
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Key{aead: aead}, nil
}

// Seal encrypts plaintext for purpose.
func (k *Key) Seal(purpose string, plaintext []byte) []byte {
	nonce := make([]byte, k.aead.NonceSize())
	_, _ = rand.Read(nonce)
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+k.aead.Overhead())
	out = append(out, version)
	out = append(out, nonce...)
	return k.aead.Seal(out, nonce, plaintext, []byte(purpose))
}

// Open decrypts a value sealed for purpose.
func (k *Key) Open(purpose string, sealed []byte) ([]byte, error) {
	n := k.aead.NonceSize()
	if len(sealed) < 1+n+k.aead.Overhead() || sealed[0] != version {
		return nil, ErrOpen
	}
	plain, err := k.aead.Open(nil, sealed[1:1+n], sealed[1+n:], []byte(purpose))
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}
