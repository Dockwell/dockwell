// SPDX-License-Identifier: AGPL-3.0-only

package compose_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// A file in the shape 1Panel writes when it installs an app, written by hand.
const installedApp = `
services:
  db:
    container_name: ${CONTAINER_NAME}
    image: postgres:18.6-alpine
    deploy:
      resources:
        limits:
          cpus: ${CPUS}
          memory: ${MEMORY_LIMIT}
    environment:
      - POSTGRES_PASSWORD=${PANEL_DB_ROOT_PASSWORD}
    healthcheck:
      test: ["CMD", "pg_isready", "-q"]
      start_period: 20s
      interval: 30s
      retries: 5
      timeout: 5s
    labels:
      createdBy: Apps
    networks:
      - 1panel-network
    ports:
      - ${HOST_IP}:${PANEL_APP_PORT_HTTP}:5432
      - 127.0.0.1:${PANEL_APP_PORT_ADMIN}:9000
    restart: always
    volumes:
      - ./data:/var/lib/postgresql
networks:
  1panel-network:
    external: true
`

const installedEnv = `CONTAINER_NAME="1Panel-postgresql-abcd"
CPUS=1.50
HOST_IP=
MEMORY_LIMIT=1024M
PANEL_APP_PORT_ADMIN=9000
PANEL_APP_PORT_HTTP=5432
PANEL_DB_ROOT_PASSWORD="pa$$word"
`

func only(t *testing.T, s *compose.Stack, name string) *resourcev1.Container {
	t.Helper()
	for _, item := range s.Services {
		if item.Name == name {
			return item.Spec.GetApp()
		}
	}
	t.Fatalf("the file makes no service %s: %+v", name, s.Services)
	return nil
}

func TestAnInstalledAppReads(t *testing.T) {
	s, err := compose.Read(t.Context(), compose.Input{File: []byte(installedApp), Env: installedEnv, Directory: "/opt/1panel/apps/postgresql/pg"})
	if err != nil {
		t.Fatal(err)
	}
	db := only(t, s, "db")
	if len(s.Networks) != 0 {
		t.Errorf("an external network made a resource: %+v", s.Networks)
	}
	if !slices.Equal(db.GetExternalNetworks(), []string{"1panel-network"}) || len(db.GetNetworks()) != 0 {
		t.Errorf("networks %v, external %v", db.GetNetworks(), db.GetExternalNetworks())
	}
	if !slices.Equal(db.GetAliases(), []string{"1Panel-postgresql-abcd"}) {
		t.Errorf("aliases %v", db.GetAliases())
	}
	if r := db.GetResources(); r.GetCpus() != "1.5" || r.GetMemory() != "1G" {
		t.Errorf("limits %v", r)
	}
	if got := db.GetEnv()["POSTGRES_PASSWORD"]; got != "pa$word" {
		t.Errorf("the password from .env = %q", got)
	}
	// ${HOST_IP} is empty, which leaves :5432:5432: every address, not a refusal.
	if got := db.GetPorts(); len(got) != 2 || got[0].GetHostIp() != "" || got[0].GetHostPort() != 5432 || got[1].GetHostIp() != "127.0.0.1" {
		t.Errorf("ports %v", got)
	}
	if m := db.GetMounts(); len(m) != 1 || m[0].GetHostPath() != "/opt/1panel/apps/postgresql/pg/data" {
		t.Errorf("mounts %v", m)
	}
	if err := (&service.Driver{}).Validate(s.Services[0].Spec); err != nil {
		t.Errorf("it doesn't validate: %v", err)
	}
	// Its name and ports are from variables, as nearly every installed app's are, and
	// a warning on them would be on every plan.
	if len(s.Exposed) != 0 {
		t.Errorf("it warns of %+v", s.Exposed)
	}
	noted := slices.ContainsFunc(s.Notices, func(n compose.Notice) bool {
		return n.Reason == "container_name" && n.Service == "db" && strings.Contains(n.Message, "1Panel-postgresql-abcd")
	})
	if !noted {
		t.Errorf("the plan doesn't say it runs under another name: %+v", s.Notices)
	}
}

func read(t *testing.T, file string, env ...string) (*compose.Stack, error) {
	t.Helper()
	return compose.Read(t.Context(), compose.Input{File: []byte(file), Env: strings.Join(env, "\n"), Directory: "/srv/app"})
}

