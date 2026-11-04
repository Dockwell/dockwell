// SPDX-License-Identifier: AGPL-3.0-only

package compose_test

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// composeCLI runs docker compose with the variables an export leaves out
// set, as a shell would, and returns what it printed.
func composeCLI(t *testing.T, env map[string]string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "docker", append([]string{"compose"}, args...)...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// On a real Docker Engine, a Compose file runs as docker compose runs it:
// docker compose lists its services, reads its export as valid, and what
// it changes by hand reads back as that change and nothing else.
func TestComposeOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker isn't on the PATH, so docker compose can't check what Dockwell made")
	}
	scope := dockertest.Scope(t)
	project := docker.Project(scope)
	dockertest.Cleanup(t, client, project)
	containers := container.Driver{Runtime: &container.Docker{Client: client}}
	reg := resource.NewRegistry(&containers, &service.Driver{Containers: containers},
		&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: &volume.Kept{}}}, &network.Driver{Client: client})
	l := resource.Local{Drivers: reg}

	port := freePort(t)
	// A server path relabelled for SELinux, as :z does; docker compose
	// writes its own binds for it when it recreates web.
	shared := t.TempDir()
	file := fmt.Sprintf(`
services:
  web:
    image: nginx:1.27-alpine
    ports: ["127.0.0.1:%d:80"]
    volumes: ["%s:/srv/shared:z"]
    environment: {GREETING: hello}
    labels: {team: web}
    networks: [front]
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1/"]
      interval: 1s
      retries: 30
  cache:
    image: redis:7.4-alpine
    volumes: [cache-data:/data]
  job:
    image: busybox:1.37
    entrypoint: ["sh", "-c"]
    command: ["sleep 3600"]
    user: nobody
    working_dir: /tmp
    volumes: ["/etc/hostname:/host-name:ro"]
volumes:
  cache-data: {}
networks:
  front: {}
`, port, shared)
	stack, err := compose.Parse(t.Context(), []byte(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	type planned struct {
		ref  resource.Ref
		spec proto.Message
	}
	var all []planned
	for _, n := range stack.Networks {
		all = append(all, planned{resource.Ref{Kind: network.Kind, Scope: scope, Name: n.Name}, n.Spec})
	}
	for _, v := range stack.Volumes {
		all = append(all, planned{resource.Ref{Kind: volume.Kind, Scope: scope, Name: v.Name}, v.Spec})
	}
	for _, s := range stack.Services {
		all = append(all, planned{resource.Ref{Kind: service.Kind, Scope: scope, Name: s.Name}, s.Spec})
	}
	for _, p := range all {
		if err := reg.Validate(p.ref.Kind, p.spec); err != nil {
			t.Fatal(err)
		}
		steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: p.ref, Spec: p.spec}})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Apply(t.Context(), steps[0], p.spec); err != nil {
			t.Fatalf("%s: %v", p.ref, err)
		}
	}
	// Nothing Docker adds reads as drift, whatever the images set.
	for _, p := range all {
		steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: p.ref, Spec: p.spec}})
		if err != nil {
			t.Fatal(err)
		}
		if steps[0].Action != resource.ActionNone {
			t.Errorf("%s drifted right after it was made: %s %+v", p.ref, steps[0].Action, steps[0].Changes)
		}
	}

	// docker compose sees the project, by its name alone.
	ps := composeCLI(t, nil, "-p", project, "ps", "--format", "{{.Service}}")
	for _, svc := range []string{"web", "cache", "job"} {
		if !strings.Contains(ps, svc) {
			t.Errorf("docker compose -p %s ps doesn't list %s:\n%s", project, svc, ps)
		}
	}

	// The export is a file docker compose takes, and lists the same.
	out, err := compose.Export(project, stack)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, out.File, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GREETING": "hello"}
	composeCLI(t, env, "-f", path, "config", "--quiet")
	var listed []string
	type entry struct{ Service string }
	psJSON := strings.TrimSpace(composeCLI(t, env, "-f", path, "ps", "--format", "json"))
	var many []entry
	if json.Unmarshal([]byte(psJSON), &many) != nil {
		// docker compose 2.21 and later write a line for each.
		for _, line := range strings.Split(psJSON, "\n") {
			var c entry
			if json.Unmarshal([]byte(line), &c) == nil {
				many = append(many, c)
			}
		}
	}
	for _, c := range many {
		if c.Service != "" {
			listed = append(listed, c.Service)
		}
	}
	slices.Sort(listed)
	if !slices.Equal(listed, []string{"cache", "job", "web"}) {
		t.Errorf("docker compose -f <export> ps lists %v", listed)
	}

	// docker compose recreates web from the export with a variable more:
	// Dockwell reads that change back as drift, and nothing else.
	edited := strings.Replace(string(out.File), "      GREETING: null\n", "      GREETING: null\n      DEBUG: \"1\"\n", 1)
	if edited == string(out.File) {
		t.Fatalf("can't find web's environment in the export:\n%s", out.File)
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := client.ContainerNamed(t.Context(), project+"-web-1")
	if err != nil {
		t.Fatal(err)
	}
	composeCLI(t, env, "-f", path, "up", "--detach", "--no-deps", "web")
	after, err := client.ContainerNamed(t.Context(), project+"-web-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.ID == before.ID {
		t.Fatal("docker compose up didn't recreate web")
	}
	web := resource.Ref{Kind: service.Kind, Scope: scope, Name: "web"}
	observed, err := l.Observe(t.Context(), []resource.Ref{web})
	if err != nil {
		t.Fatal(err)
	}
	var want *resourcev1.Service
	for _, s := range stack.Services {
		if s.Name == "web" {
			want = s.Spec
		}
	}
	changes := resource.Diff(want, observed[0])
	if len(changes) != 1 || changes[0].Path != "app.env[DEBUG]" || changes[0].Op != resource.OpAdded {
		t.Errorf("after docker compose up, drift is %+v; want app.env[DEBUG] added, and nothing else", changes)
	}

	// Replacing a container keeps the volumes Docker made for its image.
	cache := resource.Ref{Kind: service.Kind, Scope: scope, Name: "cache"}
	spec := proto.CloneOf(stack.Services[0].Spec)
	if stack.Services[0].Name != "cache" {
		t.Fatalf("services in order: %v", names(stack.Services))
	}
	spec.GetApp().Mounts = nil
	_ = reg.Validate(service.Kind, spec)
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: cache, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatal(err)
	}
	first, _ := client.ContainerNamed(t.Context(), project+"-cache-1")
	spec.GetApp().Env = map[string]string{"REDIS_ARGS": "--save 60 1"}
	steps, _ = l.Plan(t.Context(), []resource.Desired{{Ref: cache, Spec: spec}})
	if steps[0].Action != resource.ActionReplace {
		t.Fatalf("changing cache's environment = %s, want a replace", steps[0].Action)
	}
	if err := l.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatal(err)
	}
	second, _ := client.ContainerNamed(t.Context(), project+"-cache-1")
	anon := func(c *docker.Container) []string {
		var out []string
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Destination == "/data" {
				out = append(out, m.Name)
			}
		}
		return out
	}
	if a, b := anon(first), anon(second); len(a) != 1 || !slices.Equal(a, b) {
		t.Errorf("redis's /data was %v, and %v after the replace; want the same volume", a, b)
	}

	// Deleting a volume keeps it, as Docker can't rename one.
	data := resource.Ref{Kind: volume.Kind, Scope: scope, Name: "cache-data"}
	steps, err = l.Plan(t.Context(), []resource.Desired{{Ref: data}})
	if err != nil {
		t.Fatal(err)
	}
	if !steps[0].Destroys {
		t.Error("deleting a volume doesn't say it destroys data")
	}
	if err := l.Apply(t.Context(), steps[0], nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.InspectVolume(t.Context(), project+"_cache-data"); err != nil {
		t.Errorf("the deleted volume is gone at once: %v", err)
	}
}

// docker compose resolves a file's variables as Dockwell does, wherever
// it leaves none blank.
func TestVariablesOnRealDocker(t *testing.T) {
	dockertest.Real(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker isn't on the PATH, so docker compose can't resolve the variables")
	}
	for tmpl, want := range resolved {
		t.Run(tmpl, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "compose.yaml")
			if err := os.WriteFile(path, environment(tmpl), 0o600); err != nil {
				t.Fatal(err)
			}
			out := composeCLI(t, vars, "-f", path, "-p", "dw-variables", "config", "--format", "json")
			var cfg struct {
				Services map[string]struct {
					Environment map[string]*string `json:"environment"`
				} `json:"services"`
			}
			if err := json.Unmarshal([]byte(out), &cfg); err != nil {
				t.Fatalf("docker compose config printed %s: %v", out, err)
			}
			got := ""
			if v := cfg.Services["web"].Environment["V"]; v != nil {
				// It writes a $ of its own as $$, to read back the same.
				got = strings.ReplaceAll(*v, "$$", "$")
			}
			if got != want {
				t.Errorf("docker compose resolves %s as %q, Dockwell as %q", tmpl, got, want)
			}
		})
	}
}
