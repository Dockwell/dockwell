// SPDX-License-Identifier: AGPL-3.0-only

// Package formtest checks, in tests, that a check of a name's form takes exactly
// what the regular expression it replaced took. Such a check has its length apart
// from its form, as a counted repetition compiles to as many copies of what it
// repeats, which every agent pays for as it starts
// (docs/adr/0060-what-an-idle-agent-keeps-in-memory.md). The expression it was
// stays in the test, as what the check must agree with.
package formtest

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// Agree fails the test for each case that now and the expression was disagree on.
func Agree(t *testing.T, was string, now func(string) bool, cases []string) {
	t.Helper()
	re := regexp.MustCompile(was)
	took, failed := 0, 0
	for _, c := range cases {
		want := re.MatchString(c)
		if want {
			took++
		}
		if got := now(c); got != want {
			failed++
			if failed <= 10 {
				t.Errorf("%q (%d bytes): the check says %v, and %s said %v", c, len(c), got, was, want)
			}
		}
	}
	// Cases that all fail, or all pass, would say nothing of the other side.
	if took == 0 || took == len(cases) {
		t.Errorf("%s took %d of %d cases, so they don't tell a check that takes too much from one that takes too little", was, took, len(cases))
	}
}

// Corpus returns strings to try a form with: of each length from 0 to 4, and
// of each length given with the one below it and the one above, many of each. Most
// are of the characters in valid alone, in any order; the rest have one character
// of odd somewhere, which is what a form refuses. They're the same on every run.
func Corpus(valid, odd string, lengths ...int) []string {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test cases, the same each run
	sizes := []int{0, 1, 2, 3, 4}
	for _, n := range lengths {
		sizes = append(sizes, n-1, n, n+1)
	}
	var out []string
	for _, size := range sizes {
		if size < 0 {
			continue
		}
		for range 300 {
			b := make([]byte, size)
			for i := range b {
				b[i] = valid[r.IntN(len(valid))]
			}
			// A form often asks more of the first and last characters, so most cases
			// start and end with the first of valid, which a caller makes one any
			// position takes.
			if size > 0 && r.IntN(4) != 0 {
				b[0], b[size-1] = valid[0], valid[0]
			}
			s := string(b)
			if size > 0 && odd != "" && r.IntN(5) == 0 {
				at := r.IntN(size)
				s = s[:at] + string(odd[r.IntN(len(odd))]) + s[at+1:]
			}
			out = append(out, s)
		}
	}
	return out
}

// Joined returns strings of count parts each, joined by sep, with the parts drawn
// from parts, and the same on every run.
func Joined(parts []string, sep string, count, n int) []string {
	r := rand.New(rand.NewPCG(3, uint64(count))) //nolint:gosec // test cases, the same each run
	out := make([]string, 0, n)
	for range n {
		picked := make([]string, count)
		for i := range picked {
			picked[i] = parts[r.IntN(len(parts))]
		}
		out = append(out, strings.Join(picked, sep))
	}
	return out
}
