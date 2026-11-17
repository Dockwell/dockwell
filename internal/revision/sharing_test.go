// SPDX-License-Identifier: AGPL-3.0-only

package revision

import "testing"

// An app is a service: two services of one environment are two apps, and links no
// service reads yet are one app of their environment, where no service there reads
// one (the re-check's N2).
func TestAppsAreServices(t *testing.T) {
	for _, tc := range []struct {
		apps []string
		want int
	}{
		{[]string{"env_a/notes", "env_a/gallery"}, 2},
		{[]string{"env_a/notes", "env_a"}, 1},
		{[]string{"env_a", "env_b"}, 2},
		{[]string{"env_a/notes", "env_b"}, 2},
		{[]string{"env_a"}, 1},
	} {
		set := map[string]bool{}
		for _, a := range tc.apps {
			set[a] = true
		}
		if got := countApps(set); got != tc.want {
			t.Errorf("%v count as %d apps, want %d", tc.apps, got, tc.want)
		}
	}
	if !sameApp("env_a", "env_a/notes") || sameApp("env_a/notes", "env_a/gallery") || sameApp("env_a", "env_ab/notes") {
		t.Error("sameApp tells apps apart wrongly")
	}
}
