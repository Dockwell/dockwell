// SPDX-License-Identifier: AGPL-3.0-only

package variable

import (
	"testing"

	"github.com/dockwell/dockwell/internal/formtest"
)

// CheckName takes what the expression it was took, to the byte.
func TestCheckNameTakesWhatItsExpressionTook(t *testing.T) {
	formtest.Agree(t, `^[A-Za-z_][A-Za-z0-9_]{0,254}$`,
		func(name string) bool { return CheckName("variable", name) == nil },
		formtest.Corpus("A_z09", "-. é\n", 255))
}
