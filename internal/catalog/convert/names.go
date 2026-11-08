// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"slices"
	"strings"
)

// secretWords are the words of a variable's name that say it holds a secret: a
// password, a key, a salt or a token, however a source spells them.
var secretWords = []string{
	"PASSWORD", "PASSWD", "PASS", "PWD", "PASSPHRASE",
	"SECRET", "TOKEN", "SALT",
	"KEY", "APIKEY", "VKEY",
}

// aboutWords are the words that, after a secret's word, say the name is about the
// secret rather than the secret itself: where it is, how long, how many rounds.
var aboutWords = []string{
	"URL", "URI", "PATH", "FILE", "DIR", "HOST",
	"LENGTH", "LEN", "MIN", "MAX", "ROUNDS", "SIZE", "BITS",
	"TTL", "EXPIRY", "EXPIRE", "EXPIRES", "EXPIRATION", "TIMEOUT", "LIFETIME",
	"TYPE", "NAME", "ID", "HEADER", "PREFIX", "ALGORITHM", "ALG",
	"COUNT", "LIMIT", "ENABLED", "ENABLE", "DISABLED", "REQUIRED", "POLICY", "MODE",
}

// secretNamed says a variable holds a secret, by its name's words, each whole: one is
// a secret's word, as in ADMIN_PASSWORD, RABBITMQ_DEFAULT_PASS or SECRET_KEY_BASE, and
// none after it says the name is about the secret instead, as in TOKEN_URL,
// SSH_KEY_PATH, PASSWORD_MIN_LENGTH or SALT_ROUNDS. A public key is no secret.
func secretNamed(name string) bool {
	words := strings.FieldsFunc(strings.ToUpper(name), func(r rune) bool { return r == '_' || r == '-' || r == '.' })
	if slices.Contains(words, "PUBLIC") {
		return false
	}
	for i, w := range words {
		if !slices.Contains(secretWords, w) {
			continue
		}
		if !slices.ContainsFunc(words[i+1:], func(after string) bool { return slices.Contains(aboutWords, after) }) {
			return true
		}
	}
	return false
}
