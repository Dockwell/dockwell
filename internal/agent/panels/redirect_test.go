// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// coolifyWWW is an app as Coolify labels one with two domains, the bare one and
// www, and a redirect between them: an HTTP router and an HTTPS one for each,
// each with the redirect, as Coolify puts it on every router of the app.
func coolifyWWW(regex, replacement, permanent string) map[string]string {
	l := map[string]string{
		"traefik.enable":             "true",
		"com.docker.compose.service": "web",
		"traefik.http.middlewares.redirect-to-https.redirectscheme.scheme": "https",
		"traefik.http.middlewares.gzip.compress":                           "true",
		"traefik.http.middlewares.www.redirectregex.regex":                 regex,
		"traefik.http.middlewares.www.redirectregex.replacement":           replacement,
		"traefik.http.services.http-0-k4s9.loadbalancer.server.port":       "3000",
	}
	for i, host := range []string{"shop.example.com", "www.shop.example.com"} {
		n := string(rune('0' + i))
		maps.Copy(l, map[string]string{
			"traefik.http.routers.http-" + n + "-k4s9.rule":         "Host(`" + host + "`) && PathPrefix(`/`)",
			"traefik.http.routers.http-" + n + "-k4s9.entrypoints":  "http",
			"traefik.http.routers.http-" + n + "-k4s9.middlewares":  "redirect-to-https,www",
			"traefik.http.routers.http-" + n + "-k4s9.service":      "http-0-k4s9",
			"traefik.http.routers.https-" + n + "-k4s9.rule":        "Host(`" + host + "`) && PathPrefix(`/`)",
			"traefik.http.routers.https-" + n + "-k4s9.entrypoints": "https",
			"traefik.http.routers.https-" + n + "-k4s9.middlewares": "gzip,www",
			"traefik.http.routers.https-" + n + "-k4s9.service":     "http-0-k4s9",
		})
	}
	if permanent != "" {
		l["traefik.http.middlewares.www.redirectregex.permanent"] = permanent
	}
	return l
}

func byDomain(routes []*dockwellv1.PanelRoute) map[string]*dockwellv1.PanelRoute {
	out := map[string]*dockwellv1.PanelRoute{}
	for _, r := range routes {
		out[r.GetDomain()+r.GetPath()] = r
	}
	return out
}

// Coolify's redirect between www and the bare domain comes as a route that
// answers with that redirect, for the domain it sends on; the other answers as
// before (ADR 0070).
func TestCoolifysWWWRedirectComesAsARedirect(t *testing.T) {
	for name, c := range map[string]struct {
		regex, replacement, permanent string
		from, to                      string
		temporary                     bool
	}{
		"to the bare domain": {`^(http|https)://www\.(.+)`, "${1}://${2}", "false", "www.shop.example.com", "https://shop.example.com", true},
		"to www":             {`^(http|https)://(?:www\.)?(.+)`, "${1}://www.${2}", "true", "shop.example.com", "https://www.shop.example.com", false},
		"with $1 and $2":     {`^(http|https)://www\.(.+)`, "$1://$2", "", "www.shop.example.com", "https://shop.example.com", true},
	} {
		t.Run(name, func(t *testing.T) {
			got := byDomain(routesOf(coolifyWWW(c.regex, c.replacement, c.permanent), "web-k4s9", nil))
			if len(got) != 2 {
				t.Fatalf("read %v", got)
			}
			want := &resourcev1.RouteRedirect{To: c.to, Temporary: c.temporary}
			for d, r := range got {
				if r.GetLeft() != "" || r.GetPort() != 3000 {
					t.Errorf("%s reads %v", d, r)
				}
				if d == c.from && !proto.Equal(r.GetRedirect(), want) {
					t.Errorf("%s redirects %v, want %v", d, r.GetRedirect(), want)
				}
				if d != c.from && r.GetRedirect() != nil {
					t.Errorf("%s, which the redirect leaves, redirects %v", d, r.GetRedirect())
				}
			}
		})
	}
}

