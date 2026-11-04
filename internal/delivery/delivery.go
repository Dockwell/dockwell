// SPDX-License-Identifier: AGPL-3.0-only

// Package delivery is how an agent receives the values of the variables and
// secrets a step uses. The agent makes a key for each stream to the control
// plane and keeps it in memory only (NewKey). The control plane seals the
// values for one step to it with HPKE (secrets.SealValues), and the agent
// opens them in memory for that step alone (Key.Open). See
// docs/adr/0022-secrets-encrypted-at-rest.md.
//
// The agent build links this package, and with it only the opening side:
// sealing, and keeping values at all, is the control plane's
// (docs/adr/0023-a-full-build-and-an-agent-build.md).
package delivery

import (
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// infoPrefix starts the HPKE info, which binds sealed values to one
// server and one resource, so values sealed for one step can't be passed
// off as another's.
const infoPrefix = "dockwell.values.v1"

// The HPKE suite: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, AES-256-GCM.
var (
	kem  = hpke.DHKEM(ecdh.X25519())
	kdf  = hpke.HKDFSHA256()
	aead = hpke.AES256GCM()
)

// Suite is the HPKE suite values are sealed with, for the side that seals
// them.
func Suite() (hpke.KEM, hpke.KDF, hpke.AEAD) { return kem, kdf, aead }

// Info is the HPKE info values for a resource on a server are sealed
// with: "dockwell.values.v1", the server's ID, and the resource's kind,
// scope and name, each after a zero byte.
func Info(server string, ref resource.Ref) []byte {
	return []byte(strings.Join([]string{infoPrefix, server, string(ref.Kind), ref.Scope, ref.Name}, "\x00"))
}

// Key is an agent stream's delivery key. The agent makes one for each
// stream and keeps it in memory only: values sealed to it can't be opened
// once the stream ends, so nothing an agent could keep would open them.
type Key struct {
	private hpke.PrivateKey
	public  []byte
}

// NewKey makes a delivery key.
func NewKey() (*Key, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("can't make a delivery key: %w", err)
	}
	pk, err := hpke.NewDHKEMPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("can't make a delivery key: %w", err)
	}
	return &Key{private: pk, public: priv.PublicKey().Bytes()}, nil
}

// Public is what the agent sends in its hello.
func (k *Key) Public() []byte { return slices.Clone(k.public) }

// Open opens values sealed to this key for a resource on this server.
func (k *Key) Open(server string, ref resource.Ref, sealed []byte) (map[string]string, error) {
	plain, err := hpke.Open(k.private, kdf, aead, Info(server, ref), sealed)
	if err != nil {
		return nil, errors.New("can't open the values sent for this step: they weren't sealed for this stream, server and resource")
	}
	defer clear(plain)
	var v agentv1.Values
	if err := proto.Unmarshal(plain, &v); err != nil {
		return nil, fmt.Errorf("can't read the values sent for this step: %w", err)
	}
	return v.GetValues(), nil
}
