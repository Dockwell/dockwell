// SPDX-License-Identifier: AGPL-3.0-only

package auth

import "testing"

// A skeleton is one for what reads the same: by case, marks, spaces, what
// prints as nothing, and the common lookalikes from Cyrillic, Greek and digits;
// and two for what doesn't (#801's review, M1).
func TestASkeletonIsOneForWhatReadsTheSame(t *testing.T) {
	same := [][2]string{
		{"Mira Chen", "mira chen"},
		{"Mira Chen", "MIRA   CHEN"},
		{"Mira Chen", "Mіra Chen"}, // Cyrillic і
		{"Mira Chen", "Мira Сhen"}, // Cyrillic М and С
		{"Mira Chen", "Μira Chen"}, // Greek Μ
		{"Mira Chen", "Mira Chén"}, // a mark
		{"Mira Chen", "Mira\u200b Chen\u200d"},
		{"Mira Chen", "\u202eMira Chen\u202c"},
		{"Mira Chen", "Ｍｉｒａ Ｃｈｅｎ"}, // full width
		{"Mira Chen", "MIra Chen"},
		{"Mira Chen", "M1ra Chen"},
		{"mira@acme.dev", "mіra@acme.dev"},
		{"mira@acme.dev", "mira@acrne.dev"},
		{"mira@acme.dev", "mirа@аcmе.dеv"}, // Cyrillic а and е
		{"ops@example.com", "0ps@example.com"},
		{"wes@example.com", "vves@example.com"},
		{"Strass", "Straß"},
		{"MAX", "МАХ"}, // Cyrillic capitals
	}
	for _, tt := range same {
		if a, b := skeleton(tt[0]), skeleton(tt[1]); a != b {
			t.Errorf("%q reads as %q, %q as %q, want one skeleton", tt[0], a, tt[1], b)
		}
	}
	apart := [][2]string{
		{"Mira Chen", "Mira Cohen"},
		{"Mira Chen", "Miro Chen"},
		{"Mira Chen", "MiraChen"},
		{"mira@acme.dev", "mira@acme.dev.example"},
		{"sam@example.com", "sam@example.org"},
	}
	for _, tt := range apart {
		if skeleton(tt[0]) == skeleton(tt[1]) {
			t.Errorf("%q and %q read as one, want two", tt[0], tt[1])
		}
	}
}

// What prints as nothing, or turns text around, isn't part of a name.
func TestInvisibleCharactersLeaveANameAsItReads(t *testing.T) {
	for in, want := range map[string]string{
		"Mira\u200bChen":            "MiraChen",
		"\u202eneh\u202c Mira":      "neh Mira",
		"Mira\u00ad Chen\ufeff":     "Mira Chen",
		"Mira \u2066Chen\u2069":     "Mira Chen",
		"Ｍｉｒａ":                      "Mira",
		"Mira\u3164Chen":            "MiraChen",
		"  Mira \u200d  Chen\u200c": "Mira Chen",
	} {
		got, err := normalizeName(in)
		if err != nil || got != want {
			t.Errorf("%q kept as %q, %v; want %q", in, got, err, want)
		}
	}
	for _, empty := range []string{"\u200b", "\u202e\u202c", "\u3164", " \ufeff "} {
		if _, err := normalizeName(empty); err == nil {
			t.Errorf("%q is a name, want none", empty)
		}
	}
}
