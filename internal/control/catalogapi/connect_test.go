// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"testing"

	"github.com/dockwell/dockwell/internal/control/connections"
)

// A connection is named for the install's environment only as one of a set named
// for each environment, as uploads-production beside uploads-staging: one whose
// name merely ends in -production isn't (the slice 1 review, L1).
func TestAConnectionNamedForTheEnvironmentIsOneOfASet(t *testing.T) {
	saved := func(names ...string) []connections.Saved {
		var out []connections.Saved
		for _, n := range names {
			out = append(out, connections.Saved{Name: n})
		}
		return out
	}
	for name, want := range map[string]bool{
		"uploads-production":    true,
		"backups-production":    false,
		"production":            false,
		"uploads":               false,
		"media-team-production": false,
	} {
		all := saved("uploads-production", "uploads-staging", "backups-production", "uploads", "media-team-production", "media-team-x-y")
		if got := namedForEnvironment(name, all); got != want {
			t.Errorf("%s is named for the environment: %v, want %v", name, got, want)
		}
	}
}
