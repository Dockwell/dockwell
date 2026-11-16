// SPDX-License-Identifier: AGPL-3.0-only

package link_test

import (
	"testing"

	"github.com/dockwell/dockwell/internal/resource/link"
)

// Two prefixes meet where one is the other or holds it, and a free prefix is the
// one wanted or the first after it that meets none taken (the slice 1 review, H2).
func TestAFreePrefixMeetsNoneTaken(t *testing.T) {
	for _, m := range []struct {
		a, b string
		meet bool
	}{{"ghost/", "ghost/", true}, {"ghost/", "ghost/media/", true}, {"", "ghost/", true}, {"ghost/", "ghost-2/", false}, {"gh/", "ghost/", false}} {
		if link.PrefixesMeet(m.a, m.b) != m.meet || link.PrefixesMeet(m.b, m.a) != m.meet {
			t.Errorf("%q and %q meet: %v", m.a, m.b, !m.meet)
		}
	}
	for want, taken := range map[string][]string{
		"ghost/":   nil,
		"ghost-3/": {"ghost/", "ghost-2/"},
		"ghost-2/": {"ghost/media/"},
	} {
		if got := link.FreePrefix("ghost/", taken); got != want {
			t.Errorf("FreePrefix(ghost/, %v) = %q, want %q", taken, got, want)
		}
	}
	if got := link.FreePrefix("ghost/media/", []string{"ghost/"}); got != "ghost-2/" {
		t.Errorf("within a taken prefix, FreePrefix is %q", got)
	}
	if got := link.FreePrefix("ghost/", []string{""}); got != "ghost/" {
		t.Errorf("beside a link at the root, FreePrefix is %q", got)
	}
}
