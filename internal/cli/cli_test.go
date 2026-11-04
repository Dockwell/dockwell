// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/buildinfo"
)

type result struct {
	code     int
	out, err string
}

func run(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Execute(t.Context(), args, IO{
		Out:    &out,
		Err:    &errOut,
		Getenv: func(k string) string { return env[k] },
	})
	return result{code: code, out: out.String(), err: errOut.String()}
}

func TestVersion(t *testing.T) {
	r := run(t, nil, "version")
	if r.code != exitOK {
		t.Fatalf("exit %d, stderr %q", r.code, r.err)
	}
	info := buildinfo.Read()
	for _, want := range []string{"dockwell  " + info.Version, "go", info.GoVersion, "platform", info.Platform} {
		if !strings.Contains(r.out, want) {
			t.Errorf("version output missing %q:\n%s", want, r.out)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	r := run(t, nil, "version", "--json")
	if r.code != exitOK {
		t.Fatalf("exit %d, stderr %q", r.code, r.err)
	}
	// Same field names as dockwell.v1.BuildInfo over the API (protojson).
	var got map[string]any
	if err := json.Unmarshal([]byte(r.out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, r.out)
	}
	for _, key := range []string{"version", "goVersion", "platform"} {
		if s, _ := got[key].(string); s == "" {
			t.Errorf("JSON is missing %q: %s", key, r.out)
		}
	}
}

func TestWriteVersion(t *testing.T) {
	var buf bytes.Buffer
	err := writeVersion(&buf, buildinfo.Info{
		Version:    "v0.4.0",
		Commit:     "3f2a9c1d4e5b6a7f8091a2b3c4d5e6f708192a3b",
		CommitTime: time.Date(2026, 11, 3, 21, 40, 0, 0, time.UTC),
		Modified:   true,
		GoVersion:  "go1.27.3",
		Platform:   "linux/arm64",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "" +
		"dockwell   v0.4.0\n" +
		"commit     3f2a9c1d4e5b (modified)\n" +
		"committed  3 Nov 2026 21:40 UTC\n" +
		"go         go1.27.3\n" +
		"platform   linux/arm64\n"
	if buf.String() != want {
		t.Errorf("writeVersion() =\n%s\nwant\n%s", buf.String(), want)
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown command", []string{"deploy-everything"}, `unknown command "deploy-everything". Run "dockwell --help" to see the commands`},
		{"unknown flag", []string{"version", "--yaml"}, `Run "dockwell version --help" to see the flags`},
		{"bad log level", []string{"--log-level", "loud", "run"}, `unknown log level "loud": use debug, info, warn or error`},
		{"stray argument to version", []string{"version", "extra"}, `dockwell version takes no arguments, but got "extra". Run "dockwell version --help"`},
		{"stray argument to run", []string{"run", "extra"}, `dockwell run takes no arguments, but got "extra". Run "dockwell run --help"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := run(t, nil, tt.args...)
			if r.code != exitUsage {
				t.Errorf("exit %d, want %d", r.code, exitUsage)
			}
			if !strings.HasPrefix(r.err, "dockwell: ") || !strings.Contains(r.err, tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", r.err, tt.want)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	r := run(t, nil)
	if r.code != exitOK {
		t.Fatalf("exit %d, stderr %q", r.code, r.err)
	}
	for _, want := range []string{"Commands:", "run", "version", "--log-level", `Run "dockwell <command> --help"`} {
		if !strings.Contains(r.out, want) {
			t.Errorf("help missing %q:\n%s", want, r.out)
		}
	}
	// Content rule: sentence case everywhere, including cobra's headings.
	for _, banned := range []string{"Available Commands", "Global Flags", "Use \"dockwell [command]"} {
		if strings.Contains(r.out, banned) {
			t.Errorf("help still contains %q:\n%s", banned, r.out)
		}
	}

	sub := run(t, nil, "run", "--help")
	if !strings.Contains(sub.out, "Global flags:") || !strings.Contains(sub.out, "--listen") {
		t.Errorf("run --help:\n%s", sub.out)
	}
}

func TestRunExplainsListenFailures(t *testing.T) {
	var lc net.ListenConfig
	busy, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	tests := []struct {
		name, listen, want string
	}{
		{"address in use", busy.Addr().String(), "Stop the process using that address, or pass a different --listen address"},
		{"malformed address", "no-port-here", "Pass --listen as host:port, such as 127.0.0.1:7600"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := run(t, map[string]string{envLogFormat: "json", envDataDir: t.TempDir()},
				"run", "--listen", tt.listen, "--agent-listen", "127.0.0.1:0")
			if r.code != exitError {
				t.Fatalf("exit %d, want %d; stderr %q", r.code, exitError, r.err)
			}
			if !strings.Contains(r.err, "can't listen on "+tt.listen) || !strings.Contains(r.err, tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", r.err, tt.want)
			}
		})
	}
}

func TestWithListenHint(t *testing.T) {
	if withListenHint(nil, "--listen") != nil {
		t.Error("withListenHint(nil) should stay nil")
	}
	eacces := fmt.Errorf("can't listen on 0.0.0.0:80: %w", &net.OpError{Op: "listen", Err: syscall.EACCES})
	if got := withListenHint(eacces, "--agent-listen").Error(); !strings.Contains(got, "CAP_NET_BIND_SERVICE") || !strings.Contains(got, "--agent-listen") {
		t.Errorf("EACCES hint = %q", got)
	}
	malformed := fmt.Errorf("can't listen on foo: %w", &net.AddrError{Err: "missing port in address", Addr: "foo"})
	if got := withListenHint(malformed, "--agent-listen").Error(); !strings.Contains(got, "such as :7443") {
		t.Errorf("the agent listener's example should be its own default, got %q", got)
	}
	if got := withListenHint(eacces, "--listen").Error(); !strings.Contains(got, "CAP_NET_BIND_SERVICE") {
		t.Errorf("EACCES hint = %q", got)
	}
	other := errors.New("something else")
	if got := withListenHint(other, "--listen"); !errors.Is(got, other) || got.Error() != "something else" {
		t.Errorf("unrelated errors should pass through untouched, got %q", got)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	var out, errOut bytes.Buffer
	go func() {
		done <- Execute(ctx, []string{"run", "--listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0", "--data-dir", t.TempDir(), "--log-format", "json"}, IO{
			Out: &out, Err: &errOut, Getenv: func(string) string { return "" },
		})
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit %d after cancel, stderr:\n%s", code, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop within 5 s of cancellation")
	}
}

// startRun runs `dockwell run` on loopback ports with its own data
// directory, and waits until the control server's agent has its identity.
func startRun(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errOut syncBuffer
	go func() {
		done <- Execute(ctx, []string{"run", "--listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0", "--data-dir", dir, "--log-format", "json"}, IO{
			Out: io.Discard, Err: &errOut, Getenv: func(string) string { return "" },
		})
	}()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != exitOK {
			t.Errorf("run exited %d:\n%s", code, errOut.String())
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "agent", "agent.json")); err == nil {
			return dir
		}
		if time.Now().After(deadline) {
			t.Fatalf("the control server's agent never got an identity:\n%s", errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAddAServerThroughTheCLI(t *testing.T) {
	control := startRun(t)
	none := map[string]string{envLogFormat: "json"}

	r := run(t, none, "token", "create", "--data-dir", control, "--ttl", "30m")
	if r.code != exitOK {
		t.Fatalf("token create exit %d: %s", r.code, r.err)
	}
	ticket := regexp.MustCompile(`dockwell join (dwj1\.\S+)`).FindStringSubmatch(r.out)
	if ticket == nil || !strings.Contains(r.out, "expires in 30 min") {
		t.Fatalf("token create printed:\n%s", r.out)
	}

	added := t.TempDir()
	r = run(t, none, "join", ticket[1], "--data-dir", added, "--name", "hz-2")
	if r.code != exitOK || !strings.HasPrefix(r.out, "Joined as hz-2 (srv_") {
		t.Fatalf("join exit %d:\n%s%s", r.code, r.out, r.err)
	}

	r = run(t, none, "join", ticket[1], "--data-dir", added)
	if r.code != exitError || !strings.Contains(r.err, "already joined") {
		t.Errorf("joining twice = %d %q", r.code, r.err)
	}
	r = run(t, none, "join", ticket[1], "--data-dir", t.TempDir())
	if r.code != exitError || !strings.Contains(r.err, "invalid, already used or expired") {
		t.Errorf("reusing a ticket = %d %q", r.code, r.err)
	}
	r = run(t, none, "token", "create", "--data-dir", t.TempDir())
	if r.code != exitError || !strings.Contains(r.err, "there's no control plane") {
		t.Errorf("token create away from the control server = %d %q", r.code, r.err)
	}
	r = run(t, none, "join")
	if r.code != exitUsage || !strings.Contains(r.err, "takes one join ticket") {
		t.Errorf("join without a ticket = %d %q", r.code, r.err)
	}
}

func TestLoopbackURL(t *testing.T) {
	for _, tt := range []struct {
		addr *net.TCPAddr
		want string
	}{
		{&net.TCPAddr{IP: net.IPv4zero, Port: 7443}, "https://127.0.0.1:7443"},
		{&net.TCPAddr{IP: net.IPv6unspecified, Port: 7443}, "https://127.0.0.1:7443"},
		{&net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 9443}, "https://10.0.0.5:9443"},
		{&net.TCPAddr{IP: net.IPv6loopback, Port: 7443}, "https://[::1]:7443"},
	} {
		if got := loopbackURL(tt.addr); got != tt.want {
			t.Errorf("loopbackURL(%v) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}
