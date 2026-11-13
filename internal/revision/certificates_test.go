// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"fmt"
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
			shared: []certificate.Shared{{ID: "crt_wild", Name: "wild", Spec: &resourcev1.DnsCertificate{
				Domains: []string{"*.shop.example.com", "shop.example.com"}, DnsProvider: "cloudflare", Projects: []string{"prj_shop"},
			}}},
		}
	}
	route := func(domain string) *resourcev1.Route { return &resourcev1.Route{Domain: domain, Proof: "dns"} }

	if a, n := e.keyFor(t.Context(), fresh(), route("app.example.com"), ref, "srv_1"); a != nil || n != nil {
		t.Errorf("a server with its own key: %v, %v", a, n)
	}
	a, n := e.keyFor(t.Context(), fresh(), route("app.example.com"), ref, "srv_2")
	if n != nil || a == nil || a.Kind != resource.AccessCertificateKey || a.Address != "srv_2" || a.Path != "app.example.com" || a.Detail != "" {
		t.Errorf("a server's first key: %+v, %v", a, n)
	}
	a, n = e.keyFor(t.Context(), fresh(), route("api.shop.example.com"), ref, "srv_1")
	if n != nil || a == nil || a.Address != "crt_wild" || a.Detail != "*.shop.example.com, shop.example.com" {
		t.Errorf("a shared certificate's first key to a server with its own: %+v, %v", a, n)
	}
	blog := resource.Ref{Kind: "route", Scope: "env_2", Name: "www"}
	if a, n := e.keyFor(t.Context(), fresh(), route("blog.example.com"), blog, "srv_2"); a != nil || n == nil || n.Reason != "no_dns_provider" {
		t.Errorf("a name no provider proves for its project: %+v, %+v", a, n)
	}

	k := fresh()
	k.own["srv_2"] = map[string]bool{}
	for i := range certificate.MaxNames {
		k.own["srv_2"][fmt.Sprintf("n%d.example.com", i)] = true
	}
	if a, n := e.keyFor(t.Context(), k, route("n7.example.com"), ref, "srv_2"); a == nil || n != nil {
		t.Errorf("a name the server's certificate holds already: %+v, %+v", a, n)
	}
	if a, n := e.keyFor(t.Context(), k, route("more.example.com"), ref, "srv_2"); a != nil || n == nil || n.Reason != "too_many_dns_names" {
		t.Errorf("a name past 100: %+v, %+v", a, n)
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
