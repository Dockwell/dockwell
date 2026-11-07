// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
	"github.com/dockwell/dockwell/internal/website"
)

func websiteClient(c connect.HTTPClient, origin string) dockwellv1connect.WebsiteServiceClient {
	return dockwellv1connect.NewWebsiteServiceClient(c, origin, connect.WithProtoJSON())
}

func toURL(url string) *resourcev1.Website {
	return &resourcev1.Website{Serves: &resourcev1.Website_Proxy{Proxy: &resourcev1.WebsiteProxy{Url: url}}}
}

// onPort3000 sends a website to port 3000 on the server.
func onPort3000() *resourcev1.Website {
	return &resourcev1.Website{Serves: &resourcev1.Website_Proxy{Proxy: &resourcev1.WebsiteProxy{Port: 3000}}}
}

// applyReviewed applies a change with the fingerprint of the plan, as the panel
// does, which a plan with host access needs.
func applyReviewed(t *testing.T, c dockwellv1connect.ChangeServiceClient, ch *dockwellv1.Change) {
	t.Helper()
	w := watch(t, c, ch.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err != nil {
		t.Fatalf("ApplyChange %s: %v", ch.GetSummary(), err)
	}
	got := waitWatch(t, w).last()
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("%s ended %v: %s", ch.GetSummary(), got.GetState(), got.GetError())
	}
}

func listWebsites(t *testing.T, c dockwellv1connect.WebsiteServiceClient) []*dockwellv1.WebsiteInfo {
	t.Helper()
	res, err := c.ListWebsites(t.Context(), connect.NewRequest(&dockwellv1.ListWebsitesRequest{}))
	if err != nil {
		t.Fatalf("ListWebsites: %v", err)
	}
	return res.Msg.GetWebsites()
}

// connects is a probe for a fake engine's containers, which listen nowhere: each
// check passes, as a web server that started would.
func connects(context.Context, *health.Check, string) (health.Result, error) {
	return health.Result{Passed: true, Output: "connected", At: time.Now()}, nil
}

func planned(ch *dockwellv1.Change) []string {
	var out []string
	for _, r := range ch.GetPlan().GetResources() {
		out = append(out, r.GetResource().GetKind()+" "+r.GetResource().GetName()+" "+strings.ToLower(strings.TrimPrefix(r.GetAction().String(), "PLAN_ACTION_")))
	}
	return out
}

// A website that sends to a URL is a project of its own, with the web server and a
// route to it, made through one plan that reaches nothing a token can't. It moves to
// a port on the server, which a person approves and which needs no web server, and
// back with undo. Deleting it takes all of it, and undo brings it all back.
func TestAWebsiteFromAPlanToItsUndo(t *testing.T) { storetest.Each(t, testAWebsite) }

