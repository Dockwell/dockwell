// SPDX-License-Identifier: AGPL-3.0-only

package gitssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

// KeyPair is a generated Ed25519 SSH deploy key.
type KeyPair struct {
	PrivateKeyPEM string
	PublicKey     string
	KeyType       string
	Fingerprint   string
}

// GenerateKey generates a fresh Ed25519 SSH deploy key pair in memory.
func GenerateKey() (KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "dockwell deploy key")
	if err != nil {
		return KeyPair{}, err
	}
	privBytes := pem.EncodeToMemory(block)
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		clear(privBytes)
		return KeyPair{}, err
	}
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " dockwell"
	return KeyPair{
		PrivateKeyPEM: string(privBytes),
		PublicKey:     authorized,
		KeyType:       sshPub.Type(),
		Fingerprint:   ssh.FingerprintSHA256(sshPub),
	}, nil
}

// PublicKeyFromPrivate derives the OpenSSH public key line, key type, and
// SHA-256 fingerprint from an unencrypted OpenSSH private key PEM.
func PublicKeyFromPrivate(privateKeyPEM string) (publicKey, keyType, fingerprint string, err error) {
	raw := []byte(privateKeyPEM)
	defer clear(raw)
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return "", "", "", errors.New("the deploy key's private half couldn't be opened")
	}
	pub := signer.PublicKey()
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " dockwell", pub.Type(), ssh.FingerprintSHA256(pub), nil
}
