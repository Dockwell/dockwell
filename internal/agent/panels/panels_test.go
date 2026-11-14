// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

func TestDomainsAreReadFromTheProxysLabels(t *testing.T) {
	for name, c := range map[string]struct {
		labels map[string]string
		want   []string
	}{
		"Traefik, one host":          {map[string]string{"traefik.enable": "true", "traefik.http.routers.a.rule": "Host(`blog.example.com`) && PathPrefix(`/`)"}, []string{"blog.example.com"}},
		"Traefik, two rules":         {map[string]string{"traefik.enable": "true", "traefik.http.routers.a.rule": "Host(`a.example.com`) || Host(`b.example.com`)"}, []string{"a.example.com", "b.example.com"}},
		"Traefik, one Host with two": {map[string]string{"traefik.enable": "true", "traefik.http.routers.a.rule": "Host(`a.example.com`,`www.a.example.com`)"}, []string{"a.example.com", "www.a.example.com"}},
		"Caddy":                      {map[string]string{"caddy_0": "https://shop.example.com", "caddy_1": "http://www.shop.example.com:8080/x"}, []string{"shop.example.com", "www.shop.example.com"}},
		"no domain, a secret":        {map[string]string{"traefik.enable": "true", "traefik.http.middlewares.a.basicauth.users": "admin:$apr1$x", "traefik.http.routers.a.rule": "PathPrefix(`/api`)"}, nil},
		"not a host at all":          {map[string]string{"traefik.enable": "true", "traefik.http.routers.a.rule": "Host(`localhost`)"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := domainsOf(c.labels); !slices.Equal(got, c.want) {
				t.Errorf("domainsOf = %q, want %q", got, c.want)
			}
		})
	}
}

func TestWhatAnImageSays(t *testing.T) {
	for image, want := range map[string][2]string{
		"ghcr.io/coollabsio/coolify:4.7.5":         {"4.7.5", "application"},
		"dokploy/dokploy:v0.31.2@sha256:00":        {"v0.31.2", "application"},
		"postgres:16":                              {"16", "postgres"},
		"docker.io/library/mariadb:11.4":           {"11.4", "mariadb"},
		"ghcr.io/tursodatabase/libsql-server:main": {"main", "libsql"},
		"registry.example.com:5000/app":            {"", "application"},
	} {
		if got := [2]string{tagOf(image), engineOf(image)}; got != want {
			t.Errorf("%s says %q, want %q", image, got, want)
		}
	}
}

// A server with neither panel says so, as a fact, and reads what Docker says alone.
func TestNoPanelSaysWhy(t *testing.T) {
	e := dockertest.New(t)
	e.AddContainer(docker.Container{Name: "/web", Config: docker.Config{Image: "nginx:1.27"}})
	r := Reader{Docker: e.Client()}
	for kind, want := range map[dockwellv1.PanelKind]string{
		dockwellv1.PanelKind_PANEL_KIND_COOLIFY: "Coolify doesn't run here",
		dockwellv1.PanelKind_PANEL_KIND_DOKPLOY: "Dokploy doesn't run here",
	} {
		got, err := r.Read(t.Context(), kind)
		if err != nil || got.GetFound() != nil || !strings.HasPrefix(got.GetNotHere(), want) {
			t.Errorf("%v = %v, %v, want %q", kind, got, err, want)
		}
	}
}