// A redirect a route can't say keeps its domain behind, with why: one elsewhere,
// one that loses the path, one only some requests take, an expression Dockwell
// doesn't know, a path's alone, and routers that don't agree.
func TestARedirectARouteCantSayStaysBehind(t *testing.T) {
	toBare := coolifyWWW(`^(http|https)://www\.(.+)`, "${1}://${2}", "")
	for name, c := range map[string]struct {
		more map[string]string
		says string
	}{
		"elsewhere":       {map[string]string{"traefik.http.middlewares.www.redirectregex.replacement": "https://evil.example/${2}"}, "not to https://shop.example.com"},
		"losing the path": {map[string]string{"traefik.http.middlewares.www.redirectregex.replacement": "${1}://shop.example.com"}, "keeping the path"},
		"an expression":   {map[string]string{"traefik.http.middlewares.www.redirectregex.regex": `^https://www\.shop\.example\.com/old(.*)`}, "(redirectregex), which a plain route would drop"},
		"two HTTPS routers": {map[string]string{
			"traefik.http.routers.https-2-k4s9.rule":        "Host(`www.shop.example.com`)",
			"traefik.http.routers.https-2-k4s9.entrypoints": "https",
			"traefik.http.routers.https-2-k4s9.middlewares": "gzip",
			"traefik.http.routers.https-2-k4s9.service":     "http-0-k4s9",
		}, "don't agree"},
		// HTTPS serves the page, as a redirectscheme does nothing there, and HTTP
		// redirects to the bare domain without going to HTTPS first.
		"a redirectscheme on HTTPS": {map[string]string{
			"traefik.http.routers.http-1-k4s9.middlewares":  "www",
			"traefik.http.routers.https-1-k4s9.middlewares": "redirect-to-https",
		}, "don't agree"},
		"a path alone": {map[string]string{
			"traefik.http.routers.http-1-k4s9.rule":  "Host(`www.shop.example.com`) && PathPrefix(`/old`)",
			"traefik.http.routers.https-1-k4s9.rule": "Host(`www.shop.example.com`) && PathPrefix(`/old`)",
		}, "a path of the domain alone"},
	} {
		t.Run(name, func(t *testing.T) {
			l := maps.Clone(toBare)
			maps.Copy(l, c.more)
			var www *dockwellv1.PanelRoute
			for _, r := range routesOf(l, "web-k4s9", nil) {
				if strings.HasPrefix(r.GetDomain(), "www.") {
					www = r
				}
			}
			if www == nil || !strings.Contains(www.GetLeft(), c.says) || www.GetRedirect() != nil {
				t.Errorf("www reads %v, want it behind, saying %q", www, c.says)
			}
		})
	}
	// An HTTP router that sends plain HTTP to HTTPS first says nothing of what
	// HTTPS serves: where the HTTPS router doesn't redirect, the page is served.
	served := maps.Clone(toBare)
	served["traefik.http.routers.https-1-k4s9.middlewares"] = "gzip"
	if got := byDomain(routesOf(served, "web-k4s9", nil))["www.shop.example.com"]; got.GetLeft() != "" || got.GetRedirect() != nil {
		t.Errorf("www served over HTTPS reads %v", got)
	}
	// An HTTP router alone that redirects says what's served, as nothing else does.
	l := maps.Clone(toBare)
	for _, k := range []string{"rule", "entrypoints", "middlewares", "service"} {
		delete(l, "traefik.http.routers.https-1-k4s9."+k)
	}
	if got := byDomain(routesOf(l, "web-k4s9", nil))["www.shop.example.com"]; got.GetLeft() != "" || got.GetRedirect().GetTo() != "https://shop.example.com" {
		t.Errorf("an HTTP router alone reads %v", got)
	}
}

// Coolify's redirect for Caddy reads as Caddy does it: to the other half of each
// address, for good with permanent, or not at all.
func TestCoolifysCaddyRedirectComesAsARedirect(t *testing.T) {
	caddy := func(redir string) *dockwellv1.PanelRoute {
		l := map[string]string{"caddy_0": "https://shop.example.com", "caddy_0.handle_path": "/*", "caddy_0.handle_path.0_reverse_proxy": "{{upstreams 8080}}", "caddy_0.redir": redir}
		got := routesOf(l, "web-1", nil)
		if len(got) != 1 {
			t.Fatalf("%s reads %v", redir, got)
		}
		return got[0]
	}
	if got := caddy("https://www.shop.example.com{uri} permanent"); got.GetLeft() != "" || !proto.Equal(got.GetRedirect(), &resourcev1.RouteRedirect{To: "https://www.shop.example.com"}) {
		t.Errorf("a permanent redirect to www reads %v", got)
	}
	if got := caddy("{scheme}://www.shop.example.com{uri}"); got.GetLeft() != "" || !got.GetRedirect().GetTemporary() {
		t.Errorf("a redirect for a while reads %v", got)
	}
	for redir, says := range map[string]string{
		"https://evil.example.com{uri}":     "which isn't its www name",
		"https://shop.example.com{uri}":     "to itself",
		"https://www.shop.example.com/sale": "in a way a route can't say",
	} {
		if got := caddy(redir); !strings.Contains(got.GetLeft(), says) || got.GetRedirect() != nil {
			t.Errorf("%s reads %v, want it behind, saying %q", redir, got, says)
		}
	}
}

