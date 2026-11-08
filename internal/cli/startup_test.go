// SPDX-License-Identifier: AGPL-3.0-only

package cli

import "time"

// startUp is how long a test waits for a dockwell run it started to get going:
// open its store, make its keys and listen. That takes well under a second on
// an idle machine, but a loaded CI runner can take many times that, and a test
// that gives up then says nothing about the code (#329).
const startUp = 60 * time.Second
