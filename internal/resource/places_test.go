// SPDX-License-Identifier: AGPL-3.0-only

package resource

import "testing"

// Places hold where in a list what a hash is of is, the first in the value
// itself: one at place 0 counts as held, and two that hash the same are both
// found, as NewAccess compares what's at each.
func TestPlacesHoldEachPlaceOnce(t *testing.T) {
	var p places
	if p.holds(func(int32) bool { return true }) {
		t.Error("no places hold something")
	}
	p = p.with(0)
	if !p.holds(func(j int32) bool { return j == 0 }) {
		t.Error("the place 0 isn't held")
	}
	p = p.with(7).with(9)
	for _, want := range []int32{0, 7, 9} {
		if !p.holds(func(j int32) bool { return j == want }) {
			t.Errorf("the place %d isn't held among %+v", want, p)
		}
	}
	if p.holds(func(j int32) bool { return j == 3 }) {
		t.Errorf("%+v holds a place never added", p)
	}
}

// Two accesses whose keys hash the same don't cover each other: a match is
// what's there, not its hash.
func TestNewAccessComparesWhatAHashHolds(t *testing.T) {
	from := []Access{{Kind: AccessPath, Path: "/srv/a", ReadOnly: true}, {Kind: AccessPath, Path: "/srv/b"}}
	to := []Access{{Kind: AccessPath, Path: "/srv/a"}, {Kind: AccessPath, Path: "/srv/b"}, {Kind: AccessPath, Path: "/srv/c"}, {Kind: AccessPath, Path: "/srv/c"}}
	got := NewAccess(from, to)
	if len(got) != 2 || got[0].Path != "/srv/a" || got[1].Path != "/srv/c" {
		t.Errorf("NewAccess = %+v, want /srv/a read-write and /srv/c once", got)
	}
}
