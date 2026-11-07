// SPDX-License-Identifier: AGPL-3.0-only

// Package authtest is a software passkey authenticator for tests. It does
// what a browser and a platform authenticator do together: it reads WebAuthn
// options as JSON, makes ES256 credentials, signs assertions, and answers
// with RegistrationResponseJSON and AuthenticationResponseJSON. It is never
// linked into the binary.
package authtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

var b64 = base64.RawURLEncoding

// Authenticator holds passkeys.
type Authenticator struct {
	// SkipUserVerification leaves the UV flag unset, as an authenticator that
	// only checks presence would.
	SkipUserVerification bool
	// Counter makes credentials count their uses, as security keys do.
	// Synced passkeys leave the count at zero.
	Counter bool
	// Synced sets the backup-eligible and backed-up flags.
	Synced bool

	mu    sync.Mutex
	creds []*Credential
}

// Credential is one passkey.
type Credential struct {
	ID         []byte
	RPID       string
	UserHandle []byte
	Key        *ecdsa.PrivateKey
	Count      uint32
}

// exported is a Credential as a file holds it.
type exported struct {
	ID, UserHandle, Key []byte
	RPID                string
}

// Export is a passkey as bytes, for a script whose later steps run as other
// processes, such as scripts/stepup. It holds the private key.
func (c *Credential) Export() ([]byte, error) {
	key, err := x509.MarshalPKCS8PrivateKey(c.Key)
	if err != nil {
		return nil, err
	}
	return json.Marshal(exported{ID: c.ID, UserHandle: c.UserHandle, Key: key, RPID: c.RPID})
}

// ImportCredential reads what Export wrote.
func ImportCredential(b []byte) (*Credential, error) {
	var e exported
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(e.Key)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the key is %T, not an ECDSA key", parsed)
	}
	return &Credential{ID: e.ID, RPID: e.RPID, UserHandle: e.UserHandle, Key: key}, nil
}

// Flags in authenticator data.
const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

type creationOptions struct {
	RP struct {
		ID string `json:"id"`
	} `json:"rp"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Challenge          string `json:"challenge"`
	ExcludeCredentials []struct {
		ID string `json:"id"`
	} `json:"excludeCredentials"`
}

type requestOptions struct {
	Challenge        string `json:"challenge"`
	RPID             string `json:"rpId"`
	AllowCredentials []struct {
		ID string `json:"id"`
	} `json:"allowCredentials"`
}

// ErrExcluded is what a browser reports as InvalidStateError: the
// authenticator already holds an excluded credential.
var ErrExcluded = errors.New("authenticator already holds an excluded credential")

// Create makes a passkey for the PublicKeyCredentialCreationOptionsJSON in
// options, on a page at origin, and returns RegistrationResponseJSON.
func (a *Authenticator) Create(options, origin string) (string, *Credential, error) {
	var o creationOptions
	if err := json.Unmarshal([]byte(options), &o); err != nil {
		return "", nil, fmt.Errorf("creation options: %w", err)
	}
	handle, err := b64.DecodeString(o.User.ID)
	if err != nil {
		return "", nil, fmt.Errorf("user.id: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ex := range o.ExcludeCredentials {
		for _, c := range a.creds {
			if b64.EncodeToString(c.ID) == ex.ID && c.RPID == o.RP.ID {
				return "", nil, ErrExcluded
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", nil, err
	}
	id := make([]byte, 32)
	_, _ = rand.Read(id)
	c := &Credential{ID: id, RPID: o.RP.ID, UserHandle: handle, Key: key}
	if a.Counter {
		c.Count = 1
	}

	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         1, // P-256
		XCoord:        pad32(key.X.Bytes()),
		YCoord:        pad32(key.Y.Bytes()),
	})
	if err != nil {
		return "", nil, err
	}
	attested := make([]byte, 0, 16+2+len(id)+len(cose))
	attested = append(attested, make([]byte, 16)...)                    // AAGUID: zero, as with attestation "none"
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(id))) //nolint:gosec // 32 bytes
	attested = append(attested, id...)
	attested = append(attested, cose...)
	authData := a.authData(c, flagAT, attested)

	clientData, err := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": o.Challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		return "", nil, err
	}
	attObj, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		return "", nil, err
	}
	out, err := json.Marshal(map[string]any{
		"id":    b64.EncodeToString(id),
		"rawId": b64.EncodeToString(id),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData),
			"attestationObject": b64.EncodeToString(attObj),
			"transports":        []string{"internal"},
		},
		"clientExtensionResults":  map[string]any{},
		"authenticatorAttachment": "platform",
	})
	if err != nil {
		return "", nil, err
	}
	a.creds = append(a.creds, c)
	return string(out), c, nil
}

// Get signs an assertion for the PublicKeyCredentialRequestOptionsJSON in
// options, on a page at origin, with cred, or with the newest passkey for
// the options' RP ID when cred is nil. It returns AuthenticationResponseJSON.
func (a *Authenticator) Get(options, origin string, cred *Credential) (string, error) {
	var o requestOptions
	if err := json.Unmarshal([]byte(options), &o); err != nil {
		return "", fmt.Errorf("request options: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if cred == nil {
		for _, c := range a.creds {
			if c.RPID == o.RPID {
				cred = c
			}
		}
	}
	if cred == nil {
		return "", fmt.Errorf("no passkey for %q", o.RPID)
	}
	if a.Counter {
		cred.Count++
	}
	authData := a.authData(cred, 0, nil)
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": o.Challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		return "", err
	}
	clientHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, cred.Key, digest[:])
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(map[string]any{
		"id":    b64.EncodeToString(cred.ID),
		"rawId": b64.EncodeToString(cred.ID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData),
			"authenticatorData": b64.EncodeToString(authData),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(cred.UserHandle),
		},
		"clientExtensionResults":  map[string]any{},
		"authenticatorAttachment": "platform",
	})
	return string(out), err
}

// Credentials returns the passkeys made so far, oldest first.
func (a *Authenticator) Credentials() []*Credential {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*Credential(nil), a.creds...)
}

func (a *Authenticator) authData(c *Credential, extra byte, attested []byte) []byte {
	rpHash := sha256.Sum256([]byte(c.RPID))
	flags := byte(flagUP) | extra
	if !a.SkipUserVerification {
		flags |= flagUV
	}
	if a.Synced {
		flags |= flagBE | flagBS
	}
	out := make([]byte, 0, 37+len(attested))
	out = append(out, rpHash[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, c.Count)
	return append(out, attested...)
}

func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	return append(make([]byte, 32-len(b)), b...)
}