// Dokploy's redirect presets, written to its middlewares.yml and named in the
// app's own file without a provider, read as Traefik reads them: from the file
// the router is in.
func TestDokploysWWWRedirectComesAsARedirect(t *testing.T) {
	files := map[string]string{}
	for _, yml := range []string{
		"http:\n  middlewares:\n    redirect-to-https:\n      redirectScheme:\n        scheme: https\n        permanent: true\n" +
			"    redirect-shop-k2m4-1:\n      redirectRegex:\n        regex: '^https?://www.(.+)'\n        replacement: https://${1}\n        permanent: true\n" +
			"    redirect-shop-k2m4-2:\n      redirectRegex:\n        regex: \"^https?://(?:www\\\\.)?(.+)\"\n        replacement: \"https://www.${1}\"\n        permanent: false\n",
	} {
		f := readTraefikFile([]byte(yml))
		maps.Copy(files, f.middlewares)
		maps.Copy(files, f.settings)
	}
	if got := files["traefik.http.middlewares.redirect-shop-k2m4-2.redirectregex.regex"]; got != `^https?://(?:www\.)?(.+)` {
		t.Fatalf("a double-quoted expression reads %q", got)
	}
	for name, c := range map[string]struct {
		middleware, from, to string
		temporary            bool
	}{
		"to the bare domain": {"redirect-shop-k2m4-1", "www.shop.example.com", "https://shop.example.com", false},
		"to www":             {"redirect-shop-k2m4-2", "shop.example.com", "https://www.shop.example.com", true},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			var rule strings.Builder
			rule.WriteString("http:\n  routers:\n")
			for i, host := range []string{"shop.example.com", "www.shop.example.com"} {
				n := string(rune('1' + i))
				rule.WriteString("    shop-k2m4-router-" + n + ":\n      rule: Host(`" + host + "`)\n      service: shop-k2m4-service-1\n      entryPoints:\n        - web\n      middlewares:\n        - redirect-to-https\n        - " + c.middleware + "\n" +
					"    shop-k2m4-router-websecure-" + n + ":\n      rule: Host(`" + host + "`)\n      service: shop-k2m4-service-1\n      entryPoints:\n        - websecure\n      middlewares:\n        - " + c.middleware + "\n      tls:\n        certResolver: letsencrypt\n")
			}
			rule.WriteString("  services:\n    shop-k2m4-service-1:\n      loadBalancer:\n        servers:\n          - url: http://shop-k2m4:3000\n        passHostHeader: true\n")
			writeDynamic(t, root, "shop-k2m4.yml", rule.String())
			got := byDomain(Reader{Root: root}.dynamicRoutesFrom("shop-k2m4", "shop-k2m4.1.x7k2", files, nil))
			if len(got) != 2 {
				t.Fatalf("read %v", got)
			}
			for d, r := range got {
				if r.GetLeft() != "" {
					t.Errorf("%s stays behind: %s", d, r.GetLeft())
				}
				want := (*resourcev1.RouteRedirect)(nil)
				if d == c.from {
					want = &resourcev1.RouteRedirect{To: c.to, Temporary: c.temporary}
				}
				if !proto.Equal(r.GetRedirect(), want) {
					t.Errorf("%s redirects %v, want %v", d, r.GetRedirect(), want)
				}
			}
		})
	}
}

