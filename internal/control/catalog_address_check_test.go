// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

func gitealike() string {
	e := addressEntry(1, "1.0", firstRelease, false)
	return strings.Replace(e, "compose: |", "addressVariables:\n  - service: web\n    variable: ROOT_URL\n    value: \"https://{domain}/\"\ncompose: |", 1)
}

func (u *upgrading) rootURL() string {
	u.t.Helper()
	svc, _ := archSpec(u.t, u)
	return svc.GetApp().GetEnv()["ROOT_URL"]
}

// Check 779, L2: add A, add B, remove A, remove B; each step's value is Dockwell's.
func TestCheck779TwoDomainsInARow(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	u.applied(u.routeTo("a", "a.example.com"))
	if got := u.url(); got != "https://a.example.com" {
		t.Fatalf("after A, URL %q", got)
	}
	u.applied(u.routeTo("b", "b.example.com"))
	if got := u.url(); got != "https://a.example.com" {
		t.Fatalf("after B beside A, URL %q", got)
	}
	ch := u.routeTo("a", "")
	if says(ch, "holds a value you set") {
		t.Errorf("removing A reads Dockwell's value as a person's")
	}
	u.applied(ch)
	if got := u.url(); got != "https://b.example.com" {
		t.Fatalf("after removing A, URL %q", got)
	}
	u.applied(u.routeTo("b", ""))
	if got := u.url(); !openAddress.MatchString(got) {
		t.Fatalf("after removing B, URL %q", got)
	}
	// And once more, with an upgrade between, which records the address.
	u.applied(u.routeTo("c", "c.example.com"))
	u.release(addressEntry(2, "2.0", secondRelease, true))
	u.applied(u.upgrade())
	if p := u.pinAddress(); p != "https://c.example.com" {
		t.Errorf("upgrade records %q", p)
	}
	u.applied(u.routeTo("c", ""))
	if got := u.url(); !openAddress.MatchString(got) {
		t.Fatalf("after removing C past an upgrade, URL %q", got)
	}
	u.applied(u.routeTo("d", "d.example.com"))
	if got := u.url(); got != "https://d.example.com" {
		t.Fatalf("after D, URL %q", got)
	}
}

// Check 779, H1: a {domain} value, as Gitea's ROOT_URL, after its domain goes and
// another comes in a later change.
func TestCheck779ADomainValueAfterRemoveThenAdd(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, gitealike())
	u.applied(u.routeTo("a", "a.example.com"))
	if got := u.rootURL(); got != "https://a.example.com/" {
		t.Fatalf("after A, ROOT_URL %q", got)
	}
	u.applied(u.routeTo("a", ""))
	if got := u.rootURL(); got != "https://a.example.com/" {
		t.Fatalf("after removing A, ROOT_URL %q (main kept it)", got)
	}
	ch := u.routeTo("b", "b.example.com")
	if says(ch, "holds a value you set") {
		t.Errorf("adding B after A went reads Dockwell's value as a person's: %v", ch.GetPlan().GetNotes())
	}
	u.applied(ch)
	if got := u.rootURL(); got != "https://b.example.com/" {
		t.Errorf("after removing A then adding B, ROOT_URL %q", got)
	}
}

// Check 779, H1: the same swap in one change.
func TestCheck779ADomainValueSwappedInOneChange(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, gitealike())
	u.applied(u.routeTo("a", "a.example.com"))
	u.applied(u.routeTo("b", "b.example.com"))
	u.applied(u.routeTo("a", ""))
	if got := u.rootURL(); got != "https://b.example.com/" {
		t.Errorf("add B then remove A, ROOT_URL %q", got)
	}
}

// Check 779, N5: a value asked for as the entry writes it, with no route in the
// change, as an older panel's Set to sent, is rendered at the app's address; one its
// address can't give is refused, in words.
func TestCheck779AValueAsTheEntryWritesItIsRendered(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, strings.Replace(addressEntry(1, "1.0", firstRelease, true), "addressVariables:\n", "addressVariables:\n  - service: web\n    variable: ROOT_URL\n    value: \"https://{domain}/\"\n", 1))
	svc, _ := archSpec(t, u)
	svc.GetApp().Env["URL"] = "{origin}"
	u.edit("Set URL as the entry writes it", "web", svc)
	if got := u.url(); !openAddress.MatchString(got) {
		t.Errorf("URL asked for as {origin} is %q", got)
	}
	svc, r := archSpec(t, u)
	svc.GetApp().Env["ROOT_URL"] = "https://{domain}/"
	_, err := u.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Set ROOT_URL", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: "web"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}}, BaseRevision: int64(r.Revision),
	}}}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "which its address can't give it now, so nothing was planned. Add a domain first, or give ROOT_URL a value of its own") {
		t.Errorf("ROOT_URL asked for as https://{domain}/ with no domain: %v", err)
	}
}

// listed is the app's install as the catalog lists it.
func (u *upgrading) listed() *dockwellv1.CatalogInstall {
	u.t.Helper()
	res, err := u.cat.ListCatalogInstalls(u.t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		u.t.Fatal(err)
	}
	for _, i := range res.Msg.GetInstalls() {
		if i.GetEnvironmentId() == u.env {
			return i
		}
	}
	return nil
}

// The install says where it's reached and which variables don't hold what that
// gives them, as its page shows them, from the control plane (#779's check, N2).
// A member who can't see its project sees no install at all.
func TestAnInstallSaysItsAddressAndWhatDoesntFollow(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	if i := u.listed(); !openAddress.MatchString(i.GetAddress()) || len(i.GetAddressKept()) != 0 {
		t.Errorf("installed: address %q, kept %v", i.GetAddress(), i.GetAddressKept())
	}
	svc, _ := archSpec(t, u)
	svc.GetApp().Env["URL"] = "https://mine.example.net"
	u.edit("Set URL", "web", svc)
	u.applied(u.routeTo("docs", "docs.example.com"))
	if i := u.listed(); i.GetAddress() != "https://docs.example.com" || len(i.GetAddressKept()) != 1 || i.GetAddressKept()[0] != "web/URL" {
		t.Errorf("with a value set by hand: address %q, kept %v", i.GetAddress(), i.GetAddressKept())
	}
	// Asked for as the entry writes it, it follows again.
	svc, _ = archSpec(t, u)
	svc.GetApp().Env["URL"] = "{origin}"
	u.edit("Set URL to the address", "web", svc)
	if i := u.listed(); len(i.GetAddressKept()) != 0 || u.url() != "https://docs.example.com" {
		t.Errorf("set to the address: URL %q, kept %v", u.url(), i.GetAddressKept())
	}
	hold := aMember(t, u.f)
	hold(store.Grant{ProjectID: envProjectByName(t, u.f, scope.DefaultProject), Role: store.RoleDeveloper})
	if i := u.listed(); i != nil {
		t.Errorf("a member of another project sees the install: %v", i)
	}
}
