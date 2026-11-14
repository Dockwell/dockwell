// SPDX-License-Identifier: AGPL-3.0-only

// Package oidctest is an OpenID Connect provider for tests: keys that sign as a
// provider's do, the tokens they sign, and a provider over TLS with its discovery
// document, keys and token endpoint. It doesn't import internal/oidc, so that
// package's own tests use it too.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// Issuer is a provider's issuer, where no fake provider runs.
	Issuer = "https://auth.example.com/"
	// Client is Dockwell's client at a provider.
	Client = "dockwell"
	// Secret is that client's secret at a Provider.
	Secret = "s3cret"
	// Nonce is a sign-in's nonce.
	Nonce = "n-0S6_WzA2Mj"
)

// Now is when tests sign in.
var Now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// Signer is a provider's key: its JWK, and how it signs.
type Signer struct {
	Alg, Kid string
	JWK      map[string]string
	sign     func(signed []byte) []byte
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// NewSigner makes a key that signs with alg: RS256, PS256, ES256 or EdDSA.
func NewSigner(t testing.TB, alg, kid string) Signer {
	t.Helper()
	s := Signer{Alg: alg, Kid: kid}
	switch alg {
	case "RS256", "PS256":
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		s.JWK = map[string]string{"kty": "RSA", "kid": kid, "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes())}
		s.sign = func(signed []byte) []byte {
			sum := sha256.Sum256(signed)
			var sig []byte
			if alg == "PS256" {
				sig, err = rsa.SignPSS(rand.Reader, key, crypto.SHA256, sum[:], nil)
			} else {
				sig, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
			}
			if err != nil {
				t.Fatal(err)
			}
			return sig
		}
	case "ES256":
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := key.PublicKey.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		s.JWK = map[string]string{"kty": "EC", "kid": kid, "crv": "P-256", "x": b64(raw[1:33]), "y": b64(raw[33:])}
		s.sign = func(signed []byte) []byte {
			sum := sha256.Sum256(signed)
			r, ss, err := ecdsa.Sign(rand.Reader, key, sum[:])
			if err != nil {
				t.Fatal(err)
			}
			return append(r.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
		}
	case "EdDSA":
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		s.JWK = map[string]string{"kty": "OKP", "kid": kid, "crv": "Ed25519", "x": b64(pub)}
		s.sign = func(signed []byte) []byte { return ed25519.Sign(key, signed) }
	default:
		t.Fatalf("no signer for %s", alg)
	}
	return s
}

// Token is claims signed as a JWT, with the signer's algorithm and key ID.
func (s Signer) Token(claims map[string]any) string {
	return s.TokenWith(map[string]any{"alg": s.Alg, "kid": s.Kid, "typ": "JWT"}, claims)
}

// TokenWith is claims signed under a header of the test's own.
func (s Signer) TokenWith(header, claims map[string]any) string {
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signed := b64(hb) + "." + b64(cb)
	return signed + "." + b64(s.sign([]byte(signed)))
}

// KeySet is the signers' public keys, as a provider's jwks_uri sends them.
func KeySet(signers ...Signer) []byte {
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	for _, s := range signers {
		set.Keys = append(set.Keys, s.JWK)
	}
	b, _ := json.Marshal(set)
	return b
}

// GoodClaims are an ID token's claims at Issuer for Client, made at Now.
func GoodClaims() map[string]any {
	return map[string]any{
		"iss": Issuer, "aud": Client, "sub": "248289761001", "email": "Mira@Example.com", "email_verified": true,
		"nonce": Nonce, "iat": Now.Unix(), "exp": Now.Add(time.Hour).Unix(), "groups": []string{"ops", "admins"},
	}
}

// Provider is an OpenID Connect provider over TLS: its discovery document, its keys,
// and a token endpoint that answers each code with what ID says, for Client with
// Secret, as client_secret_basic sends it.
type Provider struct {
	Srv      *httptest.Server
	Signer   Signer
	KeyReads atomic.Int32
	// ID is the claims a code's ID token carries, from the token request's form; the
	// provider writes its own issuer into them.
	ID func(form url.Values) map[string]any
	// EndSession is where its discovery document says to sign out, if anywhere.
	EndSession string
}

// NewProvider starts a provider that signs with ES256.
func NewProvider(t testing.TB) *Provider {
	t.Helper()
	p := &Provider{Signer: NewSigner(t, "ES256", "k1")}
	p.Srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := p.Issuer()
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			doc := map[string]any{
				"issuer": base, "authorization_endpoint": base + "authorize", "token_endpoint": base + "token", "jwks_uri": base + "keys",
				"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
			}
			if p.EndSession != "" {
				doc["end_session_endpoint"] = p.EndSession
			}
			_ = json.NewEncoder(w).Encode(doc)
		case "/keys":
			p.KeyReads.Add(1)
			_, _ = w.Write(KeySet(p.Signer))
		case "/token":
			_ = r.ParseForm()
			if user, secret, ok := r.BasicAuth(); !ok || user != Client || secret != Secret || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
				return
			}
			claims := p.ID(r.Form)
			claims["iss"] = base
			_ = json.NewEncoder(w).Encode(map[string]string{"id_token": p.Signer.Token(claims), "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Srv.Close)
	return p
}

// Issuer is the provider's issuer: its address, with a slash.
func (p *Provider) Issuer() string { return p.Srv.URL + "/" }
