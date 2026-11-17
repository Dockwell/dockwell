// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/publish"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// throughWebOne is a website's publish through the stand's tunnel.
func throughWebOne() *resourcev1.RoutePublish {
	return &resourcev1.RoutePublish{Way: resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL, Tunnel: webOne, Provider: "cloudflare"}
}

func recordNames(s *publishStand) []string {
	var out []string
	for _, r := range s.cf.Records("example.com") {
		out = append(out, r.Name)
	}
	slices.Sort(out)
	return out
}

// A new website published through the server's tunnel: its domain and the name that
// redirects to it each get their record, and the provider, which didn't serve the
// new website's project, serves it from the same change, shown as its own step
// (ADR 0104, the lead's decision on websites).
func TestANewWebsitePublishedThroughTheTunnel(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		site := onPort3000()
		site.RedirectFrom = []string{"help.example.com"}
		made, err := websiteClient(s.b, s.b.origin).CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
			Domain: "docs.example.com", Website: site, Tls: "off", TrustedProxies: []string{"127.0.0.1"}, Publish: throughWebOne(),
		}))
		if err != nil {
			t.Fatal(err)
		}
		ch := made.Msg.GetChange()
		if !slices.ContainsFunc(ch.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
			return r.GetResource().GetKind() == "dns.provider" && r.GetResource().GetName() == "cloudflare"
		}) {
			t.Fatalf("the provider should serve the new website's project in the same plan: %v", ch.GetPlan().GetResources())
		}
		creates := stepsOf(ch, dockwellv1.ProviderAction_PROVIDER_ACTION_CREATE_RECORD)
		if len(creates) != 2 {
			t.Fatalf("a record for the domain and for the name that redirects to it: %v", providerSteps(ch))
		}
		s.applied(t, ch)
		if got := recordNames(s); !slices.Equal(got, []string{"docs.example.com", "help.example.com"}) {
			t.Fatalf("records after: %v", got)
		}

		// Its domain moves, and a name is added, published the same way.
		site.RedirectFrom = []string{"help.example.com", "faq.example.com"}
		moved, err := websiteClient(s.b, s.b.origin).UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{
			Id: ch.GetPlan().GetResources()[0].GetResource().GetName(), Website: site, Domain: "manual.example.com", Publish: throughWebOne(),
		}))
		if err != nil {
			t.Fatal(err)
		}
		s.applied(t, moved.Msg.GetChange())
		if got := recordNames(s); !slices.Equal(got, []string{"faq.example.com", "help.example.com", "manual.example.com"}) {
			t.Fatalf("records after the move: %v", got)
		}
	})
}

// A new website has no project yet, so an admin's check that asks for a way reads
// every provider's zones, as the plan adds the project to the provider; one that
// asks for nothing, as Domains' is, reads DNS alone, and a developer's says nothing
// of Cloudflare (ADR 0104, the review's B-M2).
func TestANewWebsitesNameIsCheckedInEveryZoneForAnAdmin(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	check := func(publish *resourcev1.RoutePublish) *dockwellv1.CloudflareCheck {
		t.Helper()
		got, err := dc.CheckDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: s.server, Domain: "docs.example.com", Publish: publish}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.GetCloudflare()
	}
	if cf := check(&resourcev1.RoutePublish{}); cf.GetZone() != "example.com" || cf.GetProvider() != "cloudflare" || cf.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_FREE {
		t.Fatalf("an admin's check for a new website: %v", cf)
	}
	before := len(s.cf.Calls())
	if cf := check(nil); cf != nil {
		t.Fatalf("a check that asks for no way reads Cloudflare: %v", cf)
	}
	if n := len(s.cf.Calls()) - before; n != 0 {
		t.Fatalf("a check that asks for no way called Cloudflare %d times", n)
	}
	hold := aMember(t, s.f)
	hold(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
	if cf := check(&resourcev1.RoutePublish{}); cf != nil {
		t.Fatalf("a developer's check with no project reads Cloudflare: %v", cf)
	}
}

// A new website through Cloudflare's proxy, as the check offers for a server with a
// public address and no tunnel: its record is a proxied A to the server, and its
// route proves its domain by DNS, through the provider (the review's B-H1).
func TestANewWebsitePublishedThroughTheProxy(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.f.server.publisher.Server = func(context.Context, string) (publish.Facts, error) {
			return publish.Facts{Name: "hz-1", Public: []netip.Addr{netip.MustParseAddr("203.0.113.10")}, WaysOut: []string{"203.0.113.10"}}, nil
		}
		made, err := websiteClient(s.b, s.b.origin).CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
			Domain: "docs.example.com", Website: onPort3000(), TrustedProxies: []string{"cloudflare"},
			Publish: &resourcev1.RoutePublish{Way: resourcev1.RouteWay_ROUTE_WAY_PROXY, Provider: "cloudflare"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		s.applied(t, made.Msg.GetChange())
		got := s.cf.Records("example.com")
		if len(got) != 1 || got[0].Type != "A" || got[0].Content != "203.0.113.10" || !got[0].Proxied {
			t.Fatalf("a proxied A record to the server: %+v", got)
		}
		dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		listed, err := dc.ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range listed.Msg.GetDomains() {
			if d.GetName() == "docs.example.com" && d.GetRoutes()[0].GetProof() != "dns" {
				t.Fatalf("its route proves its domain by DNS: %v", d.GetRoutes()[0])
			}
		}
	})
}
