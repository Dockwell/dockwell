// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// addressEntry is the test entry with URL, the address it gives browsers, which its
// file writes on loopback and, where follows, follows the app's address (ADR 0103).
func addressEntry(release int, version, image string, follows bool) string {
	e := testEntry(release, version, image, "")
	e = strings.Replace(e, "        PASSWORD: ${PASSWORD}\n", "        PASSWORD: ${PASSWORD}\n        URL: http://localhost:${PORT}\n", 1)
	if follows {
		e = strings.Replace(e, "compose: |", "addressVariables:\n  - service: web\n    variable: URL\n    value: \"{origin}\"\ncompose: |", 1)
	}
	return e
}

// startRouting is startUpgrading on a server whose agent runs routes too, in memory.
func startRouting(t *testing.T, backend store.Backend, entry string) *upgrading {
	t.Helper()
	return startRoutingWith(t, backend, entry, nil)
}

// startRoutingWith is startRouting with the control plane's config as tweak leaves it.
func startRoutingWith(t *testing.T, backend store.Backend, entry string, tweak func(*Config)) *upgrading {
	t.Helper()
	u := &upgrading{t: t, dir: t.TempDir()}
	useCatalogDir(t, u.dir)
	u.release(entry)
	u.f = startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, tweak)
	u.b = signedIn(t, u.f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	u.e = u.f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(u.e)
	u.c = changeClient(u.b, u.b.origin)
	u.cat = catalogClient(u.b)
	apply(t, u.c, installApp(t, u.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "thing"}))
	for _, r := range mustList(t, u.b).GetResources() {
		u.env = r.GetId()
	}
	return u
}

// openAddress is a service's Open address on a panel served on localhost.
var openAddress = regexp.MustCompile(`^http://[a-z2-7]{10}\.localhost$`)

// url is what web's URL holds now.
func (u *upgrading) url() string {
	u.t.Helper()
	svc, _ := archSpec(u.t, u)
	return svc.GetApp().GetEnv()["URL"]
}

// pinAddress is the address the install's pin records.
func (u *upgrading) pinAddress() string {
	u.t.Helper()
	res, err := u.cat.ListCatalogInstalls(u.t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		u.t.Fatal(err)
	}
	for _, i := range res.Msg.GetInstalls() {
		if i.GetEnvironmentId() == u.env {
			return i.GetPin().GetAddress()
		}
	}
	u.t.Fatal("no install")
	return ""
}

// routeTo plans a route of web's whole path at domain, or its deletion where domain
// is "".
func (u *upgrading) routeTo(name, domain string) *dockwellv1.Change {
	u.t.Helper()
	edit := &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: u.env, Name: name}, Delete: domain == ""}
	if domain != "" {
		edit.Spec = &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: domain, Service: "web", Port: 80}}}
	}
	return propose(u.t, u.c, "Route "+name, edit)
}

// says reports a plan note that holds words.
func says(ch *dockwellv1.Change, words string) bool {
	for _, n := range ch.GetPlan().GetNotes() {
		if strings.Contains(n.GetMessage(), words) {
			return true
		}
	}
	return false
}

// An app's address variable is its Open address once installed, follows the first
// domain routed to it, in the plan that routes it, through an upgrade, and goes
// back to the Open address as its last domain goes. Its pin records the address at
// install and upgrade, as a route change leaves the project, an admin's, alone
// (ADR 0103).
func TestAnAppsAddressFollowsItsDomainOrOpen(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testAnAppsAddressFollowsItsDomainOrOpen)
}

