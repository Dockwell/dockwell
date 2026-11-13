// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
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
	return secretName(name) || secretShape(value)
}

// secretWords are the words of a name that say its value may be a secret, as in
// DB_PASSWORD, API_KEY, auth.token or SECRET_KEY_BASE.
var secretWords = map[string]bool{
	"PASSWORD": true, "PASSWORDS": true, "PASSWD": true, "PASS": true, "PWD": true, "PW": true, "PSWD": true,
	"PASSPHRASE": true, "SECRET": true, "SECRETS": true, "TOKEN": true, "TOKENS": true, "KEY": true, "KEYS": true,
	"APIKEY": true, "PRIVATE": true, "CREDENTIAL": true, "CREDENTIALS": true, "CRED": true, "CREDS": true,
	"AUTH": true, "SALT": true, "DSN": true, "SIGNING": true, "HMAC": true, "OTP": true, "PIN": true, "SEED": true,
	"BEARER": true, "JWT": true, "ENCRYPTION": true, "CIPHER": true, "WEBHOOK": true,
}

// secretParts are what says a name may hold a secret anywhere in it, written together
// with other words, as dbPassword or MYAPIKEY are.
var secretParts = []string{"PASSWORD", "PASSWD", "SECRET", "TOKEN", "APIKEY", "PRIVATEKEY", "ACCESSKEY", "CREDENTIAL"}

func secretName(name string) bool {
	upper := strings.ToUpper(name)
	for _, part := range secretParts {
		if strings.Contains(upper, part) {
			return true
		}
	}
	for _, word := range strings.FieldsFunc(upper, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if secretWords[word] {
			return true
		}
	}
	return false
}

var (
	// userinfo is a URL's user, and password, as in postgres://app:hunter2@db/app, or
	// a key in place of a user, as a Sentry DSN has.
	userinfo = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^/@\s]+@`)
	// tokenRun is a run of what a key or a token is made of.
	tokenRun = regexp.MustCompile(`[A-Za-z0-9+/=_-]{20,}`)
	// keyPrefixes start keys well known services issue, which may be short.
	keyPrefixes = []string{"sk-", "sk_", "pk_", "rk_", "ghp_", "gho_", "ghu_", "ghs_", "github_pat_", "glpat-", "xox", "AKIA", "ASIA", "AIza", "SG.", "eyJ"}
)

func secretShape(value string) bool {
	switch {
	case strings.Contains(value, "-----BEGIN"), userinfo.MatchString(value):
		return true
	}
	for _, p := range keyPrefixes {
		if strings.HasPrefix(value, p) {
			return true
		}
	}
	for _, run := range tokenRun.FindAllString(value, -1) {
		if random(run) {
			return true
		}
	}
	return false
}

// random reports a run that reads as random rather than as words: three kinds of
// character mixed, or two in a long one, as a hex key or a UUID has. A path, a word
// joined by underscores or a version reads as one or two kinds, and short.
func random(run string) bool {
	var lower, upper, digit bool
	for _, r := range run {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	kinds := 0
	for _, has := range []bool{lower, upper, digit} {
		if has {
			kinds++
		}
	}
	return kinds >= 3 || kinds == 2 && len(run) >= 32
}
