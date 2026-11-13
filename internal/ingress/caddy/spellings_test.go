// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"encoding/json"
	"net/url"
	"regexp"
	"testing"

	"github.com/dockwell/dockwell/internal/ingress"
)

// refusedAsSent reports whether a domain with a private route and another refuses a
// request target, as withSpellings has Caddy read it: as the client wrote it, then
// decoded and not cleaned.
func refusedAsSent(t *testing.T, target string) bool {
	t.Helper()
	fine, odd := regexp.MustCompile(asSentFine), regexp.MustCompile(decodedOdd)
	u, err := url.ParseRequestURI(target)
	if err != nil {
		t.Fatalf("Go refuses %q before Caddy reads it: %v", target, err)
	}
	return !fine.MatchString(target) || odd.MatchString(u.Path)
}

// A domain with a private route refuses each request target an app could read as
// another route's path, written any way a server behind reads differently from
// Caddy's matchers, and takes what a browser writes for a site's own links (#606's
// final check of H2).
func TestADomainWithAPrivateRouteRefusesWhatAnAppCouldReadAsAnotherPath(t *testing.T) {
	for _, target := range []string{
		// What Caddy cleans before it matches, and Tomcat reads after taking ";x" off.
		"/x/;/../admin", "/x/.;/../admin", "/x/.;x/../admin", "/x/;x/../admin", "/x/y/z/..;/../../admin",
		"/x/%3b/../admin", "/x/%2e%3b/../admin", "/x/;/%2e%2e/admin", "/x/.;/../manager/html",
		"/x/..;/admin", "/;x/admin", "/.;/admin", "/manager;x/html", "/admin;", "/Admin;jsessionid=1",
		// Below a public path, to a private root.
		"/api/..;/secret", "/api/.;/../secret", "/api/x%5c..%5c..%5csecret", "/api/;/../secret",
		// "\" as IIS reads it, and encoded ASCII of every kind.
		"/admin%5cx/..", "/%5cadmin", "/%5Cadmin", "/x%5c..%5cadmin", "/x%2f..%2fadmin", "/admin%2fx", "/%2e%2e/admin",
		"/%61dmin", "/admin%253b", "/admin%00", "/admin%09x", "/admin%7f", "/admin%23", "/admin%3f", "/admin%23x",
		"/admin%3fx", "/admin%20", "/admin%20/x", "/admin.%20/x", "/ad%0amin", "/admin::%24DATA",
		// Dot segments, and segments Windows takes a trailing dot off.
		"/x/../admin", "/./admin", "/admin/.", "/..", "/.", "/x/..", "/admin./x", "/admin.", "/admin..", "/x/.../admin",
		"/admin..;/",
		// Bytes that aren't UTF-8, overlong among them, raw or encoded.
		"/%c0%afadmin", "/x/..%c0%afadmin", "/x%e0%80%af..%e0%80%afadmin", "/%c1%9cadmin", "/admin%c0%ae",
		"/admin%ed%a0%80", "/admin%f4%90%80%80", "/admin\xc0\xaf", "/caf\xc3\xa9",
		// What no one sees, look-alikes of "/", "\" and ".", and white space a server takes off.
		"/admin%e2%80%8b", "/admin%ef%bb%bf", "/admin%e2%80%ae", "/admin%c2%ad", "/admin%c2%85",
		"/%ef%bc%8fadmin", "/x%ef%bc%8f..%ef%bc%8fadmin", "/%ef%bc%bcadmin", "/x%ef%bc%bc..%ef%bc%bcadmin",
		"/x%ef%bc%8e%ef%bc%8e/admin", "/x%e2%88%95admin", "/x%c2%a5admin",
		"/admin%c2%a0", "/admin%e3%80%80", "/admin%e2%80%80/x",
		// NTFS's streams, and what's no path.
		"/admin::$DATA", "/admin:$I30:$INDEX_ALLOCATION",
		"http://app.example.net/x/;/../admin", "https://app.example.net/x/..;/admin?a=1", "app.example.net:443",
		// A host to Python's urlparse and Node's URL, with the rest as the path (#606's last check, M1).
		"//admin", "//a/admin", "//app.example.net/admin", "///admin", "//api/secret", "http://app.example.net//a/admin", "//",
		// ".." and "..." under NFKC (L1), and what shows as nothing: tags, variation selectors, Hangul fillers.
		"/x/%ef%b8%b0/admin", "/api/%ef%b8%b0/secret", "/x/%e2%80%a6/admin", "/x/%ef%b8%99/admin", "/x/%e2%80%a5/admin",
		"/admin%f3%a0%80%a0", "/admin%f3%a0%81%bf", "/adm%ef%b8%80in", "/admin%f3%a0%84%80", "/admin%e3%85%a4", "/admin%ef%be%a0",
		"/admin%e1%85%9f", "/admin%e1%85%a0",
	} {
		if !refusedAsSent(t, target) {
			t.Errorf("%q is taken", target)
		}
	}
	for _, target := range []string{
		"/", "/admin", "/admin/", "/ADMIN", "/x//admin", "/a/", "/who", "/a.b/c", "/..x", "/.well-known/acme-challenge/x-Y_z0",
		"/.well-known/security.txt", "/api/v1/items(3)", "/x/a,b", "/search?q=a;b&r=%2f..%2f&s=%3b", "/x?",
		"/caf%C3%A9", "/%E4%B8%AD%E6%96%87/%E6%96%87%E4%BB%B6.txt", "/%D9%85%DB%8C%E2%80%8C%D8%AE%D9%88%D8%A7%D9%87%D9%85",
		"/%E3%80%80x", "/~user/", "/@scope/pkg", "/a+b=c:d!$&'()*", "/x[1]|y^", "*", "http://app.example.net/x?y=1",
		"/emoji/%F0%9F%91%8D", "/emoji/%E2%9D%A4%EF%B8%8F", "/emoji/%E2%98%BA%EF%B8%8E",
	} {
		if refusedAsSent(t, target) {
			t.Errorf("%q is refused", target)
		}
	}
}

