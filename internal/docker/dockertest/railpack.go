// SPDX-License-Identifier: AGPL-3.0-only

package dockertest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// RailpackRepository is Railpack's frontend image, whose containers the fake engine
// runs as internal/build does: one copies the railpack binary into a volume.
const RailpackRepository = "ghcr.io/railwayapp/railpack-frontend"

// railpackBin finds the path a builder's script runs railpack from, which names
// its version.
var railpackBin = regexp.MustCompile(`/tmp/railpack/bin/railpack-[0-9A-Za-z.\-]+`)

// RailpackCopies counts the containers that copied Railpack into its volume.
func (e *Engine) RailpackCopies() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.railpackCopies
}

// fakeCopyRailpack stands in for a container of Railpack's image copying its
// binary into the volume mounted at /tmp/railpack, where the script says.
func (e *Engine) fakeCopyRailpack(c *container, stderr io.Writer) int {
	script := strings.Join(c.Config.Cmd, " ")
	bin := railpackBin.FindString(script)
	e.mu.Lock()
	dir, ok := e.mountsOf(c)["/tmp/railpack"]
	e.railpackCopies++
	e.mu.Unlock()
	if bin == "" || !ok {
		_, _ = fmt.Fprintln(stderr, "cp: can't create the binary: no volume at /tmp/railpack")
		return 1
	}
	p := filepath.Join(dir, strings.TrimPrefix(bin, "/tmp/railpack"))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err := os.WriteFile(p, []byte("railpack"), 0o750); err != nil { //nolint:gosec // a stand-in for a binary
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// hasRailpack reports whether the volume a builder mounts at /tmp/railpack has the
// binary its script runs.
func (e *Engine) hasRailpack(c *container) bool {
	bin := railpackBin.FindString(strings.Join(c.Config.Cmd, " "))
	e.mu.Lock()
	dir, ok := e.mountsOf(c)["/tmp/railpack"]
	e.mu.Unlock()
	if bin == "" || !ok {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, strings.TrimPrefix(bin, "/tmp/railpack")))
	return err == nil
}

// fakeDetect stands in for a detect container: it checks out the commit, and says
// whether it has its Dockerfile, with its FROM lines, or what Railpack makes of it.
func (e *Engine) fakeDetect(c *container, stderr io.Writer) int {
	envs := envOf(c)
	say := func(s string) { _, _ = fmt.Fprintf(stderr, "dockwell:%s\n", s) }
	co, code := checkout(envs, stderr)
	if code != 0 {
		return code
	}
	defer os.RemoveAll(co.src)
	if envs["DW_BUILDER"] == "dockerfile" && co.dockerfile == "" {
		say("fail dockerfile")
		return 3
	}
	if co.dockerfile != "" && envs["DW_BUILDER"] != "railpack" {
		say("dockerfile yes")
		b, _ := os.ReadFile(co.dockerfile)
		var from []string
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) > 0 && strings.EqualFold(f[0], "FROM") {
				from = append(from, l)
			}
		}
		say("from " + base64.StdEncoding.EncodeToString([]byte(strings.Join(from, "\n")+"\n")))
		return 0
	}
	if !e.hasRailpack(c) {
		say("fail need-railpack")
		return 4
	}
	vars := map[string]string{}
	args := c.Config.Cmd
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" {
			k, v, _ := strings.Cut(args[i+1], "=")
			vars[k] = v
			i++
		}
	}
	rp := fakePrepare(co.context, envs["DW_BUILD_CMD"], envs["DW_START_CMD"], vars)
	say("railpack-info " + base64.StdEncoding.EncodeToString(rp.info))
	if rp.plan != nil {
		say("railpack-plan " + base64.StdEncoding.EncodeToString(rp.plan))
	}
	return 0
}

// prepared is what the stand-in for railpack prepare wrote, and the steps a build
// of it runs.
type prepared struct {
	ok         bool
	info, plan []byte
	pretty     []string
	steps      []step
	start      string
	env        []string
}

