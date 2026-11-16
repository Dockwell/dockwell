// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"log/slog"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A connection whose endpoint someone below an admin chose keeps reaching no
// private address when an admin changes only its name (#710 review M1).
func TestAnAdminsRenameKeepsWhoChoseTheEndpoint(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(connection.Type{})
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	org := tree(t, db).Organization.ID
	ref := resource.Ref{Kind: connection.Kind, Scope: org, Name: "uploads"}
	spec := func(name string) *resourcev1.ConnectionSpec {
		return &resourcev1.ConnectionSpec{
			DisplayName: name, Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{Endpoint: "http://10.0.0.5:9000", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: "the-secret"},
		}
	}
	saved := func() *resourcev1.ConnectionSpec {
		t.Helper()
		var out *resourcev1.ConnectionSpec
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			it, err := e.load(t.Context(), tx, ref)
			out, _ = it.current.(*resourcev1.ConnectionSpec)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Stands in for a developer's save, which the engine can't take yet: a record
	// written as one below an admin.
	below := spec("Uploads")
	below.SavedBelowAdmin = true
	apply(t, e, propose(t, e, "A developer's save", Edit{Ref: ref, Spec: below, Reverses: true}))
	if !saved().GetSavedBelowAdmin() {
		t.Fatal("the stand-in didn't record a save below an admin")
	}

	admin := place.WithCaller(t.Context(), place.Caller{UserID: "usr_mira", Grants: []store.Grant{{UserID: "usr_mira", ProjectID: store.Install, Role: store.RoleAdmin}}})
	// The admin renames it, keeping the key and the endpoint the developer chose.
	rename := spec("Uploads (team)")
	rename.S3.AccessKeyId, rename.S3.SecretKey = "", ""
	p, err := e.Propose(admin, Proposal{Summary: "Rename uploads", Actor: "user:usr_mira", Edits: []Edit{{Ref: ref, Spec: rename}}})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, e, p)
	if s := saved(); connection.Private(s) {
		t.Errorf("an admin's rename gave the developer's endpoint %s private reach", s.GetS3().GetEndpoint())
	}

	// A revision put back as it was recorded, as a rollback does, keeps its record,
	// though an admin asks for it.
	restore := spec("Uploads")
	restore.SavedBelowAdmin = true
	p, err = e.Propose(admin, Proposal{Summary: "Roll uploads back", Actor: "user:usr_mira", Edits: []Edit{{Ref: ref, Spec: restore, Restores: true}}})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, e, p)
	if s := saved(); connection.Private(s) {
		t.Error("an admin's rollback to a developer's revision gave it private reach")
	}

	// Once the admin moves it to an address of their own, it's theirs.
	moved := spec("Uploads")
	moved.S3.Endpoint = "http://10.0.0.6:9000"
	p, err = e.Propose(admin, Proposal{Summary: "Move uploads", Actor: "user:usr_mira", Edits: []Edit{{Ref: ref, Spec: moved}}})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, e, p)
	if s := saved(); !connection.Private(s) {
		t.Error("an admin's own endpoint reaches no private address")
	}
}