// What each table's row says a file does, and each refusal by name.
func TestRealShapes(t *testing.T) {
	const web = "services:\n  web:\n    image: nginx:1.27\n"
	for name, c := range map[string]struct {
		file string
		want func(*resourcev1.Container) bool
		note string
	}{
		"external, named": {
			file: web + "    networks: [front]\nnetworks:\n  front:\n    external: true\n    name: edge\n",
			want: func(c *resourcev1.Container) bool { return slices.Equal(c.GetExternalNetworks(), []string{"edge"}) },
		},
		"external, the old way": {
			file: web + "    networks: [front]\nnetworks:\n  front:\n    external: {name: edge}\n",
			want: func(c *resourcev1.Container) bool { return slices.Equal(c.GetExternalNetworks(), []string{"edge"}) },
		},
		"external by a variable": {
			file: web + "    networks: [front]\nnetworks:\n  front:\n    external: ${E:-true}\n",
			want: func(c *resourcev1.Container) bool { return slices.Equal(c.GetExternalNetworks(), []string{"front"}) },
		},
		"the default network, external": {
			file: web + "networks:\n  default:\n    external: true\n    name: edge\n",
			want: func(c *resourcev1.Container) bool { return slices.Equal(c.GetExternalNetworks(), []string{"edge"}) },
		},
		"external false": {
			file: web + "    networks: [back]\nnetworks:\n  back:\n    external: false\n",
			want: func(c *resourcev1.Container) bool {
				return slices.Equal(c.GetNetworks(), []string{"back"}) && len(c.GetExternalNetworks()) == 0
			},
		},
		"network_mode names a listed external": {
			file: web + "    network_mode: edge\nnetworks:\n  edge:\n    external: true\n",
			want: func(c *resourcev1.Container) bool { return slices.Equal(c.GetExternalNetworks(), []string{"edge"}) },
		},
		"network_mode names the file's own": {
			file: web + "    network_mode: back\nnetworks:\n  back: {}\n",
			want: func(c *resourcev1.Container) bool { return slices.Equal(c.GetNetworks(), []string{"back"}) },
		},
		"network_mode names what the file doesn't list": {
			file: web + "    network_mode: edge\n",
			want: func(c *resourcev1.Container) bool { return slices.Equal(c.GetExternalNetworks(), []string{"edge"}) },
		},
		"network_mode host": {
			file: web + "    network_mode: host\n",
			want: func(c *resourcev1.Container) bool {
				return c.GetHostNetwork() && len(c.GetNetworks())+len(c.GetExternalNetworks()) == 0
			},
		},
		"host takes no ports, as Docker drops them, and says so": {
			file: web + "    network_mode: host\n    ports: [\"8080:80\"]\n",
			want: func(c *resourcev1.Container) bool { return c.GetHostNetwork() && len(c.GetPorts()) == 0 },
			note: "host_network_ports",
		},
		"host has no alias, and says so": {
			file: web + "    network_mode: host\n    container_name: other\n",
			want: func(c *resourcev1.Container) bool { return len(c.GetAliases()) == 0 },
			note: "container_name",
		},
		"bridge has no alias, and says so": {
			file: web + "    network_mode: bridge\n    container_name: other\n",
			want: func(c *resourcev1.Container) bool { return len(c.GetAliases()) == 0 },
			note: "container_name",
		},
		"a container_name of its own name needs no alias": {
			file: web + "    container_name: web\n",
			want: func(c *resourcev1.Container) bool { return len(c.GetAliases()) == 0 },
		},
		"aliases on each network": {
			file: web + "    networks: {a: {aliases: [x]}, b: {aliases: [x]}}\nnetworks: {a: {}, b: {}}\n",
			want: func(c *resourcev1.Container) bool { return slices.Equal(c.GetAliases(), []string{"x"}) },
		},
		"top-level limits": {
			file: web + "    cpus: 0.5\n    mem_limit: 512m\n    mem_reservation: 256M\n",
			want: func(c *resourcev1.Container) bool {
				r := c.GetResources()
				return r.GetCpus() == "0.5" && r.GetMemory() == "512M" && r.GetMemoryReservation() == "256M"
			},
		},
		"both forms, agreeing": {
			file: web + "    cpus: \"1\"\n    deploy: {resources: {limits: {cpus: 1.00}}}\n",
			want: func(c *resourcev1.Container) bool { return c.GetResources().GetCpus() == "1" },
		},
		"a limit of zero is none, as 1Panel writes one": {
			file: web + "    deploy: {resources: {limits: {cpus: \"0\", memory: \"0\"}}}\n",
			want: func(c *resourcev1.Container) bool { return c.GetResources() == nil },
		},
		"memory as YAML's float, which compose-go reads as nothing": {
			file: web + "    mem_limit: 1.5e9\n",
			want: func(c *resourcev1.Container) bool { return c.GetResources().GetMemory() == "1500000000" },
		},
		"memory reserved": {
			file: web + "    deploy: {resources: {reservations: {memory: 128M}}}\n",
			want: func(c *resourcev1.Container) bool { return c.GetResources().GetMemoryReservation() == "128M" },
		},
		"CPU reserved, which only Swarm keeps": {
			file: web + "    deploy: {resources: {reservations: {cpus: \"0.25\"}}}\n",
			want: func(c *resourcev1.Container) bool { return c.GetResources() == nil },
			note: "cpu_reservation",
		},
		"build beside image": {
			file: web + "    build: ./php\n",
			want: func(c *resourcev1.Container) bool { return c.GetImage() == "nginx:1.27" },
			note: "no_build",
		},
		"build's variables need no value": {
			file: web + "    build: {context: ., args: {V: \"${NO_SUCH}\"}}\n",
			want: func(c *resourcev1.Container) bool { return c.GetImage() == "nginx:1.27" },
			note: "no_build",
		},
		"privileges": {
			file: web + "    cap_add: [NET_ADMIN]\n    cap_drop: [ALL]\n    group_add: [999, audio]\n    privileged: true\n",
			want: func(c *resourcev1.Container) bool {
				return slices.Equal(c.GetCapAdd(), []string{"NET_ADMIN"}) && slices.Equal(c.GetCapDrop(), []string{"ALL"}) &&
					len(c.GetGroupAdd()) == 2 && c.GetPrivileged()
			},
		},
		"on-failure with a count": {
			file: web + "    restart: on-failure:5\n",
			want: func(c *resourcev1.Container) bool {
				return c.GetRestart() == resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE && c.GetRestartRetries() == 5
			},
		},
		"a relative path starts at the directory": {
			file: web + "    volumes: [\"./data:/data\", \"../shared:/shared\"]\n",
			want: func(c *resourcev1.Container) bool {
				var got []string
				for _, m := range c.GetMounts() {
					got = append(got, m.GetHostPath())
				}
				return slices.Equal(got, []string{"/srv/app/data", "/srv/shared"})
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := read(t, c.file)
			if err != nil {
				t.Fatal(compose.Message(err))
			}
			got := only(t, s, "web")
			if !c.want(got) {
				t.Errorf("reads as %v", got)
			}
			if c.note != "" && !slices.ContainsFunc(s.Notices, func(n compose.Notice) bool { return n.Reason == c.note && n.Service == "web" }) {
				t.Errorf("the plan doesn't say %s: %+v", c.note, s.Notices)
			}
			if c.note == "" && len(s.Notices) > 0 {
				t.Errorf("the plan says what nothing asked it to: %+v", s.Notices)
			}
		})
	}
}

