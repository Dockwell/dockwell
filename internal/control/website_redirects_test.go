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
