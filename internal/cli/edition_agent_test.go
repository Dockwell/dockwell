// SPDX-License-Identifier: AGPL-3.0-only

//go:build agent

package cli

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

// These run with -tags agent,agenttests, against the agent build. A test binary
// may link the control plane to stand one up for the agent to join, and
// agenttests gives it the schema's files the agent build leaves out, which a
// control plane needs (ADR 0060); the agent build itself never links one, which
// cmd/dockwell's build_test.go checks.

// controlPlane is a real control plane's agent listener, on a loopback
// port, for the agent build to join.
type controlPlane struct {
	auth *identity.Authority
	hub  *fleet.Hub
	url  string
}

// startControlPlane starts a control plane's agent listener. With agents,
// it reaches the agents that connect through them, as the revision engine
// does.
func startControlPlane(t *testing.T, agents *agentapi.Agents) *controlPlane {
	t.Helper()
	dir := t.TempDir()
	key, err := seal.LoadOrCreate(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.DiscardHandler)
	auth, err := identity.Bootstrap(t.Context(), db, key, logger)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := fleet.New(t.Context(), db, logger)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := control.New(control.Config{Logger: logger, Identity: auth, Fleet: hub, AgentHosts: []string{"127.0.0.1"}, Panel: fstest.MapFS{}, Agents: agents})
	if err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ServeAgents(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("agent listener: %v", err)
		}
	})
	return &controlPlane{auth: auth, hub: hub, url: "https://" + ln.Addr().String()}
}

