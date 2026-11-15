// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/ingress"
)

// A redirect is a route, so its domain gets a certificate as any route's does, and
// it answers with where it sends, the path and query kept, over HTTPS, after Caddy's
// own hop from plain HTTP. What Caddy has reads back as Dockwell wrote it, so it's no
// drift, and changing it to a temporary one that drops the path takes at once (ADR
// 0070).
func TestARedirectGetsItsCertificateAndSendsOnOnRealCaddy(t *testing.T) {
	p := startPebble(t)
	c := startCaddyWithCA(t, p, &ingress.PanelSite{Domain: "panel.example.net"}, "http")
	p.addRecord(t, "www.example.net")
	www := ingress.Route{ID: "dockwell.env.www-example-net", Domain: "www.example.net", Redirect: &ingress.Redirect{To: "https://example.net"}}
	put(t, c, www)

	st := waitFor(t, c.Caddy, "www.example.net", ingress.CertIssued, 45*time.Second)
	if st.Problem != ingress.ProblemNone {
		t.Errorf("the certificate reads %+v", st)
	}
	status, location, kept := c.redirectAt(t, "/a/b?c=1&d=%20")
	if status != http.StatusMovedPermanently || location != "https://example.net/a/b?c=1&d=%20" || kept != "max-age=86400" {
		t.Errorf("https://www.example.net/a/b?c=1&d=%%20 answers %d to %q, kept %q, want 301 to https://example.net/a/b?c=1&d=%%20, kept a day", status, location, kept)
	}
	// What a request's own path holds can't change where it goes.
	for _, path := range []string{"//evil.example", "/%2F%2Fevil.example", "/\\evil.example"} {
		if _, location, _ := c.redirectAt(t, path); !strings.HasPrefix(location, "https://example.net/") {
			t.Errorf("%s sends to %q", path, location)
		}
	}
	if got := c.plainGet(t, "www.example.net", "/a", nil); got.status != http.StatusPermanentRedirect || got.location != "https://www.example.net/a" {
		t.Errorf("plain HTTP answers %d to %q, want Caddy's own 308 to https://www.example.net/a", got.status, got.location)
	}
	c.readsBack(t, www)

	moved := www
	moved.Redirect = &ingress.Redirect{To: "https://example.net/landing", Temporary: true, DropPath: true}
	put(t, c, moved)
	if status, location, kept := c.redirectAt(t, "/a/b?c=1"); status != http.StatusFound || location != "https://example.net/landing" || kept != "" {
		t.Errorf("a temporary one that drops the path answers %d to %q, kept %q, want 302 to https://example.net/landing, not kept", status, location, kept)
	}
	c.readsBack(t, moved)
}

// On a server where a route trusts a proxy, every change has the driver write what
// each route that proxies believes, and a redirect has none of that: changing another
// route leaves the redirect's answer as it was, and a path route's prefix stripped,
// with nothing read back as drift (ADR 0070). withTrust once wrote request headers
// over a redirect's Location.
func TestARedirectSurvivesARouteThatTrustsAProxyOnRealCaddy(t *testing.T) {
	p := startPebble(t)
	c := startCaddyWithCA(t, p, &ingress.PanelSite{Domain: "panel.example.net"}, "http")
	p.addRecord(t, "app.example.net")
	p.addRecord(t, "www.example.net")
	trusting := aRoute("dockwell.env.app", "app.example.net", "", backend(t, "web"))
	trusting.TrustedProxies = []string{"203.0.113.0/24"}
	put(t, c, trusting)
	old := ingress.Route{ID: "dockwell.env.old", Domain: "www.example.net", Path: "/old-blog", Redirect: &ingress.Redirect{To: "https://blog.example.net"}}
	put(t, c, old)
	waitFor(t, c.Caddy, "www.example.net", ingress.CertIssued, 45*time.Second)

	// Another change, which writes what the trusting route believes again.
	trusting.Upstreams = []string{backend(t, "web2")}
	put(t, c, trusting)
	status, location, kept := c.redirectAt(t, "/old-blog/post-1?x=1")
	if status != http.StatusMovedPermanently || location != "https://blog.example.net/post-1?x=1" || kept != "max-age=86400" {
		t.Errorf("/old-blog/post-1?x=1 answers %d to %q, kept %q, want 301 to https://blog.example.net/post-1?x=1", status, location, kept)
	}
	c.readsBack(t, old)
}

// redirectAt asks www.example.net for a path over HTTPS, without following where
// it's sent, and says what answered.
func (r *realCaddy) redirectAt(t *testing.T, pathAndQuery string) (status int, location, kept string) {
	t.Helper()
	const domain = "www.example.net"
	client := r.client(domain)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var last error
	for range 100 {
		res, err := client.Get("https://" + domain + pathAndQuery)
		if err == nil {
			_ = res.Body.Close()
			return res.StatusCode, res.Header.Get("Location"), res.Header.Get("Cache-Control")
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never answered: %v", domain, last)
	return 0, "", ""
}

// readsBack says Caddy has a route as it was written, untouched by hand.
func (r *realCaddy) readsBack(t *testing.T, want ingress.Route) {
	t.Helper()
	routes, err := r.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(routes, func(got ingress.Route) bool { return got.ID == want.ID })
	if i < 0 {
		t.Fatalf("Caddy has no route %s", want.ID)
	}
	got := routes[i]
	if got.Edited || !reflect.DeepEqual(got.Redirect, want.Redirect) || got.Domain != want.Domain || len(got.Upstreams) != 0 {
		t.Errorf("Caddy's %s reads back as %+v (redirect %+v), want %+v", want.ID, got, got.Redirect, want.Redirect)
	}
}
