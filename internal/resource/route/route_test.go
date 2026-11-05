// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

func spec(domain, path string) *resourcev1.Route {
	return &resourcev1.Route{Domain: domain, Path: path, Service: "web", Port: 8080}
}

// Everything a token sends ends up in Caddy's config, so nothing gets through
// here that could become anything but the name it is.
func TestValidateRefusesWhatCantBeARoute(t *testing.T) {
	d := &Driver{}
	for _, tc := range []struct {
		name string
		spec *resourcev1.Route
		says string
	}{
		{"no domain", spec("", ""), "needs a domain"},
		{"a wildcard", spec("*.example.com", ""), "wildcard"},
		{"a wildcard further in", spec("a.*.example.com", ""), `has a '*'`},
		{"an IPv4 address", spec("203.0.113.9", ""), "IP address"},
		{"an IPv6 address", spec("2001:db8::1", ""), "IP address"},
		{"an IPv6 address in brackets", spec("[2001:db8::1]", ""), "IP address"},
		{"no dot", spec("localhost", ""), "needs a dot"},
		{"a trailing dot", spec("app.example.com.", ""), "ends in a dot"},
		{"capitals", spec("App.Example.com", ""), "lowercase"},
		{"unicode", spec("bücher.example", ""), "punycode"},
		{"a placeholder", spec("{env.HOME}.example.com", ""), `has a '{'`},
		{"a placeholder's end", spec("a}.example.com", ""), `has a '}'`},
		{"a quote", spec(`a".example.com`, ""), `has a '"'`},
		{"a space", spec("a b.example.com", ""), `has a ' '`},
		{"a newline", spec("a\nb.example.com", ""), `has a '\n'`},
		{"a slash", spec("a/b.example.com", ""), `has a '/'`},
		{"a colon and port", spec("app.example.com:8080", ""), `has a ':'`},
		{"an underscore", spec("a_b.example.com", ""), `has a '_'`},
		{"an empty part", spec("a..example.com", ""), "isn't a valid domain"},
		{"a leading dash", spec("-a.example.com", ""), "isn't a valid domain"},
		{"a trailing dash", spec("a-.example.com", ""), "isn't a valid domain"},
		{"a long label", spec(strings.Repeat("a", 64)+".example.com", ""), "isn't a valid domain"},
		{"too long", spec(strings.Repeat("a.", 130)+"com", ""), "253"},
		{"a number at the end", spec("a.123", ""), "ends in a number"},
		{"a reserved name", spec("app.local", ""), ".local"},
		{"another reserved name", spec("a.localhost", ""), ".localhost"},
		{"an internal name", spec("db.internal", ""), ".internal"},
		{"an onion", spec("a.onion", ""), ".onion"},

		{"a path without its slash", spec("a.example.com", "api"), "start with a /"},
		{"a path's trailing slash", spec("a.example.com", "/api/"), "ends in a /"},
		{"an empty part in a path", spec("a.example.com", "/a//b"), "empty part"},
		{"a dot segment", spec("a.example.com", "/a/./b"), "a . in it"},
		{"a dot-dot segment", spec("a.example.com", "/a/../b"), "a .. in it"},
		{"capitals in a path", spec("a.example.com", "/Api"), "lowercase"},
		{"a wildcard in a path", spec("a.example.com", "/api/*"), `has a '*'`},
		{"a placeholder in a path", spec("a.example.com", "/{http.request.host}"), `has a '{'`},
		{"an escape in a path", spec("a.example.com", "/a%2fb"), `has a '%'`},
		{"a query", spec("a.example.com", "/a?b=c"), `has a '?'`},
		{"a long path", spec("a.example.com", "/"+strings.Repeat("a", 1100)), "1024"},

		{"no service", &resourcev1.Route{Domain: "a.example.com", Port: 80}, "needs a service"},
		{"a service that isn't a name", &resourcev1.Route{Domain: "a.example.com", Service: "web/../x", Port: 80}, "isn't a valid service name"},
		{"no port", &resourcev1.Route{Domain: "a.example.com", Service: "web"}, "from 1 to 65535"},
		{"a port too high", &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 70000}, "from 1 to 65535"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.Validate(proto.Clone(tc.spec))
			if err == nil {
				t.Fatalf("Validate(%v) took it", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("Validate(%v) = %q, want it to say %q", tc.spec, err, tc.says)
			}
		})
	}
}

