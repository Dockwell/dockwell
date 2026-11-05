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

// fakeDocker puts a docker on the path that says these containers and networks
// exist, and writes down what it's asked to do.
func fakeDocker(t *testing.T, containers, networks, attached string) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "docker.log")
	stub := `#!/bin/sh
printf '%s\n' "$*" >>"` + log + `"
case "$*" in
"ps -aq --filter label=dev.dockwell.ingress") printf '%s\n' ` + containers + ` ;;
"network ls --format {{.Name}} --filter label=dev.dockwell.ingress") printf '%s\n' ` + networks + ` ;;
"ps -aq --filter network="*) printf '%s\n' ` + attached + ` ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, log
}

// uninstallWith runs the script's uninstall, with no service to stop and docker
// found on dir.
func uninstallWith(t *testing.T, dir, removeIngress string, withBinary bool) string {
	t.Helper()
	steps := installSteps(t)
	data := layout(t, "control/state.db")
	root := t.TempDir()
	if withBinary {
		if err := os.WriteFile(filepath.Join(root, "dockwell"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; BIN=$2/dockwell; DATA=$3; UNIT=$2/unit; INITD=$2/initd; booted=; init=systemd; remove_ingress=$4; uninstall`,
		"sh", steps, root, data, removeIngress)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}
	return string(out)
}

// Uninstalling says the ingress stays, what that holds, how to free it, and to do
// that before deleting the data, as Docker would otherwise remake its directories
// as root. It takes nothing away that wasn't asked for.
func TestUninstallSaysWhatTheIngressLeaves(t *testing.T) {
	dir, log := fakeDocker(t, "c1 c2", "dockwell_ingress", "''")
	out := uninstallWith(t, dir, "", true)
	for _, want := range []string{"dockwell-ingress", "dockwell-ingress-keep", "dockwell_ingress", "--uninstall --remove-ingress", "before you delete", "as root", "restart for ever", "Removed Dockwell:"} {
		if !strings.Contains(out, want) {
			t.Errorf("uninstall doesn't say %q:\n%s", want, out)
		}
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "rm ") || strings.Contains(string(b), "disconnect") {
		t.Errorf("uninstall changed something it wasn't asked to:\n%s", b)
	}
}

func TestUninstallSaysNothingOfAnIngressThatIsntThere(t *testing.T) {
	dir, _ := fakeDocker(t, "''", "''", "''")
	if out := uninstallWith(t, dir, "", true); strings.Contains(out, "ingress") {
		t.Errorf("uninstall speaks of an ingress with none there:\n%s", out)
	}
}

// Asked to, it removes the containers, takes what's on the network off it, including
// a stopped service, and removes the network, in that order.
func TestUninstallRemovesTheIngressWhenAsked(t *testing.T) {
	dir, log := fakeDocker(t, "c1 c2", "dockwell_ingress", "svc1 svc2")
	out := uninstallWith(t, dir, "1", true)
	if !strings.Contains(out, "the ports it held are free") || !strings.Contains(out, "docker update --restart=always") {
		t.Errorf("it says:\n%s", out)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var did []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "rm ") || strings.HasPrefix(line, "network disconnect") || strings.HasPrefix(line, "network rm") {
			did = append(did, line)
		}
	}
	want := []string{
		"rm --force c1", "rm --force c2",
		"network disconnect --force dockwell_ingress svc1", "network disconnect --force dockwell_ingress svc2",
		"network rm dockwell_ingress",
	}
	if strings.Join(did, "\n") != strings.Join(want, "\n") {
		t.Errorf("it did:\n%s\nwant:\n%s", strings.Join(did, "\n"), strings.Join(want, "\n"))
	}
}

