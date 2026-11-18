// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
)

// checkWiki is the stand's check of wiki.example.com for its project, fresh where it says.
func (s *publishStand) checkWiki(t *testing.T, fresh bool) *dockwellv1.CloudflareCheck {
	t.Helper()
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	req := connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: s.server, Domain: "wiki.example.com", ProjectId: s.project})
	if fresh {
		req.Header().Set("Cache-Control", "no-cache")
	}
	got, err := dc.CheckDomain(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return got.Msg.GetCloudflare()
}

func hasTunnelWay(cf *dockwellv1.CloudflareCheck) bool {
	for _, w := range cf.GetWays() {
		if w.GetWay() == resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL && w.GetTunnel() == webOne {
			return true
		}
	}
	return false
}

// The check says, to an admin alone, the rule made by hand that serves a name, so
// the panel can take it on or move it to Dockwell (the review's B-M4).
func TestTheCheckNamesARuleMadeByHand(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	s.cf.SetDocument(webOne, json.RawMessage(`{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},`+wikiByHand+`,{"service":"http_status:404"}]}`))
	hand := s.checkWiki(t, false).GetHandRule()
	if hand.GetTunnel() != webOne || hand.GetTunnelName() != "web-1" || hand.GetHostname() != "wiki.example.com" || hand.GetService() != "http://127.0.0.1:8080" || hand.GetReachesProxy() {
		t.Fatalf("an admin's check: %v", hand)
	}
	hold := aMember(t, s.f)
	hold(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
	if hand := s.checkWiki(t, false).GetHandRule(); hand != nil {
		t.Fatalf("a developer sees the rule made by hand: %v", hand)
	}
}

// Check again reads the server's tunnels now, past the 30 s they're kept, so a
// cloudflared that just started, or stopped, is seen at once (the review's open
// question 3).
func TestCheckAgainReadsTheTunnelsNow(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	if !hasTunnelWay(s.checkWiki(t, false)) {
		t.Fatal("the server's tunnel isn't offered")
	}
	s.proc.stop("100")
	if !hasTunnelWay(s.checkWiki(t, false)) {
		t.Fatal("within 30 s a check reads the tunnels it kept")
	}
	if hasTunnelWay(s.checkWiki(t, true)) {
		t.Fatal("Check again should read the tunnels now")
	}
}

// A check Cloudflare can't answer names the provider with no state, so the panel
// says so with Check again rather than planning as if the zone weren't Cloudflare's
// (the review's B-M1).
func TestACheckCloudflareCantAnswerSaysSo(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	delete(s.cf.Tokens, dnsToken)
	cf := s.checkWiki(t, false)
	if cf == nil || cf.GetProvider() != "cloudflare" || cf.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_UNSPECIFIED || len(cf.GetWays()) != 0 {
		t.Fatalf("a check that couldn't be made: %v", cf)
	}
}

// Domains reads what DNS says of each name in one listing per connected zone, kept a
// while, never a check per name; a proxied record says nothing of its origin, to an
// admin or anyone else (ADR 0104, the review's B-M2).
func TestDomainsReadDNSOncePerZone(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
	s.applied(t, propose(t, s.person, "Publish docs", s.published("docs", "docs.example.com")))
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	read := func() []*dockwellv1.Domain {
		t.Helper()
		got, err := dc.ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{ReadDns: true}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.GetDomains()
	}
	listings := func() int {
		n := 0
		for _, c := range s.cf.Calls() {
			if strings.HasPrefix(c, "GET /client/v4/zones/") && strings.HasSuffix(c, "/dns_records") || strings.HasPrefix(c, "GET /zones/") && strings.HasSuffix(c, "/dns_records") {
				n++
			}
		}
		return n
	}
	before := listings()
	for range 2 {
		for _, d := range read() {
			if !d.GetDns().GetProxied() || len(d.GetDns().GetFound()) != 0 {
				t.Fatalf("%s: %v", d.GetName(), d.GetDns())
			}
		}
	}
	if n := listings() - before; n != 1 {
		t.Fatalf("two lists of two names read the zone %d times", n)
	}
	hold := aMember(t, s.f)
	hold(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
	for _, d := range read() {
		if !d.GetDns().GetProxied() || len(d.GetDns().GetFound()) != 0 {
			t.Fatalf("a developer's %s: %v", d.GetName(), d.GetDns())
		}
	}
}

// The server's tunnel says where it reaches the server's proxy, for the way line, and
// a record through it reads by the tunnel's name, never its ID (the polish round).
func TestATunnelWayAndItsRecordReadInWords(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	var reaches string
	for _, w := range s.checkWiki(t, false).GetWays() {
		if w.GetWay() == resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
			reaches = w.GetReaches()
		}
	}
	if reaches != "http://127.0.0.1:80" {
		t.Fatalf("where the tunnel reaches: %q", reaches)
	}
	ch := propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com"))
	creates := stepsOf(ch, dockwellv1.ProviderAction_PROVIDER_ACTION_CREATE_RECORD)
	if len(creates) != 1 || creates[0].GetTo() != "CNAME to web-1's tunnel, proxied" {
		t.Fatalf("the record's words: %v", providerSteps(ch))
	}
}
