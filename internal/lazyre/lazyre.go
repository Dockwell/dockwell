// SPDX-License-Identifier: AGPL-3.0-only

// Package lazyre compiles a regular expression the first time it's used, not as the
// program starts. An expression a package keeps for a check that runs only sometimes
// costs an idle agent nothing until then: compiled as it starts, each one is heap the
// agent keeps, and code it runs, whether it ever checks anything (ADR 0060, #622).
package lazyre

import (
	"regexp"
	"sync"
)

// Regexp returns the expression, compiled on its first use and kept after. An
// expression that doesn't compile panics there, as regexp.MustCompile would as the
// program starts: each one's tests use it.
func Regexp(expr string) func() *regexp.Regexp {
	return sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(expr) })
}
