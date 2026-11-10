// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"testing"

	"github.com/dockwell/dockwell/internal/formtest"
)

// CheckLabel and IsLabel take what the expression they were took, to the byte.
func TestCheckLabelTakesWhatItsExpressionTook(t *testing.T) {
	const was = `^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`
	cases := formtest.Corpus("a-z09", "A._ é\n", 63)
	formtest.Agree(t, was, IsLabel, cases)
	formtest.Agree(t, was, func(name string) bool { return CheckLabel("service", name, "web") == nil }, cases)
}