// Coolify's resources come by their kind and UUID, with their names from its labels,
// each container of a service in one, its folder where it's on the server, and a
// version this reader wasn't written from read the same way.
func TestCoolifysResourcesComeByTheirLabels(t *testing.T) {
	e := dockertest.New(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data", "coolify", "services", "k4s9"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.AddContainer(docker.Container{Name: "/coolify", Config: docker.Config{Image: "ghcr.io/coollabsio/coolify:4.8.0"}})
	add := func(name, service string) {
		e.AddContainer(docker.Container{Name: "/" + name, Config: docker.Config{Image: "ghost:5", Labels: map[string]string{
			"coolify.managed": "true", "coolify.type": "service", "coolify.serviceUuid": "k4s9", "coolify.resourceName": "blog",
			"coolify.projectName": "personal", "coolify.environmentName": "production", "com.docker.compose.project": "k4s9",
			"com.docker.compose.service": service,
		}}})
	}
	add("ghost-k4s9", "ghost")
	add("mysql-k4s9", "mysql")
	got, err := Reader{Docker: e.Client(), Root: root}.Read(t.Context(), dockwellv1.PanelKind_PANEL_KIND_COOLIFY)
	if err != nil {
		t.Fatal(err)
	}
	p := got.GetFound()
	if p.GetVersion() != "4.8.0" || p.GetKnownVersion() || len(p.GetOwn()) != 1 || len(p.GetApps()) != 1 {
		t.Fatalf("found %v", p)
	}
	a := p.GetApps()[0]
	if a.GetName() != "blog" || a.GetKind() != "service" || a.GetId() != "k4s9" || a.GetComposeProject() != "k4s9" ||
		!slices.Equal(a.GetContainers(), []string{"ghost-k4s9", "mysql-k4s9"}) || a.GetFolder() != "/data/coolify/services/k4s9" {
		t.Errorf("the service reads as %v", a)
	}
}

// Dokploy's stack comes as one app of its services, and a service of someone
// else's in the swarm isn't Dokploy's.
func TestDokploysStackIsOneApp(t *testing.T) {
	e := dockertest.New(t)
	spec := func(name string, labels map[string]string) docker.Service {
		var s docker.ServiceSpec
		s.Name, s.Labels, s.TaskTemplate.ContainerSpec.Image = name, labels, "nginx:1.27"
		return docker.Service{ID: name, Spec: s}
	}
	e.Services = []docker.Service{
		spec("dokploy", nil),
		spec("forum-q1_web", map[string]string{"com.docker.stack.namespace": "forum-q1"}),
		spec("forum-q1_db", map[string]string{"com.docker.stack.namespace": "forum-q1"}),
		spec("someone-elses", nil),
	}
	got, err := Reader{Docker: e.Client()}.Read(t.Context(), dockwellv1.PanelKind_PANEL_KIND_DOKPLOY)
	if err != nil {
		t.Fatal(err)
	}
	apps := got.GetFound().GetApps()
	if len(apps) != 1 || apps[0].GetKind() != "stack" || !apps[0].GetSwarm() || !slices.Equal(apps[0].GetContainers(), []string{"forum-q1_db", "forum-q1_web"}) {
		t.Errorf("the apps are %v", apps)
	}
}

// A label names a folder only as a plain word: one that leads anywhere else, or out
// of the panel's folder, names none, and neither does a link.
func TestAFolderStaysUnderThePanels(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"data/coolify/services/k4s9", "data/coolify/x", "etc/secret"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "etc"), filepath.Join(root, "data", "coolify", "services", "l1nk")); err != nil {
		t.Fatal(err)
	}
	r := Reader{Root: root}
	if got := r.folder(coolifyFolder, "services", "k4s9"); got != "/data/coolify/services/k4s9" {
		t.Errorf("a plain word's folder = %q", got)
	}
	for _, parts := range [][]string{
		{"services", ".."},
		{"services", "../x"},
		{"..", "..", "etc", "secret"},
		{"services", "/etc/secret"},
		{"services", "K4S9"},
		{"services", "k4s9/"},
		{"services", ""},
		{"services", "l1nk"},
	} {
		if got := r.folder(coolifyFolder, parts...); got != "" {
			t.Errorf("%q names the folder %q", parts, got)
		}
	}
}

