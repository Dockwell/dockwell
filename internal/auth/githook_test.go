// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"testing"
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
