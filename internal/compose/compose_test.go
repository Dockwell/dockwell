// SPDX-License-Identifier: AGPL-3.0-only

package compose_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

const project = "env_01kat4zr6e3v0y5xbkzb5d9q6m"

const shop = `
name: ignored
x-common: &common
  restart: unless-stopped
services:
  web:
    <<: *common
    image: ghcr.io/acme/shop:${TAG:-1.4.2}
    command: ["serve", "--port", "8080"]
    environment:
      DATABASE_URL: postgres://shop@db/shop
      API_KEY: ${API_KEY}
    ports:
      - "8080:8080"
      - "127.0.0.1:9090:9090/udp"
    depends_on:
      db:
        condition: service_healthy
    networks: [front, back]
    labels:
      team: shop
      com.docker.compose.project: overridden
    expose: ["8080"]
  db:
    image: postgres:17
    volumes:
      - db-data:/var/lib/postgresql/data
      - /srv/backups:/backups:ro
      - /srv/conf:/etc/postgresql:ro,z
      - /srv/run:/var/run/postgresql:Z,cached
      - type: bind
        source: /srv/logs
        target: /logs
        bind: {selinux: z}
    environment:
      - POSTGRES_DB=shop
    healthcheck:
      test: pg_isready -U shop
      interval: 10s
      retries: 5
    networks: [back]
  worker:
    image: ghcr.io/acme/worker:1.0
    entrypoint: /bin/worker
    user: "1000"
    working_dir: /app
volumes:
  db-data:
    labels:
      backup: daily
networks:
  front: {}
  back:
    internal: true
`

func names[T any](items []compose.Item[T]) []string {
	var out []string
	for _, i := range items {
		out = append(out, i.Name)
	}
	return out
}

