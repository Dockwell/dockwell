// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/website"
)

// A website's other domains redirect to it (ADR 0070): each is a route its list
// writes, permanent and keeping the path, reached as the site's own route is.
// Taking one out deletes only the route the list wrote, never one a person added,
// and making www the main domain swaps the two in one change, which undo puts back.
func TestAWebsitesOtherDomains(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	c := changeClient(b, b.origin)
	sites := websiteClient(b, b.origin)
	at := func(env, name string) *resourcev1.Route {
		return ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: name}]
	}

	withWWW := toURL("https://backend.example.com")
	withWWW.RedirectFrom = []string{" WWW.example.com"}
	res, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "example.com", Website: withWWW}))
	if err != nil {
		t.Fatalf("CreateWebsite: %v", err)
	}
	made := res.Msg.GetChange()
	if order := planned(made); !slices.Contains(order, "route site create") || !slices.Contains(order, "route www-example-com create") {
		t.Fatalf("the plan has %v", order)
	}
	applyReviewed(t, c, made)
	list := listWebsites(t, sites)
	if len(list) != 1 || !slices.Equal(list[0].GetSpec().GetRedirectFrom(), []string{"www.example.com"}) {
		t.Fatalf("ListWebsites = %v", list)
	}
	env := list[0].GetEnvironmentId()
	www := at(env, "www-example-com")
	if www.GetDomain() != "www.example.com" || www.GetRedirect().GetTo() != "https://example.com" || www.GetRedirect().GetTemporary() || www.GetRedirect().GetDropPath() {
		t.Errorf("the server has www as %v", www)
	}
	if list[0].GetDomain() != "example.com" || list[0].GetRouteId() == "" {
		t.Errorf("the website reads %v, as if its redirect gave it its domain", list[0])
	}

	// A redirect a person adds by hand in its environment is theirs.
	promo := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "promo"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "www.example.com", Path: "/sale", Redirect: &resourcev1.RouteRedirect{To: "https://example.com/offers"}}}},
	}
	if got := apply(t, c, propose(t, c, "Send the old sale page on", promo)); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the hand-made redirect: %v", got)
	}

	// The site's domain in its own list, the same domain twice, and a domain another
	// website serves are each refused, saying which.
	for _, from := range [][]string{{"example.com"}, {"old.example.com", "Old.example.com"}} {
		w := toURL("https://backend.example.com")
		w.RedirectFrom = from
		_, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Website: w}))
		if reasonOf(err) != "invalid" {
			t.Errorf("redirect_from %v = %v, want it refused", from, err)
		}
	}
	other, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "shop.example.com", Website: toURL("https://backend.example.com")}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, other.Msg.GetChange())
	taken := toURL("https://backend.example.com")
	taken.RedirectFrom = []string{"shop.example.com"}
	_, err = sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Website: taken}))
	if reasonOf(err) != "domain_taken" || !strings.Contains(err.Error(), "shop.example.com is a website already") {
		t.Errorf("another website's domain = %v", err)
	}
	// One that redirects to another website says so, and not that it's a website.
	shopsOld := toURL("https://backend.example.com")
	shopsOld.RedirectFrom = []string{"old-shop.example.com"}
	moving, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "shop.example.com", Website: shopsOld}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, moving.Msg.GetChange())
	_, err = sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Website: shopsOld}))
	if reasonOf(err) != "domain_taken" || !strings.Contains(err.Error(), "old-shop.example.com redirects to shop.example.com already. Stop sending it there first, or use another domain") {
		t.Errorf("another website's other domain = %v", err)
	}

	// Making www the main domain moves the site's route to it, and sends the bare
	// domain there, in one change.
	swapped := toURL("https://backend.example.com")
	swapped.RedirectFrom = []string{"example.com", "www.example.com"}
	res2, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Domain: "www.example.com", Website: swapped}))
	if err != nil {
		t.Fatalf("making www the main domain: %v", err)
	}
	swap := res2.Msg.GetChange()
	order := planned(swap)
	for _, want := range []string{"route site update", "route example-com create", "route www-example-com delete"} {
		if !slices.Contains(order, want) {
			t.Errorf("making www the main domain plans %v, and no %s", order, want)
		}
	}
	if slices.Contains(order, "route promo delete") {
		t.Errorf("it deletes the redirect a person added: %v", order)
	}
	if want := "Move the website example.com to www.example.com"; swap.GetSummary() != want {
		t.Errorf("summary %q, want %q", swap.GetSummary(), want)
	}
	// Browsers keep www's redirect to example.com, which now sends back: said once.
	reversed := func(ch *dockwellv1.Change) []*dockwellv1.PlanNote {
		var out []*dockwellv1.PlanNote
		for _, n := range ch.GetPlan().GetNotes() {
			if n.GetReason() == "redirect_reversed" {
				out = append(out, n)
			}
		}
		return out
	}
	if got := reversed(swap); len(got) != 1 || !slices.Equal(got[0].GetNames(), []string{"www.example.com", "example.com"}) {
		t.Errorf("the swap notes %v", got)
	}
	applyReviewed(t, c, swap)
	if got := at(env, "site"); got.GetDomain() != "www.example.com" || got.GetService() == "" {
		t.Errorf("the site's route is %v", got)
	}
	if got := at(env, "example-com"); got.GetDomain() != "example.com" || got.GetRedirect().GetTo() != "https://www.example.com" {
		t.Errorf("the bare domain is %v", got)
	}
	if at(env, "www-example-com") != nil || at(env, "promo") == nil {
		t.Errorf("the server routes %v", ing.routes)
	}
	list = listWebsites(t, sites)
	if i := slices.IndexFunc(list, func(w *dockwellv1.WebsiteInfo) bool { return w.GetEnvironmentId() == env }); i < 0 || list[i].GetDomain() != "www.example.com" || !slices.Equal(list[i].GetSpec().GetRedirectFrom(), []string{"example.com"}) {
		t.Errorf("after the swap, ListWebsites = %v", list)
	}

	// Undo puts both back.
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: swap.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	// Undoing it sends www back to the bare domain, which browsers keep sending to www.
	if got := reversed(undo.Msg.GetChange()); len(got) != 1 || !slices.Equal(got[0].GetNames(), []string{"example.com", "www.example.com"}) ||
		!strings.Contains(got[0].GetMessage(), "Browsers keep example.com's earlier redirect to www.example.com for up to a day. Anyone who loaded example.com since goes round in a circle until it expires") {
		t.Errorf("the undo notes %v", got)
	}
	applyReviewed(t, c, undo.Msg.GetChange())
	if at(env, "site").GetDomain() != "example.com" || at(env, "www-example-com").GetRedirect().GetTo() != "https://example.com" || at(env, "example-com") != nil {
		t.Errorf("after undo, the server routes %v", ing.routes)
	}

	// Taking www out deletes its route, and leaves the one a person added.
	res3, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Website: toURL("https://backend.example.com")}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "Change the domains that redirect to example.com"; res3.Msg.GetChange().GetSummary() != want {
		t.Errorf("summary %q, want %q", res3.Msg.GetChange().GetSummary(), want)
	}
	if order := planned(res3.Msg.GetChange()); !slices.Contains(order, "route www-example-com delete") || slices.Contains(order, "route promo delete") {
		t.Errorf("taking www out plans %v", order)
	}
	applyReviewed(t, c, res3.Msg.GetChange())
	if at(env, "www-example-com") != nil || at(env, "promo") == nil {
		t.Errorf("the server routes %v", ing.routes)
	}
}