// Nothing refused is dropped: each is refused by name, with the fix.
func TestStillRefused(t *testing.T) {
	const web = "services:\n  web:\n    image: nginx:1.27\n"
	for name, c := range map[string]struct{ file, want string }{
		"replicas":                  {web + "    deploy: {replicas: 2}\n", "the service web asks for 2 replicas"},
		"restart_policy":            {web + "    deploy: {restart_policy: {condition: any}}\n", "the service web's deploy uses `restart_policy`"},
		"placement":                 {web + "    deploy: {placement: {}}\n", "the service web's deploy uses `placement`"},
		"update_config":             {web + "    deploy: {update_config: {monitor: 5s}}\n", "the service web's deploy.update_config uses `monitor`"},
		"a limit of pids":           {web + "    deploy: {resources: {limits: {pids: 100}}}\n", "the service web's deploy.resources.limits uses `pids`"},
		"a device reserved":         {web + "    deploy: {resources: {reservations: {devices: [{driver: nvidia}]}}}\n", "the service web's deploy.resources.reservations uses the server's devices"},
		"other resources":           {web + "    deploy: {resources: {nope: {}}}\n", "the service web's deploy.resources uses `nope`"},
		"limits that differ":        {web + "    cpus: 1\n    deploy: {resources: {limits: {cpus: 2}}}\n", "sets cpus to 1 and deploy.resources.limits.cpus to 2"},
		"memory that differs":       {web + "    mem_limit: 1G\n    deploy: {resources: {limits: {memory: 2G}}}\n", "sets mem_limit to 1G and deploy.resources.limits.memory to 2G"},
		"not a number of CPUs":      {web + "    cpus: lots\n", "the service web's cpus: \"lots\" isn't a number of CPUs"},
		"not an amount":             {web + "    mem_limit: lots\n", "the service web's mem_limit: \"lots\" isn't an amount of memory"},
		"build alone":               {"services:\n  web:\n    build: .\n", "the service web builds an image, which Dockwell can't do yet"},
		"network_mode none":         {web + "    network_mode: none\n", "network_mode none"},
		"network_mode service":      {web + "    network_mode: service:db\n", "sharing another container's network"},
		"network_mode container":    {web + "    network_mode: container:db\n", "sharing another container's network"},
		"network_mode ns":           {web + "    network_mode: ns:/proc/1/ns/net\n", "network_mode ns:/proc/1/ns/net, which Dockwell can't run yet"},
		"mode beside networks":      {web + "    network_mode: host\n    networks: [a]\nnetworks: {a: {}}\n", "has both network_mode and networks"},
		"external with a driver":    {web + "    networks: [a]\nnetworks:\n  a: {external: true, driver: bridge}\n", "the network a is external, so Dockwell doesn't make it"},
		"external with options":     {web + "    networks: [a]\nnetworks:\n  a: {external: true, internal: true}\n", "the network a is external, so Dockwell doesn't make it"},
		"named, not external":       {web + "    networks: [a]\nnetworks:\n  a: {name: other}\n", "names itself in Docker"},
		"external volume":           {web + "    volumes: [\"v:/v\"]\nvolumes:\n  v: {external: true}\n", "the volume v is `external`"},
		"aliases that differ":       {web + "    networks: {a: {aliases: [x]}, b: {aliases: [y]}}\nnetworks: {a: {}, b: {}}\n", "has other aliases on the network b"},
		"an address on a network":   {web + "    networks: {a: {ipv4_address: 10.0.0.2}}\nnetworks: {a: {}}\n", "the service web's network a uses `ipv4_address`"},
		"devices":                   {web + "    devices: [/dev/net/tun]\n", "uses the server's devices"},
		"pid":                       {web + "    pid: host\n", "shares a process namespace"},
		"ipc":                       {web + "    ipc: host\n", "shares an IPC namespace"},
		"a home directory":          {web + "    volumes: [\"~/x:/x\"]\n", "a path relative to the Compose file"},
		"a count on another policy": {web + "    restart: unless-stopped:5\n", "only on-failure takes a count"},
		"a count that isn't one":    {web + "    restart: on-failure:lots\n", "on-failure's count is a number of times"},
		"cap_add of a word":         {web + "    cap_add: 5\n", "where Compose takes a list"},
		"build has its shape, and takes it no further": {"services:\n  web:\n    build: {context: .}\n", "the service web builds an image"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := read(t, c.file)
			if !errors.Is(err, compose.ErrInvalid) || !strings.Contains(compose.Message(err), c.want) {
				t.Errorf("Read = %v, want %q", err, c.want)
			}
		})
	}
	// With no directory, a relative path says how to give one.
	_, err := compose.Read(t.Context(), compose.Input{File: []byte(web + "    volumes: [\"./x:/x\"]\n")})
	if err == nil || !strings.Contains(err.Error(), "Pass the directory it is in, as directory (--directory on the CLI)") {
		t.Errorf("a relative path with no directory = %v", err)
	}
	for dir, want := range map[string]string{
		"relative/path": "isn't an absolute path", "/srv/../etc": "has .. in it, which a server reads through symlinks", "/srv:/x": "a colon",
		"/srv\n/x": "a control character",
		// A shell's home directory, which a server doesn't read.
		"~/apps/redis": "starts with ~, which a shell reads as a home directory, and a server takes a path as it is written. Give the full path, such as /root/apps/redis",
		"~mira/apps":   "Give the full path, such as /home/mira/apps",
	} {
		_, err := compose.Read(t.Context(), compose.Input{File: []byte(web), Directory: dir})
		if !errors.Is(err, compose.ErrInvalid) || !strings.Contains(compose.Message(err), want) {
			t.Errorf("the directory %q = %v, want it refused as %q", dir, err, want)
		}
	}
	// A . leads where it would without, so it is taken out.
	for dir, want := range map[string]string{"/srv/./app": "/srv/app/data", "/./srv/app/.": "/srv/app/data", "/.": "/data"} {
		s, err := compose.Read(t.Context(), compose.Input{File: []byte(web + "    volumes: [\"./data:/data\"]\n"), Directory: dir})
		if err != nil {
			t.Errorf("the directory %q = %v", dir, err)
			continue
		}
		if got := s.Services[0].Spec.GetApp().GetMounts()[0].GetHostPath(); got != want {
			t.Errorf("with the directory %q, ./data is %q, want %q", dir, got, want)
		}
	}
}

