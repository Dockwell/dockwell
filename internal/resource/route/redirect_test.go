// SPDX-License-Identifier: AGPL-3.0-only

package route

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

func redirect(domain, path, to string, dropPath bool) *resourcev1.Route {
	return &resourcev1.Route{Domain: domain, Path: path, Redirect: &resourcev1.RouteRedirect{To: to, DropPath: dropPath}}
}

// A redirect sends nowhere but where it says, and only where a browser can go (ADR
// 0070). What it says ends up in Caddy's config, so nothing gets through that Caddy
// would read as its own words.
func TestARedirectTakesOnlyWhereABrowserCanGo(t *testing.T) {
	d := &Driver{}
	for _, tc := range []struct {
		name string
		spec *resourcev1.Route
		says string
	}{
		{"a service too", &resourcev1.Route{Domain: "www.example.com", Service: "web", Port: 8080, Redirect: &resourcev1.RouteRedirect{To: "https://example.com"}}, "sends to no service"},
		{"a port on the server too", &resourcev1.Route{Domain: "www.example.com", HostPort: 3000, Redirect: &resourcev1.RouteRedirect{To: "https://example.com"}}, "sends to no service"},
		{"nowhere", redirect("www.example.com", "", "", false), "needs where to"},
		{"no scheme", redirect("www.example.com", "", "example.com", false), "isn't an address a browser can go to"},
		{"another scheme", redirect("www.example.com", "", "ftp://example.com", false), "isn't an address a browser can go to"},
		{"javascript", redirect("www.example.com", "", "javascript:alert(1)", false), "isn't an address a browser can go to"},
		{"JavaScript, in capitals", redirect("www.example.com", "", "JaVaScRiPt://example.com/%0aalert(1)", false), "isn't an address a browser can go to"},
		{"data", redirect("www.example.com", "", "data:text/html,hi", true), "isn't an address a browser can go to"},
		{"a bare path", redirect("www.example.com", "", "/blog", false), "isn't an address a browser can go to"},
		{"a host without a scheme", redirect("www.example.com", "", "//example.org/blog", false), "isn't an address a browser can go to"},
		// A redirect has nothing behind it to protect (ADR 0070, with ADR 0087).
		{"a password", &resourcev1.Route{Domain: "admin.example.com", Access: &resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{{Name: "ci", PasswordHash: bcryptHash(t)}}}, Redirect: &resourcev1.RouteRedirect{To: "https://example.com"}}, "asks for a password, and a redirect has nothing behind it to protect. Clear its access in the same change"},
		{"some addresses", &resourcev1.Route{Domain: "admin.example.com", Access: &resourcev1.RouteAccess{Allow: []string{"198.51.100.0/24"}}, Redirect: &resourcev1.RouteRedirect{To: "https://example.com"}}, "lets in only some addresses"},
		{"a sign-in", &resourcev1.Route{Domain: "admin.example.com", Access: &resourcev1.RouteAccess{SignIn: &resourcev1.RouteSignIn{Provider: "team", EmailDomains: []string{"example.com"}}}, Redirect: &resourcev1.RouteRedirect{To: "https://example.com"}}, "asks visitors to sign in"},
		{"no host", redirect("www.example.com", "", "https:///path", false), "isn't an address a browser can go to"},
		{"a user", redirect("www.example.com", "", "https://me:pw@example.com", false), "user name"},
		{"a fragment", redirect("www.example.com", "", "https://example.com/#top", true), "# part"},
		{"a query, keeping the path", redirect("www.example.com", "", "https://example.com/?a=1", false), "has a query"},
		{"a placeholder", redirect("www.example.com", "", "https://{http.request.host}", false), "{ or }"},
		{"a space", redirect("www.example.com", "", "https://example.com/a b", true), "space"},
		{"a host not in ASCII", redirect("www.example.com", "", "https://bücher.example", false), "xn-- form"},
		{"too long", redirect("www.example.com", "", "https://example.com/"+strings.Repeat("a", 2100), true), "longer than 2048"},
		// Alone, a route knows it loops where it lands on its own path exactly.
		{"itself", redirect("example.com", "", "https://example.com", false), "round in a circle"},
		{"itself, by case and port", redirect("www.example.com", "", "https://WWW.example.com:443/", false), "round in a circle"},
		{"its own root, whatever the path", redirect("example.com", "", "https://example.com/", true), "round in a circle"},
		{"its own path", redirect("example.com", "/x", "https://example.com/x", true), "round in a circle"},
		{"its own prefix, kept", &resourcev1.Route{Domain: "example.com", Path: "/old", Redirect: &resourcev1.RouteRedirect{To: "https://example.com", KeepPrefix: true}}, "round in a circle"},
		// Caddy matches a path decoded and without case, so these land where they start.
		{"its own path, in capitals", redirect("example.com", "/x", "https://example.com/X", true), "round in a circle"},
		{"its own path, encoded", redirect("example.com", "/y", "https://example.com/%79", true), "round in a circle"},
		// A port a browser can't go to, and a domain no site answers to with its dot.
		{"port 0", redirect("www.example.com", "", "https://example.com:0", false), "from 1 to 65535"},
		{"a port past 65535", redirect("www.example.com", "", "https://example.com:99999", false), "from 1 to 65535"},
		{"a domain ending in a dot", redirect("www.example.com", "", "https://example.com./", false), "ends in a dot"},
		// What the slice 3 review typed into Add a domain's Redirect to.
		{"a line break", redirect("www.example.com", "", "https://evil.example/\r\nSet-Cookie:x", true), "control character"},
		{"one slash", redirect("www.example.com", "", "https:/evil.example", false), "isn't an address a browser can go to"},
		{"backslashes", redirect("www.example.com", "", `https:\\evil.example`, false), "isn't an address a browser can go to"},
		{"a letter that looks like a Latin one", redirect("www.example.com", "", "https://ex\u0430mple.com", false), "xn-- form"},
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

func TestARedirectTakesWhatOneCanBe(t *testing.T) {
	d := &Driver{}
	for _, s := range []*resourcev1.Route{
		redirect("www.example.com", "", "https://example.com", false),
		redirect("old.example.com", "", "https://example.org/blog", false),
		// The most common path redirect: what follows /blog goes after /articles.
		redirect("example.com", "/blog", "https://example.com/articles", false),
		// Whether these loop depends on what else answers there, which CheckAcross says.
		redirect("example.com", "", "https://example.com/blog", true),
		redirect("example.com", "/old", "https://example.com/old/new", false),
		redirect("promo.example.com", "", "https://example.com/?from=promo", true),
		redirect("www.example.net", "", "http://example.net", false),
		{Domain: "www.example.net", Tls: "off", TrustedProxies: []string{"127.0.0.1"}, Redirect: &resourcev1.RouteRedirect{To: "https://example.net", Temporary: true}},
		// A domain in its xn-- form, an address, and a line break kept encoded.
		redirect("old.example.net", "", "https://xn--bcher-kva.example", false),
		redirect("old.example.net", "", "http://[2001:db8::1]:8080", false),
		redirect("old.example.net", "", "https://example.org/%0d%0a", true),
	} {
		if err := d.Validate(proto.Clone(s)); err != nil {
			t.Errorf("Validate(%v) = %v", s, err)
		}
	}
}

// to is stored one way of writing its address, so the checks after compare like with
// like, and what's kept never doubles a /.
func TestWhereARedirectSendsIsNormalized(t *testing.T) {
	for _, c := range []struct {
		to, path string
		drop     bool
		want     string
	}{
		{"https://Example.COM:443/", "", false, "https://example.com"},
		{"HTTP://example.com:80/blog/", "", false, "http://example.com/blog"},
		{"https://example.com:8443/", "", false, "https://example.com:8443"},
		{"https://example.com/", "", true, "https://example.com/"},
		{"https://example.com/?from=promo", "", true, "https://example.com/?from=promo"},
		{"https://[2001:DB8::1]:443/x/", "", false, "https://[2001:db8::1]/x"},
	} {
		r := &resourcev1.Route{Domain: "www.example.org", Path: c.path, Redirect: &resourcev1.RouteRedirect{To: c.to, DropPath: c.drop}}
		if err := (&Driver{}).Validate(r); err != nil {
			t.Fatalf("Validate(%s) = %v", c.to, err)
		}
		if got := r.GetRedirect().GetTo(); got != c.want {
			t.Errorf("%s is stored as %q, want %q", c.to, got, c.want)
		}
	}
	// A prefix to keep is a path route's, keeping what follows it.
	r := &resourcev1.Route{Domain: "www.example.org", Redirect: &resourcev1.RouteRedirect{To: "https://example.com", KeepPrefix: true}}
	if err := (&Driver{}).Validate(r); err != nil || r.GetRedirect().GetKeepPrefix() {
		t.Errorf("a whole domain keeping its prefix = %v, %v", r.GetRedirect(), err)
	}
}

// Whoever writes a whole route without knowing the field keeps its redirect, as the
// panel's "Move behind what's in front" does; naming a service turns it back into a
// route that sends there; and a route that has nothing to keep is refused, as one
// that's new with nothing is (ADR 0070).
func TestWholeRouteWritersKeepARedirect(t *testing.T) {
	d := &Driver{}
	stored := &resourcev1.Route{Domain: "www.example.com", Redirect: &resourcev1.RouteRedirect{To: "https://example.com", Temporary: true}}
	moved := &resourcev1.Route{Domain: "www.example.com", Tls: "off", TrustedProxies: []string{"127.0.0.1"}}
	if err := d.Validate(moved); err != nil {
		t.Fatalf("a whole route naming nothing = %v", err)
	}
	if _, err := d.Keep(stored, moved); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(moved.GetRedirect(), stored.GetRedirect()) || moved.GetTls() != "off" {
		t.Errorf("moving behind a tunnel left %v", moved)
	}
	back := &resourcev1.Route{Domain: "www.example.com", Service: "web", Port: 8080}
	if _, err := d.Keep(stored, back); err != nil || back.GetRedirect() != nil {
		t.Errorf("naming a service = %v, %v", back, err)
	}
	served := &resourcev1.Route{Domain: "www.example.com", Service: "web", Port: 8080}
	if _, err := d.Keep(served, &resourcev1.Route{Domain: "www.example.com"}); err == nil || !strings.Contains(err.Error(), "needs a service") {
		t.Errorf("a route with no redirect to keep, naming nothing = %v", err)
	}
	if err := d.CheckNew(&resourcev1.Route{Domain: "new.example.com"}); err == nil || !strings.Contains(err.Error(), "needs a service") {
		t.Errorf("a new route naming nothing = %v", err)
	}
}

// Whether a redirect loops depends on which route of the install answers where it
// lands, by the longest path, and on redirects that lead back to it (ADR 0070).
func TestALoopIsDecidedByWhichRouteAnswers(t *testing.T) {
	d := &Driver{}
	ref := func(name string) resource.Ref { return resource.Ref{Kind: Kind, Scope: "env_1", Name: name} }
	site := &resourcev1.Route{Domain: "example.com", Service: "web", Port: 8080}
	blog := &resourcev1.Route{Domain: "example.com", Path: "/blog", Service: "blog", Port: 80}
	for _, tc := range []struct {
		name  string
		route *resourcev1.Route
		also  map[string]*resourcev1.Route
		says  string
	}{
		{"its own root to a path nothing else answers", redirect("example.com", "", "https://example.com/blog", true), nil, "back to itself at example.com/blog"},
		{"its own root to a path another route answers", redirect("example.com", "", "https://example.com/blog", true), map[string]*resourcev1.Route{"blog": blog}, ""},
		{"a section to another the root answers", redirect("example.com", "/blog", "https://example.com/articles", false), map[string]*resourcev1.Route{"site": site}, ""},
		{"a section to under itself", redirect("example.com", "/old", "https://example.com/old/new", false), map[string]*resourcev1.Route{"site": site}, "back to itself"},
		{"two domains to each other", redirect("old.example.com", "", "https://new.example.com", false), map[string]*resourcev1.Route{"new": redirect("new.example.com", "", "https://old.example.com", false)}, "new.example.com redirects back to it"},
		{"three, round", redirect("a.example.com", "", "https://b.example.com", false), map[string]*resourcev1.Route{
			"b": redirect("b.example.com", "", "https://c.example.com", false),
			"c": redirect("c.example.com", "", "https://a.example.com", false),
		}, "c.example.com redirects back to it"},
		{"to plain HTTP of itself", redirect("example.com", "/x", "http://example.com/x/y", true), nil, "back to itself"},
		{"to a site off the install", redirect("www.example.com", "", "https://example.org", false), map[string]*resourcev1.Route{"site": site}, ""},
		{"to the site", redirect("www.example.com", "", "https://example.com", false), map[string]*resourcev1.Route{"site": site}, ""},
		// Under itself in capitals still lands on itself, as Caddy matches without case,
		// and the path would grow by a segment on each hop.
		{"a section to under itself, in capitals", redirect("example.com", "/old", "https://example.com/OLD/new", false), map[string]*resourcev1.Route{"site": site}, "back to itself"},
		{"a section to under itself, encoded", redirect("example.com", "/old", "https://example.com/%6Fld/new", false), map[string]*resourcev1.Route{"site": site}, "back to itself"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := proto.Clone(tc.route).(*resourcev1.Route)
			if err := d.Validate(r); err != nil {
				t.Fatalf("Validate = %v", err)
			}
			all := map[resource.Ref]proto.Message{ref("this"): r}
			for name, other := range tc.also {
				o := proto.Clone(other).(*resourcev1.Route)
				if err := d.Validate(o); err != nil {
					t.Fatalf("Validate(%s) = %v", name, err)
				}
				all[ref(name)] = o
			}
			err := d.CheckAcross(ref("this"), r, all, func(resource.Ref) bool { return true })
			switch {
			case tc.says == "" && err != nil:
				t.Errorf("CheckAcross = %v", err)
			case tc.says != "" && (err == nil || !strings.Contains(err.Error(), tc.says)):
				t.Errorf("CheckAcross = %v, want it to say %q", err, tc.says)
			}
		})
	}
}

