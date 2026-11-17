// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/tunnels"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
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
	return newPublishStandRunning(t, "cloudflared\x00tunnel\x00run\x00--token\x00"+token+"\x00")
}

// newPublishStandRunning is the stand with cloudflared run by the command line
// cmdline, its arguments each ended by a NUL.
func newPublishStandRunning(t *testing.T, cmdline string) *publishStand {
	t.Helper()
	finder := &tunnels.Finder{
		Proc: fstest.MapFS{
			"self/ns/net": {Data: []byte("net:[1]"), Mode: fs.ModeSymlink},
			"100/stat":    {Data: []byte("100 (cloudflared) S 1 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 9 0 500 1000 100")},
			"100/cmdline": {Data: []byte(cmdline)},
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
	// Activity says each write by what it did, as the person whose change it was,
	// with the change (the review's M9).
	ec := dockwellv1connect.NewEventServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	events, err := ec.ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	wrote := 0
	for _, e := range events.Msg.GetEvents() {
		if e.GetAction() != "cloudflare.write" {
			continue
		}
		wrote++
		if e.GetCloudflare().GetChangeId() != first.GetId() || e.GetActor().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_USER {
			t.Fatalf("a write on Activity: %v", e)
		}
	}
	if wrote != 2 {
		t.Fatalf("Activity has %d writes: %v", wrote, events.Msg.GetEvents())
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
	// Activity keeps it, with the version read and the one written, for the alert
	// that tells whoever made the edit (the review's M5).
	ec := dockwellv1connect.NewEventServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	events, err := ec.ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_HEALTH}}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events.Msg.GetEvents(), func(e *dockwellv1.Event) bool {
		cf := e.GetCloudflare()
		return e.GetAction() == "cloudflare.replaced" && cf.GetTarget() == "web-1" && cf.GetName() == "*.example.com" && cf.GetReadVersion() == 7 && cf.GetVersion() == 9
	}) {
		t.Fatalf("Activity should keep the race: %v", events.Msg.GetEvents())
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

// A name made in Cloudflare that points at the tunnel goes through the zone's rule:
// removing the last route that published through it keeps the rule, and the plan
// and Activity say why (the review's M4).
func TestARuleStaysWhileANameMadeByHandUsesIt(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
	files := s.cf.Put("example.com", cftest.Record{Type: "CNAME", Name: "files.example.com", Content: webOne + ".cfargotunnel.com", Proxied: true})

	gone := propose(t, s.person, "Remove wiki", s.removed("wiki"))
	steps := providerSteps(gone)
	keep := slices.IndexFunc(steps, func(p *dockwellv1.ProviderStep) bool {
		return p.GetAction() == dockwellv1.ProviderAction_PROVIDER_ACTION_KEEP_RULE
	})
	if len(steps) != 2 || keep < 0 || steps[keep].GetRouting() != "files.example.com goes through it, so it stays" ||
		steps[1-keep].GetAction() != dockwellv1.ProviderAction_PROVIDER_ACTION_DELETE_RECORD {
		t.Fatalf("the plan's steps at Cloudflare: %v", steps)
	}
	s.applied(t, gone)
	if got := rulesOf(t, s.cf); !slices.Contains(got, "*.example.com=http://127.0.0.1:80") {
		t.Fatalf("the rule went: %v", got)
	}
	if left := s.cf.Records("example.com"); len(left) != 1 || left[0].Name != "files.example.com" {
		t.Fatalf("records left: %+v", left)
	}
	// Made between the plan and its apply, it makes the plan stale, and the plan made
	// again keeps the rule.
	s.applied(t, propose(t, s.person, "Publish docs", s.published("docs", "docs.example.com")))
	s.cf.Remove("example.com", files)
	removes := func(ch *dockwellv1.Change) bool {
		return slices.ContainsFunc(providerSteps(ch), func(p *dockwellv1.ProviderStep) bool {
			return p.GetAction() == dockwellv1.ProviderAction_PROVIDER_ACTION_REMOVE_RULE
		})
	}
	last := propose(t, s.person, "Remove docs", s.removed("docs"))
	if !removes(last) {
		t.Fatalf("with no name made by hand: %v", providerSteps(last))
	}
	s.cf.Put("example.com", cftest.Record{Type: "CNAME", Name: "files.example.com", Content: webOne + ".cfargotunnel.com", Proxied: true})
	if _, err := applyChange(t, s.person, &dockwellv1.ApplyChangeRequest{Id: last.GetId(), Fingerprint: last.GetPlan().GetFingerprint()}); err == nil {
		t.Fatal("a plan made before the name was applied")
	}
	if again := propose(t, s.person, "Remove docs", s.removed("docs")); removes(again) {
		t.Fatalf("planned again: %v", providerSteps(again))
	}
}

// Below an admin, a refusal on the tunnel's way says its name and a fixed reason,
// and a domain's row says its record's type alone (the review's M7).
func TestBelowAnAdminTheCheckSaysLittle(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
	s.cf.Tunnels[webOne].Connectors = append(s.cf.Tunnels[webOne].Connectors, cftest.Connector{ID: "connector-elsewhere"})
	hold := aMember(t, s.f)
	hold(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	got, err := dc.CheckDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: s.server, Domain: "blog.example.com", ProjectId: s.project}))
	if err != nil {
		t.Fatal(err)
	}
	off := ""
	for _, w := range got.Msg.GetCloudflare().GetWays() {
		if w.GetWay() == resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
			off = w.GetOff()
		}
	}
	if off != "Dockwell can't publish through web-1 now. An admin can see why" {
		t.Fatalf("a refusal, as a developer sees it: %q", off)
	}
	listed, err := dc.ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, d := range listed.Msg.GetDomains() {
		for _, p := range d.GetPublished() {
			rows++
			if p.GetRecord() != "CNAME, proxied" {
				t.Fatalf("a row's record, as a developer sees it: %q", p.GetRecord())
			}
		}
	}
	if rows != 1 {
		t.Fatalf("rows: %d", rows)
	}
}

