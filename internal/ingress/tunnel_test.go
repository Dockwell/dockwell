// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

func plainSpec(domain string) *resourcev1.Route {
	return &resourcev1.Route{Domain: domain, Service: "web", Port: 80, Tls: TLSOff}
}

func hostPortSpec(port uint32) *resourcev1.Route {
	return &resourcev1.Route{Domain: "app.example.com", HostPort: port}
}

// take has a process hold a port, and returns what lets it go.
func (s *setup) take(t *testing.T, port int, name string) (release func()) {
	t.Helper()
	inode := strconv.Itoa(4000 + port)
	s.proc.listen("tcp", port, "0A", inode)
	s.proc.process(port, name, "", inode)
	return func() {
		if err := os.Remove(filepath.Join(s.proc.root, "net", "tcp")); err != nil {
			t.Fatal(err)
		}
	}
}

// judge plans one route, as the agent does.
func (s *setup) judge(t *testing.T, name string, r *resourcev1.Route) *resource.IngressEffect {
	t.Helper()
	ref := routeRef(name)
	var want proto.Message
	if r != nil {
		want = r
	}
	effect, _, err := s.Judge(t.Context(), ref, r, []resource.Desired{{Ref: ref, Spec: want}})
	if err != nil {
		t.Fatal(err)
	}
	return effect
}

// A server whose routes take no certificate needs port 80 alone, so it runs beside
// whatever holds 443, and what it says and does says so.
func TestAPlainFirstRouteTakesPort80Alone(t *testing.T) {
	s := newSetup(t)
	s.take(t, 443, "nginx")
	ref := routeRef("www")
	route := plainSpec("app.example.com")
	if err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: route}}); err != nil {
		t.Fatalf("Check with port 443 taken = %v", err)
	}
	effect := s.judge(t, "www", route)
	if effect == nil || effect.Change != resource.IngressStarts || !slices.Equal(effect.Ports, []string{"80/tcp"}) {
		t.Fatalf("the plan says %+v, want it to start on port 80 alone", effect)
	}
	s.apply(t, "www", route)
	if !s.proxy.plainOnly {
		t.Error("the proxy started with its HTTPS port too")
	}
	routes, _ := s.proxy.Routes(t.Context())
	if len(routes) != 1 || !routes[0].Plain {
		t.Fatalf("the proxy has %+v", routes)
	}
	// Reconcile doesn't ask for more than that of a proxy that stopped.
	s.proxy.status = Status{Exists: true}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile with only port 443 taken = %v", err)
	}
	if !s.proxy.plainOnly || !s.proxy.status.Running {
		t.Errorf("reconcile left it as %+v, plain only %v", s.proxy.status, s.proxy.plainOnly)
	}
	// A service may publish what the proxy doesn't hold, and not what it does.
	if held, err := s.PublicPort(t.Context(), 443, false); err != nil || held {
		t.Errorf("PublicPort(443) = %v, %v, on a server that doesn't hold it", held, err)
	}
	if held, err := s.PublicPort(t.Context(), 80, false); err != nil || !held {
		t.Errorf("PublicPort(80) = %v, %v", held, err)
	}
}

func TestAPlainFirstRouteIsRefusedWithPort80Taken(t *testing.T) {
	s := newSetup(t)
	s.take(t, 80, "nginx")
	ref := routeRef("www")
	route := plainSpec("app.example.com")
	err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: route}})
	if err == nil || !strings.Contains(err.Error(), "Port 80 is held by nginx") {
		t.Errorf("Check = %v, want it refused by name", err)
	}
}

// The panel's site needs HTTPS wherever it is served, so a plain first route on a
// server with a panel on a domain takes both.
func TestAPlainFirstRouteStillMovesThePanel(t *testing.T) {
	s := newSetup(t)
	calls := &panelCalls{}
	s.Panel = calls.panel()
	effect := s.judge(t, "www", plainSpec("app.example.com"))
	if effect == nil || effect.PanelDomain == "" || !slices.Contains(effect.Ports, "443/tcp") {
		t.Fatalf("the plan says %+v, want the panel to move, with port 443", effect)
	}
	s.apply(t, "www", plainSpec("app.example.com"))
	if s.proxy.plainOnly {
		t.Error("the proxy started without HTTPS, with the panel's site to serve")
	}
}

