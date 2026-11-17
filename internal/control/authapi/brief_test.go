// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Below an admin, an update's reason that names a server's architecture is told in
// brief, wherever in a response it is (#771's review, L2).
func TestAnUpdatesReasonIsBriefBelowAnAdmin(t *testing.T) {
	res := &dockwellv1.ListServiceUpdatesResponse{Services: []*dockwellv1.ServiceUpdates{{Update: &dockwellv1.ServiceUpdate{
		Available: []*dockwellv1.AvailableUpdate{
			{ToVersion: "1.5.0", Unavailable: "1.5.0 runs on arm64 alone, and web runs on web-3, which is amd64"},
			{ToVersion: "1.6.0"},
		},
	}}}}
	brief(res)
	got := res.GetServices()[0].GetUpdate().GetAvailable()
	if got[0].GetUnavailable() != "1.5.0 doesn't run on the server web is on" || got[1].GetUnavailable() != "" {
		t.Errorf("briefed: %q, %q", got[0].GetUnavailable(), got[1].GetUnavailable())
	}
}
