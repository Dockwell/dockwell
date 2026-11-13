// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"maps"
	"slices"
	"strings"
	"unicode"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// ShownEnv names the values of an app's own environment that a read may show: each
// that neither its name nor its shape says could be a secret (MaySecret), sorted. It
// is for values whose source is known to hold no secret, as a catalog entry's text
// and its inputs that aren't secrets are: what a person typed into an app's
// environment by hand could be anything, whatever it's called, so it never goes
// through this (ADR 0081).
func ShownEnv(app *resourcev1.Container) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(app.GetEnv())) {
		if !MaySecret(name, app.GetEnv()[name]) {
			out = append(out, name)
		}
	}
	return out
}

// MaySecret reports a value that could be a secret, by its name or its shape. It errs
// towards yes: a value it wrongly calls a secret stays hidden, which costs a person a
// look, and one it wrongly calls plain would show a password to everyone who can read
// the app.
func MaySecret(name, value string) bool {
	return SecretNamed(name) || service.SecretShape(value)
}

// secretWords are the words of a name that say its value may be a secret, as in
// DB_PASSWORD, API_KEY, auth.token, SECRET_KEY_BASE or RABBITMQ_ERLANG_COOKIE.
var secretWords = map[string]bool{
	"PASSWORD": true, "PASSWORDS": true, "PASSWD": true, "PASS": true, "PWD": true, "PW": true, "PSWD": true,
	"PASSPHRASE": true, "PASSCODE": true, "SECRET": true, "SECRETS": true, "TOKEN": true, "TOKENS": true,
	"KEY": true, "KEYS": true, "APIKEY": true, "VKEY": true, "PRIVATE": true, "CREDENTIAL": true, "CREDENTIALS": true,
	"CRED": true, "CREDS": true, "AUTH": true, "SALT": true, "DSN": true, "SIGNING": true, "HMAC": true,
	"OTP": true, "PIN": true, "SEED": true, "BEARER": true, "JWT": true, "ENCRYPTION": true, "CIPHER": true,
	"WEBHOOK": true, "COOKIE": true, "SESSION": true, "NONCE": true,
}

// secretEnds are what a word that holds a secret's word joined to others ends in,
// as PGPASSWORD, REDISPASS, LDAP_BINDPW, MASTERKEY and SERVICEACCOUNTTOKEN do.
var secretEnds = []string{"PASSWORD", "PASSWD", "PASS", "PWD", "PW", "SECRET", "TOKEN", "KEY", "COOKIE", "CREDENTIAL", "CREDENTIALS"}

// secretParts are what says a name may hold a secret anywhere in a word, written
// together with others, as MYAPIKEY or SECRETKEYBASE are.
var secretParts = []string{"PASSWORD", "PASSWD", "SECRET", "TOKEN", "APIKEY", "PRIVATEKEY", "ACCESSKEY", "ACCESSCODE", "PASSCODE", "CREDENTIAL"}

// SecretNamed reports a name that says its value could be a secret, by its words:
// one of secretWords, or a word ending in one of secretEnds or holding one of
// secretParts, or CODE after ACCESS or PASS, as lobe-chat's ACCESS_CODE. Words split
// where a name's case changes too, as in dbPass, smtpPass, adminPwd or appKey. It
// takes no word after one as saying the name is about the secret rather than the
// secret, as PASSWORD_MIN_LENGTH is: that would show a value by guesswork, and
// hiding one that's no secret costs a look. So every name the catalog's converter
// calls a secret is one here too (convert.secretNamed), and more.
func SecretNamed(name string) bool {
	words := nameWords(name)
	for i, w := range words {
		code := w == "CODE" && i > 0 && (words[i-1] == "ACCESS" || words[i-1] == "PASS")
		if code || secretWords[w] {
			return true
		}
		for _, end := range secretEnds {
			if strings.HasSuffix(w, end) {
				return true
			}
		}
		for _, part := range secretParts {
			if strings.Contains(w, part) {
				return true
			}
		}
	}
	return false
}

// nameWords are a name's words, in capitals: split at anything but a letter or a
// digit, and where a lower-case letter is followed by a capital, as dbPassword's
// are DB and PASSWORD.
func nameWords(name string) []string {
	var out []string
	var word []rune
	flush := func() {
		if len(word) > 0 {
			out = append(out, strings.ToUpper(string(word)))
			word = word[:0]
		}
	}
	prev := rune(0)
	for _, r := range name {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			word = append(word, r)
		default:
			word = append(word, r)
		}
		prev = r
	}
	flush()
	return out
}
