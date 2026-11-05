// SPDX-License-Identifier: AGPL-3.0-only

// Command guardchild is what the control plane's child does, for the tests of
// its limits. They build it without the race detector, which needs far more
// memory and CPU than the limits are set for, so the limits read as they do
// on a server, whatever the tests themselves are built with.
package main

import (
	"fmt"
	"os"

	"github.com/dockwell/dockwell/internal/compose"
)

func main() {
	if err := compose.RunGuard(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
