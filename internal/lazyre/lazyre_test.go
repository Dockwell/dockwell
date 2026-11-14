// SPDX-License-Identifier: AGPL-3.0-only

package lazyre

import (
	"testing"
)

// An expression is compiled on its first use, once, and a broken one panics there.
func TestAnExpressionCompilesOnFirstUseOnce(t *testing.T) {
	re := Regexp(`^[a-z]+$`)
	if !re().MatchString("abc") || re().MatchString("ABC") {
		t.Error("it doesn't match as compiled")
	}
	if first, again := re(), re(); first != again {
		t.Error("it compiles each time it's used")
	}
	broken := Regexp(`(`)
	defer func() {
		if recover() == nil {
			t.Error("a broken expression didn't panic on first use")
		}
	}()
	broken()
}

// Making one compiles nothing.
func TestMakingOneCompilesNothing(t *testing.T) {
	if n := testing.AllocsPerRun(10, func() { _ = Regexp(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`) }); n > 4 {
		t.Errorf("making one allocates %v times, as if it compiled", n)
	}
}