// The first route that wants a certificate takes what a certificate needs, and
// the last that does frees it. Neither stops anything.
func TestPortsFollowWhatRoutesNeed(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "plain", plainSpec("plain.example.com"))
	ref := routeRef("secure")
	secure := spec("secure.example.com", "", "web", 80)
	effect := s.judge(t, "secure", secure)
	if effect == nil || effect.Change != resource.IngressTakesPorts || !slices.Equal(effect.Ports, []string{"443/tcp", "443/udp"}) {
		t.Fatalf("the first route with a certificate plans %+v", effect)
	}
	// Taken, it is refused by name at plan and on apply, before anything changes.
	release := s.take(t, 443, "nginx")
	if err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: secure}}); err == nil || !strings.Contains(err.Error(), "Port 443 is held by nginx") {
		t.Errorf("Check = %v, want it refused by name", err)
	}
	if err := s.Apply(t.Context(), ref, secure, effect); err == nil || !strings.Contains(err.Error(), "Port 443 is held by nginx") {
		t.Errorf("Apply = %v, want it refused by name", err)
	}
	release()
	before := slices.Clone(s.proxy.log)
	s.apply(t, "secure", secure)
	if slices.Contains(s.proxy.log[len(before):], "start") || slices.Contains(s.proxy.log[len(before):], "stop") {
		t.Errorf("taking more ports did %v, with nothing to stop or start", s.proxy.log[len(before):])
	}
	// While one wants a certificate, another changes nothing.
	if effect := s.judge(t, "other", spec("other.example.com", "", "web", 80)); effect != nil {
		t.Errorf("a second route with a certificate plans %+v", effect)
	}
	effect = s.judge(t, "secure", nil)
	if effect == nil || effect.Change != resource.IngressFreesPorts || !slices.Equal(effect.Ports, []string{"443/tcp", "443/udp"}) {
		t.Fatalf("removing the last route with a certificate plans %+v", effect)
	}
	// Editing it to take none frees them too.
	if effect := s.judge(t, "secure", plainSpec("secure.example.com")); effect == nil || effect.Change != resource.IngressFreesPorts {
		t.Errorf("turning off the last route's certificate plans %+v", effect)
	}
	// The last route stops the proxy, and says what that frees: what it holds.
	s.apply(t, "secure", nil)
	effect = s.judge(t, "plain", nil)
	if effect == nil || effect.Change != resource.IngressStops || !slices.Equal(effect.Ports, []string{"80/tcp"}) {
		t.Errorf("the last plain route plans %+v", effect)
	}
}

// A domain is served one way or the other, whatever a plan does to its routes.
func TestADomainsRoutesAgreeOnACertificate(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "a", spec("app.example.com", "", "web", 80))
	ref := routeRef("b")
	plain := &resourcev1.Route{Domain: "app.example.com", Path: "/api", Service: "web", Port: 80, Tls: TLSOff}
	err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: plain}})
	if err == nil || !strings.Contains(err.Error(), "app.example.com would have routes that take a certificate and routes that don't") {
		t.Errorf("Check = %v, want it refused", err)
	}
	// Changing them in one plan is what to do.
	all := []resource.Desired{{Ref: ref, Spec: plain}, {Ref: routeRef("a"), Spec: plainSpec("app.example.com")}}
	if err := s.Check(t.Context(), ref, all); err != nil {
		t.Errorf("both in one plan = %v", err)
	}
	// Two in one plan, as well as one against what is there.
	mixed := []resource.Desired{{Ref: ref, Spec: plain}, {Ref: routeRef("c"), Spec: spec("new.example.com", "/x", "web", 80)}, {Ref: routeRef("d"), Spec: plainSpec("new.example.com")}}
	if err := s.Check(t.Context(), ref, mixed); err == nil || !strings.Contains(err.Error(), "new.example.com") {
		t.Errorf("mixed within one plan = %v", err)
	}
	// The steps of a plan change them one by one, which applying allows.
	if err := s.Apply(t.Context(), ref, plain, nil); err != nil {
		t.Errorf("the first step of a plan that changes them both = %v", err)
	}
}

