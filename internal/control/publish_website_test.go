// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
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
