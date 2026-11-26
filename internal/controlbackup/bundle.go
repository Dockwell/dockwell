// SPDX-License-Identifier: AGPL-3.0-only

// Package controlbackup seals the control plane's state for offline recovery.
package controlbackup

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	chunks "github.com/dockwell/dockwell/internal/backup/seal"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	magic     = "dockwell-control-backup-1\n"
	maxHeader = 64 << 10
	keySize   = 32
	memoryKiB = 64 << 10
	passes    = 3
	lanes     = 1
)

// KDF is the bounded Argon2id work factor recorded in the header.
type KDF struct {
	Memory uint32 `json:"memory_kib"`
	Passes uint32 `json:"passes"`
	Lanes  uint8  `json:"lanes"`
	Salt   []byte `json:"salt"`
}

func (k KDF) valid() bool {
	return k.Memory >= 8<<10 && k.Memory <= 256<<10 && k.Passes >= 1 && k.Passes <= 10 &&
		k.Lanes >= 1 && k.Lanes <= 4 && validSalt(k.Salt)
}

func validSalt(s []byte) bool { return len(s) >= 16 && len(s) <= 64 }

func (k KDF) derive(phrase string) []byte {
	return argon2.IDKey([]byte(phrase), k.Salt, k.Passes, k.Memory, k.Lanes, keySize)
}

// Recovery lives only in memory or sealed under the install key.
type Recovery struct {
	Phrase string
	Base   []byte
	KDF    KDF
	SetAt  time.Time
	ID     string
}

// NewRecovery derives the base key once when a recovery passphrase is set.
func NewRecovery(phrase string, at time.Time) Recovery {
	kdf := KDF{Memory: memoryKiB, Passes: passes, Lanes: lanes, Salt: randomSalt()}
	return Recovery{Phrase: phrase, Base: kdf.derive(phrase), KDF: kdf, SetAt: at.UTC(), ID: id.New("phrase")}
}

// GeneratePhrase gives 160 random bits in eight Crockford base32 groups.
func GeneratePhrase() string {
	var raw [20]byte
	_, _ = rand.Read(raw[:])
	s := id.Crockford(raw[:])
	var groups []string
	for s != "" {
		groups = append(groups, s[:4])
		s = s[4:]
	}
	return strings.Join(groups, "-")
}

func randomSalt() []byte {
	salt := make([]byte, 32)
	_, _ = rand.Read(salt)
	return salt
}

// Header is public. All its bytes are authenticated by each chunk, including the
// install and bundle IDs that bind the bytes to a particular backup.
type Header struct {
	Install      string    `json:"install"`
	Bundle       string    `json:"bundle"`
	Generation   int64     `json:"generation"`
	GenerationID string    `json:"generation_id"`
	Schema       int       `json:"schema"`
	Version      string    `json:"build_version"`
	Commit       string    `json:"build_commit"`
	Taken        time.Time `json:"taken"`
	PassphraseAt time.Time `json:"passphrase_at"`
	KDF          KDF       `json:"kdf"`
	Salt         []byte    `json:"bundle_salt"`
}

func validID(prefix, value string) bool {
	_, err := id.Parse(prefix, value)
	return err == nil
}

func (h Header) valid() bool {
	return validID("ins", h.Install) && validID("cbk", h.Bundle) && h.Generation > 0 &&
		validID("gen", h.GenerationID) && h.Schema >= 58 && !h.Taken.IsZero() &&
		!h.PassphraseAt.IsZero() && h.KDF.valid() && validSalt(h.Salt)
}

func (h Header) key(base []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, base, h.Salt, "dockwell control bundle\x00"+h.Install, keySize)
}