func TestTheAgentBuildJoinsAndRunsTheAgent(t *testing.T) {
	cp := startControlPlane(t, nil)
	tk, err := cp.auth.CreateTicket(t.Context(), cp.url, "cli:test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := map[string]string{envLogFormat: "json"}

	if r := run(t, env, "role", "--data-dir", dir); r.code != exitOK || r.out != "none\n" {
		t.Errorf("role before joining = %d %q %q, want none", r.code, r.out, r.err)
	}
	r := run(t, env, "run", "--data-dir", dir)
	if r.code != exitError || !strings.Contains(r.err, "this server hasn't joined a control plane, and this is the agent build") ||
		!strings.Contains(r.err, "dockwell join <ticket>") || !strings.Contains(r.err, "installs the full build") {
		t.Errorf("run before joining = %d %q, want it to say how to join, or get the full build", r.code, r.err)
	}
	if _, err := os.Stat(filepath.Join(dir, "control")); err == nil {
		t.Error("run before joining made a control plane's directory")
	}

	r = run(t, env, "join", tk.Value, "--data-dir", dir, "--name", "hz-2")
	if r.code != exitOK || !strings.HasPrefix(r.out, "Joined as hz-2 (srv_") {
		t.Fatalf("join = %d %q %q", r.code, r.out, r.err)
	}
	if r := run(t, env, "role", "--data-dir", dir); r.code != exitOK || r.out != "agent "+cp.url+"\n" {
		t.Errorf("role after joining = %d %q, want agent %s", r.code, r.out, cp.url)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	var logs syncBuffer
	go func() {
		done <- Execute(ctx, []string{"run", "--data-dir", dir, "--log-format", "json"}, IO{
			Out: io.Discard, Err: &logs, Getenv: noEnv,
		})
	}()
	// The hello says which build the agent is, and the control plane keeps
	// it with the server, where the API serves it.
	deadline := time.Now().Add(startUp)
	for {
		var build *dockwellv1.BuildInfo
		for _, s := range cp.hub.List() {
			if s.GetName() == "hz-2" && s.GetConnection() == dockwellv1.Connection_CONNECTION_CONNECTED {
				build = s.GetAgentBuild()
			}
		}
		if build != nil {
			if build.GetEdition() != dockwellv1.BuildEdition_BUILD_EDITION_AGENT {
				t.Errorf("the agent's hello says it is %v, want the agent build", build.GetEdition())
			}
			break
		}
		select {
		case code := <-done:
			done <- code // for whatever waits on it after
			t.Fatalf("dockwell run exited %d before the agent connected:\n%s", code, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent didn't connect within %v:\n%s", startUp, logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !regexp.MustCompile(`"msg":"starting dockwell".*"build":"agent"`).MatchString(logs.String()) {
		t.Errorf("run didn't log which build it is:\n%s", logs.String())
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Errorf("run exited %d after it was stopped:\n%s", code, logs.String())
	}
}

// A server that joined runs the agent build, so its agent has the Docker
// driver, registered as dockwell run registers it, on the Docker at the
// socket DOCKER_HOST names: the control plane plans, applies and observes
// a container there through the agent's stream.
func TestTheAgentBuildRunsContainers(t *testing.T) {
	e := dockertest.New(t)
	e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx", "-g", "daemon off;"}}}
	kinds := resource.NewRegistry(&container.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{})
	agents := agentapi.NewAgents(kinds, slog.New(slog.DiscardHandler))
	cp := startControlPlane(t, agents)
	tk, err := cp.auth.CreateTicket(t.Context(), cp.url, "cli:test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if r := run(t, nil, "join", tk.Value, "--data-dir", dir, "--name", "hz-2"); r.code != exitOK {
		t.Fatalf("join = %d %q %q", r.code, r.out, r.err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	var logs syncBuffer
	go func() {
		done <- Execute(ctx, []string{"run", "--data-dir", dir, "--log-format", "json"}, IO{
			Out: io.Discard, Err: &logs, Getenv: quiet(func(k string) string {
				if k == "DOCKER_HOST" {
					return "unix://" + e.Socket
				}
				return ""
			}),
		})
	}()
	defer func() {
		cancel()
		if code := <-done; code != exitOK {
			t.Errorf("run exited %d after it was stopped:\n%s", code, logs.String())
		}
	}()
	var server string
	var agent resource.Agent
	for deadline := time.Now().Add(startUp); agent == nil; time.Sleep(20 * time.Millisecond) {
		for _, s := range cp.hub.List() {
			if s.GetName() == "hz-2" {
				server = s.GetId()
			}
		}
		if a, ok := agents.Agent(server); ok {
			agent = a
			continue
		}
		select {
		case code := <-done:
			done <- code // for the cleanup, which waits on it
			t.Fatalf("dockwell run exited %d before the agent connected:\n%s", code, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent didn't connect within %v:\n%s", startUp, logs.String())
		}
	}

	// It offers every Docker kind. The control plane checks each spec, and
	// fills in its defaults, before an agent sees it.
	envID := "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"
	others := []resource.Desired{
		{Ref: resource.Ref{Kind: service.Kind, Scope: envID, Name: "api"}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}}},
		{Ref: resource.Ref{Kind: volume.Kind, Scope: envID, Name: "data"}, Spec: &resourcev1.Volume{}},
		{Ref: resource.Ref{Kind: network.Kind, Scope: envID, Name: "backend"}, Spec: &resourcev1.Network{}},
	}
	for _, d := range others {
		if err := kinds.Validate(d.Ref.Kind, d.Spec); err != nil {
			t.Fatalf("checking %s: %v", d.Ref, err)
		}
	}
	steps, err := agent.Plan(t.Context(), others)
	if err != nil {
		t.Fatalf("planning a service, a volume and a network: %v\n%s", err, logs.String())
	}
	for i, step := range steps {
		if step.Action != resource.ActionCreate {
			t.Errorf("%s plans %s, want create", others[i].Ref, step.Action)
		}
	}

	web := resource.Ref{Kind: container.Kind, Scope: server, Name: "web"}
	spec := &resourcev1.Container{Image: "nginx:1.27"}
	if err := kinds.Validate(web.Kind, spec); err != nil {
		t.Fatal(err)
	}
	steps, err = agent.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}})
	if err != nil || len(steps) != 1 || steps[0].Action != resource.ActionCreate {
		t.Fatalf("planning %s = %+v, %v, want create:\n%s", web, steps, err, logs.String())
	}
	if err := agent.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatalf("applying %s: %v\n%s", web, err, logs.String())
	}
	if c := e.Container("web"); c == nil || !c.State.Running || c.Config.Image != "nginx:1.27" {
		t.Fatalf("Docker has %+v, want web running nginx:1.27", c)
	}
	steps, err = agent.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}})
	if err != nil || steps[0].Action != resource.ActionNone {
		t.Errorf("planning %s again = %+v, %v, want nothing to do", web, steps, err)
	}
	observed, err := agent.Observe(t.Context(), []resource.Ref{web})
	if err != nil || len(observed) != 1 || observed[0].(*resourcev1.Container).GetImage() != "nginx:1.27" {
		t.Errorf("observing %s = %v, %v", web, observed, err)
	}
}

func TestTheAgentBuildNeverRunsAControlPlane(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "control"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "control", "key"), []byte("a control plane's key"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{envLogFormat: "json"}
	for _, args := range [][]string{{"run"}, {"role"}} {
		r := run(t, env, append(args, "--data-dir", dir)...)
		if r.code != exitError || !strings.Contains(r.err, "holds a control plane") || !strings.Contains(r.err, "Install the full build on this server") {
			t.Errorf("%s on a control plane's data = %d %q, want it to say to install the full build", args[0], r.code, r.err)
		}
	}
	r := run(t, env, "join", "dwj1.anything", "--data-dir", dir)
	if r.code != exitError || !strings.Contains(r.err, "runs a control plane") {
		t.Errorf("join on a control plane's data = %d %q", r.code, r.err)
	}
}