func TestValidateTakesWhatARouteCanBe(t *testing.T) {
	d := &Driver{}
	for _, s := range []*resourcev1.Route{
		spec("app.example.com", ""),
		spec("a.b.c.example.co.uk", "/api/v1"),
		spec("xn--bcher-kva.example.com", "/a-b_c.d~e"),
		spec("1.example.com", ""),
		{Domain: "app.example.com", Service: "api-v2", Port: 65535},
	} {
		if err := d.Validate(proto.Clone(s)); err != nil {
			t.Errorf("Validate(%v) = %v", s, err)
		}
	}
}

func TestSlashIsTheWholeDomainAsEmptyIs(t *testing.T) {
	d := &Driver{}
	a, b := spec("app.example.com", "/"), spec("app.example.com", "")
	if err := d.Validate(a); err != nil {
		t.Fatal(err)
	}
	if err := d.Validate(b); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(a, b) {
		t.Errorf("%v and %v mean the same, and compare different", a, b)
	}
}

func TestThePanelsDomainCantBeClaimed(t *testing.T) {
	d := &Driver{Reserved: "panel.example.com"}
	for _, domain := range []string{"panel.example.com", "x.panel.example.com", "a.b.panel.example.com"} {
		if err := d.Validate(spec(domain, "")); err == nil || !strings.Contains(err.Error(), "panel") {
			t.Errorf("a route for %s = %v, want it refused, naming the panel", domain, err)
		}
	}
	for _, domain := range []string{"example.com", "app.example.com", "panel.example.org", "xpanel.example.com"} {
		if err := d.Validate(spec(domain, "")); err != nil {
			t.Errorf("a route for %s = %v", domain, err)
		}
	}
}

func TestAClaimIsItsDomainAndPath(t *testing.T) {
	d := &Driver{}
	got := d.Claims(spec("app.example.com", "/api"))
	if len(got) != 1 || got[0].Key != "app.example.com/api" || got[0].Group != "app.example.com" {
		t.Errorf("Claims = %+v", got)
	}
}

func TestAServiceReachIsntARoutes(t *testing.T) {
	d := &Driver{}
	refs := d.References(spec("app.example.com", ""))
	if len(refs) != 1 || refs[0].Name != "web" || !refs[0].Apart {
		t.Errorf("References = %+v, want the service, apart from what it reaches", refs)
	}
}

// A route that dials a port on the server can say whose it is. That's all its service
// is to it: the host access is the port's, and what the service reaches stays apart.
func TestARouteToAPortOnTheServerCanSayWhoseItIs(t *testing.T) {
	d := &Driver{}
	s := &resourcev1.Route{Domain: "a.example.com", HostPort: 8080, Service: "onhost"}
	if err := d.Validate(proto.Clone(s)); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	refs := d.References(s)
	if len(refs) != 1 || refs[0].Name != "onhost" || refs[0].Verb != "serve" || !refs[0].Apart {
		t.Errorf("References = %+v, want the service, apart from what it reaches", refs)
	}
	got := d.HostAccess(s)
	if len(got) != 1 || got[0].Kind != resource.AccessServerPort {
		t.Errorf("HostAccess = %+v, want the port on the server, as with no service", got)
	}
	if err := d.Validate(&resourcev1.Route{Domain: "a.example.com", HostPort: 8080}); err != nil {
		t.Errorf("a port that is no service's: %v", err)
	}
}