// A domain with no certificate reads as that, not pending or failed, and DNS has
// nothing to say of it, as it points at what is in front.
func TestADomainWithNoCertificateReadsNone(t *testing.T) {
	s := newSetup(t)
	s.DNS = &DNS{Lookup: func(context.Context, string) ([]netip.Addr, error) {
		t.Error("DNS was read for a domain that takes no certificate")
		return nil, nil
	}, Addrs: s.DNS.Addrs}
	s.apply(t, "www", plainSpec("app.example.com"))
	if waiting := s.watchOnce(t.Context()); waiting {
		t.Error("it reads as waiting for a certificate, and is watched closely for ever")
	}
	list := s.Domains()
	if len(list) != 1 {
		t.Fatalf("Domains = %v", list)
	}
	cert := list[0].GetCertificate()
	if cert.GetState() != dockwellv1.CertificateState_CERTIFICATE_STATE_NONE || !strings.Contains(cert.GetMessage(), "TLS ends before this server") {
		t.Errorf("its certificate reads %v", cert)
	}
	if list[0].GetDns() != nil {
		t.Errorf("DNS reads %v", list[0].GetDns())
	}
	if s.proxy.retries != 0 {
		t.Errorf("the proxy was asked to try %d times", s.proxy.retries)
	}
	// A plan says nothing of DNS either, which would tell someone behind a tunnel
	// to point it at this server.
	_, check, err := s.Judge(t.Context(), routeRef("www"), plainSpec("app.example.com"), []resource.Desired{{Ref: routeRef("www"), Spec: plainSpec("app.example.com")}})
	if err != nil || check != nil {
		t.Errorf("a plan's DNS check is %+v, %v", check, err)
	}
}

// What takes no certificate asks nothing of the certificate authority, so it can't
// use up the limit on what waits for one, and isn't recorded as waiting.
func TestPlainDomainsDontCountAsPending(t *testing.T) {
	s := newSetup(t)
	for i := range MaxPendingDomains {
		s.apply(t, "p"+string(rune('a'+i%26))+string(rune('a'+i/26)), plainSpec("p"+string(rune('a'+i%26))+string(rune('a'+i/26))+".example.com"))
	}
	// Before anything else reads the record, which drops what doesn't count.
	b, err := os.ReadFile(filepath.Join(s.Dir, domainsFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(b) > 0 {
		var added map[string]any
		if err := json.Unmarshal(b, &added); err != nil {
			t.Fatal(err)
		}
		if len(added) > 0 {
			t.Errorf("plain domains were recorded as waiting: %v", added)
		}
	}
	ref := routeRef("one")
	if err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: spec("one.example.com", "", "web", 80)}}); err != nil {
		t.Errorf("a domain beside %d plain ones = %v", MaxPendingDomains, err)
	}
	var many []resource.Desired
	for i := range MaxPendingDomains + 10 {
		name := "m" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		many = append(many, resource.Desired{Ref: routeRef(name), Spec: plainSpec(name + ".example.com")})
	}
	if err := s.Check(t.Context(), many[0].Ref, many); err != nil {
		t.Errorf("%d plain domains in one plan = %v", len(many), err)
	}
}

// A plain route given a certificate asks the authority for something new, though
// its domain is on the server already.
func TestGivingAPlainRouteACertificateCountsAsPending(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "plain", plainSpec("plain.example.com"))
	for i := range MaxPendingDomains {
		name := "r" + strconv.Itoa(i)
		s.apply(t, name, spec(name+".example.com", "", "web", 80))
	}
	ref := routeRef("plain")
	err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: spec("plain.example.com", "", "web", 80)}})
	if err == nil || !strings.Contains(err.Error(), "limit of 50") {
		t.Errorf("giving it a certificate past the limit = %v", err)
	}
	s.apply(t, "r0", nil)
	if err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: spec("plain.example.com", "", "web", 80)}}); err != nil {
		t.Errorf("with room for it = %v", err)
	}
}

