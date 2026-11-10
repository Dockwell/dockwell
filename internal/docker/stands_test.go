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