// A Compose file becomes services, volumes and networks, each checked as
// its kind checks it, in the order to make them.
func TestParse(t *testing.T) {
	s, err := compose.Parse(t.Context(), []byte(shop), map[string]string{"API_KEY": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(s.Services); !slices.Equal(got, []string{"db", "web", "worker"}) {
		t.Errorf("services in order %v, want db before web, which depends on it", got)
	}
	if got := names(s.Volumes); !slices.Equal(got, []string{"db-data"}) {
		t.Errorf("volumes %v", got)
	}
	// worker joins the project's default network, as docker compose puts it.
	if got := names(s.Networks); !slices.Equal(got, []string{"back", "default", "front"}) {
		t.Errorf("networks %v", got)
	}
	web := s.Services[1].Spec.GetApp()
	want := &resourcev1.Container{
		Image: "ghcr.io/acme/shop:1.4.2", Command: []string{"serve", "--port", "8080"},
		Env:    map[string]string{"DATABASE_URL": "postgres://shop@db/shop", "API_KEY": "s3cret"},
		Labels: map[string]string{"team": "shop"},
		Ports: []*resourcev1.PortMapping{
			{HostPort: 8080, ContainerPort: 8080, Protocol: resourcev1.Protocol_PROTOCOL_TCP},
			{HostPort: 9090, ContainerPort: 9090, Protocol: resourcev1.Protocol_PROTOCOL_UDP, HostIp: "127.0.0.1"},
		},
		Networks: []string{"back", "front"},
		Restart:  resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
	}
	if !proto.Equal(web, want) {
		t.Errorf("web = %v\nwant %v", web, want)
	}
	db := s.Services[0].Spec.GetApp()
	wantMounts := []*resourcev1.Mount{
		{Source: &resourcev1.Mount_Volume{Volume: "db-data"}, Target: "/var/lib/postgresql/data"},
		{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/backups"}, Target: "/backups", ReadOnly: true},
		{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/conf"}, Target: "/etc/postgresql", ReadOnly: true, Relabel: resourcev1.Relabel_RELABEL_SHARED},
		{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/run"}, Target: "/var/run/postgresql", Relabel: resourcev1.Relabel_RELABEL_PRIVATE},
		{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/logs"}, Target: "/logs", Relabel: resourcev1.Relabel_RELABEL_SHARED},
	}
	if !slices.EqualFunc(db.GetMounts(), wantMounts, func(a, b *resourcev1.Mount) bool { return proto.Equal(a, b) }) {
		t.Errorf("db mounts %v\nwant %v", db.GetMounts(), wantMounts)
	}
	if hc := db.GetHealthcheck(); !slices.Equal(hc.GetTest(), []string{"CMD-SHELL", "pg_isready -U shop"}) || hc.GetInterval() != "10s" || hc.GetRetries() != 5 {
		t.Errorf("db's health check = %v", hc)
	}
	if db.GetRestart() != resourcev1.RestartPolicy_RESTART_POLICY_NO {
		t.Errorf("db restarts %v, want no, as docker compose runs a service without a restart policy", db.GetRestart())
	}
	if !s.Networks[0].Spec.GetInternal() {
		t.Error("the back network isn't internal")
	}
	reg := resource.NewRegistry(&service.Driver{}, &volume.Driver{}, &network.Driver{})
	for _, svc := range s.Services {
		if err := reg.Validate(service.Kind, svc.Spec); err != nil {
			t.Errorf("service %s: %v", svc.Name, err)
		}
	}
}

// What Dockwell can't run is refused by name, with what to do, rather
// than quietly dropped; and the control plane never reads a file for one.
func TestParseRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ file, want string }{
		"build":           {"services: {web: {build: .}}", "the service web builds an image, which Dockwell can't do yet"},
		"env_file":        {"services: {web: {image: a, env_file: /etc/shadow}}", "the service web reads env_file"},
		"extends":         {"services: {web: {image: a, extends: {file: /etc/passwd, service: x}}}", "the service web extends another service"},
		"include":         {"include: [/etc/passwd]\nservices: {web: {image: a}}", "the Compose file includes other files"},
		"unknown":         {"services: {web: {image: a, sysctls: [a=b]}}", "the service web uses sysctls, which Dockwell can't run yet. Remove it from the Compose file to import it"},
		"relative bind":   {"services: {web: {image: a, volumes: ['./data:/data']}}", "the service web mounts ./data, a path relative to the Compose file"},
		"anonymous":       {"services: {web: {image: a, volumes: ['/data']}}", "the service web mounts an unnamed volume at /data"},
		"random port":     {"services: {web: {image: a, ports: ['80']}}", "the service web publishes port 80 on a server port Docker would pick"},
		"unset":           {"services: {web: {image: 'a:${TAG}'}}", "the Compose file uses TAG, with no value"},
		"bad name":        {"services: {my_app: {image: a}}", `"my_app" isn't a valid service name`},
		"external":        {"services: {web: {image: a}}\nvolumes: {data: {external: true}}", "the volume data is external"},
		"network options": {"services: {web: {image: a, networks: {n: {ipv4_address: 10.0.0.2}}}}\nnetworks: {n: {}}", "the service web's network n uses ipv4_address, which Dockwell can't run yet"},
		"no network":      {"services: {web: {image: a, network_mode: none}}", "the service web has network_mode none, no network at all"},
		"no value":        {"services: {web: {image: a, environment: [SECRET]}}", "the service web's variable SECRET has no value"},
		"restart count":   {"services: {web: {image: a, restart: 'always:3'}}", `the service web restarts "always:3", and only on-failure takes a count`},
		"no services":     {"volumes: {data: {}}", "the Compose file has no services"},
		"not yaml":        {"services: [", "the Compose file isn't valid YAML"},
		"no volume":       {"services: {web: {image: a, volumes: ['data:/data']}}", "the service web mounts the volume data, which the file doesn't list under volumes"},
		"no such network": {"services: {web: {image: a, networks: [back]}}", "the service web joins the network back, which the file doesn't list under networks"},
		"no dependency":   {"services: {web: {image: a, depends_on: [db]}}", "the service web depends on db, which isn't a service in the file"},
		"cycle":           {"services: {a: {image: x, depends_on: [b]}, b: {image: x, depends_on: [a]}}", "the services depend on each other in a circle, a → b → a"},
		"bad port":        {"services: {web: {image: a, ports: ['eighty:80']}}", "the Compose file isn't valid"},
		"propagation":     {"services: {web: {image: a, volumes: ['/srv:/data:ro,rshared']}}", "the service web mounts /srv at /data with rshared propagation, which Dockwell can't run yet. Remove :rshared"},
		"nocopy":          {"services: {web: {image: a, volumes: ['data:/data:nocopy']}}\nvolumes: {data: {}}", "the service web mounts data at /data with nocopy, which Dockwell can't run yet"},
		"no such option":  {"services: {web: {image: a, volumes: ['/srv:/data:rx']}}", `the service web mounts /srv at /data with the option "rx", which Docker doesn't take`},
		"long propagation": {
			"services: {web: {image: a, volumes: [{type: bind, source: /srv, target: /data, bind: {propagation: rshared}}]}}",
			"the service web's volume's bind uses propagation, which Dockwell can't run yet",
		},
		"long nocopy": {
			"services: {web: {image: a, volumes: [{type: volume, source: data, target: /data, volume: {nocopy: true}}]}}\nvolumes: {data: {}}",
			"the service web's volume uses volume, which Dockwell can't run yet",
		},
		"relabelled volume": {"services: {web: {image: a, volumes: ['data:/data:z']}}\nvolumes: {data: {}}", "the service web mounts the volume data at /data with :z, which Dockwell can do for a server path alone"},
		"no such relabel": {
			"services: {web: {image: a, volumes: [{type: bind, source: /srv, target: /data, bind: {selinux: q}}]}}",
			"the service web relabels /srv with selinux q, which Docker doesn't take",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compose.Parse(t.Context(), []byte(tc.file), nil)
			if !errors.Is(err, compose.ErrInvalid) || !strings.Contains(compose.Message(err), tc.want) {
				t.Errorf("Parse = %v, want %q", err, tc.want)
			}
		})
	}
}

// An exported file reads back as what was exported, so docker compose
// runs what Dockwell runs, and it never holds a secret.
func TestExportRoundTrips(t *testing.T) {
	s, err := compose.Parse(t.Context(), []byte(shop), map[string]string{"API_KEY": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	reg := resource.NewRegistry(&service.Driver{}, &volume.Driver{}, &network.Driver{})
	for _, i := range s.Services {
		_ = reg.Validate(service.Kind, i.Spec)
	}
	for _, i := range s.Volumes {
		_ = reg.Validate(volume.Kind, i.Spec)
	}
	for _, i := range s.Networks {
		_ = reg.Validate(network.Kind, i.Spec)
	}
	// A service run from an image, on Docker's bridge, exports as one.
	s.Services = append(s.Services, compose.Item[*resourcev1.Service]{Name: "solo", Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "nginx:1.27", Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
		Ports: []*resourcev1.PortMapping{{HostPort: 8443, ContainerPort: 443, HostIp: "::1", Protocol: resourcev1.Protocol_PROTOCOL_TCP}},
		Mounts: []*resourcev1.Mount{
			{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/tls"}, Target: "/etc/tls", Relabel: resourcev1.Relabel_RELABEL_SHARED},
			{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/www"}, Target: "/usr/share/nginx/html", ReadOnly: true, Relabel: resourcev1.Relabel_RELABEL_PRIVATE},
		},
	}}}})
	out, err := compose.Export(project, s)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out.File)
	if strings.Contains(text, "s3cret") || strings.Contains(text, "postgres://shop") {
		t.Errorf("the export holds an environment variable's value:\n%s", text)
	}
	if want := []string{"db.POSTGRES_DB", "web.API_KEY", "web.DATABASE_URL"}; !slices.Equal(out.Omitted, want) {
		t.Errorf("omitted %v, want %v", out.Omitted, want)
	}
	if !strings.Contains(text, "name: "+project) || !strings.Contains(text, "network_mode: bridge") {
		t.Errorf("export:\n%s", text)
	}

	// A person reads this file: what Dockwell keeps for itself isn't in it, and
	// what looks like a mistake says why it isn't.
	if strings.Contains(text, container.LabelSet) || strings.Contains(text, "dev.dockwell") {
		t.Errorf("the export carries Dockwell's own label:\n%s", text)
	}
	if !strings.Contains(text, "NAME: null is there on purpose") {
		t.Errorf("the export doesn't explain a variable with no value:\n%s", text)
	}

	// Set the variables it left out, as the shell would, and read it back.
	vars := map[string]string{"API_KEY": "s3cret", "DATABASE_URL": "postgres://shop@db/shop", "POSTGRES_DB": "shop"}
	back, err := compose.Parse(t.Context(), out.File, vars)
	if err != nil {
		t.Fatalf("the export doesn't read back: %v\n%s", err, text)
	}
	if got, want := names(back.Services), []string{"db", "solo", "web", "worker"}; !slices.Equal(got, want) {
		t.Errorf("services read back %v, want %v", got, want)
	}
	byName := map[string]*resourcev1.Service{}
	for _, i := range back.Services {
		_ = reg.Validate(service.Kind, i.Spec)
		byName[i.Name] = i.Spec
	}
	for _, i := range s.Services {
		if !proto.Equal(byName[i.Name], i.Spec) {
			t.Errorf("%s read back as %v\nwant %v", i.Name, byName[i.Name], i.Spec)
		}
	}
	if got, want := names(back.Networks), names(s.Networks); !slices.Equal(got, want) {
		t.Errorf("networks read back %v, want %v", got, want)
	}
	for i := range back.Volumes {
		_ = reg.Validate(volume.Kind, back.Volumes[i].Spec)
		if !proto.Equal(back.Volumes[i].Spec, s.Volumes[i].Spec) {
			t.Errorf("volume %s read back as %v, want %v", back.Volumes[i].Name, back.Volumes[i].Spec, s.Volumes[i].Spec)
		}
	}
}
