// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"strings"
	"testing"
)

// What Railpack 0.43.0 says it can't do decides what a refusal tells you to do.
func TestARailpackRefusalSaysWhatToDoByWhatItSaid(t *testing.T) {
	for _, c := range []struct {
		said, problem, tool, advice string
	}{
		{"Railpack could not determine how to build the app.\n\nThe following languages are supported:\n- Node", ProblemNoProvider, "", "set both a build command and a start command"},
		{"failed to resolve version 99 of node", ProblemVersion, "node", "or set RAILPACK_NODE_VERSION as a build argument"},
		{"package `bun` not available in Mise after trying versions: 9.9. Try installing as apt package instead", ProblemVersion, "bun", "RAILPACK_BUN_VERSION"},
		{"No start command detected. Specify a start command: https://railpack.com/config/file#deploy", ProblemNoStart, "", "Set the build's start command"},
		{"failed to ensure mise is installed: failed to download and install: temporary failure fetching https://github.com/jdx/mise/releases/download/v2026.10.14/mise.tar.gz: dial tcp: lookup github.com: no such host", ProblemNetwork, "", "Check the server reaches github.com"},
		{"failed to get latest version for package `node` after trying versions: 22: failed to run mise command: error sending request for url", ProblemNetwork, "", "github.com"},
		{"`build` inputs must be an image or step input", ProblemOther, "", "Fix what it says in the repository, or add a Dockerfile"},
	} {
		problem, tool := RailpackProblem(c.said)
		if problem != c.problem || tool != c.tool {
			t.Errorf("%.50q reads as %s %q, not %s %q", c.said, problem, tool, c.problem, c.tool)
		}
		if got := RailpackRefusal(&BuildDetection{Error: c.said}, "github.com/acme/web at 8be41d2"); !strings.Contains(got, c.advice) {
			t.Errorf("%.50q is refused with %q", c.said, got)
		}
	}
}
