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
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// dnsStand is a service on a server, its environment and project, for routes
// proven by DNS.
type dnsStand struct {
	f              *fullServer
	b              *browser
	person, script dockwellv1connect.ChangeServiceClient
	env, project   string
	server         string
}

func newDNSStand(t *testing.T) *dnsStand {
	t.Helper()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	s := &dnsStand{f: f, b: b, person: changeClient(b, b.origin)}
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	s.script = changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	first := importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", nil, false)
	if got := apply(t, s.person, first); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the service: %v", got)
	}
	s.env = first.GetPlan().GetResources()[0].GetResource().GetScope()
	s.server = first.GetPlan().GetServers()[0].GetServerId()
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		e, err := tx.GetResourceByID(t.Context(), s.env)
		if err == nil {
			s.project = e.Scope
		}
		return err
	})
	return s
}

func (s *dnsStand) route(name, domain string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: s.env, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: domain, Service: "web", Port: 8080, Proof: route.ProofDNS}}},
	}
}

func (s *dnsStand) confirmed(t *testing.T) map[string]map[string]bool {
	t.Helper()
	var out map[string]map[string]bool
	if err := s.f.store.View(t.Context(), func(tx *store.Tx) error {
		var err error
		out, err = revision.KeysConfirmed(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// applyWithPasskey applies ch as the person, with a passkey for it.
func (s *dnsStand) applyWithPasskey(t *testing.T, ch *dockwellv1.Change) error {
	t.Helper()
	proof := stepUpFor(t, s.b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, ch.GetId())
	_, err := s.person.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint(), Proof: proof}))
	return err
}

func keyAccess(ch *dockwellv1.Change) []*dockwellv1.HostAccess {
	var out []*dockwellv1.HostAccess
	for _, r := range ch.GetPlan().GetResources() {
		for _, a := range r.GetHostAccess() {
			if a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_CERTIFICATE_KEY {
				out = append(out, a)
			}
		}
	}
	return out
}

// The first time a route proven by DNS would take a certificate's key to a server,
// the plan says so, no token applies it, and an admin confirms it with a passkey,
// kept once the change is taken; after that, the server's next names need no one.
// Where no provider proves the name, no key goes, and the plan says so and asks no
// one (ADR 0088).
func TestTheFirstKeyToAServerTakesAnAdminsPasskey(t *testing.T) {
	s := newDNSStand(t)

	early := propose(t, s.script, "Serve early by DNS", s.route("early", "early.example.com"))
	if a := keyAccess(early); len(a) != 0 {
		t.Errorf("a name no provider proves sends a key: %v", a)
	}
	if !slices.ContainsFunc(early.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool {
		return n.GetReason() == "no_dns_provider" && slices.Equal(n.GetNames(), []string{"early.example.com"})
	}) {
		t.Errorf("the plan doesn't say no provider proves the name: %v", early.GetPlan().GetNotes())
	}
	if got := apply(t, s.script, early); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("a token's route that sends no key: %v", got)
	}

	apply(t, s.person, propose(t, s.person, "Cloudflare", dnsEdit("cloudflare", &resourcev1.DnsProvider{Provider: "cloudflare", Zones: []string{"example.com"}, ApiToken: dnsToken, Projects: []string{s.project}})))
	ch := propose(t, s.script, "Serve app by DNS", s.route("www", "app.example.com"))
	access := keyAccess(ch)
	if len(access) != 1 || access[0].GetPath() != "app.example.com" || access[0].GetAddress() != s.server || access[0].GetDetail() != "" {
		t.Fatalf("the plan names %v, want the first key to the server", access)
	}
	if _, err := s.script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); reasonOf(err) != "needs_session" {
		t.Errorf("a token applied the first key to a server: %v", err)
	}
	if _, err := s.person.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "passkey") {
		t.Fatalf("an admin applied the first key without a passkey: %v", err)
	}
	if c := s.confirmed(t); len(c) != 0 {
		t.Fatalf("a refused apply let a server have keys: %v", c)
	}

	// The engine refuses a plan gone out of date, passkey or not, and nothing is
	// kept of it.
	apply(t, s.person, propose(t, s.person, "Serve app by HTTP", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: s.env, Name: "www"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 8080}}},
	}))
	if err := s.applyWithPasskey(t, ch); err == nil {
		t.Fatal("an out-of-date plan applied")
	}
	if c := s.confirmed(t); len(c) != 0 {
		t.Fatalf("a plan the engine refused let a server have keys: %v", c)
	}

	ch = propose(t, s.person, "Serve app by DNS", s.route("www", "app.example.com"))
	w := watch(t, s.person, ch.GetId())
	if err := s.applyWithPasskey(t, ch); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the route: %v", got)
	}
	if c := s.confirmed(t); len(c) != 1 || len(c[s.server]) != 1 || !c[s.server][s.server] {
		t.Errorf("the confirmed keys are %v, want %s's own", c, s.server)
	}

	// The server's next name proven by DNS asks no one.
	next := propose(t, s.script, "Serve shop by DNS", s.route("shop", "shop.example.com"))
	if a := keyAccess(next); len(a) != 0 {
		t.Errorf("a second name on a confirmed server asks for its key again: %v", a)
	}
	if got := apply(t, s.script, next); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("a token's second name on a confirmed server: %v", got)
	}
}