func readHeader(r io.Reader) (Header, []byte, error) {
	var h Header
	fixed := make([]byte, len(magic)+4)
	if _, err := io.ReadFull(r, fixed); err != nil || string(fixed[:len(magic)]) != magic {
		return h, nil, errors.New("the file is not a control plane backup of this version")
	}
	size := binary.BigEndian.Uint32(fixed[len(magic):])
	if size == 0 || size > uint32(maxHeader-len(magic)-4) {
		return h, nil, errors.New("the control plane backup header is too large or missing")
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(r, raw); err != nil {
		return h, nil, fmt.Errorf("read the control plane backup header: %w", err)
	}
	if err := json.Unmarshal(raw, &h); err != nil || !h.valid() {
		return h, nil, errors.New("the control plane backup header has invalid values")
	}
	return h, append(fixed, raw...), nil
}

// SealBundle writes the install key and snapshot in one sealed stream.
func SealBundle(w io.Writer, r io.Reader, size int64, installKey []byte, h Header, recovery Recovery) error {
	if len(installKey) != keySize || size <= 0 {
		return errors.New("the snapshot or install key has an invalid size")
	}
	h.KDF, h.PassphraseAt, h.Salt = recovery.KDF, recovery.SetAt, randomSalt()
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if !h.valid() || len(raw) > maxHeader-len(magic)-4 {
		return errors.New("the control plane backup header has invalid values")
	}
	head := binary.BigEndian.AppendUint32([]byte(magic), uint32(len(raw))) //nolint:gosec // bounded above
	head = append(head, raw...)
	key, err := h.key(recovery.Base)
	if err != nil {
		return err
	}
	sealer, err := chunks.WithKey(key, head)
	clear(key)
	if err != nil {
		return err
	}
	return sealer.Write(w, io.MultiReader(bytes.NewReader(installKey), r), size+keySize)
}

func openBundle(w io.Writer, r io.Reader, passphrase string) (Header, error) {
	h, head, err := readHeader(r)
	if err != nil {
		return h, err
	}
	base := h.KDF.derive(passphrase)
	key, err := h.key(base)
	clear(base)
	if err != nil {
		return h, err
	}
	sealer, err := chunks.WithKey(key, head)
	clear(key)
	if err != nil {
		return h, err
	}
	return h, sealer.Read(w, r)
}

type splitKey struct {
	key []byte
	db  io.Writer
}

func (s *splitKey) Write(p []byte) (int, error) {
	n := min(len(p), keySize-len(s.key))
	s.key = append(s.key, p[:n]...)
	if n == len(p) {
		return n, nil
	}
	written, err := s.db.Write(p[n:])
	return n + written, err
}

// Extract verifies the entire bundle before exposing its database and key in a
// new directory. It is an offline operation and does not start a control plane.
func Extract(ctx context.Context, r io.Reader, passphrase, dir string) (Header, error) {
	stage, err := os.MkdirTemp(filepath.Dir(dir), ".dockwell-open-")
	if err != nil {
		return Header{}, err
	}
	defer os.RemoveAll(stage)
	dbPath := filepath.Join(stage, "state.db")
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // inside the newly allocated private staging directory
	if err != nil {
		return Header{}, err
	}
	out := &splitKey{db: f}
	defer func() { clear(out.key) }()
	h, err := openBundle(out, r, passphrase)
	if err := errors.Join(err, f.Close()); err != nil {
		return h, err
	}
	snapshot, err := store.VerifyControlSnapshot(ctx, dbPath)
	if err != nil {
		return h, fmt.Errorf("verify the control plane store: %w", err)
	}
	if snapshot.Schema != h.Schema || snapshot.Install != h.Install ||
		snapshot.Backup.Generation != h.Generation || snapshot.Backup.GenerationID != h.GenerationID || len(out.key) != keySize {
		return h, errors.New("the control plane backup does not match its header")
	}
	if err := os.WriteFile(filepath.Join(stage, "key"), out.key, 0o600); err != nil {
		return h, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return h, err
	}
	for _, name := range []string{"state.db", "key"} {
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(dir, name)); err != nil {
			_ = os.Remove(filepath.Join(dir, "state.db"))
			_ = os.Remove(dir)
			return h, err
		}
	}
	return h, nil
}
