// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"testing"

	"github.com/dockwell/dockwell/internal/formtest"
)

// CheckSQLName takes what the expression it was took, to the byte.
func TestCheckSQLNameTakesWhatItsExpressionTook(t *testing.T) {
	formtest.Agree(t, `^[a-z][a-z0-9_]{0,31}$`,
		func(name string) bool { return CheckSQLName("user", name) == nil },
		formtest.Corpus("a_z09", "A-. é\n", 32))
}