// With Dockwell gone already, as after running it once without the option.
func TestRemovingTheIngressWorksOnceDockwellHasGone(t *testing.T) {
	dir, log := fakeDocker(t, "c1", "dockwell_ingress", "''")
	out := uninstallWith(t, dir, "1", false)
	if !strings.Contains(out, "isn't installed") || !strings.Contains(out, "Removed Dockwell's ingress") {
		t.Errorf("it says:\n%s", out)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "network rm dockwell_ingress") {
		t.Errorf("it did:\n%s", b)
	}
}

func TestRemoveIngressNeedsUninstall(t *testing.T) {
	steps := installSteps(t)
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; main --remove-ingress`, "sh", steps)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--remove-ingress goes with --uninstall") {
		t.Errorf("--remove-ingress alone = %v:\n%s", err, out)
	}
}

// The install script doesn't go back a version on its own, and says what going back
// costs on the server it runs on. On a server that joined, an older agent can lack
// what the control plane relies on, as one from before a command could name a value
// does. On a control server, an older control plane doesn't start on a database a
// newer one has moved on. The latest a mirror serves can be older than what is
// installed. Versions order as semantic versions do: a build between releases or a
// pre-release before the release it leads to, and a version it can't read, such as a
// build from source's devel, is refused as one that may be newer.
func TestTheInstallScriptDoesntGoBackOnItsOwn(t *testing.T) {
	steps := installSteps(t)
	const agentCost = "reads, plans and applies nothing on this server that names one until it is updated again"
	const controlCost = "An older control plane doesn't start on a database a newer one has moved on: none from before a command could name a value starts on one a later control plane has used. If it doesn't start, restore its database from a backup taken before the upgrade"
	for _, c := range []struct {
		installed, version string
		latest, downgrade  bool
		control            bool
		refused            bool
		says               string
	}{
		{installed: "0.3.0", version: "0.2.0", refused: true, says: "v0.2.0 is older than v0.3.0, which is installed here, so nothing was installed."},
		{installed: "0.3.0", version: "0.2.0", latest: true, refused: true, says: "It is the latest release at"},
		{installed: "0.3.0", version: "0.2.0", downgrade: true, says: "Going back from v0.3.0 to v0.2.0, as --downgrade says."},
		{installed: "0.3.0", version: "0.2.0", control: true, refused: true, says: "v0.2.0 is older than v0.3.0"},
		{installed: "0.3.0", version: "0.2.0", control: true, downgrade: true, says: "Going back from v0.3.0 to v0.2.0, as --downgrade says."},
		{installed: "0.3.0", version: "0.3.0"},
		{installed: "0.2.0", version: "0.3.0"},
		{installed: "0.2.1-0.20261105181253-d7d1a767db49", version: "0.2.0", refused: true, says: "v0.2.0 is older than"},
		{installed: "0.2.1-0.20261105181253-d7d1a767db49", version: "0.2.1"},
		{installed: "0.2.1-0.20261106000000-aaaaaaaaaaaa", version: "0.2.1-0.20261105181253-d7d1a767db49", refused: true, says: "is older than"},
		{installed: "0.2.1-0.20261105181253-d7d1a767db49", version: "0.2.1-0.20261106000000-aaaaaaaaaaaa"},
		{installed: "0.3.0", version: "0.3.0-rc.1", refused: true, says: "v0.3.0-rc.1 is older than v0.3.0"},
		{installed: "0.3.0-rc.2", version: "0.3.0-rc.1", refused: true, says: "v0.3.0-rc.1 is older than v0.3.0-rc.2"},
		{installed: "0.3.0-rc.10", version: "0.3.0-rc.9", refused: true, says: "v0.3.0-rc.9 is older than"},
		{installed: "0.3.0-rc", version: "0.3.0-1", refused: true, says: "v0.3.0-1 is older than"},
		{installed: "0.3.0-rc.1", version: "0.3.0-rc", refused: true, says: "v0.3.0-rc is older than v0.3.0-rc.1"},
		{installed: "0.3.0-rc", version: "0.3.0-rc.1"},
		{installed: "0.3.0-rc.1", version: "0.3.0"},
		{installed: "0.3.0+linux", version: "0.3.0"},
		{installed: "0.3.0", version: "0.3.0+linux"},
		{installed: "0.3.0-rc.1+linux", version: "0.3.0-rc.1"},
		{installed: "0.10.0", version: "0.9.0", refused: true, says: "v0.9.0 is older than v0.10.0"},
		{installed: "devel", version: "0.3.0", refused: true, says: "The dockwell installed here says its version is devel, so this script can't tell whether v0.3.0 is older, and nothing was installed."},
		{installed: "devel", version: "0.3.0", control: true, refused: true, says: "To install v0.3.0 anyway, run this again with --downgrade."},
		{installed: "devel", version: "0.3.0", downgrade: true, says: "whose version is devel, as --downgrade says."},
		{installed: "", version: "0.1.0"},
	} {
		latest, downgrade := "", ""
		if c.latest {
			latest = "1"
		}
		if c.downgrade {
			downgrade = "1"
		}
		data := t.TempDir()
		if c.control {
			if err := os.MkdirAll(filepath.Join(data, "control"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(data, "control", "state.db"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.CommandContext(t.Context(), "sh", "-c",
			`. "$1"; installed_version=$2; version=$3; latest=$4; downgrade=$5; DATA=$6; releases=https://releases.example; going_back; echo went on`,
			"sh", steps, c.installed, c.version, latest, downgrade, data)
		out, err := cmd.CombinedOutput()
		if refused := err != nil; refused != c.refused {
			t.Errorf("%s over %s: refused %t, want %t:\n%s", c.version, c.installed, refused, c.refused, out)
		}
		if !strings.Contains(string(out), c.says) {
			t.Errorf("%s over %s says %q, want it to say %q", c.version, c.installed, out, c.says)
		}
		if !c.refused && !c.downgrade {
			continue
		}
		cost, other := agentCost, controlCost
		if c.control {
			cost, other = controlCost, agentCost
		}
		if !strings.Contains(string(out), cost) || strings.Contains(string(out), other) {
			t.Errorf("%s over %s on a control server (%t) doesn't say what it costs there: %s", c.version, c.installed, c.control, out)
		}
		if c.refused && !strings.Contains(string(out), "run this again with --downgrade") {
			t.Errorf("%s over %s doesn't say how to go back on purpose: %s", c.version, c.installed, out)
		}
	}
}

