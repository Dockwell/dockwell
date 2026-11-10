// SPDX-License-Identifier: AGPL-3.0-only

package docker

import "testing"

// Of any two containers, exactly one stands over the other, whichever is asked
// first: the choice can't depend on the order a list has them in, or the agent's
// two looks could take different containers for one service.
func TestOneOfTwoAlwaysStands(t *testing.T) {
	all := []Stand{
		{State: "running", Number: 1, ID: "a"},
		{State: "running", Number: 1, ID: "b"},
		{State: "running", Number: 1, ID: "c"},
		{State: "running", Number: 2, ID: "d"},
		{State: "exited", Number: 1, ID: "e"},
		{State: "exited", Number: 1, ID: "f"},
		{State: "exited", Number: 3, ID: "g"},
		{State: "created", Number: 3, ID: "h"},
		{State: "paused", Number: 0, ID: "i"},
		{State: "restarting", Number: 0, ID: "j"},
	}
	for i, a := range all {
		for j, b := range all {
			if i != j && a.Over(b) == b.Over(a) {
				t.Errorf("%+v over %+v is %t both ways", a, b, a.Over(b))
			}
		}
	}
}

func TestWhichStands(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wins, over Stand
	}{
		{"one that runs, over a higher number that doesn't", Stand{State: "running", Number: 1}, Stand{State: "exited", Number: 2}},
		{"one that runs, over one that's only made", Stand{State: "running", Number: 1}, Stand{State: "created", Number: 2}},
		{"one that's paused, over one that stopped", Stand{State: "paused", Number: 1}, Stand{State: "exited", Number: 1}},
		{"the higher number, of two that run", Stand{State: "running", Number: 2}, Stand{State: "running", Number: 1}},
		{"the higher number, of two that don't", Stand{State: "exited", Number: 6}, Stand{State: "exited", Number: 5}},
		{"the greater ID, of two with one number", Stand{State: "exited", Number: 1, ID: "b"}, Stand{State: "exited", Number: 1, ID: "a"}},
	} {
		if !tc.wins.Over(tc.over) || tc.over.Over(tc.wins) {
			t.Errorf("%s: %+v should stand over %+v", tc.name, tc.wins, tc.over)
		}
	}
	for _, state := range []string{"removing", "dead"} {
		if !(Stand{State: state}).Leaving() {
			t.Errorf("%s isn't leaving", state)
		}
	}
	for _, state := range []string{"running", "exited", "created", "paused", "restarting"} {
		if (Stand{State: state}).Leaving() {
			t.Errorf("%s is leaving", state)
		}
	}
}

// Pick is the whole choice, as containers are added in any order: every order of
// a set gives the same one, with the same word on whether it stands alone.
func TestPickDoesntDependOnOrder(t *testing.T) {
	sets := [][]Stand{
		{{State: "exited", Number: 1, ID: "a"}, {State: "created", Number: 2, ID: "b"}},
		{{State: "running", Number: 1, ID: "a"}, {State: "dead", Number: 2, ID: "b"}},
		{{State: "exited", Number: 1, ID: "a"}, {State: "dead", Number: 2, ID: "b"}, {State: "removing", Number: 3, ID: "c"}},
		{{State: "exited", Number: 1, ID: "a"}, {State: "exited", Number: 1, ID: "b"}, {State: "running", Number: 0, ID: "c"}},
		{{State: "dead", Number: 1, ID: "a"}, {State: "removing", Number: 2, ID: "b"}},
		{{State: "created", Number: 0, ID: "a"}},
	}
	for _, set := range sets {
		var want Pick
		for _, s := range set {
			want.Add(s)
		}
		wantStand, wantOK := want.Stands()
		permute(set, func(order []Stand) {
			var p Pick
			var told Stand
			for _, s := range order {
				if p.Add(s) {
					told = s
				}
			}
			got, ok := p.Stands()
			if got != wantStand || ok != wantOK || p.Sole() != want.Sole() {
				t.Errorf("%+v in the order %+v stands %+v (alone %t); another order gave %+v (alone %t)", set, order, got, p.Sole(), wantStand, want.Sole())
			}
			if ok && told != got {
				t.Errorf("%+v: Add last said %+v stands, and Stands says %+v", order, told, got)
			}
		})
	}
}