// Ten checks of different names read Cloudflare once each after the first: only
// the name's records, as what else a check reads is kept for 30 seconds. Once
// Cloudflare says too many requests, checks read nothing for a minute (the
// review's M3).
func TestTheLiveCheckReadsLittle(t *testing.T) {
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
	asked := func() int { return len(s.cf.Calls()) }
	if check("app-0.example.com").GetWay() != resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
		t.Fatal("the first check found no way through the tunnel")
	}
	before := asked()
	for i := 1; i <= 10; i++ {
		if c := check("app-" + strconv.Itoa(i) + ".example.com"); c.GetWay() != resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
			t.Fatalf("check %d: %v", i, c)
		}
	}
	if n := asked() - before; n != 10 {
		t.Fatalf("ten checks asked Cloudflare %d times: %q", n, s.cf.Calls()[before:])
	}

	s.cf.Limit(func(string) bool { return true })
	if c := check("busy.example.com"); c != nil {
		t.Fatalf("a check Cloudflare refused: %v", c)
	}
	before = asked()
	for range 3 {
		if c := check("busy.example.com"); c != nil {
			t.Fatalf("a check while waiting: %v", c)
		}
	}
	if n := asked() - before; n != 0 {
		t.Fatalf("checks asked Cloudflare %d times while waiting", n)
	}
}

// cloudflared whose tunnel the agent can't read, as one whose token is in a file
// it can't open, still runs there: the check never picks publishing the server's
// address, and says why the tunnel's way is off (the review's M1).
func TestTheCheckWithCloudflaredItCantRead(t *testing.T) {
	t.Parallel()
	s := newPublishStandRunning(t, "cloudflared\x00tunnel\x00run\x00--token-file\x00/run/secrets/tunnel\x00")
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	got, err := dc.CheckDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: s.server, Domain: "wiki.example.com", ProjectId: s.project}))
	if err != nil {
		t.Fatal(err)
	}
	c := got.Msg.GetCloudflare()
	if c.GetWay() != resourcev1.RouteWay_ROUTE_WAY_FRONT {
		t.Fatalf("picked %v with cloudflared running: %v", c.GetWay(), c)
	}
	off := ""
	for _, w := range c.GetWays() {
		if w.GetWay() == resourcev1.RouteWay_ROUTE_WAY_SERVER_TUNNEL {
			off = w.GetOff()
		}
	}
	if !strings.Contains(off, "runs cloudflared Dockwell can't read") {
		t.Fatalf("the tunnel's way: %q", off)
	}
}

// One tunnel or zone that can't be read never stops the drift timer: a token
// Cloudflare refuses, and a tunnel deleted there, each mark the domain's row and the
// rest is read; a 429 ends the tick (the review's M6).
func TestDriftGoesOnPastWhatItCantRead(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
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
		t.Fatalf("no row for wiki: %v", listed.Msg.GetDomains())
		return 0
	}
	drift := s.f.server.publisher.Drift

	token := s.cf.Tokens[dnsToken]
	s.cf.SetToken(dnsToken, nil)
	if err := drift(t.Context()); err != nil {
		t.Fatalf("a refused token: %v", err)
	}
	if got := stateOf(); got != dockwellv1.PublishedState_PUBLISHED_STATE_UNREADABLE {
		t.Fatalf("with its token refused: %v", got)
	}

	s.cf.SetToken(dnsToken, &token)
	s.cf.DeleteTunnel(webOne)
	if err := drift(t.Context()); err != nil {
		t.Fatalf("a deleted tunnel: %v", err)
	}
	if got := stateOf(); got != dockwellv1.PublishedState_PUBLISHED_STATE_TUNNEL_DELETED {
		t.Fatalf("with its tunnel deleted: %v", got)
	}

	s.cf.Limit(func(string) bool { return true })
	if err := drift(t.Context()); err == nil || !strings.Contains(err.Error(), "429") && !strings.Contains(err.Error(), "throttling") {
		t.Fatalf("a 429: %v", err)
	}
	if got := stateOf(); got != dockwellv1.PublishedState_PUBLISHED_STATE_TUNNEL_DELETED {
		t.Fatalf("a 429 marked the row: %v", got)
	}
}

