// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/tunnels"
	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

const webOne = "327d9d41-b009-413d-8494-7eeeefb3ed2a"

// panelDoc is web-1's document as the owner made it: the panel's own name, with its
// own setting, and a catch-all.
const panelDoc = `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600","originRequest":{"noTLSVerify":true}},{"service":"http_status:404"}],"originRequest":{"connectTimeout":10}}`

// publishStand is a server that runs web-1 with cloudflared, Cloudflare's stand-in
// holding web-1 and example.com, and a DNS provider that serves the stand's project.
type publishStand struct {
	*dnsStand
	cf *cftest.API
}

func newPublishStand(t *testing.T) *publishStand {
	t.Helper()
	token := base64.StdEncoding.EncodeToString([]byte(`{"a":"` + cftest.Account + `","t":"` + webOne + `","s":"never-leaves"}`))
	finder := &tunnels.Finder{
		Proc: fstest.MapFS{
			"self/ns/net": {Data: []byte("net:[1]"), Mode: fs.ModeSymlink},
			"100/stat":    {Data: []byte("100 (cloudflared) S 1 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 9 0 500 1000 100")},
			"100/cmdline": {Data: []byte("cloudflared\x00tunnel\x00run\x00--token\x00" + token + "\x00")},
			"100/ns/net":  {Data: []byte("net:[1]"), Mode: fs.ModeSymlink},
		},
		ReadyIn: func(context.Context, int, string) (tunnels.Ready, error) {
			return tunnels.Ready{Connections: 4, ConnectorID: "connector-hz-1"}, nil
		},
	}
	s := newDNSStandWith(t, func(f *fullServer) { f.tunnels = finder })
	cf := s.f.cloudflare
	cf.Tokens[dnsToken] = cftest.Token{
		Reads: []string{"example.com", "example.org"}, Writes: []string{"example.com", "example.org"}, Account: true,
		Rights: []string{"Zone Read", "DNS Write", "Cloudflare Tunnel Write", "Zone Settings Read", "Account API Tokens Read"},
	}
	cf.AddTunnel(&cftest.Tunnel{ID: webOne, Name: "web-1", Config: json.RawMessage(panelDoc), Version: 7, Connectors: []cftest.Connector{{ID: "connector-hz-1"}}})
	applyConfirmed(t, s.person, propose(t, s.person, "Publish through Cloudflare", dnsEdit("cloudflare", &resourcev1.DnsProvider{
		Provider: "cloudflare", Zones: []string{"example.com"}, ApiToken: dnsToken, Projects: []string{s.project},
	})))
	return &publishStand{dnsStand: s, cf: cf}
}

func (s *publishStand) published(name, domain string, takes ...*resourcev1.TakeOver) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: s.env, Name: name},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: domain, Service: "web", Port: 8080, Tls: "off", Publish: &resourcev1.RoutePublish{
			Way: resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL, Tunnel: webOne, Provider: "cloudflare", TakesOver: takes,
		}}}},
	}
}

func (s *publishStand) removed(name string) *dockwellv1.Edit {
	return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: s.env, Name: name}, Delete: true}
}

// applied applies a change with its fingerprint, and a passkey where it asks one,
// and waits for it to end applied.
func (s *publishStand) applied(t *testing.T, ch *dockwellv1.Change) *dockwellv1.Change {
	t.Helper()
	w := watch(t, s.person, ch.GetId())
	if _, err := applyChange(t, s.person, &dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}); err != nil {
		t.Fatalf("apply %s: %v", ch.GetSummary(), err)
	}
	got := waitWatch(t, w).last()
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("%s ended %v: %s", ch.GetSummary(), got.GetState(), got.GetError())
	}
	return got
}

func providerSteps(ch *dockwellv1.Change) []*dockwellv1.ProviderStep {
	var out []*dockwellv1.ProviderStep
	for _, sp := range ch.GetPlan().GetServers() {
		for _, st := range sp.GetSteps() {
			if st.GetProvider() != nil {
				out = append(out, st.GetProvider())
			}
		}
	}
	return out
}

func rulesOf(t *testing.T, cf *cftest.API) []string {
	t.Helper()
	doc, _ := cf.Document(webOne)
	var c struct {
		Ingress []struct{ Hostname, Service string } `json:"ingress"`
	}
	if err := json.Unmarshal(doc, &c); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range c.Ingress {
		out = append(out, r.Hostname+"="+r.Service)
	}
	return out
}

