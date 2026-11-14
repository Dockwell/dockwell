// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"log/slog"
	"maps"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
)

// A step on a server's host before the host is made, as a reboot asked as the
// server joins, starts from the labels the server joined with, as making its host
// would have: none of them is lost (ADR 0091).
func TestAStepOnAHostNotYetMadeKeepsTheJoinLabels(t *testing.T) {
	_, f, db, _ := setupAt(t, store.SQLite, "srv_A")
	kinds := testKinds().With(&host.Driver{})
	e := New(db, kinds, f, secretstest.Keeper(db, kinds), slog.New(slog.DiscardHandler))
	now := time.Now()
	want := map[string]string{"region": "eu", "pool": "web"}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateJoinToken(t.Context(), store.JoinToken{ID: "jtk_A", SecretHash: []byte("x"), CreatedAt: now, ExpiresAt: now.Add(time.Hour), Labels: want}); err != nil {
			return err
		}
		return tx.MarkJoinTokenUsed(t.Context(), "jtk_A", "srv_A", now)
	}); err != nil {
		t.Fatal(err)
	}
	ref := resource.Ref{Kind: "server.host", Scope: "srv_A", Name: "host"}
	edits, err := e.hostsAsIs(t.Context(), []Edit{{Ref: ref, Reboot: true}})
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := edits[0].Spec.(*resourcev1.ServerHost)
	if !ok || !maps.Equal(spec.GetLabels(), want) || edits[0].Base != 0 {
		t.Errorf("the reboot's host is %v at %d, want the labels it joined with", edits[0].Spec, edits[0].Base)
	}
	// A server that joined with no labels starts with none.
	var bare *resourcev1.ServerHost
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		bare, err = NewHost(t.Context(), tx, "srv_B")
		return err
	}); err != nil || len(bare.GetLabels()) != 0 {
		t.Errorf("a server with no join labels starts as %v, %v", bare, err)
	}
}
