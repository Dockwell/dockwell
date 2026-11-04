// SPDX-License-Identifier: AGPL-3.0-only

// Package secrets keeps what may be secret encrypted at rest, and gets it
// only to the servers that need it. See
// docs/adr/0022-secrets-encrypted-at-rest.md.
//
// The keys form a hierarchy:
//   - the install's key, in a file beside the database (internal/seal);
//   - a key-encryption key, kept in the database sealed with the install's
//     key, which wraps every data key and rotates on its own (RotateKEK);
//   - a data key for each stored spec of a kind that may hold a secret,
//     kept in the database wrapped under the key-encryption key;
//   - the spec, sealed under its data key, bound to its resource and kind.
//
// The revision store seals and opens specs through a Keeper, so revisions
// and change items never hold such a spec in the clear. A plan's
// fingerprint digests such a spec with a key of its own (Digest), so it
// reveals nothing of it either.
package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

// magic starts every sealed spec. No protobuf encoding starts with a zero
// byte, as field number 0 doesn't exist, so a stored spec says itself
// whether it is sealed.
var magic = []byte{0, 'd', 'w', 's', 1}

const keySize = 32

// ErrOpen reports a sealed spec that can't be opened: it was sealed for
// another resource, or with keys this install doesn't have, or it was
// changed.
var ErrOpen = errors.New("can't open a sealed spec")

// Keeper seals and opens specs, and keeps the keys that do it.
type Keeper struct {
	db     *store.DB
	key    *seal.Key
	kinds  *resource.Registry
	digest []byte
	now    func() time.Time

	mu   sync.Mutex
	keks map[string]cipher.AEAD // unsealed key-encryption keys, by ID
}

// New returns a Keeper over db, with the install's key. kinds reads the
// specs it resolves and reveals.
func New(db *store.DB, key *seal.Key, kinds *resource.Registry) *Keeper {
	return &Keeper{
		db: db, key: key, kinds: kinds, digest: key.Derive("spec digests"), now: time.Now,
		keks: map[string]cipher.AEAD{},
	}
}

// Sealed reports whether a stored spec is sealed.
func Sealed(stored []byte) bool { return bytes.HasPrefix(stored, magic) }

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealWith(aead cipher.AEAD, plain, ad, prefix []byte) []byte {
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	out := append(slices.Clip(prefix), nonce...)
	return aead.Seal(out, nonce, plain, ad)
}

func openWith(aead cipher.AEAD, sealed, ad []byte) ([]byte, error) {
	n := aead.NonceSize()
	if len(sealed) < n+aead.Overhead() {
		return nil, ErrOpen
	}
	plain, err := aead.Open(nil, sealed[:n], sealed[n:], ad)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

func kekPurpose(kekID string) string { return "secrets.kek|" + kekID }

func dataKeyAD(dekID, kekID string) []byte { return []byte("dockwell.data-key|" + dekID + "|" + kekID) }

func specAD(resourceID string, kind resource.Kind) []byte {
	return []byte("dockwell.spec|" + resourceID + "|" + string(kind))
}

// kek returns a key-encryption key, unsealed with the install's key.
func (k *Keeper) kek(ctx context.Context, tx *store.Tx, kekID string) (cipher.AEAD, error) {
	k.mu.Lock()
	aead, ok := k.keks[kekID]
	k.mu.Unlock()
	if ok {
		return aead, nil
	}
	stored, err := tx.GetKEK(ctx, kekID)
	if err != nil {
		return nil, err
	}
	if stored.RetiredAt != nil {
		return nil, fmt.Errorf("key-encryption key %s was retired: %w", kekID, ErrOpen)
	}
	return k.unseal(stored)
}

func (k *Keeper) unseal(stored store.KeyEncryptionKey) (cipher.AEAD, error) {
	raw, err := k.key.Open(kekPurpose(stored.ID), stored.Sealed)
	if err != nil {
		return nil, fmt.Errorf("can't open key-encryption key %s with this install's key, which may not be the one it was made with. "+
			"Restore the key file from the backup taken with this database: %w", stored.ID, err)
	}
	defer clear(raw)
	aead, err := newAEAD(raw)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	k.keks[stored.ID] = aead
	k.mu.Unlock()
	return aead, nil
}

// activeKEK returns the key-encryption key that wraps new data keys,
// making the first one when there is none, in tx.
func (k *Keeper) activeKEK(ctx context.Context, tx *store.Tx) (string, cipher.AEAD, error) {
	stored, err := tx.ActiveKEK(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		kekID, aead, err := k.newKEK(ctx, tx)
		return kekID, aead, err
	case err != nil:
		return "", nil, err
	}
	aead, err := k.kek(ctx, tx, stored.ID)
	return stored.ID, aead, err
}

func (k *Keeper) newKEK(ctx context.Context, tx *store.Tx) (string, cipher.AEAD, error) {
	raw := make([]byte, keySize)
	_, _ = rand.Read(raw)
	defer clear(raw)
	kekID := id.New(id.KeyEncryptionKey)
	stored := store.KeyEncryptionKey{ID: kekID, Sealed: k.key.Seal(kekPurpose(kekID), raw), CreatedAt: k.now()}
	if err := tx.CreateKEK(ctx, stored); err != nil {
		return "", nil, err
	}
	aead, err := newAEAD(raw)
	if err != nil {
		return "", nil, err
	}
	k.mu.Lock()
	k.keks[kekID] = aead
	k.mu.Unlock()
	return kekID, aead, nil
}

// Seal encrypts a spec's encoding for a resource of kind, under a new data
// key that it records in tx. tx must be a write transaction.
func (k *Keeper) Seal(ctx context.Context, tx *store.Tx, resourceID string, kind resource.Kind, encoded []byte) ([]byte, error) {
	kekID, kek, err := k.activeKEK(ctx, tx)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, keySize)
	_, _ = rand.Read(raw)
	defer clear(raw)
	dekID := id.New(id.DataKey)
	if err := tx.CreateDataKey(ctx, store.DataKey{
		ID: dekID, KEKID: kekID, Wrapped: sealWith(kek, raw, dataKeyAD(dekID, kekID), nil), CreatedAt: k.now(),
	}); err != nil {
		return nil, err
	}
	dek, err := newAEAD(raw)
	if err != nil {
		return nil, err
	}
	if len(dekID) > 255 {
		return nil, fmt.Errorf("data key ID %s is too long to record in a sealed spec", dekID)
	}
	prefix := append(slices.Clone(magic), byte(len(dekID))) //nolint:gosec // checked above: it fits in a byte
	prefix = append(prefix, dekID...)
	return sealWith(dek, encoded, specAD(resourceID, kind), prefix), nil
}

