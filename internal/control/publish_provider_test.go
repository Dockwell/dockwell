// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"encoding/base64"
	"io/fs"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
	"github.com/dockwell/dockwell/internal/control/publish"
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
	// A proxied name Dockwell doesn't publish, whose server's certificate it can't
	// vouch for.
	s.cf.Put("example.com", cftest.Record{Type: "A", Name: "vpn.example.com", Content: "203.0.113.9", Proxied: true, TTL: 1, Comment: "by hand"})
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
		if len(steps) != 1 || steps[0].GetWas() != "Flexible" || steps[0].GetTo() != "Full (strict)" ||
			steps[0].GetRouting() != "Cloudflare reaches each over HTTPS with a certificate it checks, so a name whose server has none valid stops answering: vpn.example.com" {
			t.Fatalf("the plan: %v", providerSteps(ch))
		}
		// An admin approves it, with the names it can break (the review's M3).
		if !slices.ContainsFunc(ch.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
			return slices.ContainsFunc(r.GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
				return a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_ZONE_MODE && a.GetPath() == "example.com" && a.GetDetail() == "vpn.example.com"
			})
		}) {
			t.Fatalf("Full (strict) should be an admin's approval, naming what it can break: %v", ch.GetPlan().GetResources())
		}
		done := s.applied(t, ch)
		if got := s.cf.Mode("example.com"); got != "strict" {
			t.Fatalf("the zone's mode: %s", got)
		}
		// An admin's list says it, so a form that changes the provider sends it back.
		if got := s.providerInfo(t).GetStrictZones(); !slices.Equal(got, []string{"example.com"}) {
			t.Fatalf("an admin's list of zones on Full (strict): %v", got)
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

// A tunnel that gains a second server after Dockwell wrote its rule, as cloudflared
// started for it somewhere else, is found by the drift timer: the domain's row says
// so, and the domain alert raises it. Once it's gone from there, the row is
// published again (ADR 0104, section 3).
func TestDriftSaysATunnelRunsElsewhereToo(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
		dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		stateOf := func() dockwellv1.PublishedState {
			t.Helper()
			listed, err := dc.ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range listed.Msg.GetDomains() {
				if d.GetName() == "wiki.example.com" && len(d.GetPublished()) == 1 {
					return d.GetPublished()[0].GetState()
				}
			}
			t.Fatal("no row for wiki")
			return 0
		}
		drift := s.f.server.publisher.Drift
		if err := drift(t.Context()); err != nil || stateOf() != dockwellv1.PublishedState_PUBLISHED_STATE_PUBLISHED {
			t.Fatalf("before: %v, %v", err, stateOf())
		}
		s.cf.Connect(webOne, cftest.Connector{ID: "connector-elsewhere"})
		if err := drift(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := stateOf(); got != dockwellv1.PublishedState_PUBLISHED_STATE_TUNNEL_SHARED {
			t.Fatalf("with a second server: %v", got)
		}
		s.cf.Disconnect(webOne, "connector-elsewhere")
		if err := drift(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := stateOf(); got != dockwellv1.PublishedState_PUBLISHED_STATE_PUBLISHED {
			t.Fatalf("once it's gone from there: %v", got)
		}
	})
}

// wikiState is wiki.example.com's published state on the domain's row.
func (s *publishStand) wikiState(t *testing.T) dockwellv1.PublishedState {
	t.Helper()
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	listed, err := dc.ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range listed.Msg.GetDomains() {
		if d.GetName() == "wiki.example.com" && len(d.GetPublished()) == 1 {
			return d.GetPublished()[0].GetState()
		}
	}
	t.Fatal("no row for wiki")
	return 0
}

// The drift timer never reads a tunnel it can't prove as published: one connected
// wholly from another server runs elsewhere, and one run again on the same server in
// a way that disagrees, as a fake cloudflared in a container claiming its ID, can't be
// proved there (the review's M2).
func TestDriftSaysWhenItCantProveTheTunnel(t *testing.T) {
	t.Parallel()
	t.Run("moved wholly to another server", func(t *testing.T) {
		t.Parallel()
		storetest.Each(t, func(t *testing.T, backend store.Backend) {
			s := newPublishStandOn(t, backend)
			s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
			s.cf.Disconnect(webOne, "connector-hz-1")
			s.cf.Connect(webOne, cftest.Connector{ID: "connector-elsewhere"})
			if err := s.f.server.publisher.Drift(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := s.wikiState(t); got != dockwellv1.PublishedState_PUBLISHED_STATE_TUNNEL_SHARED {
				t.Fatalf("a tunnel connected from elsewhere alone: %v", got)
			}
		})
	})
	t.Run("a second run on the same server that disagrees", func(t *testing.T) {
		t.Parallel()
		storetest.Each(t, func(t *testing.T, backend store.Backend) {
			s := newPublishStandOn(t, backend)
			s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
			token := base64.StdEncoding.EncodeToString([]byte(`{"a":"` + cftest.Account + `","t":"` + webOne + `","s":"made-up"}`))
			s.proc.add(fstest.MapFS{
				"200/stat":      {Data: []byte("200 (cloudflared) S 1 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 9 0 600 1000 100")},
				"200/cmdline":   {Data: []byte("cloudflared\x00tunnel\x00run\x00--token\x00" + token + "\x00")},
				"200/ns/net":    {Data: []byte("net:[2]"), Mode: fs.ModeSymlink},
				"200/net/route": {Data: []byte("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\neth0\t000012AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n")},
			})
			if err := s.f.server.publisher.Drift(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := s.wikiState(t); got != dockwellv1.PublishedState_PUBLISHED_STATE_TUNNEL_UNPROVEN {
				t.Fatalf("a fake run beside the real one: %v", got)
			}
		})
	})
}

// Listing DNS providers reads Cloudflare only for an admin, who sees what they keep,
// and once in 30 seconds for each kept name; below an admin it reads nothing (the
// review's M5).
func TestListingProvidersReadsCloudflareOnlyForAnAdmin(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		id := s.cf.Put("example.com", byHandCNAME)
		s.applied(t, propose(t, s.person, "Publish vpn", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id})))
		recs := s.cf.Records("example.com")
		s.cf.EditInPlace("example.com", recs[0].ID, func(r *cftest.Record) { r.Proxied = false })
		s.applied(t, propose(t, s.person, "Remove vpn", s.removed("vpn")))
		before := len(s.cf.Calls())
		for range 2 {
			if info := s.providerInfo(t); len(info.GetKept()) != 1 || !slices.Equal(info.GetProjects(), []string{s.project}) {
				t.Fatalf("an admin's list: %v", info)
			}
		}
		if n := len(s.cf.Calls()) - before; n != 1 {
			t.Fatalf("two lists by an admin read Cloudflare %d times: %q", n, s.cf.Calls()[before:])
		}
		hold := aMember(t, s.f)
		hold(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
		before = len(s.cf.Calls())
		if info := s.providerInfo(t); len(info.GetKept()) != 0 || len(info.GetProjects()) != 0 || len(info.GetStrictZones()) != 0 {
			t.Fatalf("a developer sees what's kept, or the provider's projects or modes: %v", info)
		}
		if n := len(s.cf.Calls()) - before; n != 0 {
			t.Fatalf("a developer's list read Cloudflare %d times", n)
		}
	})
}

