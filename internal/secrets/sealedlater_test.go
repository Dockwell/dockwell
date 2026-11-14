// SPDX-License-Identifier: AGPL-3.0-only

package secrets_test

import (
	"bytes"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A route stored before routes could hold passwords is in the clear, and reads as
// it is; one in the clear that holds a password's hash wasn't written by Dockwell,
// and is refused, as an unsealed secret is. What reads a route directly opens it the
// same way (store.Tx.Open), sealed or not (ADR 0087).
func TestARouteStoredBeforeItHeldPasswordsReads(t *testing.T) {
	storetest.Each(t, testARouteStoredBeforeItHeldPasswordsReads)
}

func testARouteStoredBeforeItHeldPasswordsReads(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	k := secretstest.Keeper(db, resource.NewRegistry(&route.Driver{}, variable.Type{}, variable.SecretType{}))
	at := store.InRevision("rte_A", string(route.Kind), 1)
	encode := func(r *resourcev1.Route) []byte {
		b, err := proto.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	older := encode(&resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 8080})
	if got, err := openIn(t, db, k, at, older); err != nil || !bytes.Equal(got, older) {
		t.Errorf("a route from before opened as %q, %v", got, err)
	}
	private := encode(&resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 8080, Access: &resourcev1.RouteAccess{
		Users: []*resourcev1.RouteUser{{Name: "ci", PasswordHash: "$2y$10$Q9N3oVQX2b9R5o3b0sVhVeL7m5Gz0mJ8y8dD1p6g9xY2b3c4d5e6f"}},
	}})
	if _, err := openIn(t, db, k, at, private); !errors.Is(err, secrets.ErrUnsealed) {
		t.Errorf("a route in the clear with a password's hash = %v, want ErrUnsealed", err)
	}
	sealed := sealAt(t, db, k, at, private)
	if got, err := openIn(t, db, k, at, sealed); err != nil || !bytes.Equal(got, private) {
		t.Errorf("a sealed private route opened as %q, %v", got, err)
	}
	// A secret, sealed from the start, is refused in the clear, holding a value or not.
	if _, err := openIn(t, db, k, secretAt("sec_A"), []byte{}); !errors.Is(err, secrets.ErrUnsealed) {
		t.Errorf("an empty secret in the clear = %v, want ErrUnsealed", err)
	}
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		for _, stored := range [][]byte{older, sealed} {
			got, err := tx.Open(t.Context(), at, stored)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, older) && !bytes.Equal(got, private) {
				t.Errorf("Tx.Open = %q", got)
			}
		}
		return nil
	}); err != nil {
		t.Errorf("Tx.Open: %v", err)
	}
}