// Only a domain with a private route and another route gets the 400, root included,
// and a domain with one private route, or with none private, is as it was.
func TestOnlyADomainWithAPrivateRouteAndAnotherGetsThe400(t *testing.T) {
	route := func(id, domain, path string, private bool) ingress.Route {
		r := ingress.Route{ID: id, Domain: domain, Path: path, Upstreams: []string{"172.30.0.2:80"}}
		if private {
			r.Allow = []string{"198.51.100.7/32"}
		}
		return r
	}
	routes := []ingress.Route{
		route("dockwell.env.shop", "shop.example.net", "", false),
		route("dockwell.env.shop-admin", "shop.example.net", "/admin", true),
		route("dockwell.env.app", "app.example.net", "", true),
		route("dockwell.env.app-api", "app.example.net", "/api", false),
		route("dockwell.env.alone", "alone.example.net", "", true),
		route("dockwell.env.open", "open.example.net", "", false),
		route("dockwell.env.open-api", "open.example.net", "/api", false),
	}
	var list []json.RawMessage
	for _, r := range routes {
		b, err := json.Marshal(routeJSON(r))
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, b)
	}
	got, err := withSpellings(list, secureSpellingsID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(list)+1 {
		t.Fatalf("it adds %d routes", len(got)-len(list))
	}
	var added struct {
		Match []matcher `json:"match"`
	}
	if err := json.Unmarshal(got[len(got)-1], &added); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]int{}
	for _, m := range added.Match {
		for _, h := range m.Host {
			hosts[h]++
		}
	}
	want := map[string]int{"app.example.net": 2, "shop.example.net": 2}
	if len(hosts) != len(want) || hosts["app.example.net"] != 2 || hosts["shop.example.net"] != 2 {
		t.Errorf("it answers for %v, want %v", hosts, want)
	}
	if got, err := withSpellings(list[4:], secureSpellingsID); err != nil || len(got) != 3 {
		t.Errorf("with no domain to answer for, it adds a route: %d, %v", len(got), err)
	}
}
