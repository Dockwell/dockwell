// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup

import (
	_ "embed"
	"errors"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

//go:embed common-passwords.txt
var commonPasswords string

var commonSegments = func() []string {
	_, words, _ := strings.Cut(commonPasswords, "\n")
	out := strings.Fields(words)
	slices.SortStableFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}()

func validatePhrase(input string) error {
	phrase := canonicalPhrase(input)
	if len(input) > 256 || utf8.RuneCountInString(phrase) < 20 || strings.TrimSpace(phrase) != phrase ||
		strings.ContainsFunc(input, unicode.IsControl) {
		return errors.New("use a passphrase of at least 20 characters and up to 256 bytes, on one line")
	}
	if phraseBits(phrase) < 80 {
		return errors.New("this passphrase is too easy to guess. Use a longer random phrase or generate one")
	}
	return nil
}

// Keep the estimate in step with web/src/features/backups/recoveryPhrase.ts.
// A run or repetition contributes only its new characters. A recognized common
// segment contributes one small guess space, even when surrounded by other text.
func phraseBits(phrase string) float64 {
	var normal, folded []rune
	var lower, upper, digit, symbol, other bool
	for _, c := range phrase {
		if c == ' ' || c == '-' || c == '_' {
			continue
		}
		switch {
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
			c += 'a' - 'A'
		case c >= '0' && c <= '9':
			digit = true
		case c < 128:
			symbol = true
		default:
			other = true
		}
		normal = append(normal, c)
		folded = append(folded, foldLeet(c))
	}
	classes := [...]bool{lower, upper, digit, symbol, other}
	alphabet := 0
	for class, size := range [...]int{26, 26, 10, 33, 26} {
		if classes[class] {
			alphabet += size
		}
	}
	perChar := math.Log2(float64(max(1, alphabet)))
	var bits float64
	for i := 0; i < len(normal); {
		repeated := repeatLength(folded, i)
		if repeated >= 3 {
			i += repeated
			continue
		}
		if n := commonLength(folded[i:]); n != 0 {
			bits += min(10, perChar*float64(n))
			i += n
			continue
		}
		if i == 0 || !relatedCharacters(normal[i-1], normal[i]) {
			bits += perChar
		}
		i++
	}
	return bits
}

func foldLeet(c rune) rune {
	switch c {
	case '@', '4':
		return 'a'
	case '3':
		return 'e'
	case '0':
		return 'o'
	case '1', '!':
		return 'i'
	case '5', '$':
		return 's'
	case '7', '+':
		return 't'
	default:
		return c
	}
}

func repeatLength(text []rune, at int) int {
	longest := 0
	for start := 0; start+3 <= at; start++ {
		n := 0
		for start+n < at && at+n < len(text) && text[start+n] == text[at+n] {
			n++
		}
		longest = max(longest, n)
	}
	return longest
}

func commonLength(text []rune) int {
	for _, word := range commonSegments {
		if len(word) > len(text) || rune(word[0]) != text[0] {
			continue
		}
		n := 0
		for n < len(word) && rune(word[n]) == text[n] {
			n++
		}
		if n == len(word) {
			return n
		}
	}
	return 0
}

func relatedCharacters(a, b rune) bool {
	if a == b {
		return true
	}
	delta := a - b
	if a >= '0' && a <= '9' && b >= '0' && b <= '9' && (delta == 1 || delta == -1 || delta == 9 || delta == -9) {
		return true
	}
	if a >= 'a' && a <= 'z' && b >= 'a' && b <= 'z' && (delta == 1 || delta == -1) {
		return true
	}
	rowA, xA := keyPosition(a)
	rowB, xB := keyPosition(b)
	return rowA >= 0 && rowB >= 0 && max(rowA-rowB, rowB-rowA) <= 1 && max(xA-xB, xB-xA) <= 4
}

func keyPosition(c rune) (int, int) {
	for row, keys := range [...]string{"1234567890-=", "qwertyuiop[]", "asdfghjkl;'", "zxcvbnm,./"} {
		if column := strings.IndexRune(keys, c); column >= 0 {
			return row, 4*column + row
		}
	}
	return -1, 0
}