// Narrowed to a project, the list of DNS providers names those that serve it; a
// caller who holds no role in the project is refused, so it can't learn another
// project's providers (ADR 0104, the lead's condition).
func TestDnsProvidersNarrowedToAProject(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		list := func(project string) ([]*dockwellv1.DnsProviderInfo, error) {
			got, err := dc.ListDnsProviders(t.Context(), connect.NewRequest(&dockwellv1.ListDnsProvidersRequest{ProjectId: project}))
			if err != nil {
				return nil, err
			}
			return got.Msg.GetProviders(), nil
		}
		if got, err := list(s.project); err != nil || len(got) != 1 || got[0].GetName() != "cloudflare" {
			t.Fatalf("an admin, for the project: %v, %v", got, err)
		}
		if got, err := list("prj_other"); err != nil || len(got) != 0 {
			t.Fatalf("an admin, for a project it serves not: %v, %v", got, err)
		}
		hold := aMember(t, s.f)
		hold(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
		if got, err := list(s.project); err != nil || len(got) != 1 || len(got[0].GetZones()) != 1 || got[0].GetProvider() != "" {
			t.Fatalf("a developer in the project: %v, %v", got, err)
		}
		hold(store.Grant{ProjectID: "prj_other", Role: store.RoleDeveloper})
		// Refused as a project the caller can't see, saying nothing of its providers.
		if got, err := list(s.project); err == nil || connect.CodeOf(err) != connect.CodeNotFound && connect.CodeOf(err) != connect.CodePermissionDenied || len(got) != 0 {
			t.Fatalf("a developer of another project: %v, %v", got, err)
		}
	})
}

