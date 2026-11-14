// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// The catalog's installed filter and its featured order count only the installs in
// projects the reader holds, so a developer of one project learns nothing of what
// another runs; an install admin counts every one (the review of #621, M1).
func TestTheCatalogCountsOnlyInstallsTheReaderHolds(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	for _, id := range []string{"memos", "linkding"} {
		if applied := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: id})); applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("installing %s ended %v: %s", id, applied.GetState(), applied.GetError())
		}
	}
	installs, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var memos string
	for _, i := range installs.Msg.GetInstalls() {
		if i.GetPin().GetEntry() == "memos" {
			memos = i.GetProjectId()
		}
	}
	if memos == "" {
		t.Fatalf("no install of memos in %v", installs.Msg.GetInstalls())
	}
	installed := func() []string {
		t.Helper()
		res, err := cat.ListCatalogEntries(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogEntriesRequest{Installed: true}))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, entry := range res.Msg.GetEntries() {
			out = append(out, entry.GetId())
		}
		return out
	}
	if got := installed(); !slices.Equal(got, []string{"linkding", "memos"}) {
		t.Errorf("the owner's installed apps are %v", got)
	}
	hold := aMember(t, f)
	hold(store.Grant{ProjectID: memos, Role: store.RoleDeveloper})
	if got := installed(); !slices.Equal(got, []string{"memos"}) {
		t.Errorf("memos' developer's installed apps are %v, want memos alone", got)
	}
	// Featured, past the list Dockwell holds, what's installed most comes first: here
	// only what the reader holds counts, so linkding ranks as if nothing ran it.
	res, err := cat.ListCatalogEntries(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogEntriesRequest{Order: dockwellv1.CatalogOrder_CATALOG_ORDER_FEATURED, Ids: []string{"linkding", "adminer"}}))
	if err != nil {
		t.Fatal(err)
	}
	var featured []string
	for _, entry := range res.Msg.GetEntries() {
		featured = append(featured, entry.GetId())
	}
	if !slices.Equal(featured, []string{"adminer", "linkding"}) {
		t.Errorf("featured for memos' developer: %v, want adminer first by name, as linkding's install is out of sight", featured)
	}
	hold(onInstall(store.RoleAdmin))
	if got := installed(); !slices.Equal(got, []string{"linkding", "memos"}) {
		t.Errorf("an admin's installed apps are %v", got)
	}
}