// A route's failed certificate says what to do behind a tunnel, and the panel's
// doesn't, as its domain can't be served that way.
func TestAFailedCertificateNamesTheSetting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		st    CertState
		check *resource.DomainCheck
		want  bool
	}{
		{"the CA can't reach it", CertState{Phase: CertFailed, Problem: ProblemConnection}, nil, true},
		{"something else answered", CertState{Phase: CertFailed, Problem: ProblemWrongServer}, nil, true},
		{"no record", CertState{Phase: CertFailed, Problem: ProblemDNS}, nil, true},
		{"anything else", CertState{Phase: CertFailed, Problem: ProblemOther, Detail: "no"}, nil, true},
		{"renewing failed", CertState{Phase: CertIssued, Problem: ProblemConnection}, nil, true},
		{"on its way, pointed elsewhere", CertState{Phase: CertPending}, &resource.DomainCheck{State: resource.DomainElsewhere, Found: []string{"203.0.113.9"}}, true},
		{"on its way, pointed here", CertState{Phase: CertPending}, &resource.DomainCheck{State: resource.DomainHere}, false},
		{"a rate limit", CertState{Phase: CertFailed, Problem: ProblemRateLimited}, nil, false},
		{"a CAA record", CertState{Phase: CertFailed, Problem: ProblemCAA}, nil, false},
		{"a refused name", CertState{Phase: CertFailed, Problem: ProblemRefused}, nil, false},
		{"issued", CertState{Phase: CertIssued}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := routeCertMessage("app.example.com", tc.st, tc.check)
			if has := strings.Contains(got, "set tls to off"); has != tc.want {
				t.Errorf("message %q, want the setting named: %v", got, tc.want)
			}
			if strings.Contains(certMessage("panel.example.com", tc.st, tc.check), "tls to off") {
				t.Errorf("the panel's message names a setting its domain can't take")
			}
		})
	}
}

// A route can send to a port on the server, and reads back as that, with no drift,
// no network and no keeper: nothing about it is a container's.
func TestARouteToAPortOnTheServer(t *testing.T) {
	s := newSetup(t)
	s.KeeperImage = "caddy@sha256:abc"
	s.apply(t, "www", hostPortSpec(8080))
	routes, _ := s.proxy.Routes(t.Context())
	if len(routes) != 1 || !slices.Equal(routes[0].Upstreams, []string{"127.0.0.1:8080"}) {
		t.Fatalf("the proxy has %+v", routes)
	}
	got, err := s.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, hostPortSpec(8080)) {
		t.Errorf("it reads back as %v", got)
	}
	if s.engine.Network(docker.IngressNetwork) != nil {
		t.Error("a route to a port on the server made the ingress network")
	}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.engine.Network(docker.IngressNetwork) != nil || s.engine.Container(KeeperName) != nil {
		t.Error("a reconcile made the ingress network for a route to a port on the server")
	}
	// Beside it a service's route is on the network as ever, and going leaves the
	// other route where it was.
	s.apply(t, "svc", spec("svc.example.com", "", "web", 80))
	if s.engine.Network(docker.IngressNetwork) == nil {
		t.Error("a service's route has no network")
	}
	if len(s.table.Addresses) != 1 {
		t.Errorf("the table is %v", s.table.Addresses)
	}
	s.apply(t, "svc", nil)
	if len(s.table.Addresses) != 0 {
		t.Errorf("a service with no route left kept its address: %v, as a port on the server read as what can't be known", s.table.Addresses)
	}
	// What else it dials reads as drift.
	s.proxy.routes[0].Upstreams = []string{"127.0.0.1:9090", "127.0.0.1:8080"}
	got, err = s.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if proto.Equal(got, hostPortSpec(8080)) {
		t.Errorf("a second upstream by hand reads back as nothing: %v", got)
	}
	s.proxy.routes[0].Upstreams = []string{"10.0.0.5:8080"}
	got, err = s.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if proto.Equal(got, hostPortSpec(8080)) || got.GetHostPort() != 0 {
		t.Errorf("another address reads back as %v", got)
	}
}