// A value in a file goes through what the spec takes, so nothing a file writes
// can spell out a plan's words.
func TestAFileCantSpellThePlan(t *testing.T) {
	const web = "services:\n  web:\n    image: nginx:1.27\n"
	for name, file := range map[string]string{
		"a network with words":    web + "    networks: [a]\nnetworks:\n  a: {external: true, name: \"edge and Docker's socket\"}\n",
		"a network by its mode":   web + "    network_mode: \"edge, read-only\"\n",
		"a name with words":       web + "    container_name: \"db (read-only)\"\n",
		"a name on the server's":  web + "    network_mode: host\n    container_name: \"db (read-only)\"\n",
		"a name on Docker's":      web + "    network_mode: bridge\n    container_name: \"db (read-only)\"\n",
		"a container's own form":  web + "    network_mode: host\n    container_name: env_01abc-web-1\n",
		"an image beside a build": "services:\n  web:\n    build: .\n    image: \"nginx, which is harmless\"\n",
		"an alias with words":     web + "    networks: {a: {aliases: [\"x, and nothing else\"]}}\nnetworks: {a: {}}\n",
		"a capability with words": web + "    cap_add: [\"NET_ADMIN, which is harmless\"]\n",
		"a group with words":      web + "    group_add: [\"nobody, which is harmless\"]\n",
		"an environment's":        web + "    networks: [a]\nnetworks:\n  a: {external: true, name: env_01abc_back}\n",
		"the server's own":        web + "    networks: [a]\nnetworks:\n  a: {external: true, name: host}\n",
		"no network":              web + "    networks: [a]\nnetworks:\n  a: {external: true, name: none}\n",
		"the bridge":              web + "    networks: [a]\nnetworks:\n  a: {external: true, name: bridge}\n",
		"the default":             web + "    networks: [a]\nnetworks:\n  a: {external: true, name: default}\n",
		"the default by its key":  web + "networks:\n  default: {external: true}\n",
		"the ingress's own":       web + "    networks: [a]\nnetworks:\n  a: {external: true, name: dockwell_ingress}\n",
		"the ingress's by mode":   web + "    network_mode: dockwell_ingress\n",
		"a service's container":   web + "    container_name: env_01abc-web-1\n",
	} {
		t.Run(name, func(t *testing.T) {
			s, err := read(t, file)
			if err == nil {
				err = container.Validate(only(t, s, "web"))
			}
			if err == nil {
				t.Errorf("it passes as %v", s.Services[0].Spec)
			}
		})
	}
}