func testAnAppsAddressFollowsItsDomainOrOpen(t *testing.T, backend store.Backend) {
	u := startRouting(t, backend, addressEntry(1, "1.0", firstRelease, true))
	open := u.url()
	if !openAddress.MatchString(open) || u.pinAddress() != open {
		t.Fatalf("installed, URL is %q and the pin says %q, want its Open address in both", open, u.pinAddress())
	}

	first := u.routeTo("docs", "docs.example.com")
	if !says(first, "URL follows the app's address to https://docs.example.com, which restarts web") {
		t.Errorf("routing a domain says nothing of URL: %v", first.GetPlan().GetNotes())
	}
	u.applied(first)
	if got := u.url(); got != "https://docs.example.com" || u.pinAddress() != open {
		t.Fatalf("with a domain, URL is %q and the pin says %q", got, u.pinAddress())
	}
	// A second domain moves nothing: the first is the app's address.
	second := u.routeTo("other", "other.example.com")
	if says(second, "follows the app's address") {
		t.Errorf("a second domain moves the address: %v", second.GetPlan().GetNotes())
	}
	u.applied(second)

	// An upgrade writes the release's URL at the address the app has now.
	u.release(addressEntry(2, "2.0", secondRelease, true))
	u.applied(u.upgrade())
	if got := u.url(); got != "https://docs.example.com" || u.pinAddress() != got {
		t.Fatalf("after an upgrade, URL is %q and the pin says %q", got, u.pinAddress())
	}

	// The first domain going leaves the second, and the last going leaves Open.
	u.applied(u.routeTo("docs", ""))
	if got := u.url(); got != "https://other.example.com" {
		t.Errorf("with the first domain gone, URL is %q", got)
	}
	last := u.routeTo("other", "")
	if !says(last, "URL follows the app's address to "+open) {
		t.Errorf("removing the last domain says nothing of URL: %v", last.GetPlan().GetNotes())
	}
	u.applied(last)
	if got := u.url(); got != open {
		t.Errorf("with no domain, URL is %q, want %s", got, open)
	}
}

// A value a person set stays through a domain and an upgrade, and each plan says so.
func TestAnAddressAPersonSetStays(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	svc, _ := archSpec(t, u)
	svc.GetApp().Env["URL"] = "https://mine.example.net"
	u.edit("Set URL", "web", svc)

	routed := u.routeTo("docs", "docs.example.com")
	if !says(routed, "URL holds a value you set") {
		t.Errorf("a domain over a value a person set says nothing: %v", routed.GetPlan().GetNotes())
	}
	u.applied(routed)
	if got := u.url(); got != "https://mine.example.net" {
		t.Fatalf("after a domain, URL is %q", got)
	}
	u.release(addressEntry(2, "2.0", secondRelease, true))
	upgraded := u.upgrade()
	if !says(upgraded, "URL holds a value you set") {
		t.Errorf("an upgrade over a value a person set says nothing: %v", upgraded.GetPlan().GetNotes())
	}
	u.applied(upgraded)
	if got := u.url(); got != "https://mine.example.net" {
		t.Errorf("after an upgrade, URL is %q", got)
	}
}

// An install from before its pin kept an address has the file's value, which is
// Dockwell's, so it follows the first domain; one a person set stays.
func TestAnInstallFromBeforeFollowsItsAddress(t *testing.T) {
	t.Parallel()
	for name, mine := range map[string]string{"the file's value": "", "a person's": "https://mine.example.net"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, false))
			if got := u.url(); got != "http://localhost:18100" || u.pinAddress() != "" {
				t.Fatalf("from before, URL is %q and the pin says %q", got, u.pinAddress())
			}
			if mine != "" {
				svc, _ := archSpec(t, u)
				svc.GetApp().Env["URL"] = mine
				u.edit("Set URL", "web", svc)
			}
			// Its catalog's release now says URL follows the address.
			u.release(addressEntry(1, "1.0", firstRelease, true))
			u.applied(u.routeTo("docs", "docs.example.com"))
			want := "https://docs.example.com"
			if mine != "" {
				want = mine
			}
			if got := u.url(); got != want {
				t.Errorf("after a domain, URL is %q, want %q", got, want)
			}
		})
	}
}

// A developer of the app's project plans a domain with what follows the address,
// and the plan leaves the project, an admin's to change, alone.
func TestADeveloperPlansADomainThatMovesTheAddress(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	hold := aMember(t, u.f)
	hold(store.Grant{ProjectID: envProjectByName(t, u.f, "thing"), Role: store.RoleDeveloper})
	planned := u.routeTo("docs", "docs.example.com")
	if !says(planned, "URL follows the app's address to https://docs.example.com") {
		t.Errorf("a developer's domain says nothing of URL: %v", planned.GetPlan().GetNotes())
	}
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "project" {
			t.Errorf("a developer's domain changes the project: %v", r)
		}
	}
}