// Run as someone other than root, the install script says how to run it again with
// sudo, with the options it was given, --downgrade included, and DOCKWELL_DOWNGRADE
// carried as that option, as sudo drops the environment.
func TestTheInstallScriptKeepsDowngradeForSudo(t *testing.T) {
	steps := installSteps(t)
	for _, c := range []struct {
		env, args string
		says      []string
	}{
		{args: "--downgrade", says: []string{"| sudo sh -s -- --downgrade, or run sudo sh install.sh --downgrade."}},
		{env: "1", says: []string{"| sudo sh -s -- --downgrade,", "sudo doesn't pass the DOCKWELL_ settings on, so the options carry them instead."}},
		{env: "1", args: "--version 0.2.0", says: []string{"-s -- --version 0.2.0 --downgrade,"}},
	} {
		cmd := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; uname() { echo Linux; }; id() { echo 1000; }; shift; main "$@"`, "sh", steps)
		cmd.Args = append(cmd.Args, strings.Fields(c.args)...)
		cmd.Env = append(os.Environ(), "DOCKWELL_DOWNGRADE="+c.env)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("%q with DOCKWELL_DOWNGRADE=%s went on as someone other than root:\n%s", c.args, c.env, out)
		}
		for _, says := range c.says {
			if !strings.Contains(string(out), says) {
				t.Errorf("%q with DOCKWELL_DOWNGRADE=%s says %q, want it to say %q", c.args, c.env, out, says)
			}
		}
	}
}