func TestReadingAnEnvFile(t *testing.T) {
	file := "services:\n  web:\n    image: nginx:1.27\n    environment:\n      V: ${V}\n"
	value := func(t *testing.T, env string, given map[string]string) string {
		t.Helper()
		s, err := compose.Read(t.Context(), compose.Input{File: []byte(file), Env: env, Variables: given})
		if err != nil {
			t.Fatal(compose.Message(err))
		}
		return only(t, s, "web").GetEnv()["V"]
	}
	if got := value(t, "V=from-file\n", map[string]string{"V": "from-request"}); got != "from-request" {
		t.Errorf("what the request gives doesn't win: %q", got)
	}
	if got := value(t, "A=b\nV=${A}c\n", nil); got != "bc" {
		t.Errorf("a value naming an earlier one = %q", got)
	}
	if got := value(t, "V=${A}c\n", map[string]string{"A": "given"}); got != "givenc" {
		t.Errorf("a value naming what's given = %q", got)
	}
	// A password with a $ in it would otherwise quietly be another password.
	for name, env := range map[string]string{
		"unquoted": "V=pa$sword\n", "braced": "V=pa${sword}\n", "double quotes": "V=\"pa$sword\"\n",
	} {
		_, err := compose.Read(t.Context(), compose.Input{File: []byte(file), Env: env})
		if !errors.Is(err, compose.ErrInvalid) || !strings.Contains(err.Error(), "use $$, or put the value in single quotes") {
			t.Errorf("%s = %v, want it refused, saying how to write a $", name, err)
		}
	}
	// Given a value, so what refuses it can only be its size.
	big := "V=x\n" + strings.Repeat("#", compose.MaxEnv)
	if _, err := compose.Read(t.Context(), compose.Input{File: []byte(file), Env: big}); !errors.Is(err, compose.ErrInvalid) || !strings.Contains(err.Error(), "Dockwell reads files of up to 64 KiB") {
		t.Errorf("an .env past the limit = %v", err)
	}
	if _, err := compose.Read(t.Context(), compose.Input{File: []byte(file), Env: big[:compose.MaxEnv]}); err != nil {
		t.Errorf("an .env at the limit = %v", err)
	}
}