// Where the panel is served at its domain, it opens no apps, and offers a tunnel to
// the app's own port instead: an app with no domain is at its port on loopback, which
// that tunnel reaches (#779's review, H2).
func TestAnAppIsOnLoopbackWhereThePanelOpensNone(t *testing.T) {
	t.Parallel()
	u := startRoutingWith(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true), func(c *Config) {
		c.PanelDomain.BehindProxy = "panel.example.com"
	})
	if got := u.url(); got != "http://localhost:18100" || u.pinAddress() != got {
		t.Errorf("with the panel at its domain, URL is %q and the pin says %q", got, u.pinAddress())
	}
	u.applied(u.routeTo("docs", "docs.example.com"))
	last := u.routeTo("docs", "")
	if !says(last, "URL follows the app's address to http://localhost:18100") {
		t.Errorf("its last domain going says nothing of loopback: %v", last.GetPlan().GetNotes())
	}
}

// Once the pin records an address, the file's loopback value is no longer
// Dockwell's: a person who sets it by hand keeps it through a domain and an upgrade
// (#779's review, L2).
func TestTheFilesValueSetByHandStaysOnceThereIsARecord(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, true))
	svc, _ := archSpec(t, u)
	svc.GetApp().Env["URL"] = "http://localhost:18100"
	u.edit("Set URL to its port", "web", svc)
	routed := u.routeTo("docs", "docs.example.com")
	if !says(routed, "URL holds a value you set") {
		t.Errorf("a domain over the file's value set by hand says nothing: %v", routed.GetPlan().GetNotes())
	}
	u.applied(routed)
	u.release(addressEntry(2, "2.0", secondRelease, true))
	u.applied(u.upgrade())
	if got := u.url(); got != "http://localhost:18100" {
		t.Errorf("after a domain and an upgrade, URL is %q", got)
	}
}

// An install from before its pin kept an address, whose file the current reader
// refuses, holds a domain up only where the file alone would tell a variable's
// owner, and says why, what that leaves, and what to do (#779's review, L4).
func TestAnUnreadableFileHoldsUpOnlyWhatItWouldTell(t *testing.T) {
	t.Parallel()
	u := startRouting(t, store.SQLite, addressEntry(1, "1.0", firstRelease, false))
	u.release(addressEntry(1, "1.0", firstRelease, true))
	var found store.Resource
	var spec proto.Message
	if err := u.f.store.View(t.Context(), func(tx *store.Tx) error {
		list, err := tx.ListKind(t.Context(), "project")
		for i := range list {
			if list[i].Name == "thing" {
				found = list[i]
			}
		}
		if err != nil {
			return err
		}
		spec, err = u.f.keeper.Decode(t.Context(), tx, store.InRevision(found.ID, found.Kind, found.Revision), found.Spec)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p := proto.CloneOf(spec.(*resourcev1.Project))
	p.Catalog.File = []byte("services: [")
	// As the catalog wrote it once: only its own write changes a pin's file (ADR
	// 0105, the re-check's N2).
	plan, err := u.f.engine.Propose(t.Context(), revision.Proposal{
		Summary: "A file from before", Actor: revision.SystemActor, Via: revision.ViaAutomatic,
		Edits: []revision.Edit{{Ref: resource.Ref{Kind: "project", Scope: found.Scope, Name: found.Name}, Spec: p, Pin: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.f.engine.Apply(t.Context(), plan.Change); err != nil {
		t.Fatal(err)
	}
	u.f.engine.Wait()
	_, err = u.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Route docs", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: u.env, Name: "docs"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "docs.example.com", Service: "web", Port: 80}}},
	}}}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "thing was installed from a file Dockwell can't read now") || !strings.Contains(err.Error(), "Upgrade thing, which records its address, then plan this again") {
		t.Errorf("a domain over an unreadable file: %v", err)
	}
	// A route that doesn't move the address plans as ever.
	propose(t, u.c, "A path", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: u.env, Name: "api"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "api.example.com", Path: "/api", Service: "web", Port: 80}}},
	})
}
