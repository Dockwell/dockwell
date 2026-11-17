// SPDX-License-Identifier: AGPL-3.0-only

package serviceupdates

import (
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// An update that can't be planned where the app runs doesn't count, as the panel's
// counts don't count it, so no "update available" alert fires for a release nobody
// can take (#771's review, M1). One that can still counts.
func TestAnUnavailableUpdateDoesNotCount(t *testing.T) {
	update := func(why string) *dockwellv1.ServiceUpdate {
		return &dockwellv1.ServiceUpdate{Check: dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE, Available: []*dockwellv1.AvailableUpdate{{
			Kind: dockwellv1.UpdateKind_UPDATE_KIND_MINOR, ToVersion: "2.0", CatalogRelease: 2, Unavailable: why,
		}}}
	}
	if Counts(update("2.0 runs on arm64 alone, and web runs on hz-1, which is amd64"), time.Now()) {
		t.Error("an unavailable update counts, so an alert says it's there to take")
	}
	if !Counts(update(""), time.Now()) {
		t.Error("an update that can be planned doesn't count")
	}
}