// Publishing through the server's tunnel adds the zone's rule once, after the
// panel's, and each name its record; removing the last name takes the rule out,
// leaving the document as it was (ADR 0104, sections 4 and 8).
func TestPublishingThroughTheServersTunnel(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)

	first := propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com"))
	steps := providerSteps(first)
	if len(steps) != 2 || steps[0].GetAction() != dockwellv1.ProviderAction_PROVIDER_ACTION_ADD_RULE || steps[0].GetName() != "*.example.com" ||
		steps[0].GetTarget() != "web-1" || steps[0].GetRouting() != "Every other name in web-1 goes where it went" ||
		steps[1].GetAction() != dockwellv1.ProviderAction_PROVIDER_ACTION_CREATE_RECORD || steps[1].GetName() != "wiki.example.com" {
		t.Fatalf("the plan's steps at Cloudflare: %v", steps)
	}
	// The rule is host access an admin applies once, with the routing check's words.
	if !slices.ContainsFunc(first.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
		return slices.ContainsFunc(r.GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
			return a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_TUNNEL_RULE && a.GetPath() == "*.example.com"
		})
	}) {
		t.Fatalf("the rule should be host access: %v", first.GetPlan().GetResources())
	}
	s.applied(t, first)
	if got := rulesOf(t, s.cf); !slices.Equal(got, []string{"panel.example.com=http://127.0.0.1:7600", "*.example.com=http://127.0.0.1:80", "=http_status:404"}) {
		t.Fatalf("web-1's rules: %v", got)
	}
	doc, version := s.cf.Document(webOne)
	if version != 8 || !strings.Contains(string(doc), `"noTLSVerify":true`) || !strings.Contains(string(doc), `"connectTimeout":10`) {
		t.Fatalf("the document lost what it held, or moved more than once (%d): %s", version, doc)
	}
	records := s.cf.Records("example.com")
	if len(records) != 1 || records[0].Type != "CNAME" || records[0].Content != webOne+".cfargotunnel.com" || !records[0].Proxied ||
		!strings.HasPrefix(records[0].Comment, "dockwell ") {
		t.Fatalf("the record: %+v", records)
	}

	// A second name in the zone writes its record alone.
	second := propose(t, s.person, "Publish docs", s.published("docs", "docs.example.com"))
	if steps := providerSteps(second); len(steps) != 1 || steps[0].GetAction() != dockwellv1.ProviderAction_PROVIDER_ACTION_CREATE_RECORD {
		t.Fatalf("a second name: %v", steps)
	}
	s.applied(t, second)

	// Removing one leaves the rule, which the other still uses; removing the last
	// takes it out, and the document is as the owner made it.
	gone := propose(t, s.person, "Remove wiki", s.removed("wiki"))
	if steps := providerSteps(gone); len(steps) != 1 || steps[0].GetAction() != dockwellv1.ProviderAction_PROVIDER_ACTION_DELETE_RECORD {
		t.Fatalf("removing one: %v", steps)
	}
	s.applied(t, gone)
	last := propose(t, s.person, "Remove docs", s.removed("docs"))
	if steps := providerSteps(last); len(steps) != 2 || steps[1].GetAction() != dockwellv1.ProviderAction_PROVIDER_ACTION_REMOVE_RULE {
		t.Fatalf("removing the last: %v", steps)
	}
	s.applied(t, last)
	if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(panelDoc)) {
		t.Fatalf("the document after: %s", doc)
	}
	if left := s.cf.Records("example.com"); len(left) != 0 {
		t.Fatalf("records left: %+v", left)
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return bytes.Equal(ja, jb)
}

// What stops publishing stops the plan, saying why: a name someone else holds at
// Cloudflare, a project the provider doesn't serve, a rule that catches the name
// first, and a tunnel that runs on another server too (ADR 0104).
func TestPublishingRefuses(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	refused := func(why string, edit *dockwellv1.Edit) {
		t.Helper()
		_, err := s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: why, Edits: []*dockwellv1.Edit{edit}}))
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v", why, err)
		}
	}
	s.cf.Put("example.com", cftest.Record{Type: "A", Name: "vpn.example.com", Content: "203.0.113.9"})
	refused("is taken at Cloudflare by a record Dockwell didn't make", s.published("vpn", "vpn.example.com"))
	refused("past Cloudflare's free certificate", s.published("deep", "a.docs.example.com"))

	doc, _ := s.cf.Document(webOne)
	s.cf.SetDocument(webOne, json.RawMessage(strings.Replace(string(doc), `{"service":"http_status:404"}`, `{"hostname":"*.example.com","service":"http://127.0.0.1:8080"},{"service":"http_status:404"}`, 1)))
	refused("in the tunnel catches wiki.example.com first, to http://127.0.0.1:8080", s.published("wiki", "wiki.example.com"))
	s.cf.SetDocument(webOne, doc)

	s.cf.Tunnels[webOne].Connectors = append(s.cf.Tunnels[webOne].Connectors, cftest.Connector{ID: "connector-elsewhere"})
	refused("runs on hz-1 and somewhere else too", s.published("wiki", "wiki.example.com"))
	s.cf.Tunnels[webOne].Connectors = s.cf.Tunnels[webOne].Connectors[:1]

	// A provider that doesn't serve the route's project.
	applyConfirmed(t, s.person, propose(t, s.person, "Serve no project", dnsEdit("cloudflare", &resourcev1.DnsProvider{
		Provider: "cloudflare", Zones: []string{"example.com"}, ApiToken: dnsToken,
	})))
	refused("doesn't serve this route's project", s.published("wiki", "wiki.example.com"))
}