func testAWebsite(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	c := changeClient(b, b.origin)
	sites := websiteClient(b, b.origin)
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := websiteClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	scriptChanges := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)

	if got := listWebsites(t, sites); len(got) != 0 {
		t.Fatalf("a new install has websites: %v", got)
	}

	// A token can make one that sends to a URL: it reaches what any container does.
	res, err := script.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
		Domain: "Blog.Example.com", Website: toURL("https://backend.example.com/blog/"),
	}))
	if err != nil {
		t.Fatalf("CreateWebsite: %v", err)
	}
	made := res.Msg.GetChange()
	if want := "Add the website blog.example.com"; made.GetSummary() != want {
		t.Errorf("summary %q, want %q", made.GetSummary(), want)
	}
	order := planned(made)
	for _, want := range []string{"project blog-example-com create", "environment production create", "service web create", "route site create"} {
		if !slices.Contains(order, want) {
			t.Errorf("the plan has %v, and no %s", order, want)
		}
	}
	if slices.Index(order, "service web create") > slices.Index(order, "route site create") {
		t.Errorf("planned %v: the route before what it sends to", order)
	}
	if made.GetPlan().GetHostAccess() || made.GetPlan().GetDestructive() {
		t.Errorf("host access %t, destructive %t, for a website that sends to a URL", made.GetPlan().GetHostAccess(), made.GetPlan().GetDestructive())
	}
	applyReviewed(t, scriptChanges, made)

	var web *docker.Container
	for _, name := range e.Containers() {
		if ctr := e.Container(name); ctr != nil && ctr.Config.Image == website.Image {
			web = ctr
		}
	}
	if web == nil {
		t.Fatalf("no web server runs: %v", e.Containers())
	}
	if web.Config.User != "65532" || !slices.Contains(web.HostConfig.CapDrop, "NET_RAW") || slices.Contains(web.HostConfig.CapDrop, "NET_BIND_SERVICE") {
		t.Errorf("the web server runs as %q, dropping %v", web.Config.User, web.HostConfig.CapDrop)
	}
	if config := strings.Join(web.Config.Cmd, " "); !strings.Contains(config, "reverse_proxy https://backend.example.com") || !strings.Contains(config, "rewrite * /blog{uri}") {
		t.Errorf("the web server's config is %q", config)
	}

	list := listWebsites(t, sites)
	if len(list) != 1 {
		t.Fatalf("ListWebsites = %v, want the one", list)
	}
	site := list[0]
	if site.GetDomain() != "blog.example.com" || site.GetName() != "blog-example-com" || site.GetServiceId() == "" || site.GetRouteId() == "" || site.GetServerName() != "hz-1" || site.GetUpdateTime() == nil {
		t.Errorf("the website reads %v", site)
	}
	if site.GetSpec().GetProxy().GetUrl() != "https://backend.example.com/blog" {
		t.Errorf("the website sends to %q, want the URL as it's used", site.GetSpec().GetProxy().GetUrl())
	}
	rt := ing.routes[resource.Ref{Kind: route.Kind, Scope: site.GetEnvironmentId(), Name: website.RouteName}]
	if rt.GetService() != website.WebService || rt.GetPort() != website.Port || rt.GetTls() != "" {
		t.Errorf("the server's route is %v", rt)
	}

	// A port on the server reaches past containers: a token plans it, and only a
	// person applies it.
	moved, err := script.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "blog.example.com", Website: onPort3000()}))
	if err != nil {
		t.Fatalf("UpdateWebsite: %v", err)
	}
	ch := moved.Msg.GetChange()
	if !ch.GetPlan().GetHostAccess() {
		t.Errorf("a website on the server's port plans no host access: %v", planned(ch))
	}
	if order := planned(ch); !slices.Contains(order, "service web delete") || !slices.Contains(order, "route site update") {
		t.Errorf("moving to a port plans %v", order)
	}
	_, err = scriptChanges.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}))
	if reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("a token applying a port on the server = %v, want needs_session", err)
	}
	applyReviewed(t, c, ch)
	rt = ing.routes[resource.Ref{Kind: route.Kind, Scope: site.GetEnvironmentId(), Name: website.RouteName}]
	if rt.GetHostPort() != 3000 || rt.GetService() != "" || rt.GetDomain() != "blog.example.com" {
		t.Errorf("the server's route is %v, want port 3000", rt)
	}
	if got := listWebsites(t, sites); len(got) != 1 || got[0].GetServiceId() != "" || got[0].GetSpec().GetProxy().GetPort() != 3000 {
		t.Errorf("after the move, ListWebsites = %v", got)
	}

	// The same again is nothing to plan.
	_, err = sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "blog-example-com", Website: onPort3000()}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "nothing to change") {
		t.Errorf("an update to what it serves already = %v, want it refused as nothing to change", err)
	}

	// Undo brings the web server back, sending to the URL.
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: ch.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, undo.Msg.GetChange())
	if got := listWebsites(t, sites); len(got) != 1 || got[0].GetServiceId() == "" || got[0].GetSpec().GetProxy().GetUrl() == "" {
		t.Errorf("after undo, ListWebsites = %v", got)
	}

	// Deleting it takes the route, the web server and the project, environment and all.
	gone, err := sites.DeleteWebsite(t.Context(), connect.NewRequest(&dockwellv1.DeleteWebsiteRequest{Id: site.GetId()}))
	if err != nil {
		t.Fatalf("DeleteWebsite: %v", err)
	}
	del := gone.Msg.GetChange()
	order = planned(del)
	for _, want := range []string{"route site delete", "service web delete", "project blog-example-com delete", "environment production delete"} {
		if !slices.Contains(order, want) {
			t.Errorf("deleting plans %v, and no %s", order, want)
		}
	}
	applyReviewed(t, c, del)
	if got := listWebsites(t, sites); len(got) != 0 {
		t.Errorf("after deleting, ListWebsites = %v", got)
	}
	if len(ing.routes) != 0 {
		t.Errorf("the server still routes %v", ing.routes)
	}
	back, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: del.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, back.Msg.GetChange())
	if got := listWebsites(t, sites); len(got) != 1 || got[0].GetDomain() != "blog.example.com" {
		t.Errorf("after undoing the delete, ListWebsites = %v", got)
	}
}