// fakePrepare stands in for railpack prepare, for what tests build: a Node app, by
// its package.json; a Python app, by its requirements.txt; and a static site, by
// its index.html. It writes what Railpack would, in the shape it writes it.
func fakePrepare(dir, buildCmd, startCmd string, vars map[string]string) prepared {
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	type pkg struct{ Name, RequestedVersion, ResolvedVersion, Source string }
	var (
		providers []string
		packages  = map[string]pkg{}
		logs      []map[string]string
		steps     []map[string]any
		run       []step
		start     string
	)
	warn := func(m string) { logs = append(logs, map[string]string{"Level": "warn", "Msg": m}) }
	info := func(m string) { logs = append(logs, map[string]string{"Level": "info", "Msg": m}) }
	command := func(name string, cmds ...string) {
		var cs []map[string]string
		for _, c := range cmds {
			cs = append(cs, map[string]string{"cmd": c})
		}
		steps = append(steps, map[string]any{"name": name, "commands": cs})
	}
	mise := func(tool, requested, resolved, source string) {
		packages[tool] = pkg{tool, requested, resolved, source}
		steps = append(steps, map[string]any{"name": "packages:mise", "commands": []map[string]string{{"cmd": "mise install", "customName": "install mise packages: " + tool}}})
		run = append(run, step{instr: "FILE", line: "create mise config"}, step{instr: "RUN", args: "mise install", line: "install mise packages: " + tool})
	}
	switch {
	case exists("package.json"):
		var p struct {
			Main    string            `json:"main"`
			Scripts map[string]string `json:"scripts"`
			Engines map[string]string `json:"engines"`
		}
		b, _ := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // a test's own checkout
		_ = json.Unmarshal(b, &p)
		providers = []string{"node"}
		requested, source := p.Engines["node"], "package.json > engines > node"
		if v := vars["RAILPACK_NODE_VERSION"]; v != "" {
			requested, source = v, "RAILPACK_NODE_VERSION"
		}
		if requested == "" {
			source = "railpack default"
		}
		major := strings.TrimLeft(requested, "^~>=v ")
		if i := strings.IndexAny(major, ". "); i >= 0 {
			major = major[:i]
		}
		if major == "" {
			major = "22"
		}
		pm, install := "npm", "npm install"
		switch {
		case exists("package-lock.json"):
			install = "npm ci"
		case exists("pnpm-lock.yaml"):
			pm, install = "pnpm", "pnpm install --frozen-lockfile"
		case exists("yarn.lock"):
			pm, install = "yarn", "yarn install --frozen-lockfile"
		default:
			warn("No node package manager detected, using npm")
		}
		info("Detected Node")
		info("Using " + pm + " package manager")
		mise("node", requested, major+".23.4", source)
		command("install", install)
		run = append(run, step{instr: "COPY", line: "copy package.json"}, step{instr: "RUN", args: "echo \"added 0 packages, and audited 1 package in 1s\"", line: install})
		script := func(name string) string { return p.Scripts[name] }
		build := buildCmd
		if build == "" && script("build") != "" {
			build = pm + " run build"
		}
		run = append(run, step{instr: "COPY", line: "loading ."})
		if build != "" {
			command("build", build)
			words := build
			if s, ok := strings.CutPrefix(build, pm+" run "); ok && script(s) != "" {
				words = script(s)
			}
			run = append(run, step{instr: "RUN", args: words, line: build})
		}
		switch {
		case startCmd != "":
			start = startCmd
		case script("start") != "":
			start = pm + " run start"
		case p.Main != "":
			start = "node " + p.Main
		case exists("index.js"):
			start = "node index.js"
		}
	case exists("requirements.txt"):
		providers = []string{"python"}
		info("Detected Python")
		mise("python", "", "3.13.9", "railpack default")
		command("install", "pip install -r requirements.txt")
		run = append(run, step{instr: "COPY", line: "copy requirements.txt"}, step{instr: "RUN", args: "echo \"Successfully installed\"", line: "pip install -r requirements.txt"}, step{instr: "COPY", line: "loading ."})
		if buildCmd != "" {
			command("build", buildCmd)
			run = append(run, step{instr: "RUN", args: buildCmd, line: buildCmd})
		}
		switch {
		case startCmd != "":
			start = startCmd
		case exists("main.py"):
			start = "python main.py"
		case exists("app.py"):
			start = "python app.py"
		}
	case exists("index.html"):
		providers = []string{"staticfile"}
		info("Detected Staticfile")
		mise("caddy", "", "2.10.2", "railpack default")
		run = append(run, step{instr: "COPY", line: "loading ."})
		start = cmpOr(startCmd, "caddy run --config /Caddyfile --adapter caddyfile 2>&1")
	default:
		names := []string{}
		entries, _ := os.ReadDir(dir)
		for _, en := range entries {
			if en.Name() != ".git" {
				names = append(names, "./"+en.Name())
			}
		}
		slices.Sort(names)
		logs = append(logs, map[string]string{"Level": "error", "Msg": "Railpack could not determine how to build the app.\n\nThe following languages are supported:\n- Node\n- Python\n- Staticfile\n\nThe app contents that Railpack analyzed contains:\n\n" + strings.Join(append([]string{"./"}, names...), "\n")})
		out, _ := json.Marshal(map[string]any{"railpackVersion": "0.43.0", "logs": logs, "success": false})
		return prepared{info: out, pretty: []string{"✖ Railpack could not determine how to build the app."}}
	}
	if start == "" {
		warn("No start command was found")
	}
	resolved := map[string]any{}
	for k, p := range packages {
		resolved[k] = map[string]string{"name": p.Name, "requestedVersion": p.RequestedVersion, "resolvedVersion": p.ResolvedVersion, "source": p.Source}
	}
	var secrets []string
	for k := range vars {
		secrets = append(secrets, k)
	}
	slices.Sort(secrets)
	infoJSON, _ := json.Marshal(map[string]any{
		"railpackVersion": "0.43.0", "resolvedPackages": resolved, "detectedProviders": providers,
		"metadata": map[string]string{"providers": strings.Join(providers, ",")}, "logs": logs, "success": true,
	})
	deployVars := map[string]string{"RAILPACK_VERSION": "0.43.0"}
	planJSON, _ := json.Marshal(map[string]any{
		"steps": steps, "secrets": secrets,
		"deploy": map[string]any{"startCommand": start, "variables": deployVars},
	})
	pretty := []string{"╭─────────────────╮", "│ Railpack 0.43.0 │", "╰─────────────────╯", ""}
	for _, l := range logs {
		pretty = append(pretty, "  ↳ "+l["Msg"])
	}
	pretty = append(pretty, "", "  Packages", "  ──────────")
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range packages {
			if !yield(k) {
				return
			}
		}
	}) {
		p := packages[k]
		pretty = append(pretty, fmt.Sprintf("  %s  │  %s  │  %s", p.Name, p.ResolvedVersion, p.Source))
	}
	if start != "" {
		pretty = append(pretty, "", "  Deploy", "  ──────────", "    $ "+start)
	}
	// The image keeps the plan's variables, and none of the build's secrets.
	return prepared{ok: true, info: infoJSON, plan: planJSON, pretty: pretty, steps: run, start: start, env: []string{"RAILPACK_VERSION=0.43.0"}}
}

func cmpOr(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// envOf is a container's environment, by name.
func envOf(c *container) map[string]string {
	envs := map[string]string{}
	for _, kv := range c.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		envs[k] = v
	}
	return envs
}
