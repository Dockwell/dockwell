// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Every environment is made with its protection written, protected for
// production and open for any other, so a name chosen later can't make one
// protected or open. An edit that leaves it out keeps it (ADR 0049).
func TestEveryEnvironmentIsMadeWithItsProtection(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		protection := func(envID string) resourcev1.Protection {
			t.Helper()
			var got resourcev1.Environment
			err := f.store.View(t.Context(), func(tx *store.Tx) error {
				r, err := tx.GetResourceByID(t.Context(), envID)
				if err != nil {
					return err
				}
				return proto.Unmarshal(r.Spec, &got)
			})
			if err != nil {
				t.Fatal(err)
			}
			return got.GetProtection()
		}
		environment := func(scope, name, display string, p resourcev1.Protection) *dockwellv1.Edit {
			return &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: scope, Name: name},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{DisplayName: display, Protection: p}}},
			}
		}
		web := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
		production := web.GetPlan().GetResources()[0].GetResource().GetScope()
		defaultID := envProject(t, f, production)
		if got := protection(production); got != resourcev1.Protection_PROTECTION_PROTECTED {
			t.Errorf("the default project's production: %v, want protected", got)
		}
		staging := resourceIn(t, apply(t, c, propose(t, c, "Add staging", environment(defaultID, "staging", "", 0))), "environment")
		if got := protection(staging); got != resourcev1.Protection_PROTECTION_OPEN {
			t.Errorf("a new staging: %v, want open", got)
		}
		blog := apply(t, c, propose(t, c, "Make blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		}))
		if got := protection(resourceIn(t, blog, "environment")); got != resourcev1.Protection_PROTECTION_PROTECTED {
			t.Errorf("a new project's production: %v, want protected", got)
		}
		// Edits that leave it out keep it, both ways.
		apply(t, c, propose(t, c, "Name staging", environment(defaultID, "staging", "Staging", 0)))
		apply(t, c, propose(t, c, "Name production", environment(defaultID, "production", "Live", 0)))
		if got := protection(staging); got != resourcev1.Protection_PROTECTION_OPEN {
			t.Errorf("staging after naming it: %v, want open still", got)
		}
		if got := protection(production); got != resourcev1.Protection_PROTECTION_PROTECTED {
			t.Errorf("production after naming it: %v, want protected still", got)
		}
		// One made with it written keeps what it says, whatever its name.
		live := resourceIn(t, apply(t, c, propose(t, c, "Add live", environment(defaultID, "live", "", resourcev1.Protection_PROTECTION_PROTECTED))), "environment")
		if got := protection(live); got != resourcev1.Protection_PROTECTION_PROTECTED {
			t.Errorf("live, made protected: %v", got)
		}

		// The panel reads each one's protection, and nothing else of its spec.
		listed, err := dockwellv1connect.NewResourceServiceClient(b, b.origin, connect.WithProtoJSON()).ListResources(t.Context(),
			connect.NewRequest(&dockwellv1.ListResourcesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{production: true, staging: false, live: true, resourceIn(t, blog, "environment"): true}
		for _, r := range listed.Msg.GetResources() {
			if w, ok := want[r.GetId()]; ok {
				if r.GetProtected() != w {
					t.Errorf("%s lists protected %v, want %v", r.GetResource().GetName(), r.GetProtected(), w)
				}
				delete(want, r.GetId())
			} else if r.GetProtected() {
				t.Errorf("the %s %s lists as protected, which only an environment is", r.GetResource().GetKind(), r.GetResource().GetName())
			}
		}
		if len(want) > 0 {
			t.Errorf("environments not listed: %v", want)
		}
	})
}