// A route leaving the service it sent to says so, as that's easy to miss among its fields.
func TestARouteMovingBetweenServicesSaysSo(t *testing.T) {
	to := func(service string) *resourcev1.Route {
		return &resourcev1.Route{Domain: "a.example.com", Path: "/api", Service: service, Port: 80}
	}
	dec, err := (&Driver{}).Plan(to("api"), to("nginx"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := dec.Moves; m == nil || m.Where != "a.example.com/api" || m.From != "api" || m.To != "nginx" {
		t.Errorf("Moves = %+v", m)
	}
	dec, _ = (&Driver{}).Plan(to("api"), &resourcev1.Route{Domain: "a.example.com", HostPort: 8080}, nil)
	if m := dec.Moves; m == nil || m.From != "api" || m.To != "" || m.ToPort != 8080 {
		t.Errorf("to a port on the server, Moves = %+v", m)
	}
	// Staying with its service isn't a move, and what was no service's hasn't left one.
	for name, tc := range map[string][2]*resourcev1.Route{
		"the same service": {to("api"), to("api")},
		"from no service":  {{Domain: "a.example.com", HostPort: 8080}, to("nginx")},
	} {
		if dec, _ := (&Driver{}).Plan(tc[0], tc[1], nil); dec.Moves != nil {
			t.Errorf("%s: Moves = %+v", name, dec.Moves)
		}
	}
}

// On a route to a port on the server, service says whose it is, and where it dials
// comes from the port. So a token can change or remove it on what a person approved:
// that moves the route between apps' pages and delete plans, and reaches nothing more.
func TestAServiceOnARouteToAPortIsNoMoreReach(t *testing.T) {
	d := &Driver{}
	route := func(service string) *resourcev1.Route {
		return &resourcev1.Route{Domain: "a.example.com", Service: service, HostPort: 8080}
	}
	want := d.HostAccess(route(""))
	if len(want) != 1 {
		t.Fatalf("a route to a port on the server reaches %+v", want)
	}
	for _, service := range []string{"web", "api"} {
		if got := d.HostAccess(route(service)); !reflect.DeepEqual(got, want) {
			t.Errorf("with the service %s it reaches %+v, want %+v", service, got, want)
		}
	}
}

// A plan made with no word from the server still says the route goes, and where.
func TestARouteGoingSaysWhereWithNoWordFromTheServer(t *testing.T) {
	dec, err := (&Driver{}).Plan(spec("a.example.com", "/api"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Goes == nil || dec.Goes.Where != "a.example.com/api" || dec.Goes.Domain != "a.example.com" || dec.Goes.Find != resource.VisitorsLoseIt {
		t.Errorf("Goes = %+v", dec.Goes)
	}
	if dec, _ := (&Driver{}).Plan(nil, spec("a.example.com", ""), nil); dec.Goes != nil {
		t.Errorf("a route being made goes: %+v", dec.Goes)
	}
}

// sayingIngress is a server that has its own word on a route going.
type sayingIngress struct {
	Ingress
	goes   *resource.RouteGoes
	asked  int
	seeing []resource.Desired
}

func (*sayingIngress) Judge(context.Context, resource.Ref, *resourcev1.Route, []resource.Desired) (*resource.IngressEffect, *resource.DomainCheck, error) {
	return nil, nil, nil
}

func (*sayingIngress) Returning(context.Context, resource.Ref, *resourcev1.Route, []resource.Desired) []string {
	return nil
}

func (i *sayingIngress) Goes(_ context.Context, _ resource.Ref, desired []resource.Desired) *resource.RouteGoes {
	i.asked++
	i.seeing = desired
	return i.goes
}

// A server's word on what a visitor finds replaces what a plan can say without it,
// from what the whole plan leaves, and a route that stays isn't asked about.
func TestARouteGoingTakesTheServersWord(t *testing.T) {
	ref := resource.Ref{Kind: Kind, Scope: "env_1", Name: "www"}
	desired := []resource.Desired{{Ref: ref}}
	ctx := resource.WithDesired(t.Context(), desired)
	said := &resource.RouteGoes{Where: "a.example.com", Domain: "a.example.com", Find: resource.VisitorsRefused}

	asked := &sayingIngress{goes: said}
	dec, err := (&Driver{Ingress: asked}).Judge(ctx, ref, spec("a.example.com", ""), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Goes != said {
		t.Errorf("Goes = %+v, want the server's", dec.Goes)
	}
	if len(asked.seeing) != 1 || asked.seeing[0].Ref != ref {
		t.Errorf("the server was asked with %+v, want what the plan touches", asked.seeing)
	}

	// A proxy that doesn't have the route has no word on it, so the plan's own stays.
	dec, err = (&Driver{Ingress: &sayingIngress{}}).Judge(ctx, ref, spec("a.example.com", ""), nil, nil)
	if err != nil || dec.Goes == nil || dec.Goes.Where != "a.example.com" || dec.Goes.Find != resource.VisitorsLoseIt {
		t.Errorf("with no word from the server, Goes = %+v, %v", dec.Goes, err)
	}

	staying := &sayingIngress{goes: said}
	dec, err = (&Driver{Ingress: staying}).Judge(ctx, ref, spec("a.example.com", ""), spec("a.example.com", ""), nil)
	if err != nil || dec.Goes != nil || staying.asked != 0 {
		t.Errorf("a route that stays: Goes = %+v, asked %d, %v", dec.Goes, staying.asked, err)
	}
}

func TestARoutesStepsStopNothing(t *testing.T) {
	d := &Driver{}
	for _, tc := range []struct {
		actual, desired proto.Message
		want            resource.Action
	}{
		{nil, spec("a.example.com", ""), resource.ActionCreate},
		{spec("a.example.com", ""), spec("a.example.com", "/x"), resource.ActionUpdate},
		{spec("a.example.com", ""), nil, resource.ActionDelete},
	} {
		dec, err := d.Plan(tc.actual, tc.desired, nil)
		if err != nil {
			t.Fatal(err)
		}
		if dec.Action != tc.want || dec.Downtime != 0 || dec.Destroys || dec.Effect != resource.EffectNone {
			t.Errorf("Plan(%v, %v) = %+v", tc.actual, tc.desired, dec)
		}
	}
}

// A reserved name is refused with a certificate or without, and without one, for
// a reason that isn't about a certificate, and with what to do instead.
func TestAReservedNameSaysWhyWithNoCertificateToo(t *testing.T) {
	d := &Driver{}
	withCert := d.Validate(&resourcev1.Route{Domain: "app.internal", Service: "web", Port: 80})
	if withCert == nil || !strings.Contains(withCert.Error(), "no public certificate authority issues for it") {
		t.Errorf("with a certificate: %v", withCert)
	}
	without := d.Validate(&resourcev1.Route{Domain: "app.local", Service: "web", Port: 80, Tls: "off"})
	if without == nil || strings.Contains(without.Error(), "certificate authority") || !strings.Contains(without.Error(), "public name whose DNS points at this server's private address") {
		t.Errorf("with none: %v", without)
	}
	// What's wrong for any other reason says that, whatever tls says.
	other := d.Validate(&resourcev1.Route{Domain: "nodot", Service: "web", Port: 80, Tls: "off"})
	if other == nil || !strings.Contains(other.Error(), "needs a dot") {
		t.Errorf("not a reserved name: %v", other)
	}
}

// Each rule of the fields a route behind a tunnel uses, on its own.
func TestValidateRefusesWhatARouteCantSayOfWhereItSends(t *testing.T) {
	d := &Driver{Reserved: "panel.example.com"}
	for _, tc := range []struct {
		name string
		spec *resourcev1.Route
		says string
	}{
		{"a tls it doesn't know", &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, Tls: "on"}, `takes only "off"`},
		{"a provider's name for tls", &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, Tls: "cloudflare"}, `takes only "off"`},
		{"the panel's domain, plain", &resourcev1.Route{Domain: "panel.example.com", Service: "web", Port: 80, Tls: "off"}, "passkeys and cookies need HTTPS"},
		{"under the panel's domain, plain", &resourcev1.Route{Domain: "a.panel.example.com", Service: "web", Port: 80, Tls: "off"}, "passkeys and cookies need HTTPS"},
		{"neither service nor port on the server", &resourcev1.Route{Domain: "a.example.com"}, "host_port"},
		{"a port on the server past 65535", &resourcev1.Route{Domain: "a.example.com", HostPort: 70000}, "from 1 to 65535"},
		{"a port on the server and a service's port", &resourcev1.Route{Domain: "a.example.com", HostPort: 8080, Port: 80}, "dial only one"},
		{"a port on the server, and a service and its port", &resourcev1.Route{Domain: "a.example.com", HostPort: 8080, Service: "web", Port: 80}, "dial only one"},
		{"a port on the server, for what can't be a service", &resourcev1.Route{Domain: "a.example.com", HostPort: 8080, Service: "No Such"}, "service"},
		{"everyone as a proxy", &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, TrustedProxies: []string{"0.0.0.0/0"}}, "covers too much"},
		{"what isn't a proxy", &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, TrustedProxies: []string{"{env.HOME}"}}, "isn't an address"},
		{"Docker's API", &resourcev1.Route{Domain: "a.example.com", HostPort: 2375}, "Docker's API"},
		{"Docker's API over TLS", &resourcev1.Route{Domain: "a.example.com", HostPort: 2376, Tls: "off"}, "Docker's API"},
		{"anyone, with a certificate", &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, PlainForAnyone: true}, "plain_for_anyone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.Validate(proto.Clone(tc.spec))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("Validate = %v, want it refused, saying %q", err, tc.says)
			}
		})
	}
}