func TestDNSProviderReachAndPublishing(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		noProjects := s.provider(func(p *resourcev1.DnsProvider) { p.Projects = nil })
		s.applied(t, propose(t, s.person, "Restrict access to Cloudflare", noProjects))
		wiki := s.published("wiki", "wiki.example.com")
		before := len(s.cf.Calls())
		if c := s.checkWiki(t, false); c.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_OUTSIDE_PROJECT || c.GetProvider() != "cloudflare" || c.GetZone() != "example.com" || len(c.GetWays()) != 0 {
			t.Fatalf("admin's check for an unserved project: %v", c)
		}
		hold := aMember(t, s.f)
		hold(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
		if c := s.checkWiki(t, false); c.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_OUTSIDE_PROJECT || c.GetProvider() != "" || c.GetZone() != "" || len(c.GetWays()) != 0 {
			t.Fatalf("developer's check for an unserved project: %v", c)
		}
		if _, err := s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Publish wiki", Edits: []*dockwellv1.Edit{wiki}})); err == nil || !strings.Contains(err.Error(), "doesn't serve this route's project") {
			t.Fatalf("developer tried to publish through an unserved project: %v", err)
		}
		if n := len(s.cf.Calls()) - before; n != 0 {
			t.Fatalf("checking and proposing a name outside reach read Cloudflare %d times", n)
		}
		hold(onInstall(store.RoleAdmin))
		every := s.provider(func(p *resourcev1.DnsProvider) { p.Projects, p.EveryProject = nil, true })
		s.applied(t, propose(t, s.person, "Use Cloudflare for every project", every))
		if info := s.providerInfo(t); !info.GetEveryProject() || len(info.GetProjects()) != 0 {
			t.Fatalf("admin's provider info: %v", info)
		}
		if !hasTunnelWay(s.checkWiki(t, false)) {
			t.Fatal("a provider reaching every project offers no way through the tunnel")
		}
		if _, err := s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Restrict access and publish wiki", Edits: []*dockwellv1.Edit{noProjects, wiki}})); err == nil || !strings.Contains(err.Error(), "doesn't serve this route's project") {
			t.Fatalf("same plan narrowed provider reach and published anyway: %v", err)
		}
		s.applied(t, propose(t, s.person, "Publish wiki", wiki))
		// The first route writes the zone rule. A developer can then plan a second
		// name through it without seeing the provider's reach.
		hold(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
		dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		listed, err := dc.ListDnsProviders(t.Context(), connect.NewRequest(&dockwellv1.ListDnsProvidersRequest{ProjectId: s.project}))
		if err != nil || len(listed.Msg.GetProviders()) != 1 || listed.Msg.GetProviders()[0].GetEveryProject() {
			t.Fatalf("developer's provider list: %v %v", listed, err)
		}
		blog := propose(t, s.person, "Publish blog", s.published("blog", "blog.example.com"))
		// This stand uses the protected production environment.
		hold(onInstall(store.RoleAdmin))
		s.applied(t, blog)
		if got := s.cf.Records("example.com"); len(got) != 2 {
			t.Fatalf("published records: %v", got)
		}
	})
}

