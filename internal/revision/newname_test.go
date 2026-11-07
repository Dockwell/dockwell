// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A container run by hand can't be made anew with the name of a service's
// container, shop.production.web-1, but one stored with such a name, from
// before that was a service's form, goes on being edited, as checkName runs on
// every edit (ADR 0035). A name with dots of another shape is anyone's.
func TestAServicesFormIsRefusedOnlyToANewContainer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		at := func(name, image string) Edit {
			return Edit{Ref: resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: name}, Spec: &resourcev1.Container{Image: image}, Servers: []string{"srv_A"}}
		}
		refuseProposal(t, e, "a new container of a service's form", ErrInvalid, `"shop.production.web-1" has the form of a service's container`, at("shop.production.web-1", "nginx:1.27"))
		for _, name := range []string{"blog.example.com", "my.app.v2", "nginx.1.27"} {
			if _, err := e.Propose(t.Context(), Proposal{Summary: "Run " + name, Actor: "usr_test", Edits: []Edit{at(name, "nginx:1.27")}}); err != nil {
				t.Errorf("a new container named %s: %v", name, err)
			}
		}

		// One stored before the form was a service's.
		seed := propose(t, e, "Open the firewall", rule("srv_A", "http", 80))
		apply(t, e, seed)
		spec, err := resource.Encode(&resourcev1.Container{Image: "nginx:1.27"})
		if err != nil {
			t.Fatal(err)
		}
		rid := id.New(id.Resource)
		if err := db.Revise(t.Context(), func(tx *store.RevisionTx) error {
			sealed, err := e.sealer.Seal(t.Context(), tx.Tx, store.InRevision(rid, string(container.Kind), 1), spec)
			if err != nil {
				return err
			}
			_, err = tx.AddRevision(t.Context(), store.NewRevision{
				Kind: string(container.Kind), Scope: "srv_A", Name: "shop.production.web-1", ID: rid, Number: 1, Spec: sealed, Change: seed.Change, Position: 99, At: time.Now(),
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Propose(t.Context(), Proposal{Summary: "Update it", Actor: "usr_test", Edits: []Edit{at("shop.production.web-1", "nginx:1.28")}}); err != nil {
			t.Errorf("editing a container stored as shop.production.web-1: %v", err)
		}
	})
}
