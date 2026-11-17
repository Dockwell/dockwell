// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// What an input's hash_of writes: Argon2id with the parameters OWASP's Password
// Storage Cheat Sheet recommends, 19 MiB of memory, two passes and one lane, a salt
// of 16 random bytes and a key of 32 (ADR 0105).
const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonSalt    = 16
	argonKey     = 32
)

// HashOf is the Argon2id PHC string of a secret, as an app that takes a hash in
// place of its admin token reads it, such as Vaultwarden's ADMIN_TOKEN:
// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>, in base64 without padding. It never
// keeps or says the secret.
func HashOf(secret string) (string, error) {
	salt := make([]byte, argonSalt)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("hashing a secret: %w", err)
	}
	key := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKey)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}