// An .env reads as docker compose reads one, for everything but a variable with
// no value, which it leaves blank and Dockwell refuses.
func TestAnEnvFileReadsAsComposeReadsIt(t *testing.T) {
	const web = "services:\n  web:\n    image: nginx:1.27\n"
	for _, env := range []string{
		"A=1", "A=1\n", "A=1\r\n", "  A = 1  \n", "export A=1\n", "export\tA=1\n", "# a comment\nA=1 # another\n", "A=1#not a comment\n",
		"A=\n", "A=\"\"\n", "A=''\n", "A: 1\n", "A=\"x y\"\n", "A='x y'\n", "A=x y\n", "A=\"x\" # after\n",
		`A="a\nb"` + "\n", `A="tab\there"` + "\n", `A="q\"q"` + "\n", `A="b\\s"` + "\n", `A="d\$x"` + "\n", `A='l\nit'` + "\n", `A='no $X here'` + "\n",
		"A=\"two\nlines\"\n", "A='two\nlines'\n", "A=a\nB=${A}b\n", "A=a\nB=$A\n", "A=a\nB=\"$A\"\n", "A=$$x\n", "A=\"$$x\"\n", "A=a\nB=${A:-z}\n",
		"B=${NOT_SET:-z}\n", "B=${NOT_SET-z}\n", "A=a\nB=${A:+z}\n", "A=1\nA=2\n", "A.B=1\n", "A-B=1\n", "A=1\n\n\nB=2\n", "\n\n# only comments\n",
		"A=x=y\n", "A=x:y\n", "A=https://example.com/?a=b&c=d\n", "A=  spaced\n",
	} {
		theirs, err := dotenv.UnmarshalWithLookup(env, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatalf("%q: docker compose refuses it: %v", env, err)
		}
		// The variables' values reach what a file uses, so each is compared there.
		var file strings.Builder
		file.WriteString(web + "    environment:\n      KEPT: x\n")
		for k := range theirs {
			if !strings.ContainsAny(k, ".-") {
				file.WriteString("      " + k + ": ${" + k + "}\n")
			}
		}
		s, err := compose.Read(t.Context(), compose.Input{File: []byte(file.String()), Env: env})
		if err != nil {
			t.Errorf("%q: %v", env, compose.Message(err))
			continue
		}
		for k, want := range theirs {
			if strings.ContainsAny(k, ".-") {
				continue
			}
			if got := only(t, s, "web").GetEnv()[k]; got != want {
				t.Errorf("%q: %s is %q, and docker compose reads %q", env, k, got, want)
			}
		}
	}
}

