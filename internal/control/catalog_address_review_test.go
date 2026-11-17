// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/opener"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// Review of #779: deleting a catalog app with its domain, as the panel's Delete
// does (route deletions and the service's in one change), plans.
func TestReview779DeletingAnAppWithItsDomain(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	u.applied(u.routeTo("docs", "docs.example.com"))
	_, err := u.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete web", Edits: []*dockwellv1.Edit{
		{Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: u.env, Name: "docs"}, Delete: true},
		{Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: "web"}, Delete: true},
	}}))
	if err != nil {
		t.Fatalf("deleting the app with its domain: %v", err)
	}
}

// Review of #779: a {domain} variable the file doesn't write, as Gitea's ROOT_URL,
// n8n's WEBHOOK_URL, Vaultwarden's DOMAIN and Nextcloud's OVERWRITEPROTOCOL are, is
// set by the plan that routes the first domain, as the panel's addressEdits did.
func TestReview779ADomainSetsAVariableTheFileLacks(t *testing.T) {
	t.Parallel()
	e := addressEntry(1, "1.0", firstRelease, false)
	e = strings.Replace(e, "compose: |", "addressVariables:\n  - service: web\n    variable: ROOT_URL\n    value: \"https://{domain}/\"\n  - service: web\n    variable: OVERWRITEPROTOCOL\n    value: \"https\"\ncompose: |", 1)
	u := startRouting(t, store.SQLite, e)
	u.applied(u.routeTo("docs", "docs.example.com"))
	svc, _ := archSpec(t, u)
	env := svc.GetApp().GetEnv()
	if env["ROOT_URL"] != "https://docs.example.com/" || env["OVERWRITEPROTOCOL"] != "https" {
		t.Errorf("after the first domain, ROOT_URL is %q and OVERWRITEPROTOCOL %q", env["ROOT_URL"], env["OVERWRITEPROTOCOL"])
	}
}

// Review of #779: the Open address written at install is the label of the web
// service the install made, by its ID.
func TestReview779TheOpenAddressIsWebsOwn(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	key, err := opener.LoadKey(filepath.Join(u.f.config.DataDir, "opener.key"))
	if err != nil {
		t.Fatal(err)
	}
	_, r := archSpec(t, u)
	want := "http://" + (&opener.Opener{Key: key}).Label(r.ID) + ".localhost"
	if got := u.url(); got != want {
		t.Errorf("URL is %q, want web's own label %q", got, want)
	}
}

// Review of #779: a value a person set to what Open gives, by hand, follows the
// next domain all the same, as it can't be told apart from Dockwell's (ADR 0103).
func TestReview779AValueSetToTheOpenAddress(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	open := u.url()
	u.applied(u.routeTo("docs", "docs.example.com"))
	svc, _ := archSpec(t, u)
	svc.GetApp().Env["URL"] = open
	u.edit("Keep URL at Open", "web", svc)
	u.applied(u.routeTo("docs", ""))
	u.applied(u.routeTo("other", "other.example.com"))
	if got := u.url(); got != "https://other.example.com" {
		t.Errorf("a person's Open value after the next domain is %q", got)
	}
}

// Review of #779: a domain on a path, a redirect, and a route to another service
// move nothing.
func TestReview779RoutesThatArentTheAddress(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	open := u.url()
	for name, r := range map[string]*resourcev1.Route{
		"path":     {Domain: "p.example.com", Path: "/app", Service: "web", Port: 80},
		"redirect": {Domain: "r.example.com", Redirect: &resourcev1.RouteRedirect{To: "https://example.com"}},
	} {
		ch := propose(t, u.c, "Route "+name, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: u.env, Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: r}},
		})
		if says(ch, "follows the app's address") {
			t.Errorf("%s moves the address: %v", name, ch.GetPlan().GetNotes())
		}
		u.applied(ch)
	}
	if got := u.url(); got != open {
		t.Errorf("URL is %q, want %q", got, open)
	}
}

// Review of #779: an older panel's Add a domain, which sent addressEdits that replace
// {domain} alone, leaves the literal {origin} in the app.
func TestReview779AnOlderPanelsAddADomain(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	svc, _ := archSpec(t, u)
	svc.GetApp().Env["URL"] = strings.ReplaceAll("{origin}", "{domain}", "docs.example.com")
	ch := propose(t, u.c, "Add docs.example.com",
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: u.env, Name: "docs"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "docs.example.com", Service: "web", Port: 80}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: "web"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
		})
	u.applied(ch)
	if got := u.url(); got != "https://docs.example.com" {
		t.Errorf("an older panel's domain leaves URL %q", got)
	}
}

// Review of #779: an app installed again under the name of one deleted before takes
// the deleted web's ID back, so the label the install wrote must be that one's.
func TestReview779AReinstallsOpenAddress(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	_, before := archSpec(t, u)
	gone := propose(t, u.c, "Delete thing", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "thing"}, Delete: true, WithContents: true,
	})
	applyConfirmed(t, u.c, gone)
	again := apply(t, u.c, installApp(t, u.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "thing"}))
	t.Logf("reinstall %s: %v %s", again.GetSummary(), again.GetState(), again.GetError())
	for _, r := range again.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" {
			u.env = r.GetResource().GetScope()
		}
	}
	key, err := opener.LoadKey(filepath.Join(u.f.config.DataDir, "opener.key"))
	if err != nil {
		t.Fatal(err)
	}
	_, r := archSpec(t, u)
	want := "http://" + (&opener.Opener{Key: key}).Label(r.ID) + ".localhost"
	t.Logf("web was %s, is %s", before.ID, r.ID)
	if got := u.url(); got != want {
		t.Errorf("reinstalled, URL is %q, want web's own label %q", got, want)
	}
}
