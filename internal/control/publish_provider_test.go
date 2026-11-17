// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
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

// strictStand is the publishing stand whose token may set a zone's mode, with
// wiki.example.com published, proxied, and the zone on Flexible.
func strictStand(t *testing.T, backend store.Backend) *publishStand {
	t.Helper()
	s := newPublishStandOn(t, backend)
	s.f.engine.SetControlServer(s.server)
	tok := s.cf.Tokens[dnsToken]
	tok.Rights = append(tok.Rights, "Zone Settings Write")
	s.cf.SetToken(dnsToken, &tok)
	s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
	s.cf.SetMode("example.com", "flexible")
	return s
}

func zoneModeSteps(ch *dockwellv1.Change) []*dockwellv1.ProviderStep {
	var out []*dockwellv1.ProviderStep
	for _, p := range providerSteps(ch) {
		if p.GetAction() == dockwellv1.ProviderAction_PROVIDER_ACTION_SET_ZONE_MODE {
			out = append(out, p)
		}
	}
	return out
}

// An admin sets a zone to Full (strict) on the DNS provider: its own plan, naming
// the zone's proxied names, which keeps the mode it had. Undo puts it back (ADR
// 0104, section 2).
func TestAZoneSetToFullStrictAndBack(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := strictStand(t, backend)
		ch := propose(t, s.person, "Use Full (strict)", s.provider(func(p *resourcev1.DnsProvider) { p.StrictZones = []string{"example.com"} }))
		steps := zoneModeSteps(ch)
		if len(steps) != 1 || steps[0].GetWas() != "Flexible" || steps[0].GetTo() != "Full (strict)" || steps[0].GetRouting() != "Changes how Cloudflare reaches wiki.example.com" {
			t.Fatalf("the plan: %v", providerSteps(ch))
		}
		done := s.applied(t, ch)
		if got := s.cf.Mode("example.com"); got != "strict" {
			t.Fatalf("the zone's mode: %s", got)
		}
		undo, err := s.person.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: done.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		back := undo.Msg.GetChange()
		if steps := zoneModeSteps(back); len(steps) != 1 || steps[0].GetTo() != "Flexible" {
			t.Fatalf("undo's plan: %v", providerSteps(back))
		}
		s.applied(t, back)
		if got := s.cf.Mode("example.com"); got != "flexible" {
			t.Fatalf("undo should put Flexible back: %s", got)
		}
	})
}

// A zone's mode changed in the dashboard since Dockwell set it stays as it is when
// it's taken off; one on Full (strict) already isn't Dockwell's to put back; and a
// provider that set a zone's mode can't go until it's taken off.
func TestAZoneModeIsOnlyPutBackWhileItsDockwells(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := strictStand(t, backend)
		s.applied(t, propose(t, s.person, "Use Full (strict)", s.provider(func(p *resourcev1.DnsProvider) { p.StrictZones = []string{"example.com"} })))
		if _, err := s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Remove it", Edits: []*dockwellv1.Edit{
			{Resource: &dockwellv1.ResourceRef{Kind: "dns.provider", Name: "cloudflare"}, Delete: true},
		}})); err == nil || !strings.Contains(err.Error(), "Take it off the provider's strict zones first") {
			t.Fatalf("removing a provider that set a zone's mode: %v", err)
		}
		s.cf.SetMode("example.com", "full")
		off := s.applied(t, propose(t, s.person, "Stop", s.provider(func(p *resourcev1.DnsProvider) {})))
		said := false
		for _, st := range off.GetSteps() {
			said = said || strings.Contains(st.GetMessage(), "example.com's mode was changed in Cloudflare since, so it stays Full")
		}
		if !said || s.cf.Mode("example.com") != "full" {
			t.Fatalf("a mode changed since should stay, saying so: %v, %s", off.GetSteps(), s.cf.Mode("example.com"))
		}
		s.cf.SetMode("example.com", "strict")
		if steps := zoneModeSteps(propose(t, s.person, "Already", s.provider(func(p *resourcev1.DnsProvider) { p.StrictZones = []string{"example.com"} }))); len(steps) != 0 {
			t.Fatalf("a zone on Full (strict) already takes no step: %v", steps)
		}
	})
}

// A step run again, as a resume runs one, finds the mode it set and what it kept,
// and a token that lacks Zone Settings edit fails the step, saying what to add.
func TestAZoneModeStepRunAgainAndWithoutTheRight(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := strictStand(t, backend)
		pub := s.f.server.publisher
		step := resource.ProviderStep{Provider: "cloudflare", Action: resource.ProviderSetZoneMode, Target: "example.com", Name: "example.com", ZoneID: cftest.ZoneID("example.com"), Mode: "strict"}
		for range 2 {
			if _, err := pub.Apply(t.Context(), "chg_strict", revision.PublishRoute{}, step); err != nil {
				t.Fatal(err)
			}
		}
		step.Mode = "flexible"
		if _, err := pub.Apply(t.Context(), "chg_back", revision.PublishRoute{}, step); err != nil || s.cf.Mode("example.com") != "flexible" {
			t.Fatalf("put back after a run again: %v, %s", err, s.cf.Mode("example.com"))
		}
		tok := s.cf.Tokens[dnsToken]
		tok.Rights = []string{"Zone Read", "DNS Write", "Cloudflare Tunnel Write", "Zone Settings Read", "Account API Tokens Read"}
		s.cf.SetToken(dnsToken, &tok)
		step.Mode = "strict"
		if _, err := pub.Apply(t.Context(), "chg_denied", revision.PublishRoute{}, step); err == nil || !strings.Contains(err.Error(), "the token lacks Zone Settings edit") {
			t.Fatalf("without the right: %v", err)
		}
	})
}
