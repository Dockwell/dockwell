// SPDX-License-Identifier: AGPL-3.0-only

package oidc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/oidc/oidctest"
)

const (
	issuer = oidctest.Issuer
	client = oidctest.Client
	nonce  = oidctest.Nonce
)

var now = oidctest.Now

type signer = oidctest.Signer

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newSigner(t *testing.T, alg, kid string) signer { return oidctest.NewSigner(t, alg, kid) }

func keySet(t *testing.T, signers ...signer) []Key {
	t.Helper()
	keys, err := ParseKeys(oidctest.KeySet(signers...))
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func goodClaims() map[string]any { return oidctest.GoodClaims() }

func token(s signer, claims map[string]any) string { return s.Token(claims) }

var want = Want{Issuer: issuer, ClientID: client, Nonce: nonce}

// A token a provider signed with each algorithm Dockwell takes is taken, with who
// signed in: the subject, the email in lower case, whether it's verified, and the
// groups, from the claim the provider names.
func TestAGoodTokenIsTakenWithEachAlgorithm(t *testing.T) {
	for _, alg := range Algorithms {
		t.Run(alg, func(t *testing.T) {
			s := newSigner(t, alg, "k1")
			got, err := Verify(token(s, goodClaims()), keySet(t, s, newSigner(t, "ES256", "k2")), want, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Subject != "248289761001" || got.Email != "mira@example.com" || !got.EmailVerified || strings.Join(got.Groups, ",") != "ops,admins" {
				t.Errorf("it reads %+v", got)
			}
		})
	}
	s := newSigner(t, "ES256", "k1")
	claims := goodClaims()
	claims["roles"], claims["email_verified"] = []string{"staff"}, "true"
	got, err := Verify(token(s, claims), keySet(t, s), Want{Issuer: issuer, ClientID: client, Nonce: nonce, GroupsClaim: "roles"}, now)
	if err != nil || strings.Join(got.Groups, ",") != "staff" || !got.EmailVerified {
		t.Errorf("groups from another claim read %+v, %v", got, err)
	}
}

// Each token Dockwell can't trust is refused on its own: from another issuer, for
// another client or given to another, to another sign-in, out of its time, signed
// with no algorithm or with an HMAC keyed by the public key, signed by another key,
// or with a key the set doesn't hold, which reads as one to fetch the set again for.
func TestATokenDockwellCantTrustIsRefused(t *testing.T) {
	s := newSigner(t, "RS256", "k1")
	keys := keySet(t, s)
	with := func(edit func(c map[string]any)) string {
		c := goodClaims()
		edit(c)
		return token(s, c)
	}
	// An HMAC "signed" with the provider's public key, as a confused library would check it.
	hmacWithPublic := func() string {
		pub, _ := x509.MarshalPKIXPublicKey(keys[0].Pub)
		hb, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": "k1"})
		cb, _ := json.Marshal(goodClaims())
		signed := b64(hb) + "." + b64(cb)
		mac := hmac.New(sha256.New, pub)
		mac.Write([]byte(signed))
		return signed + "." + b64(mac.Sum(nil))
	}
	unsigned := func() string {
		hb, _ := json.Marshal(map[string]any{"alg": "none"})
		cb, _ := json.Marshal(goodClaims())
		return b64(hb) + "." + b64(cb) + "."
	}
	other := newSigner(t, "RS256", "k1")
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"another issuer", with(func(c map[string]any) { c["iss"] = "https://evil.example.com/" }), "another issuer"},
		{"the issuer without its slash", with(func(c map[string]any) { c["iss"] = "https://auth.example.com" }), "another issuer"},
		{"another client", with(func(c map[string]any) { c["aud"] = "someone-else" }), "another client"},
		{"several clients, no azp", with(func(c map[string]any) { c["aud"] = []string{client, "someone-else"} }), "several clients"},
		{"given to another", with(func(c map[string]any) { c["azp"] = "someone-else" }), "given to another client"},
		{"another sign-in", with(func(c map[string]any) { c["nonce"] = "replayed" }), "another sign-in"},
		{"run out", with(func(c map[string]any) { c["exp"] = now.Add(-3 * time.Minute).Unix() }), "run out"},
		{"made in the future", with(func(c map[string]any) { c["iat"] = now.Add(5 * time.Minute).Unix() }), "in the future"},
		{"not yet valid", with(func(c map[string]any) { c["nbf"] = now.Add(5 * time.Minute).Unix() }), "isn't valid yet"},
		{"no subject", with(func(c map[string]any) { delete(c, "sub") }), "no subject"},
		{"no expiry", with(func(c map[string]any) { delete(c, "exp") }), "when it runs out"},
		{"alg none", unsigned(), `signed with "none"`},
		{"HS256 with the public key", hmacWithPublic(), `signed with "HS256"`},
		{"another key, same kid", token(other, goodClaims()), "doesn't match"},
		{"not a JWT", "abc.def", "isn't a signed JWT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Verify(tc.raw, keys, want, now); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("it reads %v, want %q", err, tc.want)
			}
		})
	}
	rotated := newSigner(t, "RS256", "k9")
	if _, err := Verify(token(rotated, goodClaims()), keys, want, now); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("a key the set doesn't hold reads %v, want ErrUnknownKey", err)
	}
	// A token two minutes out of time is still taken, for clocks that drift.
	if _, err := Verify(with(func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }), keys, want, now); err != nil {
		t.Errorf("a token a minute past its end reads %v", err)
	}
}

// A key set keeps only the keys that sign and that Dockwell can check: a short RSA
// key, another curve and an encryption key are left out.
func TestAKeySetKeepsWhatSigns(t *testing.T) {
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	es := newSigner(t, "ES256", "ok")
	body, _ := json.Marshal(map[string]any{"keys": []map[string]string{
		{"kty": "RSA", "kid": "small", "n": b64(small.N.Bytes()), "e": "AQAB"},
		{"kty": "EC", "kid": "p384", "crv": "P-384", "x": b64(make([]byte, 48)), "y": b64(make([]byte, 48))},
		{"kty": "RSA", "kid": "enc", "use": "enc", "n": es.JWK["x"], "e": "AQAB"},
		es.JWK,
	}})
	keys, err := ParseKeys(body)
	if err != nil || len(keys) != 1 || keys[0].ID != "ok" {
		t.Errorf("it keeps %+v, %v", keys, err)
	}
	if _, err := ParseKeys([]byte(`{"keys":[]}`)); err == nil {
		t.Error("an empty key set is taken")
	}
}

// An address in letters outside ASCII names no one: the dotted capital I lowers to
// another letter, so it would read as another address (#598's review, L1).
func TestAnAddressOutsideASCIIIsNoOnesAddress(t *testing.T) {
	raw := map[string]json.RawMessage{}
	for k, v := range goodClaims() {
		raw[k], _ = json.Marshal(v)
	}
	raw["email"], _ = json.Marshal("\u0130nfo@example.com")
	c, err := checkClaims(raw, want, now)
	if err != nil || c.Email != "" || c.EmailVerified {
		t.Errorf("an address with a dotted capital I reads as %+v, %v", c, err)
	}
}
