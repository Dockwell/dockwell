// SPDX-License-Identifier: AGPL-3.0-only

package compose_test

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/health"
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
		"build":            {"services: {web: {build: .}}", "the service web builds an image, which Dockwell can't do yet"},
		"env_file":         {"services: {web: {image: a, env_file: /etc/shadow}}", "the service web reads `env_file`"},
		"extends":          {"services: {web: {image: a, extends: {file: /etc/passwd, service: x}}}", "the service web extends another service"},
		"include":          {"include: [/etc/passwd]\nservices: {web: {image: a}}", "the Compose file includes other files"},
		"unknown":          {"services: {web: {image: a, sysctls: [a=b]}}", "the service web uses `sysctls`, which Dockwell can't run yet. Remove it from the Compose file to import it"},
		"relative bind":    {"services: {web: {image: a, volumes: ['./data:/data']}}", "the service web mounts ./data, a path relative to the Compose file"},
		"anonymous":        {"services: {web: {image: a, volumes: ['/data']}}", "the service web mounts an unnamed volume at /data"},
		"random port":      {"services: {web: {image: a, ports: ['80']}}", "the service web publishes port 80 on a server port Docker would pick"},
		"unset":            {"services: {web: {image: 'a:${TAG}'}}", "the Compose file uses TAG, with no value"},
		"bad name":         {"services: {my_app: {image: a}}", `"my_app" isn't a valid service name`},
		"external":         {"services: {web: {image: a}}\nvolumes: {data: {external: true}}", "the volume data is `external`"},
		"network options":  {"services: {web: {image: a, networks: {n: {ipv4_address: 10.0.0.2}}}}\nnetworks: {n: {}}", "the service web's network n uses `ipv4_address`, which Dockwell can't run yet"},
		"no network":       {"services: {web: {image: a, network_mode: none}}", "the service web has network_mode none, no network at all"},
		"no value":         {"services: {web: {image: a, environment: [SECRET]}}", "the service web's variable SECRET has no value"},
		"restart count":    {"services: {web: {image: a, restart: 'always:3'}}", `the service web restarts "always:3", and only on-failure takes a count`},
		"no services":      {"volumes: {data: {}}", "the Compose file has no services"},
		"not yaml":         {"services: [", "line 1: a bracket doesn't close"},
		"no volume":        {"services: {web: {image: a, volumes: ['data:/data']}}", "the service web mounts the volume data, which the file doesn't list under volumes"},
		"no such network":  {"services: {web: {image: a, networks: [back]}}", "the service web joins the network back, which the file doesn't list under networks"},
		"no dependency":    {"services: {web: {image: a, depends_on: [db]}}", "the service web depends on db, which isn't a service in the file"},
		"cycle":            {"services: {a: {image: x, depends_on: [b]}, b: {image: x, depends_on: [a]}}", "the services depend on each other in a circle, a → b → a"},
		"bad port":         {"services: {web: {image: a, ports: ['eighty:80']}}", "the Compose file isn't valid"},
		"propagation":      {"services: {web: {image: a, volumes: ['/srv:/data:ro,rshared']}}", "the service web mounts /srv at /data with rshared propagation, which Dockwell can't run yet. Remove :rshared"},
		"nocopy":           {"services: {web: {image: a, volumes: ['data:/data:nocopy']}}\nvolumes: {data: {}}", "the service web mounts data at /data with nocopy, which Dockwell can't run yet"},
		"no such option":   {"services: {web: {image: a, volumes: ['/srv:/data:rx']}}", `the service web mounts /srv at /data with the option "rx", which Docker doesn't take`},
		"replicas":         {"services: {web: {image: a, deploy: {replicas: 3}}}", "the service web asks for 3 replicas"},
		"parallelism":      {"services: {web: {image: a, deploy: {update_config: {parallelism: 2}}}}", "the service web updates 2 containers at a time"},
		"failure action":   {"services: {web: {image: a, deploy: {update_config: {failure_action: continue}}}}", "the service web has failure_action continue, and Dockwell rolls a failed deploy back"},
		"update order":     {"services: {web: {image: a, deploy: {update_config: {order: sideways}}}}", "the service web has the update order sideways"},
		"deploy keys":      {"services: {web: {image: a, deploy: {mode: global}}}", "the service web's deploy uses `mode`, which Dockwell can't run yet"},
		"update keys":      {"services: {web: {image: a, deploy: {update_config: {monitor: 5s}}}}", "the service web's deploy.update_config uses `monitor`, which Dockwell can't run yet"},
		"extension keys":   {"services: {web: {image: a, x-dockwell: {nope: 1}}}", "the service web's x-dockwell uses `nope`, which Dockwell can't run yet"},
		"extension kind":   {"services: {web: {image: a, x-dockwell: nope}}", "the service web's x-dockwell should be a mapping"},
		"check keys":       {"services: {web: {image: a, x-dockwell: {healthcheck: {test: [CMD, 'true']}}}}", "the service web's x-dockwell's healthcheck uses `test`, which Dockwell can't run yet"},
		"check of neither": {"services: {web: {image: a, x-dockwell: {healthcheck: {interval: 5s}}}}", "needs http or tcp"},
		"http keys":        {"services: {web: {image: a, x-dockwell: {healthcheck: {http: {url: 'http://169.254.169.254/'}}}}}", "the service web's x-dockwell's healthcheck's http uses `url`, which Dockwell can't run yet"},
		"two checks":       {"services: {web: {image: a, healthcheck: {test: [CMD, 'true']}, x-dockwell: {healthcheck: {tcp: {port: 80}}}}}", "a service has one check"},
		"owner kind":       {"services: {web: {image: a, x-dockwell: {make_owner: 1000}}}", "the service web's x-dockwell's make_owner should be a mapping of each mount's target to its owner"},
		"owner shape":      {"services: {web: {image: a, volumes: [data:/data], x-dockwell: {make_owner: {/data: www-data}}}}\nvolumes: {data: {}}", "the service web's x-dockwell's make_owner for /data"},
		"owner of nothing": {"services: {web: {image: a, x-dockwell: {make_owner: {/data: '1000:1000'}}}}", "names /data, which no volume or server path of it mounts"},
		"mode of a path":   {"services: {web: {image: a, volumes: ['/srv:/data'], x-dockwell: {make_mode: {/data: '0770'}}}}", "names /data, which no volume of it mounts"},
		"mode with setuid": {"services: {web: {image: a, volumes: [data:/data], x-dockwell: {make_mode: {/data: '4755'}}}}\nvolumes: {data: {}}", "read, write and execute bits alone"},
		"seed of nothing":  {"services: {web: {image: a, volumes: [data:/data], x-dockwell: {seed: {/data: {a.conf: nope}}}}}\nvolumes: {data: {}}", "names the config nope, which the file doesn't write"},
		"long propagation": {
			"services: {web: {image: a, volumes: [{type: bind, source: /srv, target: /data, bind: {propagation: rshared}}]}}",
			"the service web's volume's bind uses `propagation`, which Dockwell can't run yet",
		},
		"long nocopy": {
			"services: {web: {image: a, volumes: [{type: volume, source: data, target: /data, volume: {nocopy: true}}]}}\nvolumes: {data: {}}",
			"the service web's volume uses `volume`, which Dockwell can't run yet",
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
// What Compose has a field for comes from there, and what it hasn't from
// x-dockwell, and both read back from an export as what they were.
func TestRolloutSettingsRoundTrip(t *testing.T) {
	const file = `
services:
  web:
    image: nginx:1.27
    stop_grace_period: 1m30s
    deploy:
      replicas: 1
      update_config: {order: start-first, parallelism: 1, failure_action: rollback}
    healthcheck: {disable: true}
    x-dockwell:
      drain: ${DRAIN:-45s}
      healthcheck:
        http: {port: 8080, path: /healthz, host: app.example.com}
        interval: 5s
        timeout: 2s
        start_period: 30s
        retries: 4
  db:
    image: postgres:17
    deploy: {update_config: {order: stop-first}}
    x-dockwell:
      healthcheck: {tcp: {port: 5432}}
`
	parsed, err := compose.Parse(t.Context(), []byte(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := resource.NewRegistry(&service.Driver{})
	byName := func(s *compose.Stack) map[string]*resourcev1.Container {
		out := map[string]*resourcev1.Container{}
		for _, i := range s.Services {
			if err := reg.Validate(service.Kind, i.Spec); err != nil {
				t.Fatalf("%s: %v", i.Name, err)
			}
			out[i.Name] = i.Spec.GetApp()
		}
		return out
	}
	got := byName(parsed)
	web, db := got["web"], got["db"]
	if web.GetStopGracePeriod() != "1m30s" || web.GetRollout().GetOrder() != resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST || web.GetRollout().GetDrain() != "45s" {
		t.Errorf("web reads %v", web)
	}
	hc := web.GetHealthcheck()
	if h := hc.GetHttp(); h.GetPort() != 8080 || h.GetPath() != "/healthz" || h.GetHost() != "app.example.com" || hc.GetInterval() != "5s" || hc.GetTimeout() != "2s" || hc.GetStartPeriod() != "30s" || hc.GetRetries() != 4 || len(hc.GetTest()) != 0 {
		t.Errorf("web's check reads %v", hc)
	}
	if db.GetRollout().GetOrder() != resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST || db.GetHealthcheck().GetTcp().GetPort() != 5432 {
		t.Errorf("db reads %v", db)
	}
	out, err := compose.Export(project, parsed)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out.File)
	for _, want := range []string{"stop_grace_period: 1m30s", "order: start-first", "x-dockwell:", "disable: true", health.Label} {
		if !strings.Contains(text, want) {
			t.Errorf("the export doesn't have %q:\n%s", want, text)
		}
	}
	again, err := compose.Parse(t.Context(), out.File, map[string]string{})
	if err != nil {
		t.Fatalf("reading the export back: %v\n%s", err, text)
	}
	for name, c := range byName(again) {
		if !proto.Equal(c.GetHealthcheck(), got[name].GetHealthcheck()) || !proto.Equal(c.GetRollout(), got[name].GetRollout()) || c.GetStopGracePeriod() != got[name].GetStopGracePeriod() {
			t.Errorf("%s reads back as %v, want %v", name, c, got[name])
		}
	}
}

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

	// What docker compose makes from this has to read back as what Dockwell
	// made, and what looks like a mistake in it says why it isn't.
	if !strings.Contains(text, container.LabelSet+": ") {
		t.Errorf("the export has no %s label, so what compose makes reads as drift:\n%s", container.LabelSet, text)
	}
	if !strings.Contains(text, "# A "+container.LabelSet+" label is there on purpose") {
		t.Errorf("the export doesn't say what its own label is for:\n%s", text)
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

// A network's options read back as Docker's own without the label, so what
// compose makes from an export would refuse a re-import of the unchanged file.
func TestAnExportKeepsANetworksOwnLabel(t *testing.T) {
	out, err := compose.Export("env_x", &compose.Stack{Networks: []compose.Item[*resourcev1.Network]{{
		Name: "back", Spec: &resourcev1.Network{Driver: "bridge", Options: map[string]string{"com.docker.network.driver.mtu": "1400"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out.File)
	if !strings.Contains(text, "option:com.docker.network.driver.mtu") {
		t.Errorf("the export's network has no label for its option:\n%s", text)
	}
	if !strings.Contains(text, "# A "+container.LabelSet+" label is there on purpose") {
		t.Errorf("the export doesn't say what the label is for:\n%s", text)
	}
}

// extra_hosts reads from a list or a mapping into one form. What /etc/hosts couldn't
// hold, the plan refuses, as it does any field of a container.
func TestExtraHosts(t *testing.T) {
	for name, file := range map[string]string{
		"a list":    "services:\n  web:\n    image: nginx:1.27\n    extra_hosts: [\"host.docker.internal:host-gateway\", \"db.example.com:10.0.0.5\"]\n",
		"a mapping": "services:\n  web:\n    image: nginx:1.27\n    extra_hosts:\n      host.docker.internal: host-gateway\n      db.example.com: 10.0.0.5\n",
	} {
		t.Run(name, func(t *testing.T) {
			stack, err := compose.Read(t.Context(), compose.Input{File: []byte(file)})
			if err != nil {
				t.Fatal(err)
			}
			got := stack.Services[0].Spec.GetApp().GetExtraHosts()
			if want := []string{"db.example.com:10.0.0.5", "host.docker.internal:host-gateway"}; !slices.Equal(got, want) {
				t.Errorf("extra_hosts reads as %v, want %v", got, want)
			}
		})
	}
}

// What a service asks of its container itself reads into its spec, and an export
// writes it back as it came (ADR 0085). A pull policy and a check's start interval
// go in no spec, as they change nothing Dockwell does.
func TestWhatAContainerAsksOfItself(t *testing.T) {
	file := `services:
  web:
    image: nginx:1.27
    hostname: mail.example.com
    init: true
    tty: true
    stdin_open: true
    stop_signal: SIGQUIT
    read_only: true
    pids_limit: 200
    dns: [10.0.0.53, 1.1.1.1]
    pull_policy: always
    security_opt: [no-new-privileges:true]
    ulimits:
      nproc: 65535
      nofile: {soft: "20000", hard: 40000}
    healthcheck: {test: [CMD, run], start_interval: 5s}
`
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(file)})
	if err != nil {
		t.Fatal(err)
	}
	c := stack.Services[0].Spec.GetApp()
	if c.GetHostname() != "mail.example.com" || !c.GetInit() || !c.GetTty() || !c.GetStdinOpen() || c.GetStopSignal() != "SIGQUIT" ||
		!c.GetReadOnly() || c.GetPidsLimit() != 200 || !slices.Equal(c.GetDns(), []string{"10.0.0.53", "1.1.1.1"}) || !c.GetNoNewPrivileges() {
		t.Errorf("reads as %v", c)
	}
	limits := map[string][2]int64{}
	for _, u := range c.GetUlimits() {
		limits[u.GetName()] = [2]int64{u.GetSoft(), u.GetHard()}
	}
	if limits["nproc"] != [2]int64{65535, 65535} || limits["nofile"] != [2]int64{20000, 40000} {
		t.Errorf("limits read as %v", limits)
	}
	out, err := compose.Export("env_x", stack)
	if err != nil {
		t.Fatal(err)
	}
	again, err := compose.Read(t.Context(), compose.Input{File: out.File})
	if err != nil {
		t.Fatalf("the export doesn't read back: %v\n%s", err, out.File)
	}
	if got := again.Services[0].Spec.GetApp(); !proto.Equal(got, c) {
		t.Errorf("the export reads back as another spec: %v\n%s", resource.Diff(c, got), out.File)
	}
	if strings.Contains(string(out.File), "pull_policy") || strings.Contains(string(out.File), "start_interval") {
		t.Errorf("the export writes what no spec holds:\n%s", out.File)
	}
}

// A pull policy of build builds an image, which Dockwell can't, and a limit that's no
// number is refused by its name, not with a panic in what reads the file.
func TestWhatAContainerAsksOfItselfThatDockwellCant(t *testing.T) {
	for name, c := range map[string]struct{ file, want string }{
		"a build":           {"services:\n  web:\n    image: nginx:1.27\n    pull_policy: build\n", "builds an image"},
		"a limit of a word": {"services:\n  web:\n    image: nginx:1.27\n    ulimits: {nofile: {soft: \"${N}\", hard: 4096}}\n", "takes a whole number"},
		"seccomp loosened":  {"services:\n  web:\n    image: nginx:1.27\n    security_opt: [\"seccomp:unconfined\"]\n", "loosens what confines the container"},
		"apparmor loosened": {"services:\n  web:\n    image: nginx:1.27\n    security_opt: [\"apparmor=unconfined\"]\n", "loosens what confines the container"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compose.Read(t.Context(), compose.Input{File: []byte(c.file), Variables: map[string]string{"N": "lots"}})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Read = %v, want %q", err, c.want)
			}
		})
	}
}

// A config a file writes out as its content is a file Dockwell writes for each
// service that mounts it, read-only, at its target or at /<name>, and an export
// writes it back as a config (ADR 0085). One read from a file, a variable of the
// shell or Swarm is refused with what to do, and so is one written as another
// user's.
// A deploy's restart_policy is the container's restart, as docker compose up reads it,
// where the service sets none of its own (ADR 0085).
func TestARestartPolicyIsTheContainersRestart(t *testing.T) {
	file := `services:
  a: {image: nginx:1.27, deploy: {restart_policy: {condition: on-failure, max_attempts: 3, delay: 5s}}}
  b: {image: nginx:1.27, deploy: {restart_policy: {condition: any}}}
  c: {image: nginx:1.27, deploy: {restart_policy: {condition: none}, replicas: 1}}
  d: {image: nginx:1.27, restart: unless-stopped, deploy: {restart_policy: {condition: none}}}
`
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(file)})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range stack.Services {
		app := s.Spec.GetApp()
		got[s.Name] = fmt.Sprintf("%v/%d", app.GetRestart(), app.GetRestartRetries())
	}
	want := map[string]string{
		"a": "RESTART_POLICY_ON_FAILURE/3", "b": "RESTART_POLICY_ALWAYS/0", "c": "RESTART_POLICY_NO/0", "d": "RESTART_POLICY_UNLESS_STOPPED/0",
	}
	if !maps.Equal(got, want) {
		t.Errorf("restarts = %v, want %v", got, want)
	}
	for bad, why := range map[string]string{
		"services: {a: {image: nginx:1.27, deploy: {restart_policy: {condition: sometimes}}}}":    "the condition \"sometimes\"",
		"services: {a: {image: nginx:1.27, deploy: {restart_policy: {condition: any, nope: 1}}}}": "deploy.restart_policy uses `nope`",
	} {
		if _, err := compose.Read(t.Context(), compose.Input{File: []byte(bad)}); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v, want %q", bad, err, why)
		}
	}
}

// Whom a volume or a server path is made for is the service's x-dockwell's, by the
// mount's target, as Compose has no field for it, and an export writes it back
// (ADR 0085).
func TestAMountsOwnerComesFromXDockwell(t *testing.T) {
	file := `services:
  web:
    image: nginx:1.27
    volumes:
      - data:/var/lib/app/
      - /srv/site:/srv
      - cache:/cache
    x-dockwell:
      make_owner:
        /var/lib/app: "1000:1000"
        /srv: "0082:82"
volumes:
  data: {}
  cache: {}
`
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(file), Directory: "/opt/app"})
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{}
	for _, m := range stack.Services[0].Spec.GetApp().GetMounts() {
		owners[m.GetTarget()] = m.GetMakeOwner()
	}
	if owners["/var/lib/app"] != "1000:1000" || owners["/srv"] != "82:82" || owners["/cache"] != "" {
		t.Errorf("owners = %v", owners)
	}
	out, err := compose.Export("env_x", stack)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(out.File); !strings.Contains(text, "make_owner:") || !strings.Contains(text, `/var/lib/app: "1000:1000"`) {
		t.Errorf("the export doesn't say whom each mount is made for:\n%s", text)
	}
	again, err := compose.Read(t.Context(), compose.Input{File: out.File})
	if err != nil {
		t.Fatalf("reading the export: %v\n%s", err, out.File)
	}
	for _, m := range again.Services[0].Spec.GetApp().GetMounts() {
		if m.GetMakeOwner() != owners[m.GetTarget()] {
			t.Errorf("the export reads back with %s made for %q, want %q", m.GetTarget(), m.GetMakeOwner(), owners[m.GetTarget()])
		}
	}
}

