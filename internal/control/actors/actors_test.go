// SPDX-License-Identifier: AGPL-3.0-only

package actors

import (
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Names reads each name once, and keeps it across transactions for Fresh,
// so a watch that reads the store every few seconds doesn't read every
// name again each time; it shows a new name once Fresh has passed.
func TestNamesKeepWhatTheyRead(t *testing.T) {
	db, location := storetest.Open(t, store.SQLite)
	ctx := t.Context()
	if err := db.Update(ctx, func(tx *store.Tx) error {
		return tx.CreateServer(ctx, store.Server{ID: "srv_A", Name: "hz-1", CreatedAt: time.Now(), CertSerial: "1", CertNotAfter: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	names := New()
	names.now = func() time.Time { return now }
	read := func() (*dockwellv1.Actor, string) {
		var a *dockwellv1.Actor
		var name string
		_ = db.View(ctx, func(tx *store.Tx) error {
			look := names.In(ctx, tx)
			a, name = look.Actor("server:srv_A"), look.Name("srv_A")
			return nil
		})
		return a, name
	}
	if a, name := read(); a.GetKind() != dockwellv1.ActorKind_ACTOR_KIND_SERVER || a.GetId() != "srv_A" || a.GetName() != "hz-1" || name != "hz-1" {
		t.Errorf("srv_A = %v, %q, want the server hz-1", a, name)
	}
	storetest.Exec(t, location, `UPDATE servers SET name = 'hz-9' WHERE id = 'srv_A'`)
	if a, _ := read(); a.GetName() != "hz-1" {
		t.Errorf("a name read a moment ago = %q, want the one kept, hz-1", a.GetName())
	}
	now = now.Add(Fresh)
	if a, name := read(); a.GetName() != "hz-9" || name != "hz-9" {
		t.Errorf("a name read %v ago = %q, %q, want it read again as hz-9", Fresh, a.GetName(), name)
	}
	_ = db.View(ctx, func(tx *store.Tx) error {
		look := names.In(ctx, tx)
		if a := look.Actor("server:srv_gone"); a.GetName() != "srv_gone" || look.Name("srv_gone") != "" {
			t.Errorf("a server that is gone = %v, want its ID as its name and no name of its own", a)
		}
		return nil
	})
}
