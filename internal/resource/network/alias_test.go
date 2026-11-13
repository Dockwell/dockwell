// SPDX-License-Identifier: AGPL-3.0-only

package network

import (
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
)

// A member's alias reads back as what's left of Docker's list once the container's
// own name and short ID are gone, even an alias that begins the container's ID, as
// "db" begins one in 256 (ADR 0084).
func TestAnAliasReadsBackWhateverTheContainersID(t *testing.T) {
	const id = "db0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	summary := func(aliases ...string) *docker.ContainerSummary {
		c := &docker.ContainerSummary{ID: id, Names: []string{"/dockwell-shared-db-1"}}
		c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"dockwell-app_db-link": {Aliases: aliases}}
		return c
	}
	for name, tc := range map[string]struct {
		aliases []string
		want    string
	}{
		"its alias, beginning its ID":      {[]string{"db"}, "db"},
		"after its name and short ID":      {[]string{"dockwell-shared-db-1", id[:12], "db"}, "db"},
		"a longer prefix of its ID":        {[]string{"db0123"}, "db0123"},
		"its name and short ID alone":      {[]string{"dockwell-shared-db-1", id[:12]}, ""},
		"another container's short ID too": {[]string{"0123456789ab"}, "0123456789ab"},
	} {
		if got := aliasOn(summary(tc.aliases...), "dockwell-app_db-link"); got != tc.want {
			t.Errorf("%s: %v reads back as %q, want %q", name, tc.aliases, got, tc.want)
		}
	}
	if got := aliasOn(summary("db"), "elsewhere"); got != "" {
		t.Errorf("on a network it isn't on, it answers to %q", got)
	}
}