// A website served over plain HTTP to anyone sends its other domains to http://, and
// their routes are reached as its own is.
func TestAPlainWebsitesRedirectsStayPlain(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	c := changeClient(b, b.origin)
	sites := websiteClient(b, b.origin)
	w := toURL("https://backend.example.com")
	w.RedirectFrom = []string{"www.example.net"}
	res, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "example.net", Website: w, Tls: "off", PlainForAnyone: true}))
	if err != nil {
		t.Fatalf("CreateWebsite: %v", err)
	}
	applyReviewed(t, c, res.Msg.GetChange())
	env := listWebsites(t, sites)[0].GetEnvironmentId()
	got := ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: "www-example-net"}]
	if got.GetRedirect().GetTo() != "http://example.net" || got.GetTls() != "off" || !got.GetPlainForAnyone() {
		t.Errorf("www is %v", got)
	}
}

// A site behind a tunnel says in its plan that each other domain it adds answers only
// once the tunnel sends it here too (ADR 0070); one it had says nothing again.
func TestATunnelledWebsitesOtherDomainSaysToAddIt(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	c := changeClient(b, b.origin)
	sites := websiteClient(b, b.origin)
	w := toURL("https://backend.example.com")
	w.RedirectFrom = []string{"www.example.com"}
	res, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "example.com", Website: w, Tls: "off", TrustedProxies: []string{"127.0.0.1"}}))
	if err != nil {
		t.Fatal(err)
	}
	said := func(ch *dockwellv1.Change) []string {
		var out []string
		for _, n := range ch.GetPlan().GetNotes() {
			if n.GetReason() == "add_to_front" {
				out = append(out, n.GetMessage())
			}
		}
		return out
	}
	if got := said(res.Msg.GetChange()); len(got) != 1 || !strings.Contains(got[0], "www.example.com is behind your tunnel or proxy, as example.com is. It answers only once that sends it here: add www.example.com there too") {
		t.Errorf("the plan notes %q", got)
	}
	applyReviewed(t, c, res.Msg.GetChange())
	w.RedirectFrom = []string{"www.example.com", "old.example.com"}
	up, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Website: w}))
	if err != nil {
		t.Fatal(err)
	}
	if got := said(up.Msg.GetChange()); len(got) != 1 || !strings.Contains(got[0], "old.example.com is behind") {
		t.Errorf("adding old.example.com notes %q", got)
	}
}