func TestTheAgentBuildSaysWhereItsOtherCommandsAre(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"token", "create", "--ttl", "1h"}, "dockwell token create isn't in the agent build of dockwell, which this server runs: it runs on the control server"},
		{[]string{"claim"}, "dockwell claim isn't in the agent build"},
		{[]string{"recover", "--help"}, "dockwell recover isn't in the agent build"},
		{[]string{"servers", "list"}, "dockwell servers list isn't in the agent build of dockwell, which this server runs: " +
			"the API commands, with dockwell login and logout, are in the full build. Run it on the control server"},
		{[]string{"--log-level", "debug", "firewall-rules", "create", "--name", "ssh"}, "dockwell firewall-rules create isn't in the agent build"},
		{[]string{"login", "--url", "https://dockwell.example.com"}, "dockwell login isn't in the agent build"},
		// Anything else isn't a command of either build.
		{[]string{"deploy-everything"}, `unknown command "deploy-everything". Run "dockwell --help" to see the commands`},
		{[]string{"joni", "dwj1.anything"}, `unknown command "joni", though there is dockwell join. Run "dockwell --help" to see the commands`},
		{[]string{"servrs"}, `unknown command "servrs". Run "dockwell --help"`},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			r := run(t, nil, tt.args...)
			if r.code != exitUsage || !strings.Contains(r.err, tt.want) {
				t.Errorf("dockwell %s = %d %q, want %d and %q", strings.Join(tt.args, " "), r.code, r.err, exitUsage, tt.want)
			}
			if !strings.HasPrefix(tt.want, "dockwell ") && strings.Contains(r.err, "full build") {
				t.Errorf("dockwell %s says a command the full build doesn't have is in it: %q", strings.Join(tt.args, " "), r.err)
			}
		})
	}
	// The flag only the control plane reads isn't there either.
	if r := run(t, nil, "--database", "postgres://db/dockwell", "role"); r.code != exitUsage || !strings.Contains(r.err, "unknown flag: --database") {
		t.Errorf("--database = %d %q, want an unknown flag", r.code, r.err)
	}
}

// cobra adds completion and __complete as it runs, after the agent build
// has looked for commands it lacks, and they work as in the full build.
func TestTheAgentBuildCompletesCommands(t *testing.T) {
	if r := run(t, nil, "completion", "bash"); r.code != exitOK || !strings.Contains(r.out, "bash completion V2 for dockwell") {
		t.Errorf("completion bash = %d %q %.100q, want a bash completion script", r.code, r.err, r.out)
	}
	r := run(t, nil, "__complete", "")
	if r.code != exitOK {
		t.Fatalf("__complete = %d %q", r.code, r.err)
	}
	for _, want := range []string{"join\t", "role\t", "run\t", "version\t", ":4\n"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("__complete doesn't complete %q:\n%s", strings.TrimSuffix(want, "\t"), r.out)
		}
	}
	for _, banned := range []string{"token", "claim", "recover", "servers", "login"} {
		if strings.Contains(r.out, "\n"+banned+"\t") {
			t.Errorf("__complete offers %s, which the agent build doesn't have:\n%s", banned, r.out)
		}
	}
	if r := run(t, nil, "__completeNoDesc", "jo"); r.code != exitOK || !strings.HasPrefix(r.out, "join\n") {
		t.Errorf("__completeNoDesc jo = %d %q %q, want join", r.code, r.err, r.out)
	}
}

func TestTheAgentBuildsHelp(t *testing.T) {
	r := run(t, nil)
	if r.code != exitOK {
		t.Fatalf("exit %d, stderr %q", r.code, r.err)
	}
	for _, want := range []string{"This is the agent build of dockwell", "join", "role", "run", "version"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("help is missing %q:\n%s", want, r.out)
		}
	}
	for _, banned := range []string{"\n  token ", "\n  claim ", "\n  recover ", "\n  servers ", "\n  login ", "API commands", "--database"} {
		if strings.Contains(r.out, banned) {
			t.Errorf("help lists %q, which the agent build doesn't have:\n%s", strings.TrimSpace(banned), r.out)
		}
	}
	sub := run(t, nil, "run", "--help")
	if !strings.Contains(sub.out, "never runs a control plane") || strings.Contains(sub.out, "--listen") {
		t.Errorf("run --help:\n%s", sub.out)
	}
}
