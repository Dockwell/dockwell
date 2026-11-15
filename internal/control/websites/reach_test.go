// SPDX-License-Identifier: AGPL-3.0-only

package websites

import (
	"testing"

	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/store"
)

// What holds a domain is named only to a caller who can read its project: an admin
// of the install reads every one, and a project's own developer only that project
// (ADR 0048, as ADR 0070 asks of a website's other domains).
func TestAHoldersProjectIsNamedOnlyToWhoCanReadIt(t *testing.T) {
	admin := place.WithCaller(t.Context(), place.Caller{UserID: "usr_a", Grants: []store.Grant{{UserID: "usr_a", ProjectID: store.Install, Role: store.RoleAdmin}}})
	dev := place.WithCaller(t.Context(), place.Caller{UserID: "usr_d", Grants: []store.Grant{{UserID: "usr_d", ProjectID: "prj_mine", Role: store.RoleDeveloper}}})
	if !readsProject(admin, "prj_theirs") || !readsProject(dev, "prj_mine") || readsProject(dev, "prj_theirs") {
		t.Errorf("an admin reads another project %t, a developer their own %t and another %t",
			readsProject(admin, "prj_theirs"), readsProject(dev, "prj_mine"), readsProject(dev, "prj_theirs"))
	}
}
