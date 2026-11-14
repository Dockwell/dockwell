// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
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
		"Traefik, one host":          {map[string]string{"traefik.http.routers.a.rule": "Host(`blog.example.com`) && PathPrefix(`/`)"}, []string{"blog.example.com"}},
		"Traefik, two rules":         {map[string]string{"traefik.http.routers.a.rule": "Host(`a.example.com`) || Host(`b.example.com`)"}, []string{"a.example.com", "b.example.com"}},
		"Traefik, one Host with two": {map[string]string{"traefik.http.routers.a.rule": "Host(`a.example.com`,`www.a.example.com`)"}, []string{"a.example.com", "www.a.example.com"}},
		"Caddy":                      {map[string]string{"caddy_0": "https://shop.example.com", "caddy_1": "http://www.shop.example.com:8080/x"}, []string{"shop.example.com", "www.shop.example.com"}},
		"no domain, a secret":        {map[string]string{"traefik.http.middlewares.a.basicauth.users": "admin:$apr1$x", "traefik.http.routers.a.rule": "PathPrefix(`/api`)"}, nil},
		"not a host at all":          {map[string]string{"traefik.http.routers.a.rule": "Host(`localhost`)"}, nil},
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
	rule := []byte("    rule: Host(`shop.example.com`)\n")
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
	if got := r.dynamicDomains("shop-k2m4"); !slices.Equal(got, []string{"shop.example.com"}) {
		t.Errorf("a file's domains = %q", got)
	}
	done := make(chan []string, 1)
	go func() { done <- append(r.dynamicDomains("linked-k2m4"), r.dynamicDomains("piped-k2m4")...) }()
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
		"com.docker.compose.service":                                        "ghost",
		"traefik.http.routers.http-0-k4s9-ghost.rule":                       "Host(`blog.example.com`) && PathPrefix(`/`)",
		"traefik.http.routers.https-0-k4s9-ghost.rule":                      "Host(`blog.example.com`) && PathPrefix(`/`)",
		"traefik.http.routers.https-0-k4s9-ghost.service":                   "https-0-k4s9-ghost",
		"traefik.http.services.https-0-k4s9-ghost.loadbalancer.server.port": "2368",
		"traefik.http.routers.api.rule":                                     "Host(`blog.example.com`) && PathPrefix(`/ghost/api`)",
		"traefik.http.routers.api.service":                                  "nowhere",
	}, "ghost-k4s9")
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
	}, "web-p2q7")
	if len(got) != 2 || got[0].GetPort() != 8080 || got[1].GetDomain() != "www.shop.example.com" || got[1].GetService() != "web" {
		t.Errorf("Caddy's routes are %v", got)
	}
}