// An admin replaces someone else's record with consent, and undo writes it back
// whole (ADR 0104, H2).
func TestReplacingARecordAndUndoingIt(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	id := s.cf.Put("example.com", cftest.Record{Type: "A", Name: "vpn.example.com", Content: "203.0.113.9", TTL: 300, Comment: "by hand"})
	ch := propose(t, s.person, "Publish vpn", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id}))
	steps := providerSteps(ch)
	i := slices.IndexFunc(steps, func(p *dockwellv1.ProviderStep) bool {
		return p.GetAction() == dockwellv1.ProviderAction_PROVIDER_ACTION_REPLACE_RECORD
	})
	if i < 0 || steps[i].GetWas() != "A 203.0.113.9, made in Cloudflare" {
		t.Fatalf("the plan: %v", steps)
	}
	if !slices.ContainsFunc(ch.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
		return slices.ContainsFunc(r.GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
			return a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_CLOUDFLARE_TAKEOVER
		})
	}) {
		t.Fatal("replacing someone else's record should be a takeover")
	}
	done := s.applied(t, ch)
	if got := s.cf.Records("example.com"); len(got) != 1 || got[0].Type != "CNAME" {
		t.Fatalf("after: %+v", got)
	}
	undo, err := s.person.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: done.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	s.applied(t, undo.Msg.GetChange())
	got := s.cf.Records("example.com")
	if len(got) != 1 || got[0].Type != "A" || got[0].Content != "203.0.113.9" || got[0].TTL != 300 || got[0].Comment != "by hand" {
		t.Fatalf("undo should write the replaced record back whole: %+v", got)
	}
}

// A save in the dashboard that lands between Dockwell's read of a tunnel's
// document and its write is replaced, as Cloudflare checks no version; the step says
// so, naming when, and Dockwell's rule is in (ADR 0104, section 4).
func TestARaceWithTheDashboardIsSaid(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	once := true
	s.cf.BeforePut = func(tunnel string) {
		if !once {
			return
		}
		once = false
		doc, _ := s.cf.Document(tunnel)
		s.cf.SetDocument(tunnel, json.RawMessage(strings.Replace(string(doc), `{"service":"http_status:404"}`, `{"hostname":"ssh.example.com","service":"ssh://127.0.0.1:22"},{"service":"http_status:404"}`, 1)))
	}
	done := s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
	said := ""
	for _, st := range done.GetSteps() {
		if strings.HasPrefix(st.GetMessage(), "An edit made in Cloudflare between ") {
			said = st.GetMessage()
		}
	}
	if !strings.HasSuffix(said, "was replaced. Dockwell kept what it read, and that's what's there now") {
		t.Fatalf("the race should be said: %v", done.GetSteps())
	}
	if got := rulesOf(t, s.cf); !slices.Contains(got, "*.example.com=http://127.0.0.1:80") {
		t.Fatalf("Dockwell's rule: %v", got)
	}
}

// The live check says what Cloudflare holds at a name, and the ways in with the one
// it would take; a record's content only to an admin (ADR 0104, section 1).
func TestTheLiveCheckOfAName(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	check := func(name string) *dockwellv1.CloudflareCheck {
		t.Helper()
		got, err := dc.CheckDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: s.server, Domain: name, ProjectId: s.project}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.GetCloudflare()
	}
	tunnelWay := func(c *dockwellv1.CloudflareCheck) *dockwellv1.WayOption {
		for _, w := range c.GetWays() {
			if w.GetWay() == resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
				return w
			}
		}
		return nil
	}

	free := check("wiki.example.com")
	if free.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_FREE || free.GetZone() != "example.com" || free.GetWay() != resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
		t.Fatalf("a free name: %v", free)
	}
	if w := tunnelWay(free); w.GetTunnelName() != "web-1" || w.GetAddsRule() != "*.example.com" || w.GetOff() != "" {
		t.Fatalf("the tunnel's way: %v", w)
	}
	s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
	if w := tunnelWay(check("docs.example.com")); w.GetAddsRule() != "" || w.GetOff() != "" {
		t.Fatalf("with the zone's rule in: %v", w)
	}
	if ours := check("wiki.example.com"); ours.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_OURS {
		t.Fatalf("a name Dockwell published: %v", ours)
	}

	s.cf.Put("example.com", cftest.Record{Type: "A", Name: "vpn.example.com", Content: "203.0.113.9"})
	if taken := check("vpn.example.com"); taken.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_TAKEN || taken.GetRecord() != "A 203.0.113.9" || taken.GetRecordId() == "" {
		t.Fatalf("taken, as an admin sees it: %v", taken)
	}
	if past := check("a.docs.example.com"); past.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_PAST_CERTIFICATE || tunnelWay(past).GetOff() == "" {
		t.Fatalf("past the certificate: %v", past)
	}

	// Below an admin, a name is taken, and nothing more.
	hold := aMember(t, s.f)
	hold(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
	if taken := check("vpn.example.com"); taken.GetState() != dockwellv1.CloudflareName_CLOUDFLARE_NAME_TAKEN || taken.GetRecord() != "" || taken.GetRecordId() != "" || len(taken.GetTypes()) != 0 {
		t.Fatalf("taken, as a developer sees it: %v", taken)
	}
}
