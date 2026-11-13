// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"regexp"
	"strings"
	"sync"
)

// SecretShape reports a value whose shape says it could be a secret, whatever it's
// called: a URL with a user or a password in it, a key=value or key: value whose key
// is a secret's word (a query's password=, a connection string's Password=), a pair
// such as admin:hunter2, a PEM block, a key a well known service issues, or a run
// of characters that reads as random. It errs towards yes: what it wrongly calls a
// secret stays hidden, which costs a look, and what it wrongly calls plain would
// show a password to everyone who can read the app (ADR 0081).
func SecretShape(value string) bool {
	p := patterns()
	switch {
	case strings.Contains(value, "-----BEGIN"), p.userinfo.MatchString(value), p.keyed.MatchString(value):
		return true
	case p.pair.MatchString(value) && !p.version.MatchString(value[strings.IndexByte(value, ':')+1:]):
		return true
	}
	for _, prefix := range keyPrefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	for _, run := range p.run.FindAllString(value, -1) {
		if random(run) {
			return true
		}
	}
	return false
}

// keyPrefixes start keys well known services issue, which may be short.
var keyPrefixes = []string{"sk-", "sk_", "pk_", "rk_", "ghp_", "gho_", "ghu_", "ghs_", "github_pat_", "glpat-", "xox", "AKIA", "ASIA", "AIza", "SG.", "eyJ"}

type shapes struct {
	userinfo, keyed, pair, version, run *regexp.Regexp
}

// patterns are compiled the first time a value is looked at, so an agent, which
// never asks, holds none of them.
var patterns = sync.OnceValue(func() shapes {
	return shapes{
		// A URL's user, and password, as in postgres://app:hunter2@db/app, or a key in
		// place of a user, as a Sentry DSN has.
		userinfo: regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^/@\s]+@`),
		// A secret's word as a key, then = or :, then a value: ?password=hunter2,
		// Password=Hunter2!;, token: abc.
		keyed: regexp.MustCompile(`(?i)(^|[^a-z])(password|passwd|pwd|pass|passphrase|secret|token|apikey|api_key|access_key|secret_key|private_key|auth|credentials?)\s*[=:]\s*[^\s&;]`),
		// A user and a password and nothing else, as admin:hunter2.
		pair: regexp.MustCompile(`^[^\s:@/]+:[^\s:@/]+$`),
		// What follows a colon in a host and port, or an image and its tag.
		version: regexp.MustCompile(`^[0-9.]+$`),
		// A run of what a key or a token is made of.
		run: regexp.MustCompile(`[A-Za-z0-9+/=_-]{16,}`),
	}
})

// random reports a run that reads as random rather than as words: three kinds of
// character mixed in 16 or more, two kinds in 24 or more, as a hex key or a UUID
// has, or base32's capitals and digits, as a TOTP seed is written.
func random(run string) bool {
	var lower, upper, digit bool
	base32 := true
	for _, r := range run {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		}
		if (r < 'A' || r > 'Z') && (r < '2' || r > '7') && r != '=' {
			base32 = false
		}
	}
	kinds := 0
	for _, has := range []bool{lower, upper, digit} {
		if has {
			kinds++
		}
	}
	return kinds >= 3 || kinds == 2 && len(run) >= 24 || base32 && upper && digit
}
