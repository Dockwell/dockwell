// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"fmt"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/certificate"
	"github.com/dockwell/dockwell/internal/store"
)

// What a route proven by DNS takes to its server: the first key of the server's own,
// or of a shared certificate, for an admin to confirm; nothing where the server has
// it; and a note, asking no one, where no key goes (ADR 0088).
func TestWhichKeyARouteTakes(t *testing.T) {
	e := &Engine{controlServer: "srv_control"}
	ref := resource.Ref{Kind: "route", Scope: "env_1", Name: "www"}
	fresh := func() *keyContext {
		return &keyContext{
			confirmed: map[string]map[string]bool{"srv_1": {"srv_1": true}},
			providers: map[string]*resourcev1.DnsProvider{"cloudflare": {Zones: []string{"example.com"}, Projects: []string{"prj_shop"}}},
			projects:  map[string]string{"env_1": "prj_shop", "env_2": "prj_blog"},
			own:       map[string]map[string]bool{},
			ownRead:   true,
			shared: []certificate.Shared{{ID: "crt_wild", Name: "wild", Spec: &resourcev1.DnsCertificate{
				Domains: []string{"*.shop.example.com", "shop.example.com"}, DnsProvider: "cloudflare", Projects: []string{"prj_shop"},
			}}},
		}
	}
	route := func(domain string) *resourcev1.Route { return &resourcev1.Route{Domain: domain, Proof: "dns"} }

	if a, n, err := e.keyFor(t.Context(), fresh(), route("app.example.com"), ref, "srv_1"); a != nil || n != nil || err != nil {
		t.Errorf("a server with its own key: %v, %v, %v", a, n, err)
	}
	a, n, _ := e.keyFor(t.Context(), fresh(), route("app.example.com"), ref, "srv_2")
	if n != nil || a == nil || a.Kind != resource.AccessCertificateKey || a.Address != "srv_2" || a.Path != "app.example.com" || a.Detail != "" {
		t.Errorf("a server's first key: %+v, %v", a, n)
	}
	a, n, _ = e.keyFor(t.Context(), fresh(), route("api.shop.example.com"), ref, "srv_1")
	if n != nil || a == nil || a.Address != "crt_wild" || a.Detail != "*.shop.example.com, shop.example.com" {
		t.Errorf("a shared certificate's first key to a server with its own: %+v, %v", a, n)
	}
	blog := resource.Ref{Kind: "route", Scope: "env_2", Name: "www"}
	if _, _, err := e.keyFor(t.Context(), fresh(), route("blog.example.com"), blog, "srv_2"); err == nil || !strings.Contains(err.Error(), "no DNS provider proves it") {
		t.Errorf("a name no provider proves for its project: %v", err)
	}

	k := fresh()
	k.own["srv_2"] = map[string]bool{}
	for i := range certificate.MaxNames {
		k.own["srv_2"][fmt.Sprintf("n%d.example.com", i)] = true
	}
	if a, n, _ := e.keyFor(t.Context(), k, route("n7.example.com"), ref, "srv_2"); a == nil || n != nil {
		t.Errorf("a name the server's certificate holds already: %+v, %+v", a, n)
	}
	if a, n, _ := e.keyFor(t.Context(), k, route("more.example.com"), ref, "srv_2"); a != nil || n == nil || n.Reason != "too_many_dns_names" {
		t.Errorf("a name past 100: %+v, %+v", a, n)
	}
}

// A route left automatic is proven by DNS where HTTP can't reach its server and a
// provider proves its name for its project, and by HTTP otherwise (ADR 0088).
func TestHowARouteLeftAutomaticIsProven(t *testing.T) {
	e := &Engine{}
	k := &keyContext{
		providers: map[string]*resourcev1.DnsProvider{"cloudflare": {Zones: []string{"example.com"}, Projects: []string{"prj_shop"}}},
		projects:  map[string]string{"env_1": "prj_shop", "env_2": "prj_blog"},
	}
	shop := resource.Ref{Kind: "route", Scope: "env_1", Name: "www"}
	blog := resource.Ref{Kind: "route", Scope: "env_2", Name: "www"}
	for _, c := range []struct {
		what   string
		ref    resource.Ref
		domain string
		check  *resource.DomainCheck
		want   string
	}{
		{"no record", shop, "app.example.com", &resource.DomainCheck{State: resource.DomainNoRecord}, "dns"},
		{"behind a tunnel", shop, "app.example.com", &resource.DomainCheck{State: resource.DomainElsewhere}, "dns"},
		{"private addresses alone", shop, "app.example.com", &resource.DomainCheck{State: resource.DomainCantTell}, "dns"},
		{"carrier-grade NAT", shop, "app.example.com", &resource.DomainCheck{State: resource.DomainHere, Carrier: true}, "dns"},
		{"pointing here", shop, "app.example.com", &resource.DomainCheck{State: resource.DomainHere}, "http"},
		{"DNS that didn't answer", shop, "app.example.com", &resource.DomainCheck{State: resource.DomainUnknown}, "http"},
		{"no check", shop, "app.example.com", nil, "http"},
		{"a zone no provider holds", shop, "app.example.org", &resource.DomainCheck{State: resource.DomainNoRecord}, "http"},
		{"a project no provider names", blog, "blog.example.com", &resource.DomainCheck{State: resource.DomainNoRecord}, "http"},
	} {
		proof, note := e.autoProof(t.Context(), k, &resourcev1.Route{Domain: c.domain}, c.ref, c.check)
		if proof != c.want || (note != nil) != (c.want == "dns") {
			t.Errorf("%s: %s, %v", c.what, proof, note)
		}
	}
	if _, note := e.autoProof(t.Context(), &keyContext{}, &resourcev1.Route{Domain: "app.example.com"}, shop, &resource.DomainCheck{State: resource.DomainNoRecord}); note != nil {
		t.Error("with no provider at all, it notes proof by DNS")
	}
}

// A shared certificate names nothing outside its provider's zones, counting a
// provider in the same change as it will be (ADR 0088).
func TestASharedCertificateStaysInItsProvidersZones(t *testing.T) {
	cert := func(names ...string) item {
		return item{ref: resource.Ref{Kind: certificate.Kind, Name: "wild"}, spec: &resourcev1.DnsCertificate{Domains: names, DnsProvider: "second"}}
	}
	provider := item{ref: resource.Ref{Kind: "dns_provider", Name: "second"}, spec: &resourcev1.DnsProvider{Zones: []string{"example.org"}}}
	e, _, _ := setup(t, store.SQLite)
	if err := e.checkCertificates(t.Context(), []item{provider, cert("*.example.org", "example.org")}); err != nil {
		t.Errorf("names in the provider's zone: %v", err)
	}
	if err := e.checkCertificates(t.Context(), []item{provider, cert("*.example.org", "app.example.net")}); err == nil {
		t.Error("a name outside the provider's zones was taken")
	}
}
