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
	"USER", "USERS", "USERNAME", "PORT", "PROVIDER", "INTERVAL", "SCOPE", "RESET",
}

// joinedSecrets are the endings of a word that holds a secret's word joined to another,
// as PGPASSWORD, JWTSECRET and SERVICEACCOUNTTOKEN do, and the joined words that are one.
var (
	joinedSecrets = []string{"PASSWORD", "PASSWD", "SECRET", "TOKEN"}
	joinedWords   = []string{"SECRETKEY", "PRIVATEKEY", "APIKEY", "ACCESSTOKEN"}
)

// listsSecrets says a variable holds secrets, more than one, by its name's words:
// TOKENS, SECRETS, PASSWORDS or APIKEYS, or KEYS after API, as in Unleash's
// INIT_CLIENT_API_TOKENS, and no word after it that says the name is about them, as
// in LT_API_KEYS_DB_PATH. Such a list has a form of its app's own, which one
// generated secret can't take.
func listsSecrets(name string) bool {
	words := strings.FieldsFunc(strings.ToUpper(name), func(r rune) bool { return r == '_' || r == '-' || r == '.' })
	for i, w := range words {
		plural := slices.Contains([]string{"TOKENS", "SECRETS", "PASSWORDS", "APIKEYS"}, w) || w == "KEYS" && i > 0 && words[i-1] == "API"
		if plural && !slices.ContainsFunc(words[i+1:], func(after string) bool { return slices.Contains(aboutWords, after) }) {
			return true
		}
	}
	return false
}

// secretNamed says a variable holds a secret, by its name's words: one is a secret's
// word, whole, as in ADMIN_PASSWORD, RABBITMQ_DEFAULT_PASS or SECRET_KEY_BASE, or
// joined to another, as in PGPASSWORD or MINIO_SECRETKEY, and none after it says the
// name is about the secret instead, as in TOKEN_URL, SSH_KEY_PATH,
// PASSWORD_MIN_LENGTH or SALT_ROUNDS. A token's ID is the token itself, as Vault's
// root token's is, where a key's ID, as an access key's, is no secret. A public key
// is no secret either.
func secretNamed(name string) bool {
	words := strings.FieldsFunc(strings.ToUpper(name), func(r rune) bool { return r == '_' || r == '-' || r == '.' })
	if slices.Contains(words, "PUBLIC") {
		return false
	}
	for i, w := range words {
		token := w == "TOKEN" || strings.HasSuffix(w, "TOKEN")
		secret := slices.Contains(secretWords, w) || slices.Contains(joinedWords, w) ||
			slices.ContainsFunc(joinedSecrets, func(end string) bool { return strings.HasSuffix(w, end) })
		if !secret {
			continue
		}
		about := slices.ContainsFunc(words[i+1:], func(after string) bool {
			return slices.Contains(aboutWords, after) && (!token || after != "ID")
		})
		if !about {
			return true
		}
	}
	return false
}
