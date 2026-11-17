// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// What the publishing stand's DNS provider is, as an edit gives it again.
func (s *publishStand) provider(change func(*resourcev1.DnsProvider)) *dockwellv1.Edit {
	p := &resourcev1.DnsProvider{Provider: "cloudflare", Zones: []string{"example.com"}, Projects: []string{s.project}}
	change(p)
	return dnsEdit("cloudflare", p)
}

// providerInfo is the stand's provider as ListDnsProviders says it.
func (s *publishStand) providerInfo(t *testing.T) *dockwellv1.DnsProviderInfo {
	t.Helper()
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	got, err := dc.ListDnsProviders(t.Context(), connect.NewRequest(&dockwellv1.ListDnsProvidersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got.Msg.GetProviders() {
		if p.GetName() == "cloudflare" {
			return p
		}
	}
	t.Fatalf("no provider cloudflare: %v", got.Msg.GetProviders())
	return nil
}

// A saved second token comes off when an edit says so, and the API token does
// tunnels again; leaving it out keeps it (ADR 0104).
func TestASavedSecondTokenIsCleared(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.applied(t, propose(t, s.person, "Give tunnels a token", s.provider(func(p *resourcev1.DnsProvider) { p.TunnelApiToken = "tunnel-token-0123456789" })))
		if !s.providerInfo(t).GetTunnelApiTokenSet() {
			t.Fatal("the second token should be set")
		}
		s.applied(t, propose(t, s.person, "Keep it", s.provider(func(p *resourcev1.DnsProvider) {})))
		if !s.providerInfo(t).GetTunnelApiTokenSet() {
			t.Fatal("an edit that left it out took it off")
		}
		s.applied(t, propose(t, s.person, "Take it off", s.provider(func(p *resourcev1.DnsProvider) { p.ClearTunnelApiToken = true })))
		if s.providerInfo(t).GetTunnelApiTokenSet() {
			t.Fatal("the second token should be off")
		}
		if _, err := s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Both", Edits: []*dockwellv1.Edit{
			s.provider(func(p *resourcev1.DnsProvider) {
				p.TunnelApiToken = "another-token-0123456789"
				p.ClearTunnelApiToken = true
			}),
		}})); err == nil {
			t.Fatal("a token and a clear together were taken")
		}
	})
}