func TestProviderReachPlansAndUndo(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		expectNote := func(ch *dockwellv1.Change, reason string, words ...string) {
			t.Helper()
			for _, note := range ch.GetPlan().GetNotes() {
				if note.GetReason() == reason && !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(note.GetMessage(), w) }) {
					return
				}
			}
			t.Fatalf("want note %s with %v, got %v", reason, words, ch.GetPlan().GetNotes())
		}
		noProjects := s.provider(func(p *resourcev1.DnsProvider) { p.Projects = nil })
		s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
		dropped := propose(t, s.person, "Remove project from Cloudflare", noProjects)
		expectNote(dropped, "provider_stops_serving", "wiki.example.com", "stay in place", "restores access")
		s.applied(t, dropped)

		every := s.provider(func(p *resourcev1.DnsProvider) { p.EveryProject, p.Projects = true, nil })
		widened := propose(t, s.person, "Use Cloudflare for every project", every)
		expectNote(widened, "provider_serves_every_project", "including new ones", "publish names in example.com", "prove the names they publish")
		done := s.applied(t, widened)
		undo, err := s.person.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: done.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		back := undo.Msg.GetChange()
		expectNote(back, "provider_serves_selected_projects", "no longer serves every project")
		expectNote(back, "provider_stops_serving", "wiki.example.com", "stay in place")
		s.applied(t, back)
		if info := s.providerInfo(t); info.GetEveryProject() || len(info.GetProjects()) != 0 {
			t.Fatalf("undo should restore no reach, got %v", info)
		}
		if got := recordNames(s); !slices.Equal(got, []string{"wiki.example.com"}) {
			t.Fatalf("narrowing should keep existing DNS records, got %v", got)
		}
		created := propose(t, s.person, "Connect another provider", dnsEdit("other", &resourcev1.DnsProvider{
			Provider: "cloudflare", ApiToken: dnsToken, Zones: []string{"example.org"}, EveryProject: true,
		}))
		expectNote(created, "provider_serves_every_project", "other serves every project", "example.org")
	})
}

func TestPublishingStepChecksReachAgain(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.applied(t, propose(t, s.person, "Widen reach", s.provider(func(p *resourcev1.DnsProvider) {
			p.EveryProject, p.Projects = true, nil
		})))
		edit := s.published("wiki", "wiki.example.com")
		ch := propose(t, s.person, "Publish wiki", edit)
		// Capture the actual steps, then exercise them directly as well as applying
		// through the engine, which can refuse before running any steps.
		ref := resource.Ref{Kind: "route", Scope: s.env, Name: "wiki"}
		r := revision.PublishRoute{Ref: ref, ID: "rte_wiki", Environment: s.env, Server: s.server, After: edit.GetSpec().GetRoute()}
		pub := s.f.server.publisher
		steps, err := pub.Plan(t.Context(), ch.GetId(), []revision.PublishRoute{r}, nil)
		if err != nil || len(steps[ref]) != 2 {
			t.Fatalf("publishing should plan a tunnel rule and a record: %v, %v", steps, err)
		}
		s.applied(t, propose(t, s.person, "Narrow reach", s.provider(func(p *resourcev1.DnsProvider) { p.Projects = nil })))
		before := len(s.cf.Calls())
		refused := func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "doesn't serve this route's project") && strings.Contains(err.Error(), "Ask an admin")
		}
		for _, step := range steps[ref] {
			if _, err := pub.Apply(t.Context(), ch.GetId(), r, *step.Provider); !refused(err) {
				t.Fatalf("publishing step %v should be refused: %v", step.Provider.Action, err)
			}
		}
		if _, err := applyChange(t, s.person, &dockwellv1.ApplyChangeRequest{
			Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint(),
		}); !refused(err) {
			t.Fatalf("applying the plan after reach was revoked should be refused: %v", err)
		}
		if len(s.cf.Calls()) != before {
			t.Fatalf("a refused plan or step contacted Cloudflare: %v", s.cf.Calls()[before:])
		}
	})
}