func TestValidateTakesARouteBehindATunnel(t *testing.T) {
	d := &Driver{Reserved: "panel.example.com"}
	r := &resourcev1.Route{Domain: "a.example.com", HostPort: 8080, Tls: "off", TrustedProxies: []string{"cloudflare", "127.0.0.1", "cloudflare"}}
	if err := d.Validate(r); err != nil {
		t.Fatal(err)
	}
	if want := []string{"127.0.0.1/32", "cloudflare"}; !slices.Equal(r.GetTrustedProxies(), want) {
		t.Errorf("trusted proxies = %v, want them canonical, as they read back", r.GetTrustedProxies())
	}
	empty := &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, TrustedProxies: []string{}}
	if err := d.Validate(empty); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(empty, spec("a.example.com", "")) && empty.GetTrustedProxies() != nil {
		t.Errorf("an empty list stays %v", empty.GetTrustedProxies())
	}
}

// What a person approves of a route, and that nothing else about it needs one.
func TestWhatAPersonApprovesOfARoute(t *testing.T) {
	d := &Driver{}
	if got := d.HostAccess(spec("a.example.com", "")); got != nil {
		t.Errorf("an ordinary route reaches %v", got)
	}
	if got := d.HostAccess(&resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, Tls: "off"}); got != nil {
		t.Errorf("a route that takes no certificate reaches %v", got)
	}
	got := d.HostAccess(&resourcev1.Route{Domain: "a.example.com", Path: "/x", HostPort: 8080, TrustedProxies: []string{"10.0.0.0/8", "cloudflare"}})
	want := []resource.Access{
		{Kind: resource.AccessServerPort, Path: "127.0.0.1:8080", Detail: "a.example.com/x"},
		{Kind: resource.AccessTrustedProxy, Path: "10.0.0.0/8"},
		{Kind: resource.AccessTrustedProxy, Path: "cloudflare"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("HostAccess = %v, want %v", got, want)
	}
	// A port approved at one place and way isn't approved at another: a person who
	// approved it behind an access proxy on one name hasn't approved it on any other.
	moved := d.HostAccess(&resourcev1.Route{Domain: "b.example.com", HostPort: 8080})
	if g := resource.NewAccess(d.HostAccess(&resourcev1.Route{Domain: "a.example.com", HostPort: 8080}), moved); len(g) != 1 {
		t.Errorf("another domain gains %v", g)
	}
	plainly := d.HostAccess(&resourcev1.Route{Domain: "a.example.com", HostPort: 8080, Tls: "off"})
	if g := resource.NewAccess(d.HostAccess(&resourcev1.Route{Domain: "a.example.com", HostPort: 8080}), plainly); len(g) != 1 || g[0].Type != "http" {
		t.Errorf("plain HTTP gains %v", g)
	}
	if g := resource.NewAccess(plainly, d.HostAccess(&resourcev1.Route{Domain: "a.example.com", HostPort: 8080})); len(g) != 1 {
		t.Errorf("a certificate, which answers everyone, gains %v", g)
	}
	// Anyone is on a domain, so another domain is another approval.
	anyone := func(domain string) []resource.Access {
		return d.HostAccess(&resourcev1.Route{Domain: domain, Service: "web", Port: 80, Tls: "off", PlainForAnyone: true})
	}
	if g := anyone("a.example.com"); len(g) != 1 || g[0].Kind != resource.AccessPlainForAnyone {
		t.Errorf("anyone plans %v", g)
	}
	if g := resource.NewAccess(anyone("a.example.com"), anyone("b.example.com")); len(g) != 1 {
		t.Errorf("anyone on another domain gains %v", g)
	}
}

