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

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

// These run with -tags agent, against the agent build. A test binary may
// link the control plane to stand one up for the agent to join; the agent
// build itself never does, which cmd/dockwell's build_test.go checks.

// controlPlane is a real control plane's agent listener, on a loopback
// port, for the agent build to join.
type controlPlane struct {
	auth *identity.Authority
	hub  *fleet.Hub
	url  string
}

func startControlPlane(t *testing.T) *controlPlane {
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
	srv, err := control.New(control.Config{Logger: logger, Identity: auth, Fleet: hub, AgentHosts: []string{"127.0.0.1"}, Panel: fstest.MapFS{}})
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
	cp := startControlPlane(t)
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
			Out: io.Discard, Err: &logs, Getenv: func(string) string { return "" },
		})
	}()
	// The hello says which build the agent is, and the control plane keeps
	// it with the server, where the API serves it.
	deadline := time.Now().Add(10 * time.Second)
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
		if time.Now().After(deadline) {
			t.Fatalf("the agent didn't connect within 10 s:\n%s", logs.String())
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
		{[]string{"servers", "list"}, `"servers" isn't a command of the agent build of dockwell, which this server runs. The control plane's commands`},
		{[]string{"login", "--url", "https://dockwell.example.com"}, `"login" isn't a command of the agent build`},
		{[]string{"deploy-everything"}, `"deploy-everything" isn't a command of the agent build`},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			r := run(t, nil, tt.args...)
			if r.code != exitUsage || !strings.Contains(r.err, tt.want) {
				t.Errorf("dockwell %s = %d %q, want %d and %q", strings.Join(tt.args, " "), r.code, r.err, exitUsage, tt.want)
			}
		})
	}
	// The flag only the control plane reads isn't there either.
	if r := run(t, nil, "--database", "postgres://db/dockwell", "role"); r.code != exitUsage || !strings.Contains(r.err, "unknown flag: --database") {
		t.Errorf("--database = %d %q, want an unknown flag", r.code, r.err)
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
