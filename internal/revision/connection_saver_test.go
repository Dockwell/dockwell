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

// Whether an install admin saved a connection is Dockwell's to record, from who
// proposes the change, whatever the edit says: a connection saved below one is
// checked with no private address beyond the app's own networks (#710). Below an
// admin, nobody may save one yet, so that side is installAdmin's and StampSaver's
// to show.
func TestWhoSavedAConnectionIsDockwellsToSay(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(connection.Type{})
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	org := tree(t, db).Organization.ID
	ref := resource.Ref{Kind: connection.Kind, Scope: org, Name: "uploads"}
	save := func(claims bool) Edit {
		return Edit{Ref: ref, Spec: &resourcev1.ConnectionSpec{
			DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, SavedBelowAdmin: claims,
			S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: "the-secret"},
		}}
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
	admin := place.WithCaller(t.Context(), place.Caller{UserID: "usr_mira", Grants: []store.Grant{{UserID: "usr_mira", ProjectID: store.Install, Role: store.RoleAdmin}}})

	// An admin's save is an admin's, though the edit claims otherwise.
	p, err := e.Propose(admin, Proposal{Summary: "Add uploads", Actor: "user:usr_mira", Edits: []Edit{save(true)}})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, e, p)
	if s := saved(); s.GetSavedBelowAdmin() || !connection.Private(s) {
		t.Errorf("an admin's connection is saved as %v", s)
	}

	// Dockwell's own change, with no caller, is an admin's.
	apply(t, e, propose(t, e, "Change uploads again", save(true)))
	if s := saved(); s.GetSavedBelowAdmin() {
		t.Errorf("Dockwell's own change saves the connection as %v", s)
	}
}

// An install admin is one with an admin's role on the whole install, or Dockwell
// itself; a project's admin, as a narrowed token, and a developer aren't.
func TestAnInstallAdminIsOneOnTheWholeInstall(t *testing.T) {
	as := func(grants ...store.Grant) bool {
		return installAdmin(place.WithCaller(t.Context(), place.Caller{UserID: "usr_x", Grants: grants}))
	}
	if !installAdmin(t.Context()) {
		t.Error("Dockwell's own change isn't an install admin's")
	}
	if !as(store.Grant{ProjectID: store.Install, Role: store.RoleAdmin}) || !as(store.Grant{ProjectID: store.Install, Role: store.RoleOwner}) {
		t.Error("an admin or the owner of the install isn't an install admin")
	}
	if as(store.Grant{ProjectID: "prj_notes", Role: store.RoleAdmin}) || as(store.Grant{ProjectID: "prj_notes", Role: store.RoleDeveloper}) ||
		as(store.Grant{ProjectID: store.Install, Role: store.RoleDeveloper}) {
		t.Error("a project's admin, or a developer, is an install admin")
	}
}