// No route reaches the panel, or anything else Dockwell listens on, or dials the
// proxy itself.
func TestARouteCantReachDockwellsOwnPorts(t *testing.T) {
	s := newSetup(t)
	s.OwnPorts = []int{7600, 7443}
	for _, tc := range []struct {
		port uint32
		says string
	}{
		{7600, "one Dockwell listens on itself"},
		{7443, "one Dockwell listens on itself"},
		{80, "the one Caddy listens on"},
		{443, "the one Caddy listens on"},
	} {
		ref := routeRef("www")
		route := hostPortSpec(tc.port)
		if err := s.Check(t.Context(), ref, []resource.Desired{{Ref: ref, Spec: route}}); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Check of port %d = %v", tc.port, err)
		}
		// On apply too, for a plan made while the server wasn't there to ask.
		if err := s.Apply(t.Context(), ref, route, nil); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Apply of port %d = %v", tc.port, err)
		}
	}
	if routes, _ := s.proxy.Routes(t.Context()); len(routes) != 0 {
		t.Errorf("the proxy has %+v", routes)
	}
	s.apply(t, "www", hostPortSpec(8080))
}

// What a route names as a proxy goes to the proxy as ranges, and reads back as
// what it named, so a preset isn't drift.
func TestTrustedProxiesReadBackAsTheyWereNamed(t *testing.T) {
	s := newSetup(t)
	route := &resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 80, TrustedProxies: []string{"127.0.0.1/32", PresetCloudflare}}
	s.apply(t, "www", route)
	routes, _ := s.proxy.Routes(t.Context())
	if len(routes) != 1 || !slices.Contains(routes[0].TrustedProxies, "127.0.0.1/32") || !slices.Contains(routes[0].TrustedProxies, "173.245.48.0/20") || slices.Contains(routes[0].TrustedProxies, PresetCloudflare) {
		t.Fatalf("the proxy has %+v", routes)
	}
	got, err := s.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, route) {
		t.Errorf("it reads back as %v, want %v", got, route)
	}
	// A list that isn't all of the preset's reads as what it holds, which is drift.
	s.proxy.routes[0].TrustedProxies = slices.DeleteFunc(slices.Clone(routes[0].TrustedProxies), func(r string) bool { return r == "173.245.48.0/20" })
	got, err = s.Observe(t.Context(), routeRef("www"))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.GetTrustedProxies(), PresetCloudflare) || proto.Equal(got, route) {
		t.Errorf("a list with a range gone reads back as %v", got.GetTrustedProxies())
	}
}

func TestNormalizeTrustedProxies(t *testing.T) {
	got, err := NormalizeTrustedProxies([]string{"10.0.0.5/8", "cloudflare", "203.0.113.9", "2001:db8::1", "203.0.113.9/32"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "2001:db8::1/128", "203.0.113.9/32", "cloudflare"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// However it's written, it's what reads back: a route applied this way must
	// compare equal to what its proxy has.
	if got, err := NormalizeTrustedProxies(append(slices.Clone(cloudflareRanges), "127.0.0.1")); err != nil || !slices.Equal(got, []string{"127.0.0.1/32", PresetCloudflare}) {
		t.Errorf("a preset's ranges spelled out read %v, %v", got, err)
	}
	if got, err := NormalizeTrustedProxies([]string{cloudflareRanges[0], PresetCloudflare}); err != nil || !slices.Equal(got, []string{PresetCloudflare}) {
		t.Errorf("a range of a preset beside its name reads %v, %v", got, err)
	}
	if got, err := NormalizeTrustedProxies(nil); err != nil || len(got) != 0 {
		t.Errorf("none reads %v, %v", got, err)
	}
	for _, tc := range []struct{ entry, says string }{
		{"0.0.0.0/0", "covers too much"},
		{"::/0", "covers too much"},
		{"128.0.0.0/1", "covers too much"},
		{"0.0.0.0/7", "covers too much"},
		{"0.0.0.0", "covers too much"},
		{"::", "covers too much"},
		{"::ffff:0:0/96", "spells an IPv4 address"},
		{"::ffff:1.2.3.4", "spells an IPv4 address"},
		{"::/8", "covers too much"},
		{"::ff00:0:0/88", "covers too much"}, // past ::, and still over all of IPv4
		{"everyone", "isn't an address"},
		{"Cloudflare", "isn't an address"},
		{"{env.HOME}", "isn't an address"},
		{"fe80::1%eth0", "names a network interface"},
		{"", "isn't an address"},
	} {
		if _, err := NormalizeTrustedProxies([]string{tc.entry}); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%q = %v, want it refused, saying %q", tc.entry, err, tc.says)
		}
	}
	if _, err := NormalizeTrustedProxies(make([]string, maxTrustedProxies+1)); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("too many = %v", err)
	}
}
