// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/datadir"
)

// The install script picks the build for a server from its data directory
// and the binary installed there, before it downloads anything. These run
// its own functions, with a stand-in for the installed binary, so they need
// neither root nor a release; scripts/install-test runs the whole script in
// containers.

// installSteps is scripts/install.sh without its last line, which runs it,
// so that sourcing it only defines its functions.
func installSteps(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("there's no sh here to run the install script with")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body, ok := strings.CutSuffix(string(src), "\nmain \"$@\"\n")
	if !ok {
		t.Fatal(`scripts/install.sh doesn't end with main "$@", so these tests can't source it`)
	}
	steps := filepath.Join(t.TempDir(), "install-steps.sh")
	if err := os.WriteFile(steps, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return steps
}

// installScript sources the install script's steps, sets BIN and DATA to
// bin and data, and runs script, which prints what it found.
func installScript(t *testing.T, steps, bin, data, ticket, script string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; BIN=$2; DATA=$3; ticket=$4; `+script, "sh", steps, bin, data, ticket)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the install script's steps failed: %v\n%s", err, out)
	}
	return string(out)
}

// layout makes a data directory with these files, relative to it.
func layout(t *testing.T, files ...string) string {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		path := filepath.Join(data, f)
		if strings.HasSuffix(f, "/") {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

// installed is a stand-in for an installed dockwell: it prints json for
// dockwell version --json, fails at anything else, and counts its starts in
// <bin>.starts. No json means a binary that fails at everything.
func installed(t *testing.T, json string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dockwell")
	script := "#!/bin/sh\necho \"$*\" >>\"$0.starts\"\n"
	if json != "" {
		script += "[ \"$*\" = 'version --json' ] || exit 1\ncat <<'EOF'\n" + json + "\nEOF\n"
	} else {
		script += "exit 1\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// What dockwell version --json prints, with protojson's spacing, which
// varies from build to build.
const (
	versionV020 = `{
  "version":  "v0.2.0",
  "commit":  "d0c0ffee",
  "goVersion":  "go1.27.1",
  "platform":  "linux/amd64"
}`
	versionFull = `{
  "version": "v0.3.0",
  "commit": "d0c0ffee",
  "goVersion": "go1.27.3",
  "platform": "linux/amd64",
  "edition": "BUILD_EDITION_FULL"
}`
	versionAgent = `{
  "version":  "v0.3.0",
  "commit":  "d0c0ffee",
  "goVersion":  "go1.27.3",
  "platform":  "linux/amd64",
  "edition":  "BUILD_EDITION_AGENT"
}`
)

func TestTheInstallScriptPicksTheBuild(t *testing.T) {
	steps := installSteps(t)
	const switches = "This server has joined a control plane and runs only its agent, so it switches to the agent build"
	for _, tt := range []struct {
		name    string
		json    string // the installed binary's, or none installed
		missing bool   // no binary at all
		files   []string
		ticket  string
		build   string // the build it picks
		has     string // the build installed, as it reads it
		version string
		note    string
	}{
		{name: "a first server", missing: true, build: "full"},
		{name: "a first server with a ticket", missing: true, ticket: "dwj1.x", build: "agent"},
		{
			name: "a control server", json: versionFull, files: []string{"control/key", "control/state.db", "agent/agent.json"},
			build: "full", has: "full", version: "0.3.0",
		},
		{
			name: "a control server of v0.2.0", json: versionV020, files: []string{"control/key", "control/state.db", "agent/agent.json"},
			build: "full", has: "full", version: "0.2.0",
		},
		{
			name: "a control server on Postgres", json: versionFull, files: []string{"control/key", "agent/agent.json"},
			build: "full", has: "full", version: "0.3.0",
		},
		{
			name: "a control server with only its SQLite state", json: versionFull, files: []string{"control/state.db"},
			build: "full", has: "full", version: "0.3.0",
		},
		{
			name: "a server that joined with v0.2.0", json: versionV020, files: []string{"agent/agent.json"},
			build: "agent", has: "full", version: "0.2.0", note: switches,
		},
		{
			name: "a server that joined with the full build", json: versionFull, files: []string{"agent/agent.json"},
			build: "agent", has: "full", version: "0.3.0", note: switches,
		},
		{
			name: "a server that joined with the agent build", json: versionAgent, files: []string{"agent/agent.json"},
			build: "agent", has: "agent", version: "0.3.0",
		},
		{
			name: "a server that joined, with a spent ticket", json: versionAgent, files: []string{"agent/agent.json"}, ticket: "dwj1.x",
			build: "agent", has: "agent", version: "0.3.0",
		},
		{name: "the agent build, never joined, without a ticket", json: versionAgent, build: "full", has: "agent", version: "0.3.0"},
		// A first start as a control server that failed leaves only its
		// new key, which isn't a control plane yet.
		{
			name: "a failed first start, then a ticket", json: versionV020, files: []string{"control/key.new"}, ticket: "dwj1.x",
			build: "agent", has: "full", version: "0.2.0",
		},
		{
			name: "a failed first start, joined since", json: versionFull, files: []string{"control/key.new", "agent/agent.json"},
			build: "agent", has: "full", version: "0.3.0", note: switches,
		},
		{
			name: "a failed first start, without a ticket", json: versionFull, files: []string{"control/key.new"},
			build: "full", has: "full", version: "0.3.0",
		},
		{
			name: "certificates without a control plane, and a ticket", json: versionFull, files: []string{"control/acme/"}, ticket: "dwj1.x",
			build: "agent", has: "full", version: "0.3.0",
		},
		{name: "a binary that isn't dockwell", files: []string{"agent/agent.json"}, build: "agent"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "dockwell")
			if !tt.missing {
				bin = installed(t, tt.json)
			}
			out := installScript(t, steps, bin, layout(t, tt.files...), tt.ticket,
				`read_installed; build_note=; pick_build; printf '%s|%s|%s|%s\n' "$build" "$installed_build" "$installed_version" "$build_note"`)
			got := strings.SplitN(strings.TrimSuffix(out, "\n"), "|", 4)
			if len(got) != 4 {
				t.Fatalf("the steps printed %q", out)
			}
			if got[0] != tt.build || got[1] != tt.has || got[2] != tt.version {
				t.Errorf("build %q, installed %q, version %q; want %q, %q, %q", got[0], got[1], got[2], tt.build, tt.has, tt.version)
			}
			if (tt.note == "") != (got[3] == "") || !strings.HasPrefix(got[3], tt.note) {
				t.Errorf("the note is %q, want %q", got[3], tt.note)
			}
			// One run of the old binary tells its version and build.
			if starts, err := os.ReadFile(bin + ".starts"); err == nil && strings.Count(string(starts), "\n") > 1 {
				t.Errorf("the installed binary ran %d times:\n%s", strings.Count(string(starts), "\n"), starts)
			}
		})
	}
}

// The install script decides whether a data directory holds a control
// plane as dockwell run does.
func TestTheInstallScriptFindsAControlPlaneAsRunDoes(t *testing.T) {
	steps := installSteps(t)
	for _, files := range [][]string{
		nil,
		{"control/"},
		{"control/key.new"},
		{"control/acme/", "control/local-token.json"},
		{"control/key"},
		{"control/state.db"},
		{"control/key", "control/state.db", "agent/agent.json"},
		{"agent/agent.json"},
	} {
		data := layout(t, files...)
		want := hasControlPlane(datadir.Dir{Root: data})
		got := installScript(t, steps, "/nonexistent", data, "", `if has_control_plane; then echo true; else echo false; fi`) == "true\n"
		if got != want {
			t.Errorf("with %q, the install script says a control plane is there: %v; dockwell run says %v", files, got, want)
		}
	}
}
