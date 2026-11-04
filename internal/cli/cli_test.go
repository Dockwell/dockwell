// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
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
	for _, want := range []string{"dockwell  " + info.Version, "build", string(info.Edition), "go", info.GoVersion, "platform", info.Platform} {
		if !strings.Contains(r.out, want) {
			t.Errorf("version output missing %q:\n%s", want, r.out)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	info := buildinfo.Read()
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
	// install.sh reads the build from here to keep it on upgrades.
	want := map[buildinfo.Edition]string{buildinfo.Full: "BUILD_EDITION_FULL", buildinfo.Agent: "BUILD_EDITION_AGENT"}[info.Edition]
	if got["edition"] != want {
		t.Errorf("JSON has edition %v, want %q for the %s build: %s", got["edition"], want, info.Edition, r.out)
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
		Edition:    buildinfo.Agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "" +
		"dockwell   v0.4.0\n" +
		"build      agent\n" +
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
		// Each build says more about it: edition_agent_test.go has the agent build's.
		{"unknown command", []string{"deploy-everything"}, `"deploy-everything"`},
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

// A usage error comes first, whatever else an error carries.
func TestExitCodes(t *testing.T) {
	const claimed = 3 // dockwell claim's, in the full build
	owned := exitCodeError{claimed, errors.New("this install already has an owner")}
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{
		{"an exit code of its own", owned, claimed},
		{"a usage error with an exit code of its own", usageError{owned}, exitUsage},
		{"an exit code of its own with a usage error", exitCodeError{claimed, usageError{errors.New("no such flag")}}, exitUsage},
		{"any other error", errors.New("something failed"), exitError},
	} {
		if got := exitCode(tt.err); got != tt.want {
			t.Errorf("%s exits %d, want %d", tt.name, got, tt.want)
		}
	}
}
