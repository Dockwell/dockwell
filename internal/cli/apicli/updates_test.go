// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// An update that can't be planned where the app runs says so, and why, beside the
// version, as the panel does (#771).
func TestAnUnavailableUpdateSaysWhy(t *testing.T) {
	u := &dockwellv1.ServiceUpdate{Check: dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE, Available: []*dockwellv1.AvailableUpdate{{
		Kind: dockwellv1.UpdateKind_UPDATE_KIND_MINOR, ToVersion: "1.5.0",
		Unavailable: "Release 2 runs on arm64 alone, and web runs on web-3, which is amd64",
	}}}
	if got, want := newerWords(u, time.Now()), "1.5.0 minor, unavailable: Release 2 runs on arm64 alone, and web runs on web-3, which is amd64"; got != want {
		t.Errorf("newerWords = %q, want %q", got, want)
	}
}
