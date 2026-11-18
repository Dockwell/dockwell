// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A database the catalog installs is backed up daily from the start, to the storage
// connected off the server, as a line of the install's own plan a person sees before
// applying it; once applied, the schedule is there (#793).
func TestANewDatabaseBacksUpDailyToStorage(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testANewDatabaseBacksUpDailyToStorage)
}

func testANewDatabaseBacksUpDailyToStorage(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c, bc, cat := changeClient(b, b.origin), backupClient(b, b.origin), catalogClient(b)
	apply(t, c, propose(t, c, "Send backups to R2", destinationEdit(s3test.New(t), "wJalrXUtnFEMI-K7MDENG-the-secret")))

	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres"})
	var daily *dockwellv1.PlannedResource
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "backup.policy" {
			daily = r
		}
	}
	if daily == nil || daily.GetResource().GetName() != "db-daily" || daily.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_CREATE {
		t.Fatalf("the install plans no daily backup of its database: %v", planned.GetPlan().GetResources())
	}
	said := map[string]string{}
	for _, ch := range daily.GetChanges() {
		said[ch.GetPath()] = ch.GetTo()
	}
	if said["volume"] != "data" || said["every"] != "day" || said["destination"] != "r2" {
		t.Errorf("the daily backup plans %v, want the data volume, every day, sent to r2", said)
	}
	if applied := apply(t, c, planned); applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", applied.GetState(), applied.GetError())
	}
	listed, err := bc.ListBackupPolicies(t.Context(), connect.NewRequest(&dockwellv1.ListBackupPoliciesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := listed.Msg.GetPolicies(); len(got) != 1 || got[0].GetName() != "db-daily" || got[0].GetSpec().GetDestination() != "r2" || got[0].GetNextRun() == nil {
		t.Errorf("after the install, policies = %v", got)
	}
}

// An install that asks for a route and runs a database plans both in one change, each
// a line of its own: the route to its web service (#792), and the database's daily
// backup (#793). Applied, both are there.
func TestAnInstallsRouteAndDailyBackupAreOnePlan(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
		e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
		pullsAnything(e)
		c, bc, cat := changeClient(b, b.origin), backupClient(b, b.origin), catalogClient(b)

		planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "nextcloud", Route: &resourcev1.Route{Domain: "cloud.example.com"}})
		lines := map[string]int{}
		for _, r := range planned.GetPlan().GetResources() {
			lines[r.GetResource().GetKind()+" "+r.GetResource().GetName()]++
		}
		if lines["route cloud-example-com"] != 1 || lines["backup.policy db-daily"] != 1 {
			t.Fatalf("the install plans %v, want its route and its database's daily backup, a line each", lines)
		}
		if applied := apply(t, c, planned); applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the install ended %v: %s", applied.GetState(), applied.GetError())
		}
		listed, err := bc.ListBackupPolicies(t.Context(), connect.NewRequest(&dockwellv1.ListBackupPoliciesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if got := listed.Msg.GetPolicies(); len(got) != 1 || got[0].GetName() != "db-daily" {
			t.Errorf("after the install, policies = %v", got)
		}
	})
}