// A loop through a route of a project the caller can't see is refused in words that
// name nothing of it (ADR 0048).
func TestALoopNamesNoRouteTheCallerCantSee(t *testing.T) {
	d := &Driver{}
	mine := resource.Ref{Kind: Kind, Scope: "env_mine", Name: "a"}
	theirs := resource.Ref{Kind: Kind, Scope: "env_theirs", Name: "c"}
	all := map[resource.Ref]proto.Message{
		mine: redirect("a.example.com", "", "https://b.example.com", false),
		{Kind: Kind, Scope: "env_mine", Name: "b"}: redirect("b.example.com", "", "https://c.example.com", false),
		theirs: redirect("c.example.com", "", "https://a.example.com", false),
	}
	for _, r := range all {
		if err := d.Validate(r); err != nil {
			t.Fatal(err)
		}
	}
	err := d.CheckAcross(mine, all[mine], all, func(r resource.Ref) bool { return r.Scope == "env_mine" })
	if err == nil || !strings.Contains(err.Error(), "a route in a project you can't see redirects back to it. Send it elsewhere, or ask an admin") || strings.Contains(err.Error(), "c.example.com") {
		t.Errorf("CheckAcross = %v", err)
	}
	err = d.CheckAcross(mine, all[mine], all, func(resource.Ref) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "c.example.com redirects back to it") {
		t.Errorf("to an admin, CheckAcross = %v", err)
	}
}

// A redirect kept through a whole-route edit is a redirect again, so access in that
// edit is refused there, and not later on each server.
func TestAKeptRedirectTakesNoAccess(t *testing.T) {
	d := &Driver{}
	stored := &resourcev1.Route{Domain: "www.example.com", Redirect: &resourcev1.RouteRedirect{To: "https://example.com"}}
	private := &resourcev1.Route{Domain: "www.example.com", Access: &resourcev1.RouteAccess{Allow: []string{"198.51.100.0/24"}}}
	if _, err := d.Keep(stored, private); err == nil || !strings.Contains(err.Error(), "lets everyone through") {
		t.Errorf("making a redirect private = %v", err)
	}
}

// bcryptHash is a bcrypt hash at the cost a route takes, as the control plane makes one.
func bcryptHash(t *testing.T) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte("correct-horse-battery"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}