// A plan says who a route answers when it isn't everyone over HTTPS, from its spec,
// as a server that's offline can't be asked.
func TestAPlanSaysWhoARouteAnswers(t *testing.T) {
	d := &Driver{}
	for _, tc := range []struct {
		name string
		spec *resourcev1.Route
		want *resource.RouteAnswers
		port uint32
	}{
		{"an ordinary route", spec("a.example.com", ""), nil, 0},
		{"plain, naming none", &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, Tls: "off"}, &resource.RouteAnswers{Reach: resource.ReachThisServer, Plain: true}, 0},
		{"plain, naming a proxy", &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, Tls: "off", TrustedProxies: []string{"cloudflare"}}, &resource.RouteAnswers{Reach: resource.ReachProxies, Proxies: []string{"cloudflare"}, Plain: true}, 0},
		{"plain for anyone", &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, Tls: "off", PlainForAnyone: true}, &resource.RouteAnswers{Reach: resource.ReachAnyone, Plain: true}, 0},
		{"a port, with a certificate", &resourcev1.Route{Domain: "a.example.com", HostPort: 8080}, &resource.RouteAnswers{Reach: resource.ReachAnyone}, 8080},
		{"a port, plain", &resourcev1.Route{Domain: "a.example.com", HostPort: 8080, Tls: "off"}, &resource.RouteAnswers{Reach: resource.ReachThisServer, Plain: true}, 8080},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := d.Plan(nil, tc.spec, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(dec.Answers, tc.want) {
				t.Errorf("Answers = %+v, want %+v", dec.Answers, tc.want)
			}
			if (dec.ServerPort != nil) != (tc.port != 0) || (dec.ServerPort != nil && dec.ServerPort.Port != tc.port) {
				t.Errorf("ServerPort = %+v, want %d", dec.ServerPort, tc.port)
			}
		})
	}
}