// A save in a stale dashboard tab takes the zone's rule out of the tunnel, and a
// record is deleted by hand: the drift timer marks both, Activity says so, and the
// domain's row says where it stands; proposing the route again puts it back (ADR
// 0104, section 4).
func TestDriftIsFoundAndPutBack(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	s.applied(t, propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")))
	s.cf.SetDocument(webOne, json.RawMessage(panelDoc))
	for _, r := range s.cf.Records("example.com") {
		s.cf.Remove("example.com", r.ID)
	}
	if err := s.f.server.publisher.Drift(t.Context()); err != nil {
		t.Fatal(err)
	}
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	listed, err := dc.ListDomains(t.Context(), connect.NewRequest(&dockwellv1.ListDomainsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var row *dockwellv1.Domain
	for _, d := range listed.Msg.GetDomains() {
		if d.GetName() == "wiki.example.com" {
			row = d
		}
	}
	if len(row.GetPublished()) != 1 || row.GetPublished()[0].GetState() != dockwellv1.PublishedState_PUBLISHED_STATE_GONE_FROM_TUNNEL {
		t.Fatalf("the domain's row: %v", row)
	}
	ec := dockwellv1connect.NewEventServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	events, err := ec.ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_HEALTH}}))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range events.Msg.GetEvents() {
		if e.GetCloudflare().GetDrift() {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("Activity should say the rule and the record are gone: %v", events.Msg.GetEvents())
	}
	// Put it back: the route, proposed as it is, writes what's gone again.
	back := propose(t, s.person, "Put wiki back", s.published("wiki", "wiki.example.com"))
	if steps := providerSteps(back); len(steps) != 2 {
		t.Fatalf("putting it back: %v", steps)
	}
	s.applied(t, back)
	if got := rulesOf(t, s.cf); !slices.Contains(got, "*.example.com=http://127.0.0.1:80") || len(s.cf.Records("example.com")) != 1 {
		t.Fatalf("after putting it back: %v %v", got, s.cf.Records("example.com"))
	}
}

