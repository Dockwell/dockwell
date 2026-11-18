// SPDX-License-Identifier: AGPL-3.0-only

package githooks

import (
	"testing"

	"github.com/dockwell/dockwell/internal/revision"
)

// A push whose plan changes who reaches its server, as a firewall rule does,
// waits for a person's passkey and says so, though it isn't destructive (ADR
// 0052, #850's review, L2).
func TestAPushThatChangesAFirewallWaitsForAPerson(t *testing.T) {
	if got := needsPerson(&revision.Plan{}, true); got != "changes who reaches its server, which takes a person's passkey" {
		t.Errorf("a plan that touches access: %q", got)
	}
	if got := needsPerson(&revision.Plan{}, false); got != "" {
		t.Errorf("a plan that needs nobody: %q", got)
	}
}
