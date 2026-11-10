// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Websites are projects: lists keep only the caller's, and changing or deleting
// one is an admin's, as changing or deleting any project is (ADR 0049), by its
// project's name, ID or domain.
func TestWebsiteNamesStayWithinTheirProjects(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
		f.probe = connects
		e := f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
		pullsAnything(e)
		c := changeClient(b, b.origin)
		sites := websiteClient(b, b.origin)
		for _, domain := range []string{"blog.example.com", "shop.example.com"} {
			res, err := sites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
				Domain: domain, Website: toURL("https://backend.example.com"),
			}))
			if err != nil {
				t.Fatal(err)
			}
			applyReviewed(t, c, res.Msg.GetChange())
		}
		all := listWebsites(t, sites)
		if len(all) != 2 {
			t.Fatalf("websites = %v, want two", all)
		}
		blog, shop := all[0], all[1]
		token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		script := websiteClient(bearer{token: token, rt: b.client.Transport}, b.origin)
		hold := aMember(t, f)
		hold(in(blog.GetId(), store.RoleDeveloper))

		for _, client := range []dockwellv1connect.WebsiteServiceClient{sites, script} {
			got := listWebsites(t, client)
			if len(got) != 1 || got[0].GetId() != blog.GetId() {
				t.Errorf("blog's website list = %v, want blog alone", got)
			}
			// Catalog and config previews are pure reads, with no stored project to reach.
			if _, err := client.ListWebsiteRuntimes(t.Context(), connect.NewRequest(&dockwellv1.ListWebsiteRuntimesRequest{})); err != nil {
				t.Errorf("runtime catalog: %v", err)
			}
			if _, err := client.PreviewWebsite(t.Context(), connect.NewRequest(&dockwellv1.PreviewWebsiteRequest{Website: toURL("https://other.example.com")})); err != nil {
				t.Errorf("config preview: %v", err)
			}
			// A website is its project, which is an admin's to change or delete, as
			// the engine holds it: a developer hears that first, whatever they name,
			// so nothing says which websites there are.
			for _, name := range []string{blog.GetId(), blog.GetName(), blog.GetDomain(), shop.GetId(), shop.GetName(), shop.GetDomain(), "absent.example.com", "no-such-project"} {
				_, updated := client.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{
					Id: name, Website: toURL("https://changed.example.com"),
				}))
				needsRole(t, "a developer planning a website's change by "+name, updated)
				if updated != nil && !strings.Contains(updated.Error(), "changing a website changes its project, which only an admin or the owner can do") {
					t.Errorf("the refusal by %s says %q", name, updated)
				}
				_, deleted := client.DeleteWebsite(t.Context(), connect.NewRequest(&dockwellv1.DeleteWebsiteRequest{Id: name}))
				needsRole(t, "a developer planning a website's deletion by "+name, deleted)
			}
		}
		// REST has the same request narrowing and name resolution.
		if code, _, body := callREST(t, b, http.MethodGet, "/v1/websites", nil, ""); code != http.StatusOK || strings.Contains(body, "shop.example.com") || !strings.Contains(body, blog.GetId()) {
			t.Errorf("REST website list = %d %s", code, body)
		}
		for _, name := range []string{blog.GetId(), shop.GetId(), shop.GetName(), shop.GetDomain()} {
			code, reason, body := callREST(t, b, http.MethodPost, "/v1/websites/"+name+":delete", nil, "{}")
			if code != http.StatusForbidden || reason != "needs_role" {
				t.Errorf("REST delete %s = %d %s %s, want needs_role", name, code, reason, body)
			}
		}
		// An admin names a website by its ID, name or domain, and a missing one is not found.
		hold(onInstall(store.RoleAdmin))
		for _, name := range []string{blog.GetId(), blog.GetName(), blog.GetDomain()} {
			if _, err := sites.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: name, Website: toURL("https://changed.example.com")})); err != nil {
				t.Errorf("an admin planning a website's change by %s: %v", name, err)
			}
		}
		_, missing := sites.DeleteWebsite(t.Context(), connect.NewRequest(&dockwellv1.DeleteWebsiteRequest{Id: "absent.example.com"}))
		notFound(t, "a missing website, for an admin", missing)
		hold(onInstall(store.RoleViewer))
		if got := listWebsites(t, sites); len(got) != len(all) {
			t.Errorf("an install viewer reads %d websites, and an admin %d", len(got), len(all))
		}
	})
}