// What can't be served is refused before anything is planned, in words that say what
// to give, and a name that's taken goes to the next one free.
func TestWhatAWebsiteRefuses(t *testing.T) { storetest.Each(t, testWhatAWebsiteRefuses) }

func testWhatAWebsiteRefuses(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	c := changeClient(b, b.origin)
	sites := websiteClient(b, b.origin)

	for _, tc := range []struct {
		req  *dockwellv1.CreateWebsiteRequest
		says string
	}{
		{&dockwellv1.CreateWebsiteRequest{Domain: "blog.example.com", Website: toURL("backend.example.com")}, "no http:// or https://"},
		{&dockwellv1.CreateWebsiteRequest{Domain: "blog.example.com", Website: toURL("http://127.0.0.1:3000")}, "give the port in place of a URL"},
		{&dockwellv1.CreateWebsiteRequest{Domain: "blog.example.com"}, "needs to say what it serves"},
		{&dockwellv1.CreateWebsiteRequest{Domain: "blog", Website: onPort3000()}, "needs a dot"},
		{&dockwellv1.CreateWebsiteRequest{Website: onPort3000()}, "needs a domain"},
	} {
		_, err := sites.CreateWebsite(t.Context(), connect.NewRequest(tc.req))
		if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("CreateWebsite(%v) = %v, want invalid, saying %q", tc.req, err, tc.says)
		}
	}
	if _, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "nothing", Website: onPort3000()})); reasonOf(err) != "not_found" {
		t.Errorf("updating a website that isn't there = %v, want not_found", err)
	}

	first, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "shop.example.com", Website: toURL("https://backend.example.com")}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, first.Msg.GetChange())
	// Another website at the same domain is the route's claim to refuse.
	again, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "shop.example.com", Website: toURL("https://other.example.com")}))
	if err == nil {
		t.Errorf("a second website at shop.example.com planned %v", planned(again.Msg.GetChange()))
	} else if !strings.Contains(err.Error(), "shop.example.com") {
		t.Errorf("a second website at shop.example.com = %v, want it named", err)
	}
	// Behind a tunnel, which a person approves, and a change to what it serves keeps
	// what's in front of its domain as it is.
	tunnel, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
		Domain: "docs.example.com", Website: toURL("https://backend.example.com"), Tls: "off", TrustedProxies: []string{"127.0.0.1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !tunnel.Msg.GetChange().GetPlan().GetHostAccess() {
		t.Errorf("believing a tunnel plans no host access")
	}
	applyReviewed(t, c, tunnel.Msg.GetChange())
	moved, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: "docs.example.com", Website: toURL("https://docs-backend.example.com")}))
	if err != nil {
		t.Fatal(err)
	}
	if moved.Msg.GetChange().GetPlan().GetHostAccess() {
		t.Errorf("another URL behind the same tunnel plans host access: %v", planned(moved.Msg.GetChange()))
	}
	applyReviewed(t, c, moved.Msg.GetChange())
	for ref, rt := range ing.routes {
		if rt.GetDomain() == "docs.example.com" && (ref.Name != website.RouteName || rt.GetTls() != "off" || !slices.Equal(rt.GetTrustedProxies(), []string{"127.0.0.1/32"})) {
			t.Errorf("after the change, the tunnel's route is %s %v", ref.Name, rt)
		}
	}

	// A preview says what the web server runs, as it would be kept, and refuses alike.
	preview, err := sites.PreviewWebsite(t.Context(), connect.NewRequest(&dockwellv1.PreviewWebsiteRequest{Website: toURL("HTTPS://Backend.example.com/app/")}))
	if err != nil {
		t.Fatal(err)
	}
	if got := preview.Msg; got.GetWebsite().GetProxy().GetUrl() != "https://backend.example.com/app" || !strings.Contains(got.GetConfig(), "rewrite * /app{uri}") {
		t.Errorf("PreviewWebsite = %v", got)
	}
	if got, err := sites.PreviewWebsite(t.Context(), connect.NewRequest(&dockwellv1.PreviewWebsiteRequest{Website: onPort3000()})); err != nil || got.Msg.GetConfig() != "" {
		t.Errorf("PreviewWebsite of a port = %v, %v, want no config", got, err)
	}
	if _, err := sites.PreviewWebsite(t.Context(), connect.NewRequest(&dockwellv1.PreviewWebsiteRequest{Website: toURL("http://localhost")})); reasonOf(err) != "invalid" {
		t.Errorf("PreviewWebsite of localhost = %v, want invalid", err)
	}

	// One whose name comes out the same gets the next.
	other, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{Domain: "shop-example.com", Website: toURL("https://backend.example.com")}))
	if err != nil {
		t.Fatal(err)
	}
	if order := planned(other.Msg.GetChange()); !slices.Contains(order, "project shop-example-com-2 create") {
		t.Errorf("the second website's plan is %v, want project shop-example-com-2", order)
	}
}

