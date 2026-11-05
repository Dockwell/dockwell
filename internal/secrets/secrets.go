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
//   - the spec, sealed under its data key, bound to where it is stored: its
//     resource and kind, and the revision, or the change and item, that
//     keeps it (store.SpecAt).
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
// another resource, revision or change item, or with keys this install
// doesn't have, or it was changed.
var ErrOpen = errors.New("can't open a sealed spec")

// ErrUnsealed reports a spec of a kind that may hold secrets stored in the
// clear: written outside Dockwell, or by a version before it sealed them.
// It isn't read, as nothing says it is what Dockwell stored.
var ErrUnsealed = errors.New("the stored spec isn't sealed, as every spec of its kind is")

// unsealed says which stored spec isn't sealed, and what to do. No release wrote
// one: only a build from main between the Docker driver and sealing did, or someone
// with the database.
func unsealed(at store.SpecAt) error {
	return fmt.Errorf("%s %s: %w, so it was written outside Dockwell or by a development build from before it sealed them, and Dockwell won't read it. "+
		"Restore the database from a backup. On a development install, start again with an empty data directory", at.Kind, at.ResourceID, ErrUnsealed)
}

// Keeper seals and opens specs, with the keys in the store.
//
// It keeps no key-encryption key in memory beyond the transaction that
// uses it: each is unsealed with the install's key when a transaction
// needs it, which is one small read and one AES-GCM open. So once
// dockwell rotate-key retires one, in this process or beside it, no
// running control plane holds it, and every transaction after uses the
// new one.
type Keeper struct {
	db     *store.DB
	key    *seal.Key
	kinds  *resource.Registry
	digest []byte
	now    func() time.Time
}

