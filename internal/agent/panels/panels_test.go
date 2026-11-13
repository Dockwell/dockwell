// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
