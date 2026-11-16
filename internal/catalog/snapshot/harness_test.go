// SPDX-License-Identifier: AGPL-3.0-only

package snapshot

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v4"
)

// world is scripts/catalog-e2e.sh's own run in a fake world: Docker, Dockwell's CLI,
// curl and go stand in from testdata/harness, over a catalog repository of the
// entries and checks given, so the harness's flow and a real check's run through it
// on any machine, with no Docker. It returns the folder the fakes keep their state in.
type world struct {
	t                    *testing.T
	root, state, catalog string
	env                  []string
}

func newWorld(t *testing.T, entries map[string][]string, checks map[string]string) *world {
	t.Helper()
	for _, tool := range []string{"bash", "git", "jq", "openssl", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the harness needs %s", tool)
		}
	}
	dir := t.TempDir()
	// The harness leaves its copies read-only; they go with the folder.
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, root: root, state: filepath.Join(dir, "state"), catalog: filepath.Join(dir, "catalog")}
	tool := filepath.Join(dir, "dockwell-catalog")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", tool, "./cmd/dockwell-catalog")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building dockwell-catalog: %v\n%s", err, out)
	}
	git := func(args ...string) {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", w.catalog, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for _, d := range []string{"entries", "checks"} {
		if err := os.MkdirAll(filepath.Join(w.catalog, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	// Each entry's releases, a commit each, as the catalog's history holds them.
	for i := 0; ; i++ {
		more := false
		for id, releases := range entries {
			if i < len(releases) {
				writeFile(t, filepath.Join(w.catalog, "entries", id+".yaml"), releases[i])
				more = true
			}
		}
		if !more {
			break
		}
		if i == 0 {
			for id, check := range checks {
				writeFile(t, filepath.Join(w.catalog, "checks", id+".sh"), check)
			}
		}
		git("add", "-A")
		git("commit", "-qm", "release")
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		t.Fatal(err)
	}
	testdata, err := filepath.Abs("testdata/harness")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "E2E_") && !strings.HasPrefix(e, "PATH=") && !strings.HasPrefix(e, "TMPDIR=") {
			w.env = append(w.env, e)
		}
	}
	w.env = append(w.env, "PATH="+filepath.Join(testdata, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+tmp, "FAKE_STATE="+w.state, "FAKE_CATALOG_TOOL="+tool, "FAKE_CATALOG="+w.catalog,
		"E2E_FLOORED="+filepath.Join(dir, "floored"), "E2E_NOT_UPGRADED="+filepath.Join(dir, "not-upgraded"))
	return w
}

// run runs the harness with its flags and entries, and returns what it said.
func (w *world) run(args ...string) (string, error) {
	w.t.Helper()
	if err := os.MkdirAll(w.state, 0o750); err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(w.t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{
		filepath.Join(w.root, "scripts", "catalog-e2e.sh"),
		filepath.Join(w.root, "internal", "catalog", "snapshot", "testdata", "harness", "dockwell"), "--catalog", w.catalog,
	}, args...)...)
	cmd.Env = w.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (w *world) read(name string) string {
	w.t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(w.state), name))
	if err != nil && !os.IsNotExist(err) {
		w.t.Fatal(err)
	}
	return string(raw)
}

