// SPDX-License-Identifier: AGPL-3.0-only

package snapshot

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		"E2E_FLOORED="+filepath.Join(dir, "floored"), "E2E_NOT_UPGRADED="+filepath.Join(dir, "not-upgraded"),
		// A service on a stand-in is held to running for a second, not a minute.
		"E2E_HOLD=1")
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

// madeUp is an entry of two services at a version, a web one and a database, as
// JSON, which reads as YAML too. The web one publishes ports, as "${NAME}:<port in
// the container>", each from an input of its own; ${PORT}:8080 where none are
// given. In the fake world, a container's 8080 alone answers a page.
func madeUp(id, version string, ports ...string) string {
	if len(ports) == 0 {
		ports = []string{"${PORT}:8080"}
	}
	var compose strings.Builder
	compose.WriteString("services:\n  app:\n    image: example/" + id + ":" + version + "@sha256:" + strings.Repeat("ab", 32) + "\n    ports:\n")
	var inputs []map[string]string
	for i, p := range ports {
		compose.WriteString("      - \"127.0.0.1:" + p + "\"\n")
		name := strings.TrimPrefix(strings.Split(p, "}")[0], "${")
		inputs = append(inputs, map[string]string{"name": name, "type": "INPUT_TYPE_PORT", "defaultValue": strconv.Itoa(18090 + i)})
	}
	compose.WriteString("  db:\n    image: example/" + id + "-db:" + version + "@sha256:" + strings.Repeat("cd", 32) + "\n")
	raw, _ := json.Marshal(map[string]any{
		"id": id, "name": id, "version": version, "release": 1, "web": map[string]string{"service": "app"},
		"inputs": inputs, "compose": compose.String(),
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
		// Their releases share commits, as Ghost's and WordPress's do since
		// Dockwell/catalog#4, and each upgrades from its own (#743's first run).
		"planner": {madeUp("planner", "1.0.0"), madeUp("planner", "1.1.0")},
		// A web service that publishes SFTP's port before its web one, as SFTPGo's
		// does, and one with no PORT among its ports, as RustFS's S3 and console.
		"sftp":  {madeUp("sftp", "2.0.0", "${SFTP_PORT}:2022", "${PORT}:8080"), madeUp("sftp", "2.1.0", "${SFTP_PORT}:2022", "${PORT}:8080")},
		"store": {madeUp("store", "3.0.0", "${S3_PORT}:9000", "${CONSOLE_PORT}:8080"), madeUp("store", "3.1.0", "${S3_PORT}:9000", "${CONSOLE_PORT}:8080")},
	}, map[string]string{
		"adminer": string(check),
		"sftp":    "check_sftp() { :; }\n",
		"store":   "check_store() { :; }\n",
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
	out, err := w.run("--require-upgrade", "--no-floor", "adminer", "planner", "sftp", "store")
	if err != nil {
		t.Fatalf("the harness failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"adminer: 6.1.2, the release before its check names, installs",
		"adminer: upgrades from 6.1.2 to " + adminer["version"].(string),
		"planner: upgrades from 1.0.0 to 1.1.0",
		"sftp: upgrades from 2.0.0 to 2.1.0",
		"store: upgrades from 3.0.0 to 3.1.0",
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

// needsInputs is an entry, as JSON, with inputs a person must give, an email and a
// token no default or generator fills, and a tunnel service that reads the token
// and, as fake says: unready, never passes its health check, as cloudflared's
// wouldn't with a stand-in; exits, stops on it; or refused, both, as cloudflared's
// does on a token its far end refuses. Its app reads TUNNEL_TOKEN_TTL, a name that
// starts with the token's, and no stand-in.
func needsInputs(version, fake string) string {
	compose := "services:\n  app:\n    image: example/needs:" + version + "@sha256:" + strings.Repeat("ab", 32) +
		"\n    ports:\n      - \"127.0.0.1:${PORT}:8080\"\n    environment:\n      ADMIN_EMAIL: ${ADMIN_EMAIL}\n      TTL: ${TUNNEL_TOKEN_TTL}\n" +
		"  tunnel:\n    image: example/needs-tunnel:" + version + "@sha256:" + strings.Repeat("cd", 32) +
		"\n    x-fake: " + fake + "\n    healthcheck:\n      test: [\"CMD\", \"tunnel\", \"ready\"]\n      interval: 10s\n    environment:\n      TUNNEL_TOKEN: ${TUNNEL_TOKEN}\n"
	raw, _ := json.Marshal(map[string]any{
		"id": "needs", "name": "Needs", "version": version, "release": 1, "web": map[string]string{"service": "app"},
		"inputs": []map[string]string{
			{"name": "ADMIN_EMAIL", "label": "Your email", "type": "INPUT_TYPE_EMAIL"},
			{"name": "TUNNEL_TOKEN", "label": "Tunnel token", "type": "INPUT_TYPE_SECRET"},
			{"name": "TUNNEL_TOKEN_TTL", "label": "Token lifetime", "type": "INPUT_TYPE_TEXT", "defaultValue": "3600"},
			{"name": "PORT", "label": "Port", "type": "INPUT_TYPE_PORT", "defaultValue": "18095"},
		},
		"compose": compose,
	})
	return string(raw)
}

// An entry with no check of its own, whose inputs a person must give, is proven with
// stand-ins for them: an email at .invalid, and a token of no account anywhere. The
// tunnel that reads the token runs with no health check, which it can't pass with a
// stand-in, and is held to running with no restart; the app beside it, which reads
// TUNNEL_TOKEN_TTL and no stand-in, keeps its health check. The floored list says
// what was left unproven (#745). Before, Dockwell refused the install: "Your email
// needs a value", and then failed it on the tunnel's health check.
func TestTheFloorGivesStandInsForWhatAPersonMustGive(t *testing.T) {
	w := newWorld(t, map[string][]string{"needs": {needsInputs("1.0.0", "unready"), needsInputs("1.1.0", "unready")}}, nil)
	out, err := w.run("needs")
	if err != nil {
		t.Fatalf("the harness failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"needs: has no check to give ADMIN_EMAIL, so the floor gives it a stand-in",
		"needs: has no check to give TUNNEL_TOKEN, so the floor gives it a stand-in",
		"needs: runs tunnel with no health check, as it reads a stand-in for TUNNEL_TOKEN",
		"needs: upgrades from 1.0.0 to 1.1.0",
		"catalog-e2e: ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the harness never said %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "runs app, tunnel") || strings.Contains(out, "runs tunnel, app") {
		t.Errorf("the app, which reads TUNNEL_TOKEN_TTL and no stand-in, lost its health check:\n%s", out)
	}
	if got, want := w.read("floored"), "needs: tunnel not held to a health check, as it reads a stand-in for TUNNEL_TOKEN, which connects to nothing it would with a real one\n"; got != want {
		t.Errorf("the floored list reads %q, want %q", got, want)
	}
	for _, c := range []string{"chg_1", "chg_2"} {
		change, err := os.ReadFile(filepath.Join(w.state, "changes", c))
		if err != nil {
			t.Fatal(err)
		}
		if c == "chg_1" && (!strings.Contains(string(change), `"ADMIN_EMAIL":"owner@needs.invalid"`) || strings.Contains(string(change), `"TUNNEL_TOKEN":""`)) {
			t.Errorf("the install was given %s", change)
		}
		// Both releases run the tunnel, and it alone, with no health check.
		if strings.Count(string(change), `\"NONE\"`) != 1 {
			t.Errorf("%s doesn't run the tunnel, and it alone, with no health check: %s", c, change)
		}
	}
}

// A service that stops on a stand-in fails the floor plainly, whatever its exit code:
// a bad token's looks like a broken release's (#745's review, M1).
func TestAServiceThatStopsOnAStandInFailsTheFloor(t *testing.T) {
	w := newWorld(t, map[string][]string{"needs": {needsInputs("1.0.0", "exits"), needsInputs("1.1.0", "exits")}}, nil)
	out, err := w.run("needs")
	if err == nil || !strings.Contains(out, "can't prove tunnel: it stops on a stand-in for a secret it reads") || !strings.Contains(out, "Give checks/needs.sh") {
		t.Errorf("a tunnel that restarts on a stand-in: %v\n%s", err, out)
	}
}

// publicPorts is an entry whose app publishes its page on the server alone and a
// second port, HTTP_PORT, as its file writes it, with no address: on every one. With
// public, the entry declares that port public on purpose (ADR 0101).
func publicPorts(version string, public bool) string {
	return publicPortsWith(version, public, "")
}

// publicPortsWith is publicPorts with a second service's ports, from its own input
// DB_PORT, whose value is HTTP_PORT's.
func publicPortsWith(version string, public bool, dbPorts string) string {
	compose := "services:\n  app:\n    image: example/proxy:" + version + "@sha256:" + strings.Repeat("ab", 32) +
		"\n    ports:\n      - \"127.0.0.1:${PORT}:8080\"\n      - \"${HTTP_PORT}:80\"\n"
	inputs := []map[string]any{
		{"name": "PORT", "type": "INPUT_TYPE_PORT", "defaultValue": "18090"},
		{"name": "HTTP_PORT", "type": "INPUT_TYPE_PORT", "defaultValue": "18091", "public": public},
	}
	if dbPorts != "" {
		compose += "  db:\n    image: example/proxy-db:" + version + "@sha256:" + strings.Repeat("cd", 32) + "\n    ports:\n      - \"" + dbPorts + "\"\n"
		inputs = append(inputs, map[string]any{"name": "DB_PORT", "type": "INPUT_TYPE_PORT", "defaultValue": "18091"})
	}
	raw, _ := json.Marshal(map[string]any{
		"id": "proxy", "name": "proxy", "version": version, "release": 1, "web": map[string]string{"service": "app"},
		"inputs": inputs, "compose": compose,
	})
	return string(raw)
}

// The floor lets a port its entry declares public answer on every address, and only
// that one (ADR 0101; #752, where nginx-proxy-manager's console failed it).
func TestTheFloorAllowsOnlyADeclaredPublicPort(t *testing.T) {
	w := newWorld(t, map[string][]string{"proxy": {publicPorts("1.0.0", true), publicPorts("1.1.0", true)}}, nil)
	if out, err := w.run("proxy"); err != nil || !strings.Contains(out, "catalog-e2e: ok") {
		t.Errorf("a declared public port: %v\n%s", err, out)
	}
	w = newWorld(t, map[string][]string{"proxy": {publicPorts("1.0.0", false), publicPorts("1.1.0", false)}}, nil)
	out, err := w.run("proxy")
	if err == nil || !strings.Contains(out, "is published past this server, where its entry declares only [] public") {
		t.Errorf("a port on every address it doesn't declare public: %v\n%s", err, out)
	}
}

// The floor lets a public port past the server only on the service its entry
// declares it on: another service with the same port number on every address fails
// it (#752's review, L4).
func TestTheFloorAllowsAPublicPortOnlyOnItsService(t *testing.T) {
	w := newWorld(t, map[string][]string{"proxy": {publicPortsWith("1.0.0", true, "${DB_PORT}:5432"), publicPortsWith("1.1.0", true, "${DB_PORT}:5432")}}, nil)
	out, err := w.run("proxy")
	if err == nil || !strings.Contains(out, "proxy-production-db-1 is published past this server, where its entry declares only [\"app 18091\"] public") {
		t.Errorf("another service on the public port's number: %v\n%s", err, out)
	}
}

// refusedCheck is a check of needs that gives its inputs, a made-up token among them,
// and proves its tunnel refused, as checks/cloudflared.sh does, with the line its
// harness reads for that, as given.
func refusedCheck(line string) string {
	return `given[needs]="ADMIN_EMAIL=owner@example.com TUNNEL_TOKEN"
given_env[needs]="TUNNEL_TOKEN=made-up"
` + line + `
check_needs() {
  container tunnel >/dev/null
  echo "$1" >>"$FAKE_STATE/../needs-check"
}
`
}

// A check can prove a service its far end refuses, as cloudflared's tunnel on a
// token of no account: it says refused[needs]="tunnel", and the proof runs that
// service with no health check and doesn't hold it to running, as it stops and
// starts again on the refusal. Before and after the upgrade alike, and the check
// runs at each; the entry is floored, as a real token is left unproven. Before, no
// check could: the tunnel failed its health check, and with none, the 120 s hold
// (#745's option 1).
func TestACheckProvesAServiceItsFarEndRefuses(t *testing.T) {
	w := newWorld(t, map[string][]string{"needs": {needsInputs("1.0.0", "refused"), needsInputs("1.1.0", "refused")}},
		map[string]string{"needs": refusedCheck(`refused[needs]="tunnel"`)})
	out, err := w.run("--require-upgrade", "--no-floor", "needs")
	if err != nil {
		t.Fatalf("the harness failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"needs: runs tunnel with no health check and doesn't hold it to running, as its check proves its far end refuses it",
		"needs: upgrades from 1.0.0 to 1.1.0",
		"catalog-e2e: ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the harness never said %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stand-in") {
		t.Errorf("an entry with a check got stand-ins:\n%s", out)
	}
	if got, want := w.read("needs-check"), "first\nupgraded\n"; got != want {
		t.Errorf("the check ran as %q, want %q", got, want)
	}
	// Its check ran, but what the tunnel does with a real token is left unproven, so
	// it doesn't merge by itself.
	if got, want := w.read("floored"), "needs: tunnel's far end refuses it, so what it does with a real value is left unproven\n"; got != want {
		t.Errorf("the floored list reads %q, want %q", got, want)
	}
	if strings.Contains(out, "has no checks/needs.sh") {
		t.Errorf("the run says needs has no check:\n%s", out)
	}
	for _, c := range []string{"chg_1", "chg_2"} {
		change, err := os.ReadFile(filepath.Join(w.state, "changes", c))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(change), `\"NONE\"`) != 1 {
			t.Errorf("%s doesn't run the tunnel, and it alone, with no health check: %s", c, change)
		}
	}
}

// What a check says is refused is held to: a service it doesn't name keeps its health
// check, and a name the entry runs no service of fails the run.
func TestACheckSaysWhatItsFarEndRefuses(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"", "a container runs, but fails its health check"},
		{`refused[needs]="tunel"`, "checks/needs.sh says its far end refuses tunel, and needs runs no tunel"},
	} {
		w := newWorld(t, map[string][]string{"needs": {needsInputs("1.0.0", "refused"), needsInputs("1.1.0", "refused")}},
			map[string]string{"needs": refusedCheck(tc.line)})
		out, err := w.run("--require-upgrade", "--no-floor", "needs")
		if err == nil || !strings.Contains(out, tc.want) {
			t.Errorf("a check that says %q: %v, want it to fail with %q:\n%s", tc.line, err, tc.want, out)
		}
	}
}

// adminCompanion is an entry whose web service is a companion an install leaves out
// unless asked, as mysql's phpMyAdmin is: a database, and an admin tool beside it that
// answers on the server alone, on ADMIN_PORT (ADR 0076).
func adminCompanion(version string) string {
	compose := "services:\n  db:\n    image: example/sql:" + version + "@sha256:" + strings.Repeat("cd", 32) +
		"\n  admin:\n    image: example/sql-admin:" + version + "@sha256:" + strings.Repeat("ab", 32) +
		"\n    ports:\n      - \"127.0.0.1:${ADMIN_PORT}:8080\"\n"
	raw, _ := json.Marshal(map[string]any{
		"id": "sql", "name": "sql", "version": version, "release": 1, "web": map[string]string{"service": "admin"},
		"companions": []map[string]any{{"service": "admin", "label": "Admin", "purpose": "Browse its tables", "inputs": []string{"ADMIN_PORT"}}},
		"inputs":     []map[string]any{{"name": "ADMIN_PORT", "type": "INPUT_TYPE_PORT", "defaultValue": "18090"}},
		"compose":    compose,
	})
	return string(raw)
}

// An entry whose web service is a companion an install leaves out, proven with the
// floor alone, has it turned on, so what the entry opens runs and is proven, its port
// with it. Before, the floor installed the default, which runs no admin, then asked
// for it: "Docker runs ” for phpmyadmin, want one container" (catalog#31, mysql).
func TestTheFloorTurnsOnTheCompanionAnEntryOpens(t *testing.T) {
	w := newWorld(t, map[string][]string{"sql": {adminCompanion("1.0.0"), adminCompanion("1.1.0")}}, nil)
	out, err := w.run("sql")
	if err != nil || !strings.Contains(out, "catalog-e2e: ok") {
		t.Errorf("an entry whose web service is a companion: %v\n%s", err, out)
	}
}
