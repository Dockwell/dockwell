// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup

import (
	"errors"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validatePhrase(phrase string) error {
	phrase = canonicalPhrase(phrase)
	length := utf8.RuneCountInString(phrase)
	if len(phrase) > 256 || length < 20 || strings.TrimSpace(phrase) != phrase || strings.ContainsFunc(phrase, unicode.IsControl) {
		return errors.New("use a passphrase of at least 20 characters and up to 256 bytes, on one line")
	}
	var lower, upper, digit, symbol, other bool
	for _, c := range phrase {
		switch {
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= '0' && c <= '9':
			digit = true
		case c < 128:
			symbol = true
		default:
			other = true
		}
	}
	alphabet := 0
	classes := [...]bool{lower, upper, digit, symbol, other}
	for class, size := range [...]int{26, 26, 10, 33, 26} {
		if classes[class] {
			alphabet += size
		}
	}
	if float64(length)*math.Log2(float64(alphabet)) < 80 || predictablePhrase(phrase) {
		return errors.New("this passphrase is too easy to guess. Use a longer random phrase or generate one")
	}
	return nil
}

// Keep the same small set of checks as the panel. No compiled dictionaries are loaded.
func predictablePhrase(phrase string) bool {
	folded := strings.ToLower(phrase)
	compact := strings.Map(func(c rune) rune {
		if c == ' ' || c == '-' || c == '_' {
			return -1
		}
		return c
	}, folded)
	if len(compact) < 2 {
		return true
	}
	letters := strings.TrimRight(compact, "0123456789!@#$%^&*()+.?")
	if slices.Contains([]string{
		"password", "passphrase", "letmein", "admin", "welcome", "iloveyou", "changeme", "qwerty",
		"passwordpassword", "correcthorsebatterystaple",
	}, letters) {
		return true
	}
	for _, sequence := range []string{"abcdefghijklmnopqrstuvwxyz", "zyxwvutsrqponmlkjihgfedcba", "01234567890123456789012345678901", "qwertyuiopasdfghjklzxcvbnm"} {
		if strings.Contains(sequence, compact) {
			return true
		}
	}
	runes := []rune(compact)
	for period := 1; period <= len(runes)/2; period++ {
		if slices.Equal(runes[period:], runes[:len(runes)-period]) {
			return true
		}
	}
	return false
}