func writeFile(t *testing.T, file, text string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// madeUp is an entry of two services at a version, a web one and a database, with
// a port among its inputs, as JSON, which reads as YAML too.
func madeUp(id, version string) string {
	raw, _ := json.Marshal(map[string]any{
		"id": id, "name": id, "version": version, "release": 1, "web": map[string]string{"service": "app"},
		"inputs": []map[string]string{{"name": "PORT", "type": "INPUT_TYPE_PORT", "defaultValue": "18090"}},
		"compose": "services:\n  app:\n    image: example/" + id + ":" + version + "@sha256:" + strings.Repeat("ab", 32) +
			"\n  db:\n    image: example/" + id + "-db:" + version + "@sha256:" + strings.Repeat("cd", 32) + "\n",
	})
	return string(raw)
}

// A check runs as its own process (#726's slice 2 re-check, N3), and reads what the
// run that judges it has: its entry's version, its install's values and Compose
// project, and Dockwell's CLI to plan and apply a change of its own, as mariadb's
// does. #743's first run on Docker failed every check at its first read of the
// install, as the check's process had none of it; this runs adminer's own check
// through the harness, as Dockwell's CI does, from the release before it names.
func TestTheHarnessRunsARealCheckInItsOwnProcess(t *testing.T) {
	raw, err := os.ReadFile("entries/adminer.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var adminer map[string]any
	if err := yaml.Unmarshal(raw, &adminer); err != nil {
		t.Fatal(err)
	}
	adminerJSON, err := json.Marshal(adminer)
	if err != nil {
		t.Fatal(err)
	}
	check, err := os.ReadFile("checks/adminer.sh")
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t, map[string][]string{
		"adminer": {string(adminerJSON)},
		"planner": {madeUp("planner", "1.0.0"), madeUp("planner", "1.1.0")},
	}, map[string]string{
		"adminer": string(check),
		"planner": `check_planner() {
  local port planned
  port=$(value PORT)
  [[ "$port" == 18090 ]] || fail "planner's install keeps PORT '$port'"
  container app >/dev/null
  container db >/dev/null
  if [[ "$1" == first ]]; then
    planned=$(dw databases change-password db --project "$id" --plan-only --json) || fail "planning a new password failed"
    plans+=("$(jq -r .change.id <<<"$planned")")
    apply "${plans[-1]}"
    settled
    echo "${plans[-1]}" >"$FAKE_STATE/../planner-change"
    kept[planner]=$version
  else
    [[ "${kept[planner]:-}" == 1.0.0 && "$version" == 1.1.0 ]] || fail "planner kept '${kept[planner]:-}' at $version"
  fi
}
`,
	})
	out, err := w.run("--require-upgrade", "--no-floor", "adminer", "planner")
	if err != nil {
		t.Fatalf("the harness failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"adminer: 6.1.2, the release before its check names, installs",
		"adminer: upgrades from 6.1.2 to " + adminer["version"].(string),
		"planner: upgrades from 1.0.0 to 1.1.0",
		"catalog-e2e: ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the harness never said %q:\n%s", want, out)
		}
	}
	// The plan the check made went back to the run, which read it for its secrets.
	change := strings.TrimSpace(w.read("planner-change"))
	if change == "" || !strings.Contains(w.read("state/read-back"), change+"\n") {
		t.Errorf("the run never read back %q, the plan planner's check made", change)
	}
}

// A check can't reach the next entry through the files it writes (#726's slice 2
// re-check, N3): the run read every check before the first ran, and keeps its lists
// of entries proven with the floor alone in its own process.
func TestACheckReachesNoLaterEntryThroughFiles(t *testing.T) {
	w := newWorld(t, map[string][]string{
		"rewriter": {madeUp("rewriter", "1.0.0"), madeUp("rewriter", "1.1.0")},
		"victim":   {madeUp("victim", "1.0.0"), madeUp("victim", "1.1.0")},
		"plain":    {madeUp("plain", "1.0.0"), madeUp("plain", "1.1.0")},
	}, map[string]string{
		"rewriter": `check_rewriter() {
  printf 'check_victim() { echo rewritten >>"$FAKE_STATE/../victim"; }\n' >"$FAKE_CATALOG/checks/victim.sh"
  : >"$E2E_FLOORED"
  echo rewriter >>"$E2E_NOT_UPGRADED"
}
`,
		"victim": `check_victim() { echo "original $1" >>"$FAKE_STATE/../victim"; }
`,
	})
	out, err := w.run("rewriter", "victim", "plain")
	if err != nil {
		t.Fatalf("the harness failed: %v\n%s", err, out)
	}
	if got := w.read("victim"); got != "original first\noriginal upgraded\n" {
		t.Errorf("victim's check ran as %q, want its own", got)
	}
	if got, want := w.read("floored"), "plain\n"; got != want {
		t.Errorf("the floored list reads %q, want %q", got, want)
	}
	if got := w.read("not-upgraded"); got != "" {
		t.Errorf("the not-upgraded list reads %q, want none", got)
	}
}
