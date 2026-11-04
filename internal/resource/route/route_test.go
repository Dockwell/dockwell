// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
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
