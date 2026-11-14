// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A proxy that asks for a client certificate refuses the handshake a request
// without one makes, and one nothing listens on answers nothing: each holds the
// domain back.
func TestTheProbeHoldsWhatAHandshakeRefuses(t *testing.T) {
	mtls := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	mtls.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS12}
	mtls.StartTLS()
	defer mtls.Close()
	if got := (Reader{ProbeAddr: mtls.Listener.Addr().String()}).probe(t.Context(), "blog.example.com", "/"); got.Open || !strings.Contains(got.Answer, "no TLS handshake") {
		t.Errorf("a proxy asking for a client certificate answered %+v", got)
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	_ = closed.Close()
	if got := (Reader{ProbeAddr: addr}).probe(t.Context(), "blog.example.com", "/"); got.Open || !strings.Contains(got.Answer, "nothing listens on port 443") {
		t.Errorf("a proxy that's gone answered %+v", got)
	}
	open := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != "blog.example.com" || r.Host != "blog.example.com" {
			w.WriteHeader(http.StatusMisdirectedRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	open.StartTLS()
	defer open.Close()
	if got := (Reader{ProbeAddr: open.Listener.Addr().String()}).probe(t.Context(), "blog.example.com", "/"); !got.Open || got.Answer != "200" {
		t.Errorf("the domain's name as SNI and host answered %+v", got)
	}
}

// What a rule names: hosts from Host, HostSNI and Traefik 1's Host:, case-blind;
// patterns from HostRegexp, in Traefik 3's syntax or Traefik 2's; the literal start
// of each path.
func TestWhatARuleNames(t *testing.T) {
	hosts, patterns, paths := rulesIn("traefik.http.routers.a.rule=HOST(`Blog.Example.com`, `www.example.com`) && PathPrefix(`/api/{v:[0-9]+}`) || HostSNI(`db.example.com`)")
	if !slices.Equal(hosts, []string{"blog.example.com", "www.example.com", "db.example.com"}) || len(patterns) != 0 || !slices.Equal(paths, []string{"/api/"}) {
		t.Errorf("rulesIn = %v, %v, %v", hosts, patterns, paths)
	}
	if hosts, _, _ := rulesIn("traefik.frontend.rule=Host:old.example.com,older.example.com"); !slices.Equal(hosts, []string{"old.example.com", "older.example.com"}) {
		t.Errorf("Traefik 1's rule names %v", hosts)
	}
	for p, matches := range map[string]map[string]bool{
		`^.+\.example\.com$`:               {"blog.example.com": true, "example.org": false},
		`{sub:[a-z]+}.example.com`:         {"blog.example.com": true, "a.b.example.com": false},
		`{name}.example.com`:               {"shop.example.com": true},
		`^[a-z]+\.example\.(com|org)$`:     {"blog.example.org": true},
		`{sub:[a-z]{2,8}}.example.com`:     {"blog.example.com": true, "b.example.com": false},
		`^(?i:BLOG)\.EXAMPLE\.COM$`:        {"blog.example.com": true},
		`{host:(?:shop|blog)}.example.com`: {"blog.example.com": true, "wiki.example.com": false},
	} {
		re, ok := matcherOf(p)
		if !ok {
			t.Errorf("%s doesn't compile", p)
			continue
		}
		for d, want := range matches {
			if re.MatchString(d) != want {
				t.Errorf("%s matching %s = %v", p, d, !want)
			}
		}
	}
	if _, ok := matcherOf(`{sub:[a-z}.example.com`); ok {
		t.Error("a pattern that reads as neither syntax compiles")
	}
}

// Traefik's flags: --key=value, --key value, and --key alone for true.
func TestFlagsAsTraefikReadsThem(t *testing.T) {
	got := flagsOf([]string{"traefik", "--providers.file.directory", "/srv/routes", "--providers.docker", "--entryPoints.web.address=:80", "--log.level", "DEBUG"})
	want := [][2]string{{"--providers.file.directory", "/srv/routes"}, {"--providers.docker", "true"}, {"--entrypoints.web.address", ":80"}, {"--log.level", "debug"}}
	if !slices.Equal(got, want) {
		t.Errorf("flagsOf = %v", got)
	}
}

// The scan reads every file in a mount but what holds no rule, and holds every
// domain back where a file is past the cap.
func TestTheScanOfAMount(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "srv", "proxy")
	for name, body := range map[string]string{
		"acme.json":       `{"Certificates":[{"domain":{"main":"Host(` + "`blog.example.com`" + `)"}}]}`,
		"certs/blog.crt":  "Host(`blog.example.com`)",
		"access.log":      "Host(`blog.example.com`)",
		"Caddyfile":       "shop.example.com, *.example.org {\n\thandle /admin* {\n\t\treverse_proxy admin:80\n\t}\n}\n",
		"routes/web.yaml": "http:\n  routers:\n    w:\n      rule: Host(`web.example.com`)\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := Reader{Root: root}
	s := scan{hosts: map[string][]mention{}, paths: map[string][]string{}}
	if why := r.scanMount(&s, "/srv/proxy"); why != "" {
		t.Fatal(why)
	}
	if _, ok := s.hosts["blog.example.com"]; ok {
		t.Error("certificates, keys and logs were read for rules")
	}
	if len(s.hosts["shop.example.com"]) != 1 || !slices.Equal(s.paths["shop.example.com"], []string{"/admin"}) || len(s.hosts["web.example.com"]) != 1 || s.files != 2 {
		t.Errorf("the scan read %v, paths %v, %d files", s.hosts, s.paths, s.files)
	}
	if !slices.ContainsFunc(s.patterns, func(p hostPattern) bool { return p.re.MatchString("a.example.org") }) {
		t.Error("a Caddy wildcard reads as no pattern")
	}
	big := make([]byte, maxDynamic+1)
	if err := os.WriteFile(filepath.Join(dir, "big.yaml"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	s = scan{hosts: map[string][]mention{}, paths: map[string][]string{}}
	if why := r.scanMount(&s, "/srv/proxy"); !strings.Contains(why, "big.yaml is larger than Dockwell reads") {
		t.Errorf("a file past the cap reads %q", why)
	}
}
