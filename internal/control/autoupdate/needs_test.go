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
	if !needsPerson(plan(catalogapi.NewNeedNote), noPerson) {
		t.Error("a release with a new need applies on its own")
	}
	if needsPerson(plan("catalog_pins_images"), noPerson) {
		t.Error("a release that only pins its images waits for a person")
	}
}

// A catalog release that moves a port to the server alone, as one to an install from
// before ADR 0101 moves its console, waits for a person: whoever reached it on the
// server's address would find it gone the next morning (#752's re-check, N2).
func TestAReleaseThatMovesAPortToTheServerAloneWaitsForAPerson(t *testing.T) {
	plan := &dockwellv1.Change{Plan: &dockwellv1.Plan{Notes: []*dockwellv1.PlanNote{{Reason: catalogapi.PortToServerAloneNote, Message: "a note"}}}}
	if !needsPerson(plan, noPerson) {
		t.Error("a release that moves a port to the server alone applies on its own")
	}
}

// noPerson is an engine for which no kind takes a person.
func noPerson(string) bool { return false }

// An update whose plan changes who reaches a server, as a firewall rule does,
// waits for a person's passkey, though it isn't destructive (ADR 0052, #850's
// review, L2).
func TestAnUpdateThatChangesAFirewallWaitsForAPerson(t *testing.T) {
	plan := &dockwellv1.Change{Plan: &dockwellv1.Plan{Resources: []*dockwellv1.PlannedResource{
		{Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "postgres"}},
		{Resource: &dockwellv1.ResourceRef{Kind: "firewall.rule", Name: "postgres-deny"}},
	}}}
	takes := func(kind string) bool { return kind == "firewall.rule" }
	if !needsPerson(plan, takes) {
		t.Error("an update with a firewall rule applies on its own")
	}
	if needsPerson(plan, noPerson) {
		t.Error("an update whose kinds take no person waits for one")
	}
}
