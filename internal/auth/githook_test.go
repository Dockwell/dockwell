// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A delivery proves it knows its hook's secret by a signature of its body, or by
// the secret itself, as GitLab sends it, and by nothing less.
func TestAHookProofIsASignatureOrTheSecret(t *testing.T) {
	key, body := []byte(newHookSecret()), []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	signed := mac.Sum(nil)
	for _, tc := range []struct {
		what  string
		proof HookProof
		want  bool
	}{
		{"the body's signature", HookProof{Signature: signed}, true},
		{"another body's signature", HookProof{Signature: signed[:len(signed)-1]}, false},
		{"no signature", HookProof{}, false},
		{"the secret", HookProof{Token: string(key)}, true},
		{"the secret, cut short", HookProof{Token: string(key[:len(key)-1])}, false},
		{"the secret, and more", HookProof{Token: string(key) + "0"}, false},
		{"another secret", HookProof{Token: newHookSecret()}, false},
		{"the secret as a signature", HookProof{Signature: key}, false},
		{"the signature as a token", HookProof{Token: string(signed)}, false},
		// A token is all a proof needs: a signature beside it isn't read.
		{"a wrong token beside the right signature", HookProof{Token: "wrong", Signature: signed}, false},
	} {
		if got := tc.proof.matches(key, body); got != tc.want {
			t.Errorf("%s: %t, want %t", tc.what, got, tc.want)
		}
	}
}

// A hook acts for its maker as a token does, so it ends as their tokens do: when
// their role is lowered, and when they're removed, each on record (ADR 0046).
func TestAHookEndsWithItsMakersAccess(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := newInvited(t, backend)
		ada := f.join(t, "ada@example.com", store.RoleAdmin, "203.0.113.5:1")
		sam := f.join(t, "sam@example.com", store.RoleDeveloper, "203.0.113.6:1")
		now := time.Now()
		if err := f.db.Update(t.Context(), func(tx *store.Tx) error {
			for _, h := range []store.GitHook{
				{ID: "ghk_ada", ResourceID: "svc_web", UserID: ada.p.User.ID, Secret: []byte("x"), CreatedAt: now, SecretAt: now},
				{ID: "ghk_sam", ResourceID: "svc_api", UserID: sam.p.User.ID, Secret: []byte("x"), CreatedAt: now, SecretAt: now},
			} {
				if err := tx.CreateGitHook(t.Context(), h); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		gone := func(id string) bool {
			t.Helper()
			var err error
			_ = f.db.View(t.Context(), func(tx *store.Tx) error { _, err = tx.GetGitHook(t.Context(), id); return nil })
			return errors.Is(err, store.ErrNotFound)
		}

		lowered, err := f.setRole(t, ada.p.User.ID, store.RoleDeveloper)
		if err != nil || len(lowered.Ended.Hooks) != 1 || !gone("ghk_ada") {
			t.Fatalf("lowering ada: %+v, %v; want her hook ended", lowered.Ended, err)
		}
		proof := f.stepUpAt(t, onDomain, f.owner, f.keys, StepUpRemoveMember, sam.p.User.ID).Proof
		removed, err := f.svc.RemoveMember(t.Context(), onDomain, f.owner, sam.p.User.ID, proof)
		if err != nil || len(removed.Ended.Hooks) != 1 || !gone("ghk_sam") {
			t.Fatalf("removing sam: %+v, %v; want his hook ended", removed.Ended, err)
		}
		details := f.auditDetails(t, "git_hook.delete")
		if len(details) != 2 || !strings.Contains(details[0]+details[1], "hook ghk_ada deleted, as its maker's role was lowered") ||
			!strings.Contains(details[0]+details[1], "hook ghk_sam deleted, as its maker was removed") {
			t.Errorf("the audit log says of the hooks: %q", details)
		}
	})
}

// Wrong proofs, from anyone who knows a hook's URL, don't use up the deliveries
// its host's pushes need, and a hook there isn't answers them as one that is,
// however many come (from #253's review, M1).
func TestWrongProofsSpendNothingOfAHooksPushes(t *testing.T) {
	f := newFixture(t, store.SQLite, "")
	secret := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	h := store.GitHook{ID: "ghk_01REVIEW0000000000000000", ResourceID: "res_web", UserID: "usr_maker", CreatedAt: time.Now(), SecretAt: time.Now()}
	h.Secret = f.key.Seal(purposeHook(h.ID), secret)
	if err := f.db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateUser(t.Context(), store.User{ID: "usr_maker", Email: "maker@example.com", Name: "Maker", WebAuthnHandle: []byte("m"), CreatedAt: time.Now()}); err != nil {
			return err
		}
		return tx.CreateGitHook(t.Context(), h)
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"ref":"refs/heads/main"}`)
	for range 60 {
		_, err := f.svc.GitHookPrincipal(t.Context(), h.ID, body, HookProof{Signature: []byte("wrong")})
		if !errors.Is(err, ErrHookRejected) && !errors.Is(err, ErrTooManyRequests) {
			t.Fatal(err)
		}
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if _, err := f.svc.GitHookPrincipal(t.Context(), h.ID, body, HookProof{Signature: mac.Sum(nil)}); err != nil {
		t.Errorf("the host's push after 60 forged ones: %v, want it taken", err)
	}
	// And a hook there isn't answers as one that is, however many come.
	var unknown, known error
	for range 61 {
		_, unknown = f.svc.GitHookPrincipal(t.Context(), "ghk_01NOTAHOOK000000000000000", body, HookProof{Signature: []byte("wrong")})
		_, known = f.svc.GitHookPrincipal(t.Context(), h.ID, body, HookProof{Signature: []byte("wrong")})
	}
	if !errors.Is(unknown, known) && !errors.Is(known, unknown) {
		t.Errorf("after 61 wrong proofs, a hook there isn't answers %v, and one there is %v", unknown, known)
	}
}
