// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An install that asks for a route plans it in the same change, to the app's web
// service at the port its container serves, held as the install is, and the app's
// address follows its domain; a redirect, a port on the server, or a service of its
// own is refused (#792).
func TestAnInstallsRouteIsPlannedWithIt(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		// An app whose links follow its address, which a route's domain then is.
		top := ownerTop + "addressVariables:\n  - service: web\n    variable: BASE_URL\n    value: \"https://{domain}/\"\n"
		o := startOwning(t, backend, ownedEntry(1, "1.0", firstRelease, ownerInputs, top), nil)
		for name, bad := range map[string]*resourcev1.Route{
			"a redirect":         {Domain: "thing.example.com", Redirect: &resourcev1.RouteRedirect{To: "https://example.com"}},
			"a port on the host": {Domain: "thing.example.com", HostPort: 18100},
			"its own service":    {Domain: "thing.example.com", Service: "db", Port: 5432},
			"no domain":          {},
		} {
			if _, err := o.cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "thing", Route: bad})); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("%s: %v", name, err)
			}
		}
		o.applied(installApp(t, o.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "thing", Route: &resourcev1.Route{Domain: "Thing.Example.com"}}))
		o.env = o.install().GetEnvironmentId()
		r := o.route("thing-example-com")
		if r.GetDomain() != "thing.example.com" || r.GetService() != "web" || r.GetPort() != 80 {
			t.Fatalf("the install's route is %v", r)
		}
		if !r.GetHeld() {
			t.Error("the route of an install held until its owner is made isn't held")
		}
		if got := o.install().GetPin().GetAddress(); got != "https://thing.example.com" {
			t.Errorf("the app's address is %q, want its domain's", got)
		}
	})
}

// An install's route that takes over a record made in Cloudflare is an admin's to
// apply with a passkey, as any route's take-over is: the install's change says so,
// and has the DNS provider serve its new project (#792).
func TestAnInstallsRouteTakingOverARecordTakesAPasskey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop)), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newPublishStand(t)
	id := s.cf.Put("example.com", cftest.Record{Type: "A", Name: "thing.example.com", Content: "203.0.113.9", TTL: 300, Comment: "by hand"})
	cat := catalogClient(s.b)
	res, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "thing", Route: &resourcev1.Route{
		Domain: "thing.example.com", Tls: "off", TrustedProxies: []string{"127.0.0.1"},
		Publish: &resourcev1.RoutePublish{
			Way: resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL, Tunnel: webOne, Provider: "cloudflare",
			TakesOver: []*resourcev1.TakeOver{{RecordId: id}},
		},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	ch := res.Msg.GetChange()
	resources := ch.GetPlan().GetResources()
	if !slices.ContainsFunc(resources, func(r *dockwellv1.PlannedResource) bool {
		return slices.ContainsFunc(r.GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
			return a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_CLOUDFLARE_TAKEOVER
		})
	}) {
		t.Fatal("an install's route replacing someone else's record isn't a take-over")
	}
	if !slices.ContainsFunc(resources, func(r *dockwellv1.PlannedResource) bool {
		return strings.HasPrefix(r.GetResource().GetKind(), "dns")
	}) {
		t.Errorf("the install's change doesn't have the DNS provider serve its project: %v", resources)
	}
	if _, err := s.script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err == nil {
		t.Error("a token applied an install that takes over a record at Cloudflare")
	}
	if _, err := s.person.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("a person applied a take-over without a passkey: %v", err)
	}
	// Nothing was written at Cloudflare, as nothing applied.
	if got := s.cf.Records("example.com"); len(got) != 1 || got[0].Type != "A" {
		t.Errorf("an install not applied changed Cloudflare: %+v", got)
	}
}
