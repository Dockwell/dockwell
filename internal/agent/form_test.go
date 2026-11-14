// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"testing"

	"github.com/dockwell/dockwell/internal/formtest"
)

// A firewalld zone's name passes on what the expression it was passed on, to the
// byte.
func TestAFirewalldZoneNameTakesWhatItsExpressionTook(t *testing.T) {
	formtest.Agree(t, `^[A-Za-z0-9_-]{1,64}$`,
		func(zone string) bool {
			return len(zone) <= maxFirewalldZoneName && firewalldZoneName().MatchString(zone)
		},
		formtest.Corpus("a_-Z09", ". =é\n", 64))
}
