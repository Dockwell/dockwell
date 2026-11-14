// SPDX-License-Identifier: AGPL-3.0-only

// Package routehash makes the bcrypt hash a private route keeps of a user's
// password, on the control plane (ADR 0087). The agent build doesn't link it: an
// agent only ever gets the hash.
package routehash

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// Cost is bcrypt's cost for a route's password. Caddy checks a hash on each
// request it hasn't cached, with no limit on attempts, so a flood of wrong
// passwords costs a check each: at 10 that's tens of milliseconds, where Caddy's
// own default of 14 would be sixteen times that. The panel makes a password of 24
// random characters, so the cost isn't what protects it.
const Cost = 10

// Hash returns password's bcrypt hash at Cost.
func Hash(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), Cost)
	if err != nil {
		return "", fmt.Errorf("can't hash a password: %w", err)
	}
	return string(h), nil
}
