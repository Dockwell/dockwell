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

// Websites are projects: lists keep only the caller's, and planning by a
// project's name, ID or domain checks the same place before reading settings.
// A name outside their places says exactly what a missing website does.
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
			for _, name := range []string{blog.GetId(), blog.GetName(), blog.GetDomain()} {
				if _, err := client.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{
					Id: name, Website: toURL("https://changed.example.com"),
				})); err != nil {
					t.Errorf("planning their website by %s: %v", name, err)
				}
				if _, err := client.DeleteWebsite(t.Context(), connect.NewRequest(&dockwellv1.DeleteWebsiteRequest{Id: name})); err != nil {
					t.Errorf("planning their website's deletion by %s: %v", name, err)
				}
			}
			_, missing := client.DeleteWebsite(t.Context(), connect.NewRequest(&dockwellv1.DeleteWebsiteRequest{Id: "absent.example.com"}))
			notFound(t, "a missing website", missing)
			for _, name := range []string{shop.GetId(), shop.GetName(), shop.GetDomain(), "prj_01NOTHINGHERE0000000000000", "no-such-project"} {
				_, deleted := client.DeleteWebsite(t.Context(), connect.NewRequest(&dockwellv1.DeleteWebsiteRequest{Id: name}))
				// Invalid settings still get not found before any settings are checked.
				_, updated := client.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{Id: name}))
				for _, err := range []error{deleted, updated} {
					notFound(t, "an unreachable or missing website by "+name, err)
					if err != nil && missing != nil && err.Error() != missing.Error() {
						t.Errorf("%s says %q, and a missing website %q", name, err, missing)
					}
				}
			}
		}
		// REST has the same request narrowing and name resolution.
		if code, _, body := callREST(t, b, http.MethodGet, "/v1/websites", nil, ""); code != http.StatusOK || strings.Contains(body, "shop.example.com") || !strings.Contains(body, blog.GetId()) {
			t.Errorf("REST website list = %d %s", code, body)
		}
		for _, name := range []string{shop.GetId(), shop.GetName(), shop.GetDomain()} {
			code, reason, body := callREST(t, b, http.MethodPost, "/v1/websites/"+name+":delete", nil, "{}")
			if code != http.StatusNotFound || reason != "not_found" {
				t.Errorf("REST delete %s = %d %s %s, want not found", name, code, reason, body)
			}
		}
		hold(in(blog.GetId(), store.RoleDeveloper), in(shop.GetId(), store.RoleViewer))
		for _, name := range []string{shop.GetId(), shop.GetName(), shop.GetDomain()} {
			_, err := sites.DeleteWebsite(t.Context(), connect.NewRequest(&dockwellv1.DeleteWebsiteRequest{Id: name}))
			needsRole(t, "a viewer deleting their website by "+name, err)
		}
		hold(onInstall(store.RoleViewer))
		if got := listWebsites(t, sites); len(got) != len(all) {
			t.Errorf("an install viewer reads %d websites, and an admin %d", len(got), len(all))
		}
	})
}