// A shared certificate's key going to a server is confirmed for that certificate,
// whatever else the server has, and the plan names the certificate's names; and a
// shared certificate names nothing outside its provider's zones, whose token writes
// there alone (ADR 0088).
func TestASharedCertificatesKeyTakesItsOwnPasskey(t *testing.T) {
	s := newDNSStand(t)
	apply(t, s.person, propose(t, s.person, "Cloudflare", dnsEdit("cloudflare", &resourcev1.DnsProvider{Provider: "cloudflare", Zones: []string{"example.com"}, ApiToken: dnsToken, Projects: []string{s.project}})))
	certificate := func(name string, domains ...string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "certificate", Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Certificate{Certificate: &resourcev1.DnsCertificate{Domains: domains, DnsProvider: "cloudflare", Projects: []string{s.project}}}},
		}
	}
	_, err := s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Outside", Edits: []*dockwellv1.Edit{certificate("outside", "*.example.com", "app.example.net")}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "app.example.net") {
		t.Errorf("a certificate named a name outside its provider's zones: %v", err)
	}
	if got := apply(t, s.person, propose(t, s.person, "Wildcard", certificate("wild", "*.example.com", "example.com"))); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the wildcard: %v", got)
	}
	var wild string
	if err := s.f.store.View(t.Context(), func(tx *store.Tx) error {
		saved, err := tx.ListKind(t.Context(), "certificate")
		for _, r := range saved {
			if r.Name == "wild" {
				wild = r.ID
			}
		}
		return err
	}); err != nil || wild == "" {
		t.Fatalf("the wildcard's ID: %v", err)
	}
	// The server may have its own certificate's key, and no other.
	if err := s.f.store.Update(t.Context(), func(tx *store.Tx) error {
		return revision.ConfirmKeys(t.Context(), tx, map[string][]string{s.server: {s.server}})
	}); err != nil {
		t.Fatal(err)
	}

	ch := propose(t, s.script, "Serve api by DNS", s.route("api", "api.example.com"))
	access := keyAccess(ch)
	if len(access) != 1 || access[0].GetAddress() != wild || access[0].GetDetail() != "*.example.com, example.com" {
		t.Fatalf("the plan names %v, want the wildcard's key, %s, to the server, by its names", access, wild)
	}
	if _, err := s.script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); reasonOf(err) != "needs_session" {
		t.Errorf("a token sent a shared certificate's key to a server: %v", err)
	}
	ch = propose(t, s.person, "Serve api by DNS", s.route("api", "api.example.com"))
	w := watch(t, s.person, ch.GetId())
	if err := s.applyWithPasskey(t, ch); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, w)
	if c := s.confirmed(t); !c[s.server][wild] || !c[s.server][s.server] {
		t.Errorf("the confirmed keys are %v, want the wildcard's beside the server's own", c)
	}
}

// A route proven by DNS, through a provider that proves its project's names, is on
// its server's own certificate, which ListCertificates names for the server, and
// says where it stands, never its key; to those who read its project alone (ADR
// 0088).
func TestAServersOwnCertificateIsListed(t *testing.T) {
	s := newDNSStand(t)
	apply(t, s.person, propose(t, s.person, "Cloudflare", dnsEdit("cloudflare", &resourcev1.DnsProvider{Provider: "cloudflare", Zones: []string{"example.com"}, ApiToken: dnsToken, Projects: []string{s.project}})))
	ch := propose(t, s.person, "Serve app by DNS", s.route("www", "app.example.com"))
	w := watch(t, s.person, ch.GetId())
	if err := s.applyWithPasskey(t, ch); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, w)
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	list := func() []*dockwellv1.CertificateInfo {
		t.Helper()
		listed, err := dc.ListCertificates(t.Context(), connect.NewRequest(&dockwellv1.ListCertificatesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return listed.Msg.GetCertificates()
	}
	server := ch.GetPlan().GetServers()[0]
	c := list()
	if len(c) != 1 || c[0].GetId() != server.GetServerId() || c[0].GetName() != server.GetServerName() || strings.Join(c[0].GetDomains(), ",") != "app.example.com" || c[0].GetDnsProvider() != "cloudflare" {
		t.Fatalf("listed %v", c)
	}
	if st := c[0].GetState(); st != dockwellv1.CertificateState_CERTIFICATE_STATE_PENDING && st != dockwellv1.CertificateState_CERTIFICATE_STATE_FAILED {
		t.Errorf("a certificate no CA issued reads %v", st)
	}

	hold := aMember(t, s.f)
	hold(in("prj_somewhere", store.RoleDeveloper))
	if c := list(); len(c) != 0 {
		t.Errorf("a developer of another project lists %v", c)
	}
	hold(in(s.project, store.RoleViewer))
	if c := list(); len(c) != 1 {
		t.Errorf("a viewer of its project lists %v", c)
	}
	hold(in(store.Install, store.RoleViewer))
	if c := list(); len(c) != 1 {
		t.Errorf("a viewer of the install lists %v", c)
	}
}
