// SPDX-License-Identifier: AGPL-3.0-only

package compose_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
// set, as a shell would, and returns what it printed on standard output.
// Its warnings, such as for a variable left blank, go to standard error,
// apart from the output a test reads.
func composeCLI(t *testing.T, env map[string]string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "docker", append([]string{"compose"}, args...)...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.String())
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
    volumes:
      - "%s:/srv/shared:z"
      - {type: tmpfs, target: /run/long, tmpfs: {size: 4m}}
    shm_size: 128m
    tmpfs: ["/run/short:size=8m"]
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
	// Dropping the named volume leaves nothing shared, so that rolled, and the
	// container has moved on from its first number: find it by its labels.
	current := func() *docker.Container {
		t.Helper()
		list, err := client.ListContainers(t.Context(), dockertest.ComposeFilters(project, "cache"))
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Fatalf("cache has %d containers, want 1: %+v", len(list), list)
		}
		c, err := client.InspectContainer(t.Context(), list[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := current()
	spec.GetApp().Env = map[string]string{"REDIS_ARGS": "--save 60 1"}
	steps, _ = l.Plan(t.Context(), []resource.Desired{{Ref: cache, Spec: spec}})
	if steps[0].Action != resource.ActionReplace {
		t.Fatalf("changing cache's environment = %s, want a replace", steps[0].Action)
	}
	if r := steps[0].Rollout; r == nil || r.Rolling || !slices.Equal(r.ImageData, []string{"/data"}) {
		t.Errorf("replacing what keeps data where its image says should stop first, and say why: %+v", r)
	}
	if err := l.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatal(err)
	}
	second := current()
	if second.ID == first.ID {
		t.Error("changing cache's environment didn't replace its container")
	}
	if second.Name != first.Name {
		t.Errorf("a stop-first replace moved cache from %s to %s; want it to keep its number", first.Name, second.Name)
	}
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

// A rolling replace leaves names and numbers docker compose reads, what docker
// compose then does is read back in turn: its ps, a plain up, its --scale and its
// one-offs. So the next rollout goes on from wherever that left it.
func TestRolloutsAndDockerComposeOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker isn't on the PATH, so docker compose can't check what Dockwell made")
	}
	scope := dockertest.Scope(t)
	project := docker.Project(scope)
	dockertest.Cleanup(t, client, project)
	containers := container.Driver{Runtime: &container.Docker{Client: client}}
	reg := resource.NewRegistry(&service.Driver{Containers: containers}, &network.Driver{Client: client})
	l := resource.Local{Drivers: reg}
	web := resource.Ref{Kind: service.Kind, Scope: scope, Name: "web"}
	stack, err := compose.Parse(t.Context(), []byte(`
services:
  web:
    image: nginx:1.27-alpine
    environment: {GREETING: hello}
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Its default network comes first, as a service can't join one that isn't there.
	for _, n := range stack.Networks {
		ref := resource.Ref{Kind: network.Kind, Scope: scope, Name: n.Name}
		if err := reg.Validate(ref.Kind, n.Spec); err != nil {
			t.Fatal(err)
		}
		steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: n.Spec}})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Apply(t.Context(), steps[0], n.Spec); err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
	}
	spec := stack.Services[0].Spec
	deploy := func() resource.Step {
		t.Helper()
		if err := reg.Validate(service.Kind, spec); err != nil {
			t.Fatal(err)
		}
		steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Apply(t.Context(), steps[0], spec); err != nil {
			t.Fatal(err)
		}
		return steps[0]
	}
	// What docker compose lists, by name.
	listed := func() []string {
		t.Helper()
		out := strings.Fields(composeCLI(t, nil, "-p", project, "ps", "--format", "{{.Name}}"))
		slices.Sort(out)
		return out
	}
	name := func(n int) string { return docker.InstanceName(project, "web", n) }
	deploy()
	if got := listed(); !slices.Equal(got, []string{name(1)}) {
		t.Fatalf("docker compose lists %v after the first deploy", got)
	}
	spec.GetApp().Env["GREETING"] = "hi"
	if step := deploy(); step.Rollout == nil || !step.Rollout.Rolling {
		t.Fatalf("the deploy didn't roll: %+v", step)
	}
	if got := listed(); !slices.Equal(got, []string{name(2)}) {
		t.Fatalf("docker compose lists %v after a rolling replace, want %s", got, name(2))
	}

	// A plain up, from the export, takes what's there for its one container, whatever
	// its number. It may recreate it, and then keeps the number.
	out, err := compose.Export(project, stack)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, out.File, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GREETING": "hi"}
	composeCLI(t, env, "-f", path, "up", "--detach")
	if got := listed(); !slices.Equal(got, []string{name(2)}) {
		t.Fatalf("after a plain docker compose up, it lists %v, want %s", got, name(2))
	}

	// The next rollout goes on from there, by the next number.
	spec.GetApp().Env["GREETING"] = "hello again"
	deploy()
	if got := listed(); !slices.Equal(got, []string{name(3)}) {
		t.Fatalf("after the rollout that follows, it lists %v, want %s", got, name(3))
	}

	// A one-off isn't one of the service's, and takes no number from it.
	composeCLI(t, map[string]string{"GREETING": "hello again"}, "-f", path, "run", "--detach", "--no-deps", "--name", project+"-job", "web", "sleep", "300")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "rm", "--force", project+"-job").Run()
	})
	// --scale makes one more, by the next number. It's there to see, in the plan, and
	// it goes with the next step.
	composeCLI(t, map[string]string{"GREETING": "hello again"}, "-f", path, "up", "--detach", "--no-recreate", "--scale", "web=2")
	if got := listed(); len(got) < 2 || !slices.Contains(got, name(3)) {
		t.Fatalf("after docker compose up --scale web=2, it lists %v", got)
	}
	spec.GetApp().Env["GREETING"] = "once more"
	step := deploy()
	if step.Rollout == nil || len(step.Rollout.Others) == 0 {
		t.Errorf("the plan should name the container scaled by hand, and is %+v", step.Rollout)
	}
	all, err := client.ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject + "=" + project, docker.LabelService + "=web"}})
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, c := range all {
		if c.Labels[docker.LabelOneoff] != "True" {
			left = append(left, strings.TrimPrefix(c.Names[0], "/"))
		}
	}
	if len(left) != 1 || left[0] == name(3) {
		t.Errorf("after the rollout, the service has %v, want one new container", left)
	}
	if _, err := client.ContainerNamed(t.Context(), project+"-job"); err != nil {
		t.Errorf("the rollout took the one-off: %v", err)
	}
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != resource.ActionNone {
		t.Errorf("after all that, the plan is %s: %+v, with the one-off still there", steps[0].Action, steps[0].Changes)
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