// A domain a route beside the website serves, made by a person, is refused as one
// of its other domains in words that say what serves it, never the route's name
// (ADR 0070).
func TestARouteBesideAWebsiteIsSaidInWords(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	c := changeClient(b, b.origin)
	sites := websiteClient(b, b.origin)
	res, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "example.com", Website: toURL("https://backend.example.com")}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, res.Msg.GetChange())
	env := listWebsites(t, sites)[0].GetEnvironmentId()
	mine := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "mine"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "www.example.com", Redirect: &resourcev1.RouteRedirect{To: "https://example.org"}}}},
	}
	if got := apply(t, c, propose(t, c, "A redirect of my own", mine)); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the hand-made redirect: %v", got)
	}
	w := toURL("https://backend.example.com")
	w.RedirectFrom = []string{"www.example.com"}
	_, err = sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Website: w}))
	if reasonOf(err) != "domain_taken" || !strings.Contains(err.Error(), "www.example.com is served by a redirect to example.org beside this website already. Remove it in Domains first, or use another domain") || strings.Contains(err.Error(), "mine") {
		t.Errorf("www beside the site = %v", err)
	}
	_, err = sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Domain: "www.example.com", Website: toURL("https://backend.example.com")}))
	if reasonOf(err) != "domain_taken" || strings.Contains(err.Error(), "mine") {
		t.Errorf("moving the site to www beside it = %v", err)
	}
}

// A person's own edit of a route the site's list wrote stays through the site's next
// update, as an edit of a route does (ADR 0070): made temporary and sent to one page,
// it isn't put back to a permanent redirect that keeps the path.
func TestAnEditOfAListsRedirectStays(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	c := changeClient(b, b.origin)
	sites := websiteClient(b, b.origin)
	w := toURL("https://backend.example.com")
	w.RedirectFrom = []string{"www.example.com"}
	res, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "example.com", Website: w}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, res.Msg.GetChange())
	env := listWebsites(t, sites)[0].GetEnvironmentId()
	edit := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "www-example-com"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{
			Domain:   "www.example.com",
			Redirect: &resourcev1.RouteRedirect{To: "https://example.com/welcome", Temporary: true, DropPath: true},
		}}},
	}
	if got := apply(t, c, propose(t, c, "Send www to the welcome page for now", edit)); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the edit: %v", got)
	}
	moved := toURL("https://other-backend.example.com")
	moved.RedirectFrom = []string{"www.example.com"}
	up, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Website: moved}))
	if err != nil {
		t.Fatal(err)
	}
	if order := planned(up.Msg.GetChange()); slices.Contains(order, "route www-example-com update") {
		t.Errorf("an update of what the site serves plans %v", order)
	}
	applyReviewed(t, c, up.Msg.GetChange())
	got := ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: "www-example-com"}].GetRedirect()
	if got.GetTo() != "https://example.com/welcome" || !got.GetTemporary() || !got.GetDropPath() {
		t.Errorf("www redirects as %v", got)
	}
}