// pendingRows are what replaces kept of records made by hand, waiting on a write.
func (s *publishStand) pendingRows(t *testing.T, routeID string) []store.CloudflareRecord {
	t.Helper()
	var out []store.CloudflareRecord
	if err := s.f.store.View(t.Context(), func(tx *store.Tx) error {
		var err error
		out, err = tx.CloudflarePendingOf(t.Context(), routeID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func routeIDOf(t *testing.T, r cftest.Record) string {
	t.Helper()
	fields := strings.Fields(r.Comment)
	if len(fields) != 3 || fields[0] != "dockwell" {
		t.Fatalf("not Dockwell's: %+v", r)
	}
	return fields[2]
}

// A replace whose delete landed and whose write didn't leaves what it kept of the
// record made by hand: the next plan's write carries it, so removing the route puts
// that record back whole (the review's H1, path A).
func TestAReplaceCutShortKeepsTheRecord(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	s.applied(t, propose(t, s.person, "Publish vpn at old", s.published("vpn", "old.example.com")))
	routeID := routeIDOf(t, s.cf.Records("example.com")[0])
	byHand := cftest.Record{Type: "A", Name: "vpn.example.com", Content: "203.0.113.9", TTL: 300, Comment: "by hand"}
	id := s.cf.Put("example.com", byHand)
	saved, err := json.Marshal(dnsproviders.Record{ID: id, Type: byHand.Type, Name: byHand.Name, Content: byHand.Content, TTL: byHand.TTL, Comment: byHand.Comment})
	if err != nil {
		t.Fatal(err)
	}
	// The first half of a replace: kept, then deleted; the write never happened.
	if err := s.f.store.Update(t.Context(), func(tx *store.Tx) error {
		return tx.PutCloudflareRecord(t.Context(), store.CloudflareRecord{
			ID: "pending:" + id, Provider: "cloudflare", ZoneID: cftest.ZoneID("example.com"), Zone: "example.com", Name: "vpn.example.com",
			Type: store.CloudflarePendingType, Content: "203.0.113.9", RouteID: routeID, ChangeID: "chg_cut", Replaced: string(saved), State: "pending",
		})
	}); err != nil {
		t.Fatal(err)
	}
	s.cf.Remove("example.com", id)

	// Planned again, as a resume or a retry does: no step names the pending row.
	again := propose(t, s.person, "Publish vpn", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id}))
	for _, p := range providerSteps(again) {
		if strings.Contains(p.GetName()+p.GetWas()+p.GetTo(), "pending") {
			t.Fatalf("a step names the pending row: %v", p)
		}
	}
	s.applied(t, again)
	if left := s.pendingRows(t, routeID); len(left) != 0 {
		t.Fatalf("what was kept should be on the new record now: %+v", left)
	}
	var row store.CloudflareRecord
	_ = s.f.store.View(t.Context(), func(tx *store.Tx) error {
		row, err = tx.CloudflareRecordAt(t.Context(), cftest.ZoneID("example.com"), "vpn.example.com", "CNAME")
		return nil
	})
	if err != nil || !strings.Contains(row.Replaced, "203.0.113.9") {
		t.Fatalf("the record written: %+v %v", row, err)
	}
	// Removing the route puts the record made by hand back, whole.
	s.applied(t, propose(t, s.person, "Remove vpn", s.removed("vpn")))
	got := s.cf.Records("example.com")
	if len(got) != 1 || got[0].Type != "A" || got[0].Content != "203.0.113.9" || got[0].TTL != 300 || got[0].Comment != "by hand" {
		t.Fatalf("the record made by hand should be back: %+v", got)
	}
}

// A replace whose write fails after its delete landed fails the change, and the
// rollback puts the record made by hand back from what was kept (the review's H1).
func TestAReplaceWhoseWriteFailsPutsTheRecordBack(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	id := s.cf.Put("example.com", cftest.Record{Type: "A", Name: "vpn.example.com", Content: "203.0.113.9", TTL: 300, Comment: "by hand"})
	var failed atomic.Bool
	s.cf.FailCreate = func(name string) bool { return name == "vpn.example.com" && failed.CompareAndSwap(false, true) }
	ch := propose(t, s.person, "Publish vpn", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id}))
	w := watch(t, s.person, ch.GetId())
	if _, err := applyChange(t, s.person, &dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatal("a change whose write failed applied")
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := s.cf.Records("example.com")
		if len(got) == 1 && got[0].Type == "A" && got[0].Content == "203.0.113.9" && got[0].Comment == "by hand" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rollback should put the record made by hand back: %+v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Dockwell's record gone at Cloudflare, someone's own made in its place, and Put it
// back with an admin's consent: theirs is kept whole, and undo writes it back (the
// review's H1, path B).
func TestAReplaceAfterDriftKeepsTheirRecord(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	s.applied(t, propose(t, s.person, "Publish vpn", s.published("vpn", "vpn.example.com")))
	for _, r := range s.cf.Records("example.com") {
		s.cf.Remove("example.com", r.ID)
	}
	if err := s.f.server.publisher.Drift(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := s.cf.Put("example.com", cftest.Record{Type: "CNAME", Name: "vpn.example.com", Content: "elsewhere.example.net", TTL: 300, Comment: "by hand"})
	done := s.applied(t, propose(t, s.person, "Put vpn back", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id})))
	undo, err := s.person.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: done.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	s.applied(t, undo.Msg.GetChange())
	got := s.cf.Records("example.com")
	if len(got) != 1 || got[0].Content != "elsewhere.example.net" || got[0].Comment != "by hand" || got[0].TTL != 300 {
		t.Fatalf("their record should be back after undo: %+v", got)
	}
}

// The servers that leave from one of a server's addresses are named, the server
// itself never.
func TestServersSharingAnAddress(t *testing.T) {
	at := func(id, name, v4, out string) *dockwellv1.Server {
		return &dockwellv1.Server{Id: id, Name: name, Network: &dockwellv1.ServerNetwork{Ipv4: &dockwellv1.PublicAddress{Address: v4}}, Inventory: &dockwellv1.ServerInventory{OutboundAddress: out}}
	}
	fleet := []*dockwellv1.Server{
		at("a", "hz-1", "203.0.113.10", ""),
		at("b", "nas", "", "203.0.113.10"),
		at("c", "hz-2", "198.51.100.7", ""),
		at("d", "pi", "", ""),
	}
	if got := strings.Join(sharingAnAddress(fleet, "a", []string{"203.0.113.10"}), ","); got != "nas" {
		t.Fatalf("shares with %q", got)
	}
	if got := sharingAnAddress(fleet, "c", []string{"198.51.100.7"}); len(got) != 0 {
		t.Fatalf("shares with %q", got)
	}
}
