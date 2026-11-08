// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"errors"
	"testing"

	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Where each object belongs is its project and environment, and whether that's
// protected, whichever kind of object the ID names. An ID of a kind nothing
// places is refused, so a new kind is closed until it's placed (ADR 0048).
func TestEveryObjectHasAPlace(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
		web := done.GetPlan().GetResources()[0]
		envID := web.GetResource().GetScope()

		err := f.store.View(t.Context(), func(tx *store.Tx) error {
			env, err := tx.GetResourceByID(t.Context(), envID)
			if err != nil {
				return err
			}
			production := place.Place{Project: env.Scope, Environment: envID, Protected: true}
			for what, id := range map[string]string{"the service": web.GetId(), "its environment": envID, "the change": done.GetId()} {
				got, err := place.Of(t.Context(), tx, id)
				if err != nil || len(got) != 1 || got[0] != production {
					t.Errorf("%s (%s) is in %v, %v; want production, protected by its name, %v", what, id, got, err, production)
				}
			}
			if got, err := place.Of(t.Context(), tx, env.Scope); err != nil || len(got) != 1 || got[0] != (place.Place{Project: env.Scope}) {
				t.Errorf("the project is in %v, %v; want itself", got, err)
			}
			servers, err := tx.ListServers(t.Context())
			if err != nil || len(servers) == 0 {
				t.Fatalf("servers %v, %v", servers, err)
			}
			if got, err := place.Of(t.Context(), tx, servers[0].ID); err != nil || len(got) != 1 || got[0] != place.Install {
				t.Errorf("a server is in %v, %v; want the install", got, err)
			}
			if _, err := place.Of(t.Context(), tx, "svc_01NOTHINGHERE0000000000000"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("a service that isn't there: %v, want ErrNotFound", err)
			}
			if _, err := place.Of(t.Context(), tx, "xyz_1"); !errors.Is(err, place.ErrUnknown) {
				t.Errorf("an ID of a kind nothing places: %v, want ErrUnknown", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