// A website's list written as a project's edit, not through Websites, is checked as
// the API checks it, and can't make a route a person pointed elsewhere the list's to
// delete (ADR 0070).
func TestAListWrittenAsAProjectsEditIsChecked(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	c := changeClient(b, b.origin)
	sites := websiteClient(b, b.origin)
	res, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "example.com", Website: toURL("https://backend.example.com")}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, res.Msg.GetChange())
	site := listWebsites(t, sites)[0]
	project := func(from ...string) *dockwellv1.Edit {
		w := toURL("https://backend.example.com")
		w.RedirectFrom = from
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Scope: "", Name: site.GetName()},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "example.com", Website: w}}},
		}
	}
	// What serves a website changes with its route, so a project's edit names both.
	siteRoute := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: site.GetEnvironmentId(), Name: website.RouteName},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "example.com", Service: website.WebService, Port: website.Port}}},
	}
	for _, bad := range [][]string{{"example.com/x"}, {"www.example.com", "WWW.example.com"}} {
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "A list", Edits: []*dockwellv1.Edit{project(bad...), siteRoute}}))
		if reasonOf(err) != "invalid" || strings.Contains(err.Error(), "changes through Websites") {
			t.Errorf("a project's list %v = %v, want it refused for the list", bad, err)
		}
	}
	mine := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: site.GetEnvironmentId(), Name: "mine"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "www.example.com", Redirect: &resourcev1.RouteRedirect{To: "https://example.org"}}}},
	}
	if got := apply(t, c, propose(t, c, "Mine, and a list naming it", mine, project("www.example.com"), siteRoute)); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the edit: %v", got)
	}
	up, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Website: toURL("https://backend.example.com")}))
	if err != nil {
		t.Fatal(err)
	}
	if order := planned(up.Msg.GetChange()); slices.Contains(order, "route mine delete") {
		t.Errorf("taking www out of the list plans %v", order)
	}
}

// A domain the list names whose redirect a person pointed elsewhere leaves the list
// on the site's next update, which says so, and its redirect stays theirs: no update
// that sends the list back, as the configuration form does, is refused over it, and a
// change of the site's domain goes through too (ADR 0070).
func TestARepointedOtherDomainLeavesTheList(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	c := changeClient(b, b.origin)
	sites := websiteClient(b, b.origin)
	w := toURL("https://backend.example.com")
	w.RedirectFrom = []string{"www.example.com"}
	res, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "example.com", Website: w}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, res.Msg.GetChange())
	env := listWebsites(t, sites)[0].GetEnvironmentId()
	away := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: "www-example-com"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{
			Domain: "www.example.com", Redirect: &resourcev1.RouteRedirect{To: "https://example.org"},
		}}},
	}
	if got := apply(t, c, propose(t, c, "Send www to the other site", away)); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the edit: %v", got)
	}

	// The configuration form sends the stored list back with a new setting.
	again := toURL("https://other-backend.example.com")
	again.RedirectFrom = []string{"www.example.com"}
	up, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Website: again}))
	if err != nil {
		t.Fatalf("an update that sends the list back = %v", err)
	}
	ch := up.Msg.GetChange()
	if order := planned(ch); slices.Contains(order, "route www-example-com update") || slices.Contains(order, "route www-example-com delete") {
		t.Errorf("the update plans %v", order)
	}
	i := slices.IndexFunc(ch.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool { return n.GetReason() == "leaves_list" })
	if i < 0 || !strings.Contains(ch.GetPlan().GetNotes()[i].GetMessage(), "www.example.com redirects to example.org now, so it leaves this website's other domains, and keeps its redirect") {
		t.Errorf("the plan notes %v", ch.GetPlan().GetNotes())
	}
	applyReviewed(t, c, ch)
	if got := listWebsites(t, sites)[0].GetSpec().GetRedirectFrom(); len(got) != 0 {
		t.Errorf("the list is %v", got)
	}
	if got := ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: "www-example-com"}]; got.GetRedirect().GetTo() != "https://example.org" {
		t.Errorf("www redirects as %v", got)
	}
	// And a change of the site's domain goes through, the person's redirect as it is.
	moved, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "example.com", Domain: "example.net", Website: toURL("https://other-backend.example.com")}))
	if err != nil {
		t.Fatalf("changing the domain = %v", err)
	}
	if order := planned(moved.Msg.GetChange()); slices.Contains(order, "route www-example-com update") || slices.Contains(order, "route www-example-com delete") {
		t.Errorf("changing the domain plans %v", order)
	}
}
