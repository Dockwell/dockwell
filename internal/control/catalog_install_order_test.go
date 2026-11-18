// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An install whose app has an owner, and a route, still starts what its web service
// depends on first: the owner and the route reach the web service, which a plan
// puts before them, and placed ahead of the services they took it ahead of its
// database, as Umami, which exits without one, found on the catalog's proof.
func TestAnInstallWithAnOwnerStartsItsDatabaseFirst(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		entry := ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop)
		entry = strings.Replace(entry, "    web:\n      image:", "    web:\n      depends_on:\n        db:\n          condition: service_healthy\n      image:", 1)
		entry = strings.Replace(entry, "  volumes:\n    data: {}\n", "    db:\n      image: "+firstRelease+"\n      healthcheck:\n        test: [\"CMD\", \"true\"]\n  volumes:\n    data: {}\n", 1)
		o := startOwning(t, backend, entry, nil)
		res, err := o.cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{
			Id: "thing", Route: &resourcev1.Route{Domain: "thing.example.com"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		plan := res.Msg.GetChange().GetPlan()
		if !slices.ContainsFunc(plan.GetResources(), func(r *dockwellv1.PlannedResource) bool {
			return r.GetResource().GetKind() == "app.owner"
		}) {
			t.Fatalf("the install makes no owner: %v", plan.GetResources())
		}
		var order []string
		for _, s := range plan.GetServers() {
			for _, step := range s.GetSteps() {
				if r := step.GetResource(); r.GetKind() == "service" {
					order = append(order, r.GetName())
				}
			}
		}
		db, web := slices.Index(order, "db"), slices.Index(order, "web")
		if db < 0 || web < 0 || db > web {
			t.Errorf("the install starts its services in the order %v, want db before web", order)
		}
	})
}
