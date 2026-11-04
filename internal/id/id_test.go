// SPDX-License-Identifier: AGPL-3.0-only

package id

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNewIsPrefixedAndParses(t *testing.T) {
	at := time.Date(2026, 11, 3, 21, 40, 0, 123e6, time.UTC)
	var g generator
	s := g.next(Server, at)
	if !strings.HasPrefix(s, "srv_") || len(s) != len("srv_")+ulidLen {
		t.Fatalf("New = %q", s)
	}
	got, err := Parse(Server, s)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(at.Truncate(time.Millisecond)) {
		t.Errorf("Parse time = %v, want %v", got, at)
	}
}

func TestIDsSortByCreationTime(t *testing.T) {
	base := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	var g generator
	var ids []string
	for i := range 50 {
		ids = append(ids, g.next(Event, base.Add(time.Duration(i)*time.Millisecond)))
	}
	if !slices.IsSorted(ids) {
		t.Error("IDs made later must sort later")
	}
}

func TestIDsIncreaseWithinAMillisecond(t *testing.T) {
	at := time.Date(2026, 11, 3, 21, 40, 0, 0, time.UTC)
	var g generator
	prev := g.next(Event, at)
	for range 1000 {
		next := g.next(Event, at)
		if next <= prev {
			t.Fatalf("%s came after %s", next, prev)
		}
		prev = next
	}
	// A clock that steps back still yields increasing IDs.
	if back := g.next(Event, at.Add(-time.Second)); back <= prev {
		t.Errorf("after the clock stepped back, %s <= %s", back, prev)
	}
}

func TestIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 10_000 {
		s := New(JoinToken)
		if seen[s] {
			t.Fatalf("duplicate ID %s", s)
		}
		seen[s] = true
	}
}

func TestParseRejects(t *testing.T) {
	good := New(Server)
	for _, s := range []string{
		"",
		"srv",
		"jtk_" + good[4:],       // wrong prefix
		"srv_" + good[4:29],     // too short
		"srv_" + "U" + good[5:], // U isn't in Crockford base32
		"srv_" + "8" + good[5:], // first character out of range
		"srv_" + good[4:] + "0", // too long
		strings.ToLower(good[:4]) + strings.ToLower(good[4:]), // lower case isn't canonical
	} {
		if _, err := Parse(Server, s); !errors.Is(err, ErrMalformed) {
			t.Errorf("Parse(%q) = %v, want ErrMalformed", s, err)
		}
	}
}