// A website that serves a folder mounts it into its web server read-only, which a
// person approves, as any server path is, and a token can't.
func TestAFolderWebsiteNeedsAPerson(t *testing.T) { storetest.Each(t, testAFolderWebsite) }

func testAFolderWebsite(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	pullsAnything(e)
	c := changeClient(b, b.origin)
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := websiteClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	scriptChanges := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	root := t.TempDir()

	res, err := script.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
		Domain:  "docs.example.com",
		Website: &resourcev1.Website{Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: root + "/"}}},
	}))
	if err != nil {
		t.Fatalf("CreateWebsite: %v", err)
	}
	ch := res.Msg.GetChange()
	if !ch.GetPlan().GetHostAccess() {
		t.Fatalf("a folder plans no host access: %v", planned(ch))
	}
	var named bool
	for _, r := range ch.GetPlan().GetResources() {
		for _, a := range r.GetHostAccess() {
			named = named || (a.GetPath() == root && a.GetReadOnly())
		}
	}
	if !named {
		t.Errorf("the plan doesn't name %s, read-only: %v", root, ch.GetPlan().GetResources())
	}
	_, err = scriptChanges.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}))
	if reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("a token applying a folder = %v, want needs_session", err)
	}
	applyReviewed(t, c, ch)

	var web *docker.Container
	for _, name := range e.Containers() {
		if ctr := e.Container(name); ctr != nil && ctr.Config.Image == website.Image {
			web = ctr
		}
	}
	if web == nil {
		t.Fatalf("no web server runs: %v", e.Containers())
	}
	if want := root + ":" + website.FolderTarget + ":ro"; !slices.Contains(web.HostConfig.Binds, want) {
		t.Errorf("the web server binds %v, want %s", web.HostConfig.Binds, want)
	}
	if config := strings.Join(web.Config.Cmd, " "); !strings.Contains(config, "file_server") {
		t.Errorf("the web server's config is %q", config)
	}
	list := listWebsites(t, websiteClient(b, b.origin))
	if len(list) != 1 || list[0].GetSpec().GetFolder().GetRoot() != root {
		t.Errorf("ListWebsites = %v, want the folder as it was kept", list)
	}
}
