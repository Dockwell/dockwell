// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
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

// storedProvider is the stand's DNS provider as its revision keeps it.
func (s *publishStand) storedProvider(t *testing.T) *resourcev1.DnsProvider {
	t.Helper()
	var out *resourcev1.DnsProvider
	if err := s.f.store.View(t.Context(), func(tx *store.Tx) error {
		all, err := tx.ListKind(t.Context(), "dns.provider")
		if err != nil {
			return err
		}
		for i := range all {
			if all[i].Name == "cloudflare" {
				m, err := s.f.keeper.Decode(t.Context(), tx, store.InRevision(all[i].ID, all[i].Kind, all[i].Revision), all[i].Spec)
				out, _ = m.(*resourcev1.DnsProvider)
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A record made by hand that couldn't go back, as the name held Dockwell's record
// let go, is listed on the provider's info and on Activity. Putting it back while
// the name is taken skips, saying so, and writes nothing; once the name is free, it
// writes the record as it was, and the provider's revision never keeps what was
// asked (ADR 0104, and the lead's decision on kept backups).
func TestAKeptRecordIsListedAndPutBack(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.f.engine.SetControlServer(s.server)
		id := s.cf.Put("example.com", byHandCNAME)
		s.applied(t, propose(t, s.person, "Publish vpn", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id})))
		recs := s.cf.Records("example.com")
		s.cf.EditInPlace("example.com", recs[0].ID, func(r *cftest.Record) { r.Proxied = false })
		s.applied(t, propose(t, s.person, "Remove vpn", s.removed("vpn")))

		kept := s.providerInfo(t).GetKept()
		if len(kept) != 1 || kept[0].GetName() != "vpn.example.com" || kept[0].GetRecord() != "CNAME elsewhere.example.net" || kept[0].GetNameFree() {
			t.Fatalf("what the provider keeps: %v", kept)
		}
		ec := dockwellv1connect.NewEventServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		events, err := ec.ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_HEALTH}}))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(events.Msg.GetEvents(), func(e *dockwellv1.Event) bool {
			return e.GetAction() == "cloudflare.kept" && e.GetCloudflare().GetName() == "vpn.example.com"
		}) {
			t.Fatalf("Activity should say a copy is kept: %v", events.Msg.GetEvents())
		}

		// Taken: the step says so and writes nothing.
		putBack := func() *dockwellv1.Edit {
			return s.provider(func(p *resourcev1.DnsProvider) { p.PutBack = []string{kept[0].GetId()} })
		}
		taken := propose(t, s.person, "Put it back", putBack())
		steps := providerSteps(taken)
		if len(steps) != 1 || steps[0].GetAction() != dockwellv1.ProviderAction_PROVIDER_ACTION_RESTORE_RECORD ||
			!strings.Contains(steps[0].GetRouting(), "vpn.example.com holds CNAME "+webOne+".cfargotunnel.com now, so the record kept isn't put back") {
			t.Fatalf("a put back at a taken name: %v", steps)
		}
		s.applied(t, taken)
		if got := s.cf.Records("example.com"); len(got) != 1 || got[0].Comment == "by hand" {
			t.Fatalf("a taken name was written: %+v", got)
		}
		if got := s.storedProvider(t); len(got.GetPutBack()) != 0 {
			t.Fatalf("the provider's revision kept what was asked: %v", got.GetPutBack())
		}

		// Free: it writes the record as it was, with an admin's passkey.
		s.cf.Remove("example.com", recs[0].ID)
		if kept := s.providerInfo(t).GetKept(); len(kept) != 1 || !kept[0].GetNameFree() {
			t.Fatalf("a free name: %v", kept)
		}
		free := propose(t, s.person, "Put it back", putBack())
		if !slices.ContainsFunc(free.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
			return slices.ContainsFunc(r.GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
				return a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_CLOUDFLARE_TAKEOVER && a.GetPath() == "vpn.example.com"
			})
		}) {
			t.Fatalf("putting someone's record back takes an admin's passkey: %v", free.GetPlan().GetResources())
		}
		s.applied(t, free)
		got := s.cf.Records("example.com")
		if len(got) != 1 || got[0].Content != byHandCNAME.Content || got[0].Comment != "by hand" || got[0].TTL != 300 {
			t.Fatalf("the record should be back as it was: %+v", got)
		}
		if kept := s.providerInfo(t).GetKept(); len(kept) != 0 {
			t.Fatalf("a record put back is kept no more: %v", kept)
		}
		// Asked again, it names a record kept no more, and the plan is refused.
		if _, err := s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Again", Edits: []*dockwellv1.Edit{putBack()}})); err == nil {
			t.Fatal("putting back a record kept no more was planned")
		}
	})
}