// A redirect domain is open only where the proxy answers with its redirect, the
// path kept; a page, or a redirect anywhere else, holds it back.
func TestTheProbeOfARedirect(t *testing.T) {
	for name, c := range map[string]struct {
		answer http.HandlerFunc
		open   bool
		says   string
	}{
		"its redirect": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://shop.example.com"+r.URL.Path, http.StatusFound)
		}, true, "302 to shop.example.com, as its redirect does"},
		"a page": {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, false, "200, where its redirect would send it to shop.example.com"},
		"a sign-in": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://shop.example.com/login?rd=/", http.StatusFound)
		}, false, "where its redirect would send it to shop.example.com/admin"},
		"plain HTTP": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://shop.example.com"+r.URL.Path, http.StatusMovedPermanently)
		}, false, "301 to http://shop.example.com/admin"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewTLSServer(c.answer)
			defer srv.Close()
			got := (Reader{ProbeAddr: srv.Listener.Addr().String()}).probe(t.Context(), "www.shop.example.com", "/admin", "https://shop.example.com")
			if got.Open != c.open || !strings.Contains(got.Answer, c.says) {
				t.Errorf("answers %q, open %v; want %q, open %v", got.Answer, got.Open, c.says, c.open)
			}
		})
	}
}

func TestYAMLScalarsAsTraefiksFilesWriteThem(t *testing.T) {
	for raw, want := range map[string]string{
		` ^https?://www.(.+)`:          `^https?://www.(.+)`,
		` '^https?://www\.(.+)'`:       `^https?://www\.(.+)`,
		` 'it''s'`:                     `it's`,
		` "^https?://www\\.(.+)"`:      `^https?://www\.(.+)`,
		` "unclosed \q"`:               `"unclosed \q"`,
		` https://${1}`:                `https://${1}`,
		` "https://www.${1}"   `:       `https://www.${1}`,
		` '^https?://(?:www.)?(.+)'  `: `^https?://(?:www.)?(.+)`,
	} {
		if got := yamlScalar(raw); got != want {
			t.Errorf("yamlScalar(%q) = %q, want %q", raw, got, want)
		}
	}
}

// writeDynamic writes a file of Dokploy's Traefik folder under root.
func writeDynamic(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "etc", "dokploy", "traefik", "dynamic")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Traefik's file provider keeps the first definition of a middleware name, in the
// order it reads its files, and skips the rest, where Dockwell would keep the last.
// A name the proxy's files define more than once, in two files or one, holds what
// passes through it, as which runs can't be told for sure (slice 4's review, H1).
func TestAMiddlewareDefinedTwiceHoldsItsDomain(t *testing.T) {
	router := "http:\n  routers:\n    shop-k2m4-router-websecure-1:\n      rule: Host(`shop.example.com`)\n      service: shop-k2m4-service-1\n      entryPoints:\n        - websecure\n      middlewares:\n        - gzip\n      tls:\n        certResolver: letsencrypt\n" +
		"  services:\n    shop-k2m4-service-1:\n      loadBalancer:\n        servers:\n          - url: http://shop-k2m4:3000\n        passHostHeader: true\n"
	for name, files := range map[string]map[string]string{
		"in two files": {
			// Traefik reads a-limit.yml first: an address limit.
			"a-limit.yml":     "http:\n  middlewares:\n    gzip:\n      ipAllowList:\n        sourceRange:\n          - 192.0.2.10/32\n",
			"middlewares.yml": "http:\n  middlewares:\n    gzip:\n      compress: {}\n",
		},
		"in one": {"middlewares.yml": "http:\n  middlewares:\n    gzip:\n      compress: {}\n    gzip:\n      ipAllowList:\n        sourceRange:\n          - 192.0.2.10/32\n"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for f, body := range files {
				writeDynamic(t, root, f, body)
			}
			writeDynamic(t, root, "shop-k2m4.yml", router)
			r := Reader{Root: root}
			defs := r.fileMiddlewares(dockwellv1.PanelKind_PANEL_KIND_DOKPLOY)
			got := r.dynamicRoutesFrom("shop-k2m4", "shop-k2m4.1.x7k2", defs, nil)
			if len(got) != 1 || !strings.Contains(got[0].GetLeft(), "the proxy's files define more than once") {
				t.Errorf("gzip, defined twice, reads %v", got)
			}
		})
	}
	// Defined once, it's compression, which a route does without.
	root := t.TempDir()
	writeDynamic(t, root, "middlewares.yml", "http:\n  middlewares:\n    gzip:\n      compress: {}\n")
	writeDynamic(t, root, "shop-k2m4.yml", router)
	r := Reader{Root: root}
	if got := r.dynamicRoutesFrom("shop-k2m4", "shop-k2m4.1.x7k2", r.fileMiddlewares(dockwellv1.PanelKind_PANEL_KIND_DOKPLOY), nil); len(got) != 1 || got[0].GetLeft() != "" {
		t.Errorf("gzip, defined once, reads %v", got)
	}
}