// Open decrypts what Seal made for a resource of kind. A spec stored before
// sealing, which isn't sealed, comes back as it is.
func (k *Keeper) Open(ctx context.Context, tx *store.Tx, resourceID string, kind resource.Kind, stored []byte) ([]byte, error) {
	if !Sealed(stored) {
		return stored, nil
	}
	rest := stored[len(magic):]
	if len(rest) < 1 || len(rest) < 1+int(rest[0]) {
		return nil, ErrOpen
	}
	dekID, sealed := string(rest[1:1+int(rest[0])]), rest[1+int(rest[0]):]
	d, err := tx.GetDataKey(ctx, dekID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("data key %s is gone: %w", dekID, ErrOpen)
	}
	if err != nil {
		return nil, err
	}
	kek, err := k.kek(ctx, tx, d.KEKID)
	if err != nil {
		return nil, err
	}
	raw, err := openWith(kek, d.Wrapped, dataKeyAD(dekID, d.KEKID))
	if err != nil {
		return nil, fmt.Errorf("data key %s: %w", dekID, err)
	}
	defer clear(raw)
	dek, err := newAEAD(raw)
	if err != nil {
		return nil, err
	}
	plain, err := openWith(dek, sealed, specAD(resourceID, kind))
	if err != nil {
		return nil, fmt.Errorf("the spec of %s: %w", resourceID, err)
	}
	return plain, nil
}

// Decode opens a stored spec of kind and reads it.
func (k *Keeper) Decode(ctx context.Context, tx *store.Tx, resourceID string, kind resource.Kind, stored []byte) (proto.Message, error) {
	plain, err := k.Open(ctx, tx, resourceID, kind, stored)
	if err != nil {
		return nil, err
	}
	return k.kinds.Decode(kind, plain)
}

// Digest is a keyed hash of a spec's encoding, for a plan's fingerprint:
// the same spec always digests the same, and without the install's key
// the digest says nothing of it, as a plain hash of a short password
// would.
func (k *Keeper) Digest(encoded []byte) []byte {
	mac := hmac.New(sha256.New, k.digest)
	mac.Write(encoded)
	return mac.Sum(nil)
}

// RotateKEK makes a new key-encryption key, wraps every data key under it,
// and retires the others, forgetting them, all in one transaction. The
// data keys, and so every sealed spec, stay as they are. It records the
// rotation in the audit log as actor, and returns the new key's ID and how
// many data keys it wrapped again.
func (k *Keeper) RotateKEK(ctx context.Context, actor, surface string) (string, int, error) {
	var kekID string
	var n int
	err := k.db.Update(ctx, func(tx *store.Tx) error {
		n = 0
		olds, err := tx.LiveKEKs(ctx)
		if err != nil {
			return err
		}
		newID, next, err := k.newKEK(ctx, tx)
		if err != nil {
			return err
		}
		kekID = newID
		for _, old := range olds {
			from, err := k.kek(ctx, tx, old.ID)
			if err != nil {
				return err
			}
			after := ""
			for {
				page, err := tx.DataKeysUnder(ctx, old.ID, after, 500)
				if err != nil {
					return err
				}
				for _, d := range page {
					raw, err := openWith(from, d.Wrapped, dataKeyAD(d.ID, old.ID))
					if err != nil {
						return fmt.Errorf("can't open data key %s to wrap it again: %w", d.ID, err)
					}
					err = tx.RewrapDataKey(ctx, d.ID, newID, sealWith(next, raw, dataKeyAD(d.ID, newID), nil))
					clear(raw)
					if err != nil {
						return err
					}
					n++
				}
				if len(page) < 500 {
					break
				}
				after = page[len(page)-1].ID
			}
			if err := tx.RetireKEK(ctx, old.ID, k.now()); err != nil {
				return err
			}
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: k.now(), Actor: actor, Action: "secrets.rotate_key", Subject: newID, Target: newID, Surface: surface,
			Detail: fmt.Sprintf("wrapped %d data keys under %s", n, newID),
		})
	})
	if err != nil {
		return "", 0, err
	}
	k.mu.Lock()
	for kid := range k.keks {
		if kid != kekID {
			delete(k.keks, kid)
		}
	}
	k.mu.Unlock()
	return kekID, n, nil
}
