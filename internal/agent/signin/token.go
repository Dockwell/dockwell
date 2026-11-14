// SPDX-License-Identifier: AGPL-3.0-only

// Package signin is a private route's sign-in with an OpenID Connect provider, on
// the route's agent (ADR 0087): it answers Caddy's forward_auth before each request,
// from a cookie it signed, and talks to the provider only as someone signs in. It
// checks ID tokens with Go's standard library alone, so the agent build links no
// new module.
package signin

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// Algorithms are the signatures an ID token may carry: never "none", and never an
// HMAC, which signs with what the client also holds.
var Algorithms = []string{"EdDSA", "ES256", "PS256", "RS256"}

// skew is how far a token's times may be from this server's clock.
const skew = 2 * time.Minute

// maxToken caps an ID token, which is a few kilobytes.
const maxToken = 16 << 10

// Key is a provider's signing key, as its JSON Web Key Set gives it.
type Key struct {
	ID  string
	Alg string
	Pub crypto.PublicKey
}

// jwk is a key as a key set writes it.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// ParseKeys reads a JSON Web Key Set: the RSA, P-256 and Ed25519 keys that sign,
// leaving out what Dockwell doesn't check and what encrypts.
func ParseKeys(body []byte) ([]Key, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, errors.New("the provider's keys aren't a JSON Web Key Set")
	}
	var out []Key
	for i := range set.Keys {
		k := &set.Keys[i]
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.public()
		if err != nil {
			continue
		}
		out = append(out, Key{ID: k.Kid, Alg: k.Alg, Pub: pub})
	}
	if len(out) == 0 {
		return nil, errors.New("the provider's key set has no key Dockwell can check a signature with")
	}
	return out, nil
}

func (k *jwk) public() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("not an RSA key")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			return nil, errors.New("an RSA key under 2048 bits")
		}
		return pub, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, errors.New("a curve Dockwell doesn't check")
		}
		x, err1 := base64.RawURLEncoding.DecodeString(k.X)
		y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
		if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
			return nil, errors.New("not a P-256 key")
		}
		// The uncompressed point, which ParseUncompressedPublicKey checks is on the curve.
		return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	case "OKP":
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("not an Ed25519 key")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, errors.New("a kind of key Dockwell doesn't check")
}

// Claims are what sign-in reads from an ID token.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	// HostedDomain is Google's hd: the Workspace domain that manages the account.
	HostedDomain string
	Groups       []string
}

// asciiLower is s in lower case, or false when s isn't ASCII: an address in other
// letters is spelled more than one way, as the Kelvin sign lowers to k, so it names
// no one (#598's review, L1).
func asciiLower(s string) (string, bool) {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return "", false
		}
	}
	return strings.ToLower(s), true
}

// Want is what an ID token must say to be taken.
type Want struct {
	Issuer   string
	ClientID string
	Nonce    string
	// GroupsClaim is the claim that lists groups; empty is "groups".
	GroupsClaim string
}

// ErrUnknownKey is a token signed with a key the set doesn't hold, which a provider
// that rotated its keys signs with: the caller reads the set again, once.
var ErrUnknownKey = errors.New("the ID token is signed with a key the provider's key set doesn't hold")

// Verify checks an ID token against the provider's keys and what it must say, and
// gives what it says of who signed in. Each refusal says what was wrong, and
// nothing of the token.
func Verify(raw string, keys []Key, want Want, now time.Time) (Claims, error) {
	if len(raw) > maxToken {
		return Claims{}, errors.New("the ID token is larger than any provider sends")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("the ID token isn't a signed JWT")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := decodePart(parts[0], &header); err != nil {
		return Claims{}, errors.New("the ID token's header can't be read")
	}
	if !slices.Contains(Algorithms, header.Alg) {
		return Claims{}, fmt.Errorf("the ID token is signed with %q, and Dockwell takes %s", header.Alg, strings.Join(Algorithms, ", "))
	}
	key, err := keyFor(keys, header.Kid, header.Alg)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, errors.New("the ID token's signature can't be read")
	}
	if err := check(header.Alg, key.Pub, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return Claims{}, err
	}
	var claims map[string]json.RawMessage
	if err := decodePart(parts[1], &claims); err != nil {
		return Claims{}, errors.New("the ID token's claims can't be read")
	}
	return checkClaims(claims, want, now)
}

// keyFor is the key a token names by kid, for its algorithm: with no kid, the one
// key there is for it.
func keyFor(keys []Key, kid, alg string) (Key, error) {
	var fits []Key
	for _, k := range keys {
		if (k.Alg == "" || k.Alg == alg) && suits(k.Pub, alg) && (kid == "" || k.ID == kid) {
			fits = append(fits, k)
		}
	}
	switch {
	case len(fits) == 1:
		return fits[0], nil
	case len(fits) == 0:
		return Key{}, ErrUnknownKey
	}
	return Key{}, errors.New("the ID token names no key, and the provider has several it could be")
}