// An export reads back as what was exported, these fields too, so importing it
// changes nothing, and docker compose makes the same thing from it.
func TestAnExportOfTheNewFieldsReadsBack(t *testing.T) {
	for name, app := range map[string]*resourcev1.Container{
		"a network the server has": {ExternalNetworks: []string{"edge"}},
		"both kinds, with names":   {Networks: []string{"default"}, ExternalNetworks: []string{"edge"}, Aliases: []string{"db", "www"}},
		"the server's own":         {HostNetwork: true},
		"limits":                   {Resources: &resourcev1.Resources{Cpus: "0.5", Memory: "512M", MemoryReservation: "256M"}},
		"privileges":               {CapAdd: []string{"NET_ADMIN"}, CapDrop: []string{"ALL"}, GroupAdd: []string{"999", "audio"}, Privileged: true},
		"a count of restarts":      {Restart: resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE, RestartRetries: 5},
	} {
		t.Run(name, func(t *testing.T) {
			app.Image = "nginx:1.27"
			if app.Restart == resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
				app.Restart = resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED
			}
			if err := container.Validate(app); err != nil {
				t.Fatal(err)
			}
			spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}
			stack := &compose.Stack{Services: []compose.Item[*resourcev1.Service]{{Name: "web", Spec: spec}}}
			if len(app.GetNetworks()) > 0 {
				stack.Networks = []compose.Item[*resourcev1.Network]{{Name: "default", Spec: &resourcev1.Network{Driver: "bridge"}}}
			}
			out, err := compose.Export("env_x", stack)
			if err != nil {
				t.Fatal(err)
			}
			back, err := compose.Parse(t.Context(), out.File, nil)
			if err != nil {
				t.Fatalf("%v\n%s", compose.Message(err), out.File)
			}
			got := only(t, back, "web")
			if err := container.Validate(got); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got, app) {
				t.Errorf("reads back differently: %v\n%s", resource.Diff(app, got), out.File)
			}
		})
	}
}

// A refusal of an .env says which line, and how to fix it, and holds nothing of
// what is on the line: not a value, not what follows a $ in one, and not what reads
// as a name, as a line can be a password with a colon in it.
func TestAnEnvRefusalHoldsNothingItRead(t *testing.T) {
	const secret = "kX9qSecretTail"
	file := "services:\n  web:\n    image: nginx:1.27\n    environment:\n      V: ${V:-x}\n"
	dollar := "use $$, or put the value in single quotes"
	for name, c := range map[string]struct {
		env, says string
		line      int
	}{
		"a $ in a value":             {env: "V=hunter2$" + secret + "\n", says: dollar, line: 1},
		"a $ in double quotes":       {env: "V=\"hunter2$" + secret + "\"\n", says: dollar, line: 1},
		"a $ and braces":             {env: "V=hunter2${" + secret + "}\n", says: dollar, line: 1},
		"a $ in a default":           {env: "V=${NOPE:-${" + secret + "}}\n", says: dollar, line: 1},
		"on a later line":            {env: "A=1\n\nV=hunter2$" + secret + "\n", says: dollar, line: 3},
		"after a line of two":        {env: "A=\"two\nlines\"\nV=$" + secret + "\n", says: dollar, line: 3},
		"a $ it can't read":          {env: "V=" + secret + "${!}\n", says: dollar, line: 1},
		"braces that don't close":    {env: "V=" + secret + "${A\n", says: dollar, line: 1},
		"a reason of its own":        {env: "V=${NOPE:?" + secret + "}\n", says: dollar, line: 1},
		"a raw line, read as a name": {env: secret + ":tail$NOPE\n", says: dollar, line: 1},
		"a raw line, with a quote":   {env: secret + ":\"open\n", says: "a quote that doesn't close", line: 1},
		"a double quote left open":   {env: "V=\"" + secret + "\n", says: "a quote that doesn't close", line: 1},
		"a single quote left open":   {env: "V='" + secret + "\n", says: "a quote that doesn't close", line: 1},
		"something else in a name":   {env: "A!" + secret + "=x\n", says: "other than a name", line: 1},
		"a value with no name":       {env: "=" + secret + "\n", says: "a value with no name", line: 1},
		"too much, once it's read":   {env: "A=" + strings.Repeat(secret, 4000) + "\n" + strings.Repeat("B=hunter2$A$A$A$A$A$A$A$A\n", 20), says: "past 2 MiB", line: 6},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compose.Read(t.Context(), compose.Input{File: []byte(file), Env: c.env})
			if !errors.Is(err, compose.ErrInvalid) {
				t.Fatalf("Read = %v, want it refused", err)
			}
			got := compose.Message(err)
			if strings.Contains(got, secret) || strings.Contains(got, "hunter2") {
				t.Errorf("the refusal holds what the line does: %s", got)
			}
			if !strings.Contains(got, fmt.Sprintf("line %d ", c.line)) {
				t.Errorf("it doesn't say line %d: %s", c.line, got)
			}
			if !strings.Contains(got, c.says) {
				t.Errorf("it doesn't say %q: %s", c.says, got)
			}
		})
	}
}
