// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"strings"
	"testing"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestDockerMembershipProtectsModesAndLastNetworkAndKeepsOtherAliases(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setupDocker(t, b, "srv_A")
		env := production(t, db)
		web := uses(env, "srv_A")
		body := resource.Workload(web.Spec)
		body.Networks = []string{"back", "front"}
		body.NetworkAliases = []string{"back=prior", "front=remaining"}
		front := back(env, "srv_A")
		front.Ref.Name = "front"
		last := uses(env, "srv_A")
		last.Ref.Name = "last"
		host := uses(env, "srv_A")
		host.Ref.Name = "host"
		resource.Workload(host.Spec).Networks = nil
		resource.Workload(host.Spec).HostNetwork = true
		apply(t, e, propose(t, e, "Put workloads on networks", web, last, host, data(env, "srv_A"), back(env, "srv_A"), front))
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			if _, err := e.SpecWithNetworkMembership(t.Context(), tx, host.Ref, 1, "srv_A", "front", false, true); err == nil ||
				!strings.Contains(err.Error(), "Change its network mode in its workload plan first") || strings.Contains(err.Error(), "replica") {
				t.Fatalf("host network refusal = %v", err)
			}
			if _, err := e.SpecWithNetworkMembership(t.Context(), tx, last.Ref, 1, "srv_A", "back", false, false); err == nil || !strings.Contains(err.Error(), "last network") {
				t.Fatalf("last network refusal = %v", err)
			}
			out, err := e.SpecWithNetworkMembership(t.Context(), tx, web.Ref, 1, "srv_A", "back", false, false)
			if err != nil {
				t.Fatal(err)
			}
			app := out.(*spec.Service).GetApp()
			if len(app.GetNetworkAliases()) != 1 || app.GetNetworkAliases()[0] != "front=remaining" || len(app.GetNetworks()) != 1 || app.GetNetworks()[0] != "front" {
				t.Fatalf("remaining networks and aliases = %v", app)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
