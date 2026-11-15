// SPDX-License-Identifier: AGPL-3.0-only

package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strconv"
	"time"
)

// JWT bounds: GitHub takes one that lasts 10 minutes at most, and the App's clock
// may run ahead of GitHub's.
const (
	jwtLife = 9 * time.Minute
	jwtSkew = 60 * time.Second
)

// ParseKey reads a GitHub App's private key, as GitHub gives it: PEM, PKCS#1 or
// PKCS#8, RSA.
func ParseKey(pemText []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemText)
	if block == nil {
		return nil, errors.New("the file isn't a private key: it has no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the file isn't a private key GitHub makes: it's neither PKCS#1 nor PKCS#8")
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the file's key isn't RSA, as a GitHub App's is")
	}
	return k, nil
}

// Fingerprint is a key's SHA-256 fingerprint as GitHub shows it beside each of an
// App's keys: the base64 of the public key's DER, with "SHA256:" before it.
func Fingerprint(k *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "SHA256:" + base64.StdEncoding.EncodeToString(sum[:]), nil
}

// jwt makes the JWT the App calls GitHub with: RS256, from a minute ago to 9
// minutes on, issued by the App's ID. It's made for a call and never kept.
func (a *App) jwt() (string, error) {
	if a.Key == nil || a.ID == 0 {
		return "", errors.New("the GitHub App has no key to sign with")
	}
	now := a.now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-jwtSkew).Unix(),
		"exp": now.Add(jwtLife).Unix(),
		"iss": strconv.FormatInt(a.ID, 10),
	})
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}
