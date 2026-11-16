// SPDX-License-Identifier: AGPL-3.0-only

package connections

import (
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/probe"
)

// A check the control server ran, as it holds the keys below an install admin, says
// so, rather than speaking of the app's server (slice 2 review, M4).
func TestACheckSaysWhoRanIt(t *testing.T) {
	refused := &dockwellv1.CheckConnectionResponse{Code: probe.CodeRefused, Words: "10.0.0.5 is a private address"}
	if got := checkWordsFrom(refused, "the control server"); !strings.HasPrefix(got, "The control server sends nothing to that address") {
		t.Errorf("the control server's refusal reads %q", got)
	}
	unreachable := &dockwellv1.CheckConnectionResponse{Code: probe.CodeUnreachable}
	if got := checkFixFrom(unreachable, "the control server"); got != "Check the connection's endpoint, and that the control server can reach it" {
		t.Errorf("its fix reads %q", got)
	}
	if got := checkWords(unreachable); !strings.HasPrefix(got, "The app's server can't reach the bucket") {
		t.Errorf("the app's server's reads %q", got)
	}
}