// A volume's mode and the files it starts with are the service's x-dockwell's, by the
// volume's target, the files as configs the file writes, and an export writes both
// back (ADR 0085).
func TestAVolumesModeAndFilesComeFromXDockwell(t *testing.T) {
	file := `services:
  web:
    image: caddy:2.10
    volumes:
      - conf:/etc/caddy
      - logs:/var/log/caddy
    x-dockwell:
      make_mode:
        /var/log/caddy: "0777"
      seed:
        /etc/caddy:
          Caddyfile: caddyfile
          sites/a.conf: site-a
configs:
  caddyfile:
    content: |
      :80 { respond "hello" }
  site-a:
    content: "#!/bin/sh\nexit 0\n"
volumes:
  conf: {}
  logs: {}
`
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(file)})
	if err != nil {
		t.Fatal(err)
	}
	mounts := map[string]*resourcev1.Mount{}
	for _, m := range stack.Services[0].Spec.GetApp().GetMounts() {
		mounts[m.GetTarget()] = m
	}
	if m := mounts["/var/log/caddy"]; m.GetMakeMode() != 0o777 || len(m.GetSeed()) != 0 {
		t.Errorf("logs = %v", m)
	}
	seed := mounts["/etc/caddy"].GetSeed()
	if len(seed) != 2 || seed[0].GetPath() != "Caddyfile" || !strings.Contains(seed[0].GetContent(), "respond") || seed[0].GetMode() != 0o644 || seed[1].GetMode() != 0o755 {
		t.Errorf("conf starts with %v", seed)
	}
	if len(mounts) != 2 {
		t.Errorf("a config a volume starts with is also mounted: %v", mounts)
	}
	out, err := compose.Export("env_x", stack)
	if err != nil {
		t.Fatal(err)
	}
	again, err := compose.Read(t.Context(), compose.Input{File: out.File})
	if err != nil {
		t.Fatalf("reading the export: %v\n%s", err, out.File)
	}
	if !proto.Equal(again.Services[0].Spec.GetApp(), stack.Services[0].Spec.GetApp()) {
		t.Errorf("the export reads back as\n%v\nwant\n%v\n%s", again.Services[0].Spec.GetApp(), stack.Services[0].Spec.GetApp(), out.File)
	}
}