// Dokploy's Traefik file is read only where it's a file: a link is never followed,
// and a pipe is never waited on.
func TestOnlyAFileOfDokploysIsRead(t *testing.T) {
	root := t.TempDir()
	dynamic := filepath.Join(root, "etc", "dokploy", "traefik", "dynamic")
	if err := os.MkdirAll(dynamic, 0o755); err != nil {
		t.Fatal(err)
	}
	rule := []byte("http:\n  routers:\n    shop-k2m4-router-1:\n      rule: Host(`shop.example.com`)\n      service: shop-k2m4-service-1\n" +
		"    shop-k2m4-router-websecure-1:\n      rule: Host(`shop.example.com`)\n      service: shop-k2m4-service-1\n" +
		"  services:\n    shop-k2m4-service-1:\n      loadBalancer:\n        servers:\n          - url: http://shop-k2m4:3000\n        passHostHeader: true\n")
	if err := os.WriteFile(filepath.Join(root, "elsewhere.yml"), rule, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dynamic, "shop-k2m4.yml"), rule, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere.yml"), filepath.Join(dynamic, "linked-k2m4.yml")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dynamic, "piped-k2m4.yml"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Reader{Root: root}
	got := r.dynamicRoutes("shop-k2m4", "shop-k2m4.1.x7k2", nil)
	if len(got) != 1 || got[0].GetDomain() != "shop.example.com" || got[0].GetPort() != 3000 || got[0].GetService() != "shop-k2m4" || got[0].GetContainer() != "shop-k2m4.1.x7k2" {
		t.Errorf("a file's routes = %v", got)
	}
	done := make(chan []*dockwellv1.PanelRoute, 1)
	go func() {
		done <- append(r.dynamicRoutes("linked-k2m4", "", nil), r.dynamicRoutes("piped-k2m4", "", nil)...)
	}()
	select {
	case got := <-done:
		if len(got) > 0 {
			t.Errorf("a link or a pipe gives %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a pipe waited for a writer")
	}
}

// What a panel deploys from Git is read from what it leaves: Coolify tags an image
// it built with the commit, or docker compose names it for the project, and Dokploy
// clones into the app's code folder.
func TestWhatComesFromGit(t *testing.T) {
	for image, want := range map[string]bool{
		"m3n5:0f3c2a9d1e": true, "registry.example.com/shop:" + strings.Repeat("ab", 20): true, "m3n5-web:latest": true, "m3n5_web": true,
		"m3n5:latest": false, "m3n5:build": false, "ghost:5": false, "nginx:1.27@sha256:00": false, "sha256:" + strings.Repeat("ab", 32): false,
	} {
		if got := builtFromGit(image, "m3n5"); got != want {
			t.Errorf("builtFromGit(%q) = %v, want %v", image, got, want)
		}
	}

	e := dockertest.New(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc", "dokploy", "compose", "wiki-ab12", "code", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc", "dokploy", "compose", "raw-cd34", "code"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"wiki-ab12", "raw-cd34"} {
		e.AddContainer(docker.Container{Name: "/" + app + "-web-1", Config: docker.Config{Image: "nginx:1.27", Labels: map[string]string{
			"com.docker.compose.project": app, "com.docker.compose.project.working_dir": "/etc/dokploy/compose/" + app + "/code",
		}}})
	}
	got, err := Reader{Docker: e.Client(), Root: root}.Read(t.Context(), dockwellv1.PanelKind_PANEL_KIND_DOKPLOY)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got.GetFound().GetApps() {
		if a.GetFromGit() != (a.GetId() == "wiki-ab12") {
			t.Errorf("%s reads as from Git: %v", a.GetId(), a.GetFromGit())
		}
	}
}

// Where a domain goes is read from the proxy's labels on the container: a Traefik
// router's hosts and path, and the port of the service it names, or the one service
// the labels have; a Caddy address and the port its reverse_proxy names. A domain
// with no port says 0, and a router for the same domain twice, as for HTTP and
// HTTPS, comes once.
func TestWhereEachDomainGoes(t *testing.T) {
	got := routesOf(map[string]string{
		"traefik.enable":                                                    "true",
		"com.docker.compose.service":                                        "ghost",
		"traefik.http.routers.http-0-k4s9-ghost.rule":                       "Host(`blog.example.com`) && PathPrefix(`/`)",
		"traefik.http.routers.https-0-k4s9-ghost.rule":                      "Host(`blog.example.com`) && PathPrefix(`/`)",
		"traefik.http.routers.https-0-k4s9-ghost.service":                   "https-0-k4s9-ghost",
		"traefik.http.services.https-0-k4s9-ghost.loadbalancer.server.port": "2368",
		"traefik.http.routers.api.rule":                                     "Host(`blog.example.com`) && PathPrefix(`/ghost/api`)",
		"traefik.http.routers.api.service":                                  "nowhere",
	}, "ghost-k4s9", nil)
	want := []string{"blog.example.com/ghost/api|ghost|0", "blog.example.com|ghost|2368"}
	var have []string
	for _, r := range got {
		have = append(have, r.GetDomain()+r.GetPath()+"|"+r.GetService()+"|"+strconv.Itoa(int(r.GetPort())))
		if r.GetContainer() != "ghost-k4s9" {
			t.Errorf("%s goes to %s", r.GetDomain(), r.GetContainer())
		}
	}
	slices.Sort(have)
	if !slices.Equal(have, want) {
		t.Errorf("Traefik's routes are %v, want %v", have, want)
	}

	got = routesOf(map[string]string{
		"com.docker.compose.service": "web",
		"caddy_0":                    "https://shop.example.com, www.shop.example.com",
		"caddy_0.reverse_proxy":      "{{upstreams 8080}}",
	}, "web-p2q7", nil)
	if len(got) != 2 || got[0].GetPort() != 8080 || got[1].GetDomain() != "www.shop.example.com" || got[1].GetService() != "web" {
		t.Errorf("Caddy's routes are %v", got)
	}
}

// A domain behind a middleware a plain route would drop doesn't come over as one,
// so what was behind a login or an allowlist never answers anyone: only a redirect
// to HTTPS, compression, headers and a redirect of www pass. A stripped path, a
// rule a route can't say, an entrypoint other than the web's and a container
// Traefik leaves alone don't come either. Coolify's Caddy labels, handle_path and
// all, read as it writes them.
func TestWhatADomainCantBringStaysWithIt(t *testing.T) {
	base := map[string]string{
		"traefik.enable":             "true",
		"com.docker.compose.service": "web",
		"traefik.http.middlewares.redirect-to-https.redirectscheme.scheme": "https",
		"traefik.http.middlewares.gzip.compress":                           "true",
		"traefik.http.services.http-0-k4s9.loadbalancer.server.port":       "3000",
		"traefik.http.routers.http-0-k4s9.rule":                            "Host(`app.example.com`) && PathPrefix(`/`)",
		"traefik.http.routers.http-0-k4s9.entrypoints":                     "http",
		"traefik.http.routers.http-0-k4s9.middlewares":                     "redirect-to-https",
		"traefik.http.routers.https-0-k4s9.rule":                           "Host(`app.example.com`) && PathPrefix(`/`)",
		"traefik.http.routers.https-0-k4s9.entrypoints":                    "https",
		"traefik.http.routers.https-0-k4s9.middlewares":                    "gzip",
		"traefik.http.routers.https-0-k4s9.service":                        "http-0-k4s9",
	}
	if got := routesOf(base, "web-k4s9", nil); len(got) != 1 || got[0].GetLeft() != "" || got[0].GetPort() != 3000 {
		t.Fatalf("Coolify's plain app reads %v", got)
	}
	for name, more := range map[string]map[string]string{
		"forwardauth": {"traefik.http.routers.https-0-k4s9.middlewares": "gzip,authelia@docker", "traefik.http.middlewares.authelia.forwardauth.address": "http://authelia:9091"},
		"unread":      {"traefik.http.routers.https-0-k4s9.middlewares": "sso@file"},
		"allowlist":   {"traefik.http.routers.https-0-k4s9.middlewares": "office", "traefik.http.middlewares.office.ipallowlist.sourcerange": "192.0.2.0/24"},
		"stripprefix": {"traefik.http.routers.https-0-k4s9.middlewares": "gzip,s", "traefik.http.middlewares.s.stripprefix.prefixes": "/app"},
		"rule":        {"traefik.http.routers.https-0-k4s9.rule": "Host(`app.example.com`) && !PathPrefix(`/admin`)"},
		"entrypoint":  {"traefik.http.routers.https-0-k4s9.entrypoints": "metrics"},
	} {
		l := maps.Clone(base)
		maps.Copy(l, more)
		got := routesOf(l, "web-k4s9", nil)
		if len(got) == 0 || got[0].GetLeft() == "" {
			t.Errorf("%s: the domain comes as a plain route: %v", name, got)
		}
	}
	off := maps.Clone(base)
	off["traefik.enable"] = "false"
	if got := routesOf(off, "web-k4s9", nil); len(got) != 0 {
		t.Errorf("a container Traefik leaves alone gives %v", got)
	}

	// As Coolify writes Caddy's labels: the root path comes, a stripped one doesn't.
	caddy := func(path string) map[string]string {
		return map[string]string{
			"com.docker.compose.service":          "web",
			"caddy_0":                             "https://shop.example.com",
			"caddy_0.header":                      "-Server",
			"caddy_0.try_files":                   "{path} /index.html /index.php",
			"caddy_0.handle_path.0_reverse_proxy": "{{upstreams 8080}}",
			"caddy_0.handle_path.0_reverse_proxy.stream_close_delay": "5m",
			"caddy_0.handle_path": path + "*",
			"caddy_0.encode":      "zstd gzip",
		}
	}
	if got := routesOf(caddy("/"), "web-p2q7", nil); len(got) != 1 || got[0].GetPort() != 8080 || got[0].GetLeft() != "" || got[0].GetPath() != "" {
		t.Errorf("Coolify's Caddy app reads %v", got)
	}
	if got := routesOf(caddy("/shop"), "web-p2q7", nil); len(got) != 1 || got[0].GetLeft() == "" {
		t.Errorf("a stripped Caddy path reads %v", got)
	}
	auth := caddy("/")
	auth["caddy_0.forward_auth"] = "authelia:9091"
	if got := routesOf(auth, "web-p2q7", nil); len(got) != 1 || got[0].GetLeft() == "" {
		t.Errorf("Caddy's forward_auth reads %v", got)
	}
}

// The panel's proxy routes its own dashboard, and anything a person added, from
// its own files: those domains stop answering when it hands over its ports.
func TestThePanelsOwnDomains(t *testing.T) {
	root := t.TempDir()
	dynamic := filepath.Join(root, "data", "coolify", "proxy", "dynamic")
	if err := os.MkdirAll(dynamic, 0o755); err != nil {
		t.Fatal(err)
	}
	coolify := "http:\n  routers:\n    coolify-http:\n      rule: Host(`coolify.example.com`)\n"
	if err := os.WriteFile(filepath.Join(dynamic, "coolify.yaml"), []byte(coolify), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dynamic, "notes.txt"), []byte("rule: Host(`not.example.com`)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (Reader{Root: root}).ownDomains(dockwellv1.PanelKind_PANEL_KIND_COOLIFY); !slices.Equal(got, []string{"coolify.example.com"}) {
		t.Errorf("Coolify's own domains are %v", got)
	}
}

// Each way a guarded app could come over public, probed one at a time: a domain
// comes only where everything about it is a form Dockwell reads in full, and each
// other stays behind with a reason.
func TestAGuardedAppNeverComesOverPublic(t *testing.T) {
	app := func(more map[string]string) map[string]string {
		l := map[string]string{
			"traefik.enable":                                     "true",
			"com.docker.compose.service":                         "web",
			"traefik.http.routers.app.rule":                      "Host(`app.example.com`)",
			"traefik.http.services.app.loadbalancer.server.port": "3000",
		}
		maps.Copy(l, more)
		return l
	}
	for name, l := range map[string]map[string]string{
		// N1: one router guards /admin, so the root's plain route would serve it.
		"a sibling path behind forwardauth": app(map[string]string{
			"traefik.http.routers.admin.rule":                  "Host(`app.example.com`) && PathPrefix(`/admin`)",
			"traefik.http.routers.admin.middlewares":           "sso",
			"traefik.http.middlewares.sso.forwardauth.address": "http://authelia:9091",
			"traefik.http.routers.admin.service":               "app",
		}),
		// N2: access matchers say who may reach it.
		"a ClientIP matcher": app(map[string]string{"traefik.http.routers.app.rule": "Host(`app.example.com`) && ClientIP(`10.0.0.0/8`)"}),
		"a Method matcher":   app(map[string]string{"traefik.http.routers.app.rule": "Host(`app.example.com`) && Method(`GET`)"}),
		"a matcher to come":  app(map[string]string{"traefik.http.routers.app.rule": "Host(`app.example.com`) && Future(`x`)"}),
		"two PathPrefixes":   app(map[string]string{"traefik.http.routers.app.rule": "Host(`app.example.com`) && PathPrefix(`/a`) && PathPrefix(`/b`)"}),
		"an or of hosts":     app(map[string]string{"traefik.http.routers.app.rule": "Host(`app.example.com`) || Host(`b.example.com`)"}),
		// N3: a label-defined middleware never shadows the file one a router names.
		"a label shadowing @file": app(map[string]string{
			"traefik.http.routers.app.middlewares":   "gzip@file",
			"traefik.http.middlewares.gzip.compress": "true",
		}),
		"headers the app reads": app(map[string]string{
			"traefik.http.routers.app.middlewares":                                "h",
			"traefik.http.middlewares.h.headers.customrequestheaders.Remote-User": "admin",
		}),
	} {
		got := routesOf(l, "web-1", map[string]string{"gzip": "basicauth"})
		if len(got) == 0 {
			t.Errorf("%s: no domain named", name)
		}
		for _, r := range got {
			if r.GetLeft() == "" {
				t.Errorf("%s: %s%s comes as a plain route", name, r.GetDomain(), r.GetPath())
			}
		}
	}
	// N3: Traefik leaves alone what traefik.enable doesn't turn on.
	for _, off := range []string{"False", "0", "no", ""} {
		if got := routesOf(app(map[string]string{"traefik.enable": off}), "web-1", nil); len(got) != 0 {
			t.Errorf("traefik.enable=%q gives %v", off, got)
		}
	}
	// N3: Caddy's directives are read at every depth, without their N_ order.
	caddy := func(more map[string]string) map[string]string {
		l := map[string]string{"caddy_0": "https://shop.example.com", "caddy_0.handle_path": "/*", "caddy_0.handle_path.0_reverse_proxy": "{{upstreams 8080}}"}
		maps.Copy(l, more)
		return l
	}
	if got := routesOf(caddy(nil), "web-1", nil); len(got) != 1 || got[0].GetLeft() != "" {
		t.Fatalf("Coolify's plain Caddy app reads %v", got)
	}
	for name, more := range map[string]map[string]string{
		"nested basicauth":    {"caddy_0.handle_path.1_basicauth.bob": "$2a$14$x"},
		"nested forward_auth": {"caddy_0.handle.0_forward_auth": "authelia:9091"},
		"a matcher":           {"caddy_0.@office.remote_ip": "192.0.2.0/24"},
		"a proxy's transport": {"caddy_0.handle_path.0_reverse_proxy.transport": "http"},
	} {
		if got := routesOf(caddy(more), "web-1", nil); len(got) != 1 || got[0].GetLeft() == "" {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// N4: what Dokploy writes for a compose app's HTTPS domain, from its source at
// v0.31.2 (packages/server/src/utils/docker/domain.ts): a web router that
// redirects with redirect-to-https@file, which its middlewares.yml defines as a
// redirectScheme, and a websecure router. It comes over; a stripped path doesn't.
func TestDokploysHTTPSDomainComesOver(t *testing.T) {
	root := t.TempDir()
	dynamic := filepath.Join(root, "etc", "dokploy", "traefik", "dynamic")
	if err := os.MkdirAll(dynamic, 0o755); err != nil {
		t.Fatal(err)
	}
	middlewares := "http:\n  middlewares:\n    redirect-to-https:\n      redirectScheme:\n        scheme: https\n        permanent: true\n"
	if err := os.WriteFile(filepath.Join(dynamic, "middlewares.yml"), []byte(middlewares), 0o644); err != nil {
		t.Fatal(err)
	}
	files := Reader{Root: root}.fileMiddlewares(dockwellv1.PanelKind_PANEL_KIND_DOKPLOY)
	if files["redirect-to-https"] != "redirectscheme" {
		t.Fatalf("Dokploy's middlewares read as %v", files)
	}
	labels := map[string]string{
		"traefik.enable":                                                        "true",
		"com.docker.compose.service":                                            "wiki",
		"traefik.http.routers.wiki-ab12-x1-web.rule":                            "Host(`wiki.example.com`)",
		"traefik.http.routers.wiki-ab12-x1-web.entrypoints":                     "web",
		"traefik.http.services.wiki-ab12-x1-web.loadbalancer.server.port":       "3000",
		"traefik.http.routers.wiki-ab12-x1-web.service":                         "wiki-ab12-x1-web",
		"traefik.http.routers.wiki-ab12-x1-web.middlewares":                     "redirect-to-https@file",
		"traefik.http.routers.wiki-ab12-x1-websecure.rule":                      "Host(`wiki.example.com`)",
		"traefik.http.routers.wiki-ab12-x1-websecure.entrypoints":               "websecure",
		"traefik.http.services.wiki-ab12-x1-websecure.loadbalancer.server.port": "3000",
		"traefik.http.routers.wiki-ab12-x1-websecure.service":                   "wiki-ab12-x1-websecure",
		"traefik.http.routers.wiki-ab12-x1-websecure.tls.certresolver":          "letsencrypt",
	}
	if got := routesOf(labels, "wiki-ab12-wiki-1", files); len(got) != 1 || got[0].GetLeft() != "" || got[0].GetPort() != 3000 {
		t.Errorf("Dokploy's HTTPS domain reads %v", got)
	}
	// Its file's redirect, unread, would leave it behind: what isn't read isn't
	// told harmless.
	if got := routesOf(labels, "wiki-ab12-wiki-1", nil); len(got) != 1 || got[0].GetLeft() == "" {
		t.Errorf("with no file read, Dokploy's domain reads %v", got)
	}
	stripped := maps.Clone(labels)
	stripped["traefik.http.routers.wiki-ab12-x1-websecure.rule"] = "Host(`wiki.example.com`) && PathPrefix(`/docs`)"
	stripped["traefik.http.routers.wiki-ab12-x1-websecure.middlewares"] = "stripprefix-wiki-ab12-x1"
	stripped["traefik.http.middlewares.stripprefix-wiki-ab12-x1.stripprefix.prefixes"] = "/docs"
	for _, r := range routesOf(stripped, "wiki-ab12-wiki-1", files) {
		if r.GetLeft() == "" {
			t.Errorf("a stripped path's domain comes as %s%s", r.GetDomain(), r.GetPath())
		}
	}
}

// A rule YAML folds onto the next line, as >- writes a long one, is still read.
func TestAFoldedRuleIsRead(t *testing.T) {
	f := readTraefikFile([]byte("http:\n  routers:\n    shop-router:\n      rule: >-\n        Host(`shop.example.com`)\n      service: shop\n  services:\n    shop:\n      loadBalancer:\n        servers:\n          - url: http://shop:80\n"))
	if r := f.routers["shop-router"]; r == nil || !strings.Contains(r.rule, "Host(`shop.example.com`)") || f.ports["shop"] != 80 {
		t.Errorf("the folded file reads %+v %v", r, f.ports)
	}
}