// suits says whether a key is of the kind an algorithm signs with.
func suits(pub crypto.PublicKey, alg string) bool {
	switch pub.(type) {
	case *rsa.PublicKey:
		return alg == "RS256" || alg == "PS256"
	case *ecdsa.PublicKey:
		return alg == "ES256"
	case ed25519.PublicKey:
		return alg == "EdDSA"
	}
	return false
}

// check verifies a signature over signed with pub, by alg.
func check(alg string, pub crypto.PublicKey, signed, sig []byte) error {
	sum := sha256.Sum256(signed)
	bad := errors.New("the ID token's signature doesn't match the provider's key")
	switch alg {
	case "RS256":
		if rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, sum[:], sig) != nil { //nolint:forcetypeassert // suits checked it
			return bad
		}
	case "PS256":
		if rsa.VerifyPSS(pub.(*rsa.PublicKey), crypto.SHA256, sum[:], sig, nil) != nil { //nolint:forcetypeassert // suits checked it
			return bad
		}
	case "ES256":
		// A JWS signature is r and s, 32 bytes each.
		if len(sig) != 64 {
			return bad
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(pub.(*ecdsa.PublicKey), sum[:], r, s) { //nolint:forcetypeassert // suits checked it
			return bad
		}
	case "EdDSA":
		if !ed25519.Verify(pub.(ed25519.PublicKey), signed, sig) { //nolint:forcetypeassert // suits checked it
			return bad
		}
	default:
		return bad
	}
	return nil
}

// checkClaims checks what a verified token says against what it must.
func checkClaims(c map[string]json.RawMessage, want Want, now time.Time) (Claims, error) {
	str := func(name string) string {
		var s string
		_ = json.Unmarshal(c[name], &s)
		return s
	}
	switch {
	case str("iss") != want.Issuer:
		return Claims{}, errors.New("the ID token comes from another issuer")
	case !audienceHolds(c["aud"], want.ClientID):
		return Claims{}, errors.New("the ID token is for another client")
	case str("nonce") != want.Nonce || want.Nonce == "":
		return Claims{}, errors.New("the ID token answers another sign-in")
	}
	if aud := audiences(c["aud"]); len(aud) > 1 && str("azp") != want.ClientID {
		return Claims{}, errors.New("the ID token is for several clients, and wasn't given to this one")
	} else if azp := str("azp"); azp != "" && azp != want.ClientID {
		return Claims{}, errors.New("the ID token was given to another client")
	}
	exp, okExp := unix(c["exp"])
	iat, okIat := unix(c["iat"])
	switch {
	case !okExp || !okIat:
		return Claims{}, errors.New("the ID token doesn't say when it was made and when it runs out")
	case now.After(exp.Add(skew)):
		return Claims{}, errors.New("the ID token has run out")
	case iat.After(now.Add(skew)):
		return Claims{}, errors.New("the ID token was made in the future, by this server's clock. Check the server's time")
	}
	if nbf, ok := unix(c["nbf"]); ok && nbf.After(now.Add(skew)) {
		return Claims{}, errors.New("the ID token isn't valid yet, by this server's clock. Check the server's time")
	}
	out := Claims{Subject: str("sub")}
	email, ascii := asciiLower(strings.TrimSpace(str("email")))
	out.HostedDomain, _ = asciiLower(strings.TrimSpace(str("hd")))
	if out.Subject == "" {
		return Claims{}, errors.New("the ID token names no subject")
	}
	// email_verified is a boolean, or a string some providers send.
	var verified any
	_ = json.Unmarshal(c["email_verified"], &verified)
	out.EmailVerified = verified == true || verified == "true"
	if ascii {
		out.Email = email
	} else {
		out.EmailVerified = false
	}
	claim := want.GroupsClaim
	if claim == "" {
		claim = "groups"
	}
	var groups []any
	if json.Unmarshal(c[claim], &groups) == nil {
		for _, g := range groups {
			if s, ok := g.(string); ok && s != "" {
				out.Groups = append(out.Groups, s)
			}
		}
	}
	return out, nil
}

// audiences reads aud, which is a string or a list of them.
func audiences(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

func audienceHolds(raw json.RawMessage, client string) bool {
	return client != "" && slices.Contains(audiences(raw), client)
}

// unix reads a NumericDate: seconds since 1970, maybe with a fraction.
func unix(raw json.RawMessage) (time.Time, bool) {
	var f float64
	if raw == nil || json.Unmarshal(raw, &f) != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// decodePart reads one base64url part of a JWT as JSON.
func decodePart(part string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}