// How a route serves its domain is what the engine holds a domain's routes to.
func TestAClaimSaysHowItServes(t *testing.T) {
	d := &Driver{}
	secure := d.Claims(spec("a.example.com", ""))[0]
	plainly := d.Claims(&resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, Tls: "off"})[0]
	if secure.Manner == plainly.Manner {
		t.Errorf("they claim the same way: %+v and %+v", secure, plainly)
	}
	if !d.Weak(&resourcev1.Route{Tls: "off"}) || d.Weak(spec("a.example.com", "")) {
		t.Error("what's weak is the wrong way round")
	}
}

// A plan says when a route starts serving plain HTTP, whether or not its server can
// be asked, and not each time something else about it changes.
func TestAPlanSaysWhenARouteGoesUnencrypted(t *testing.T) {
	d := &Driver{}
	secure, plain := spec("a.example.com", ""), &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 80, Tls: "off"}
	plain2 := &resourcev1.Route{Domain: "a.example.com", Service: "web", Port: 8080, Tls: "off"}
	for _, tc := range []struct {
		name            string
		actual, desired proto.Message
		want            bool
	}{
		{"a new plain route", nil, plain, true},
		{"a new secure route", nil, secure, false},
		{"turning a certificate off", secure, plain, true},
		{"turning one on", plain, secure, false},
		{"another change to a plain route", plain, plain2, false},
		{"removing a plain route", plain, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := d.Plan(tc.actual, tc.desired, nil)
			if err != nil {
				t.Fatal(err)
			}
			if dec.Unencrypted != tc.want {
				t.Errorf("Unencrypted = %v, want %v", dec.Unencrypted, tc.want)
			}
			if len(dec.Warnings) > 0 || dec.Destroys {
				t.Errorf("it needs a person: %+v", dec)
			}
		})
	}
}

// A route holds nothing secret, so its stored spec isn't sealed, and what reads
// routes directly doesn't need the keeper: ListDomains, and dockwell run's refusal
// of a domain a route claims. If a field here is ever marked sensitive, those must
// open it as resource.Sealed then asks.
func TestARouteIsntSealed(t *testing.T) {
	if resource.Sealed(&Driver{}) {
		t.Error("a route's spec is marked as holding something sensitive, so ListDomains and refuseClaimedDomain need to open it through the keeper")
	}
}