// New returns a Keeper over db, with the install's key. kinds reads the
// specs it resolves and reveals.
func New(db *store.DB, key *seal.Key, kinds *resource.Registry) *Keeper {
	return &Keeper{db: db, key: key, kinds: kinds, digest: key.Derive("spec digests"), now: time.Now}
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

// specAD binds a sealed spec to where it is stored.
func specAD(at store.SpecAt) ([]byte, error) {
	switch {
	case at.ResourceID == "" || at.Kind == "":
		return nil, errors.New("a sealed spec needs its resource and kind")
	case at.Revision > 0 && at.Change == "":
		return fmt.Appendf(nil, "dockwell.spec|%s|%s|revision|%d", at.ResourceID, at.Kind, at.Revision), nil
	case at.Revision == 0 && at.Change != "" && at.Position >= 0:
		return fmt.Appendf(nil, "dockwell.spec|%s|%s|change|%s|%d", at.ResourceID, at.Kind, at.Change, at.Position), nil
	}
	return nil, fmt.Errorf("a sealed spec of %s is either a revision's or a change item's, not %+v", at.ResourceID, at)
}

// kek returns a key-encryption key, unsealed with the install's key, for
// the transaction tx alone.
func (k *Keeper) kek(ctx context.Context, tx *store.Tx, kekID string) (cipher.AEAD, error) {
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
	return newAEAD(raw)
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
	return kekID, aead, nil
}

// Seal encrypts a spec's encoding for where it will be stored, under a new
// data key that it records in tx. tx must be a write transaction.
func (k *Keeper) Seal(ctx context.Context, tx *store.Tx, at store.SpecAt, encoded []byte) ([]byte, error) {
	ad, err := specAD(at)
	if err != nil {
		return nil, err
	}
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
	return sealWith(dek, encoded, ad, prefix), nil
}

// Open decrypts what Seal made for where it is stored. A spec of a kind
// that holds no secret isn't sealed, and comes back as it is; one of a
// kind that may hold a secret must be sealed, and is refused otherwise
// (ErrUnsealed).
func (k *Keeper) Open(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) ([]byte, error) {
	if !Sealed(stored) {
		if k.sealedKind(resource.Kind(at.Kind)) {
			return nil, unsealed(at)
		}
		return stored, nil
	}
	dekID, sealed, err := sealedParts(stored)
	if err != nil {
		return nil, err
	}
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
	return openUnder(at, sealed, d, kek)
}

// sealedParts splits what Seal made into its data key's ID and what that key
// sealed.
func sealedParts(stored []byte) (dekID string, sealed []byte, err error) {
	rest := stored[len(magic):]
	if len(rest) < 1 || len(rest) < 1+int(rest[0]) {
		return "", nil, ErrOpen
	}
	return string(rest[1 : 1+int(rest[0])]), rest[1+int(rest[0]):], nil
}

// openUnder opens a spec sealed for at with its data key, unwrapped with kek.
func openUnder(at store.SpecAt, sealed []byte, d store.DataKey, kek cipher.AEAD) ([]byte, error) {
	ad, err := specAD(at)
	if err != nil {
		return nil, err
	}
	raw, err := openWith(kek, d.Wrapped, dataKeyAD(d.ID, d.KEKID))
	if err != nil {
		return nil, fmt.Errorf("data key %s: %w", d.ID, err)
	}
	defer clear(raw)
	dek, err := newAEAD(raw)
	if err != nil {
		return nil, err
	}
	plain, err := openWith(dek, sealed, ad)
	if err != nil {
		return nil, fmt.Errorf("the spec of %s: %w", at.ResourceID, err)
	}
	return plain, nil
}

// OpenAll is Open for many specs, in order, as a list reads them: one read of
// their data keys, and one of each key-encryption key, however many there are.
func (k *Keeper) OpenAll(ctx context.Context, tx *store.Tx, specs []store.StoredSpec) ([][]byte, error) {
	out := make([][]byte, len(specs))
	dekIDs := make([]string, len(specs))
	sealed := make([][]byte, len(specs))
	ids := make([]string, 0, len(specs))
	for i, s := range specs {
		if !Sealed(s.Spec) {
			if k.sealedKind(resource.Kind(s.At.Kind)) {
				return nil, unsealed(s.At)
			}
			out[i] = s.Spec
			continue
		}
		var err error
		if dekIDs[i], sealed[i], err = sealedParts(s.Spec); err != nil {
			return nil, err
		}
		ids = append(ids, dekIDs[i])
	}
	keys, err := tx.GetDataKeys(ctx, ids)
	if err != nil {
		return nil, err
	}
	keks := map[string]cipher.AEAD{}
	for i, s := range specs {
		if sealed[i] == nil {
			continue
		}
		d, ok := keys[dekIDs[i]]
		if !ok {
			return nil, fmt.Errorf("data key %s is gone: %w", dekIDs[i], ErrOpen)
		}
		kek, ok := keks[d.KEKID]
		if !ok {
			if kek, err = k.kek(ctx, tx, d.KEKID); err != nil {
				return nil, err
			}
			keks[d.KEKID] = kek
		}
		if out[i], err = openUnder(s.At, sealed[i], d, kek); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// sealedKind reports a kind whose specs are kept sealed. One this keeper
// doesn't know counts, as nothing says it holds no secret.
func (k *Keeper) sealedKind(kind resource.Kind) bool {
	t, err := k.kinds.Type(kind)
	return err != nil || resource.Sealed(t)
}

// Decode opens a stored spec and reads it as its kind.
func (k *Keeper) Decode(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) (proto.Message, error) {
	plain, err := k.Open(ctx, tx, at, stored)
	if err != nil {
		return nil, err
	}
	return k.kinds.Decode(resource.Kind(at.Kind), plain)
}

// DecodeAll is Decode for many specs, in order, as a list reads them.
func (k *Keeper) DecodeAll(ctx context.Context, tx *store.Tx, specs []store.StoredSpec) ([]proto.Message, error) {
	plain, err := k.OpenAll(ctx, tx, specs)
	if err != nil {
		return nil, err
	}
	out := make([]proto.Message, len(specs))
	for i := range specs {
		if out[i], err = k.kinds.Decode(resource.Kind(specs[i].At.Kind), plain[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Digest is a keyed hash of a spec's encoding, for the fingerprint of the
// change changeID. Within the change, the same spec always digests the
// same, so applying finds the plan that was reviewed. Without the install's
// key it says nothing of the spec, as a plain hash of a short password
// would, and it differs from change to change, so comparing two changes'
// fingerprints can't confirm they set the same value either, such as a
// guess and an undo that puts an old value back.
func (k *Keeper) Digest(changeID string, encoded []byte) []byte {
	mac := hmac.New(sha256.New, k.digest)
	mac.Write([]byte(changeID))
	mac.Write([]byte{0}) // no change ID holds a zero byte
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
	return kekID, n, nil
}
