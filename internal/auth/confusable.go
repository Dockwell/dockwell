// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// invisible reports a character that prints as nothing, or reorders what's
// around it: format characters (Unicode Cf, such as zero-width spaces and
// joiners, a soft hyphen and the byte-order mark), the bidi controls, variation
// selectors, and the fillers that are letters but print as space (#801's
// review, M1).
func invisible(r rune) bool {
	switch r {
	case 0x115F, 0x1160, 0x3164, 0xFFA0, 0x2800: // Hangul fillers and the blank braille cell
		return true
	}
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Bidi_Control, r) || unicode.Is(unicode.Variation_Selector, r)
}

// withoutInvisible is s in NFKC, as a name is kept, with what invisible
// reports dropped.
func withoutInvisible(s string) string {
	return strings.Map(func(r rune) rune {
		if invisible(r) {
			return -1
		}
		return r
	}, norm.NFKC.String(s))
}

// capitals are the Cyrillic and Greek capitals that read as a Latin one, each
// mapped before lowercasing, as their lowercase reads as another letter: Greek Μ
// is an M, while μ reads as a u.
var capitals = map[rune]rune{
	'А': 'A', 'В': 'B', 'Е': 'E', 'К': 'K', 'М': 'M', 'Н': 'H', 'О': 'O', 'Р': 'P', 'С': 'C', 'Т': 'T', 'Х': 'X',
	'Ѕ': 'S', 'І': 'I', 'Ј': 'J', 'Ү': 'Y',
	'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ζ': 'Z', 'Η': 'H', 'Ι': 'I', 'Κ': 'K', 'Μ': 'M', 'Ν': 'N', 'Ο': 'O', 'Ρ': 'P',
	'Τ': 'T', 'Υ': 'Y', 'Χ': 'X',
}

// lookalikes are the letters, from Cyrillic, Greek and Latin, and the digits
// and signs, that read as another letter in the fonts a panel uses, each
// mapped, once lowercased, to the one it reads as. It's the common part of
// Unicode's confusables (UTS #39), not all of it: what a person picks to pass
// for someone else, in a name or an email.
var lookalikes = map[rune]rune{
	// Cyrillic.
	'а': 'a', 'в': 'b', 'е': 'e', 'ѐ': 'e', 'з': '3', 'к': 'k', 'м': 'm', 'н': 'h', 'о': 'o', 'п': 'n', 'р': 'p',
	'с': 'c', 'т': 't', 'у': 'y', 'х': 'x', 'ь': 'b', 'ѕ': 's', 'і': 'l', 'ј': 'j', 'һ': 'h', 'ӏ': 'l', 'ԁ': 'd',
	'ԛ': 'q', 'ԝ': 'w', 'ү': 'y', 'ɡ': 'g',
	// Greek.
	'α': 'a', 'β': 'b', 'γ': 'y', 'ε': 'e', 'η': 'n', 'ι': 'l', 'κ': 'k', 'μ': 'u', 'ν': 'v', 'ο': 'o', 'ρ': 'p',
	'σ': 'o', 'ς': 'c', 'τ': 't', 'υ': 'u', 'χ': 'x', 'ω': 'w', 'ϲ': 'c', 'ϳ': 'j',
	// Latin that doesn't decompose, and i and l, which a sans font draws alike.
	'ı': 'l', 'i': 'l', 'ł': 'l', 'ƚ': 'l', 'ø': 'o', 'đ': 'd', 'ħ': 'h', 'ŧ': 't', 'ƅ': 'b',
	// Digits and signs that pass for letters.
	'0': 'o', '1': 'l', '|': 'l', '5': 's',
}

// skeleton is what s looks like, for telling two names or two emails apart by
// eye: NFKC, without what prints as nothing or the marks over letters, with
// Cyrillic and Greek capitals that read as Latin ones as those, lowercased,
// with each lookalike as the letter it reads as, rn as m and vv as w, and runs
// of space as one. Two strings with one skeleton read the same, as
// "Mira Chen" and "Mіra Chen" with a Cyrillic і do (#801's review, M1).
func skeleton(s string) string {
	var b strings.Builder
	space := false
	for _, r := range norm.NFD.String(withoutInvisible(s)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		if to, ok := capitals[r]; ok {
			r = to
		}
		r = unicode.ToLower(r)
		if r == 'ß' {
			b.WriteString("ss")
			continue
		}
		if to, ok := lookalikes[r]; ok {
			r = to
		}
		b.WriteRune(r)
	}
	out := b.String()
	return strings.NewReplacer("rn", "m", "vv", "w").Replace(out)
}