func permute(s []Stand, visit func([]Stand)) {
	var rec func(int)
	rec = func(k int) {
		if k == len(s) {
			visit(append([]Stand(nil), s...))
			return
		}
		for i := k; i < len(s); i++ {
			s[k], s[i] = s[i], s[k]
			rec(k + 1)
			s[k], s[i] = s[i], s[k]
		}
	}
	rec(0)
}

func TestPick(t *testing.T) {
	var none Pick
	if _, ok := none.Stands(); ok || none.Sole() {
		t.Error("with nothing added, something stands")
	}
	// A leaving one never stands, and doesn't count against one standing alone.
	var p Pick
	if p.Add(Stand{State: "dead", Number: 9, ID: "dead"}) {
		t.Error("a dead one was said to stand")
	}
	if _, ok := p.Stands(); ok {
		t.Error("with only a dead one, something stands")
	}
	if l, ok := p.Leaving(); !ok || l.ID != "dead" {
		t.Errorf("leaving is %+v, %t", l, ok)
	}
	if !p.Add(Stand{State: "exited", Number: 1, ID: "one"}) || !p.Sole() {
		t.Error("one that stopped, beside a dead one, doesn't stand alone")
	}
	// Another that isn't leaving ends that, whether or not it stands.
	if p.Add(Stand{State: "created", Number: 0, ID: "made"}) || p.Sole() {
		t.Error("a second one that isn't leaving left the first standing alone, or took its place")
	}
	// The same container twice, as all else being equal is: the first stays.
	var twice Pick
	twice.Add(Stand{State: "exited", Number: 1, ID: "same"})
	if twice.Add(Stand{State: "exited", Number: 1, ID: "same"}) {
		t.Error("of two the rule can't tell apart, the second took the first's place")
	}
}

// A record covers a container that was taken on, one made for a revision no later
// than its own, and one that says no revision when its image is the record's;
// never one of another image. Docker's error is news only of one that stands
// alone and is covered.
func TestCovered(t *testing.T) {
	for _, tc := range []struct {
		name     string
		s        Stand
		recorded int64
		tie      ImageTie
		want     bool
	}{
		{"the record's own revision", Stand{Revision: 3}, 3, ImageUnknown, true},
		{"an earlier one, as a fill leaves it", Stand{Revision: 1}, 3, ImageSame, true},
		{"a later one, which never passed", Stand{Revision: 4}, 3, ImageSame, false},
		{"taken on, with no revision", Stand{Adopted: true}, 3, ImageUnknown, true},
		{"taken on, of the record's image", Stand{Adopted: true}, 3, ImageSame, true},
		{"taken on, of another image", Stand{Adopted: true}, 3, ImageOther, false},
		{"no revision, of the record's image, as an older release made it", Stand{}, 3, ImageSame, true},
		{"no revision, and nothing known of its image", Stand{}, 3, ImageUnknown, false},
		{"no revision, of another image", Stand{}, 3, ImageOther, false},
		{"the record's revision, of another image, as a pull that never passed", Stand{Revision: 3}, 3, ImageOther, false},
		{"the record's revision, of its image", Stand{Revision: 3}, 3, ImageSame, true},
	} {
		got, why := tc.s.Covered(tc.recorded, tc.tie)
		if got != tc.want || got == (why != "") {
			t.Errorf("%s: covered=%t, why=%q; want %t, with why only when it isn't", tc.name, got, why, tc.want)
		}
	}
	if CouldntStart(false, true) || CouldntStart(true, false) || !CouldntStart(true, true) {
		t.Error("Docker's error is news only of one that stands alone and is covered")
	}
	for _, tc := range []struct {
		label string
		want  int64
	}{{"7", 7}, {"", 0}, {"x", 0}, {"-2", 0}, {"007", 7}, {"7 ", 0}, {"99999999999999999999", 0}} {
		if got := revisionOf(map[string]string{LabelRevision: tc.label}); got != tc.want {
			t.Errorf("a revision label %q reads %d, want %d", tc.label, got, tc.want)
		}
	}
}
