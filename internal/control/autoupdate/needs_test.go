// SPDX-License-Identifier: AGPL-3.0-only

package autoupdate

import (
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
)

// A catalog release that needs what the install didn't makes a database no one asked
// for, so an automatic update leaves its plan for a person (ADR 0084).
func TestAReleaseWithANewNeedWaitsForAPerson(t *testing.T) {
	plan := func(reason string) *dockwellv1.Change {
		return &dockwellv1.Change{Plan: &dockwellv1.Plan{Notes: []*dockwellv1.PlanNote{{Reason: reason, Message: "a note"}}}}
	}
	if !needsPerson(plan(catalogapi.NewNeedNote)) {
		t.Error("a release with a new need applies on its own")
	}
	if needsPerson(plan("catalog_pins_images")) {
		t.Error("a release that only pins its images waits for a person")
	}
}