func TestCheckUsesTheMostSpecificConnectedZone(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.applied(t, propose(t, s.person, "Connect delegated zone", dnsEdit("delegated", &resourcev1.DnsProvider{
			Provider: "cloudflare", ApiToken: dnsToken, Zones: []string{"dev.example.com"},
		})))
		before := len(s.cf.Calls())
		dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		check := func() *dockwellv1.CloudflareCheck {
			t.Helper()
			res, err := dc.CheckDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckDomainRequest{
				Domain: "app.dev.example.com", ServerId: s.server, ProjectId: s.project, Publish: &resourcev1.RoutePublish{},
			}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg.GetCloudflare()
		}
		if c := check(); c.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_OUTSIDE_PROJECT || c.GetProvider() != "delegated" || c.GetZone() != "dev.example.com" {
			t.Fatalf("expected refusal at the delegated zone: %v", c)
		}
		aMember(t, s.f)(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
		if c := check(); c.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_OUTSIDE_PROJECT || c.GetProvider() != "" || c.GetZone() != "" {
			t.Fatalf("expected a trimmed refusal: %v", c)
		}
		if len(s.cf.Calls()) != before {
			t.Fatalf("a name in an unserved delegated zone contacted Cloudflare: %v", s.cf.Calls()[before:])
		}
	})
}

func TestEveryProjectCanOnlyProveNamesItsRoutesPublish(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.f.server.publisher.Server = func(context.Context, string) (publish.Facts, error) {
			return publish.Facts{Name: "hz-1", Public: []netip.Addr{netip.MustParseAddr("203.0.113.10")}}, nil
		}
		manual := s.route("manual", "manual.example.com")
		s.applied(t, propose(t, s.person, "Prove a manually pointed name", manual))
		s.applied(t, propose(t, s.person, "Use Cloudflare for every project", s.provider(func(p *resourcev1.DnsProvider) {
			p.EveryProject, p.Projects = true, nil
		})))
		other := s.route("other", "other.example.com")
		other.GetSpec().GetRoute().Publish = &resourcev1.RoutePublish{Way: resourcev1.RouteWay_ROUTE_WAY_FRONT, Provider: "cloudflare"}
		managed := s.route("managed", "managed.example.com")
		managed.GetSpec().GetRoute().Publish = &resourcev1.RoutePublish{Way: resourcev1.RouteWay_ROUTE_WAY_PROXY, Provider: "cloudflare"}
		for _, edit := range []*dockwellv1.Edit{s.route("outside", "outside.example.com"), other} {
			_, err := s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
				Summary: "Prove another name by DNS", Edits: []*dockwellv1.Edit{edit},
			}))
			if err == nil || !strings.Contains(err.Error(), "no DNS provider proves it") {
				t.Fatalf("a name without managed publishing should be refused: %v", err)
			}
		}
		s.applied(t, propose(t, s.person, "Publish and prove the managed name", managed))
		dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		check := func(want ...string) {
			t.Helper()
			res, err := dc.ListCertificates(t.Context(), connect.NewRequest(&dockwellv1.ListCertificatesRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range res.Msg.GetCertificates() {
				got = append(got, c.GetDomains()...)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("DNS certificate names %v, want %v", got, want)
			}
		}
		check("managed.example.com")
		s.applied(t, propose(t, s.person, "Name this project again", s.provider(func(*resourcev1.DnsProvider) {})))
		check("managed.example.com", "manual.example.com")
		// Widening while the project stays listed, as the panel does, takes no proof
		// away (re-check N1).
		s.applied(t, propose(t, s.person, "Every project, keeping this one", s.provider(func(p *resourcev1.DnsProvider) {
			p.EveryProject = true
		})))
		check("managed.example.com", "manual.example.com")
	})
}