func TestConfigsAreFilesDockwellWrites(t *testing.T) {
	file := `services:
  web:
    image: nginx:1.27
    configs:
      - source: nginx
        target: /etc/nginx/nginx.conf
      - source: run
        mode: 0555
      - motd
configs:
  nginx:
    content: |
      events {}
  run:
    content: "#!/bin/sh\nexec nginx\n"
  motd:
    content: hello
`
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(file)})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*resourcev1.MountFile{}
	for _, m := range stack.Services[0].Spec.GetApp().GetMounts() {
		if !m.GetReadOnly() {
			t.Errorf("the file at %s isn't read-only", m.GetTarget())
		}
		files[m.GetTarget()] = m.GetFile()
	}
	if files["/etc/nginx/nginx.conf"].GetContent() != "events {}\n" || files["/run"].GetMode() != 0o555 || files["/motd"].GetContent() != "hello" {
		t.Errorf("mounts read as %v", files)
	}
	out, err := compose.Export("env_x", stack)
	if err != nil {
		t.Fatal(err)
	}
	again, err := compose.Read(t.Context(), compose.Input{File: out.File})
	if err != nil {
		t.Fatalf("the export doesn't read back: %v\n%s", err, out.File)
	}
	if got, want := again.Services[0].Spec.GetApp(), stack.Services[0].Spec.GetApp(); !proto.Equal(got, want) {
		t.Errorf("the export reads back as another spec: %v\n%s", resource.Diff(want, got), out.File)
	}

	for name, c := range map[string]struct{ file, want string }{
		"from a file":        {"services:\n  web:\n    image: nginx:1.27\n    configs: [c]\nconfigs:\n  c:\n    file: ./nginx.conf\n", "Write what it holds as content"},
		"from the shell":     {"services:\n  web:\n    image: nginx:1.27\n    configs: [c]\nconfigs:\n  c:\n    environment: CONF\n", "Write it as content"},
		"from Swarm":         {"services:\n  web:\n    image: nginx:1.27\n    configs: [c]\nconfigs:\n  c:\n    external: true\n", "Write what it holds as content"},
		"one not written":    {"services:\n  web:\n    image: nginx:1.27\n    configs: [c]\n", "doesn't write"},
		"another's":          {"services:\n  web:\n    image: nginx:1.27\n    configs: [{source: c, uid: \"1000\"}]\nconfigs:\n  c:\n    content: x\n", "as root's"},
		"with nothing in it": {"services:\n  web:\n    image: nginx:1.27\n    configs: [c]\nconfigs:\n  c: {}\n", "has no content"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := compose.Read(t.Context(), compose.Input{File: []byte(c.file)}); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Read = %v, want %q", err, c.want)
			}
		})
	}
}
