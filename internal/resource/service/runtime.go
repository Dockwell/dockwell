// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/dockwell/dockwell/internal/lazyre"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Runtime is one version of a language an app from a folder runs on: its
// official image, pinned by tag and by the digest of its multi-platform index.
type Runtime struct {
	Language string
	Version  string
	// Release is the version it is exactly, such as "24.22.0".
	Release string
	Image   string
	// Retired says Dockwell no longer offers the version: an app on it keeps
	// running, and changing, and no new app runs on it.
	Retired bool
}

// Language is how a language runs an app from a folder, unless the app says.
type Language struct {
	// Name is what people call it, such as "Node.js".
	Name string
	// Install and Start are its commands, run in /app with sh.
	Install, Start string
	// Port is what it listens on unless the app says.
	Port uint32
	// Env is what it's given, beside PORT and HOME: where its tools keep caches.
	Env map[string]string
	// Path is put first on the PATH before the app starts, where it installs.
	Path string
}

// EnvNames are what the language gives an app in its environment, beside what the
// app sets, sorted: PORT, HOME and where its tools keep caches.
func (l Language) EnvNames() []string {
	names := []string{"HOME", "PORT"}
	for k := range l.Env {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

// languages are the languages an app from a folder runs on (ADR 0058).
var languages = map[string]Language{
	"node": {
		Name:    "Node.js",
		Install: "if [ -f package-lock.json ]; then npm ci; else npm install; fi",
		Start:   "npm start",
		Port:    3000,
		Env:     map[string]string{"NPM_CONFIG_CACHE": "/tmp/.npm", "NPM_CONFIG_UPDATE_NOTIFIER": "false"},
	},
	"python": {
		Name:    "Python",
		Install: "python -m venv .venv && if [ -f requirements.txt ]; then .venv/bin/pip install -q -r requirements.txt; fi",
		Start:   "python app.py",
		Port:    8000,
		Env:     map[string]string{"PIP_CACHE_DIR": "/tmp/.cache/pip", "PYTHONUNBUFFERED": "1"},
		Path:    "/app/.venv/bin",
	},
	"go": {
		Name:    "Go",
		Install: "go build -o /tmp/app .",
		Start:   "/tmp/app",
		Port:    8080,
		Env:     map[string]string{"GOCACHE": "/tmp/.cache/go-build", "GOPATH": "/tmp/go"},
	},
	"java": {
		Name: "Java",
		// The Maven wrapper's package goes in target, named for the project: the
		// jar that isn't Spring Boot's -plain one is the app. Without a wrapper,
		// the app is app.jar.
		Install: `if [ -x mvnw ]; then ./mvnw -q -DskipTests package; for j in target/*.jar; do case "$j" in *-plain.jar) ;; *) cp "$j" /tmp/app.jar ;; esac; done; else cp app.jar /tmp/app.jar; fi`,
		Start:   "java -jar /tmp/app.jar",
		Port:    8080,
	},
}

// runtimes are the newest build of each version Dockwell knows, newest first for
// each language. Pin a new set to releases at least 14 days old, by the digest of
// each tag's multi-platform index. Moving a version's pin puts the one it moved
// from in pastBuilds; a version no longer offered stays, Retired.
var runtimes = []Runtime{
	{"node", "26", "26.12.0", "node:26.12.0-bookworm-slim@sha256:742a0fe7320e09e0321222b68165e6ac731d30470e7ea7f98f9bfc3a6e1034fe", false},
	{"node", "24", "24.22.0", "node:24.22.0-bookworm-slim@sha256:91acba91b842bec0bf7ea6f5e09c644c7ccad72c4996ed9616d861d1c7cd60d4", false},
	{"node", "22", "22.23.3", "node:22.23.3-bookworm-slim@sha256:3ed98443660ff48413b5af19ecefce3b9d50845a901a43420d47cc63a9fddbe9", false},
	{"python", "3.15", "3.15.1", "python:3.15.1-slim-bookworm@sha256:575f8bc12ec26542e0678bc6b60e385218248019d433157ec327264a78c3a853", false},
	{"python", "3.14", "3.14.9", "python:3.14.9-slim-bookworm@sha256:945c2676dc2b18e0c1f16879244122c57c0ffede60006bf3df159c507a31bcae", false},
	{"python", "3.13", "3.13.17", "python:3.13.17-slim-bookworm@sha256:e32be0e55814a08977501ca863271d9bd02443710db8f2afaf8372e2c4508530", false},
	{"go", "1.27", "1.27.2", "golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61", false},
	{"go", "1.26", "1.26.9", "golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c", false},
	{"java", "25", "25.0.4", "eclipse-temurin:25.0.4_7-jdk-noble@sha256:2feab631bffce6236d8bb5261a4abe19a8d6f85bad1c01166f74686c983d011f", false},
	{"java", "21", "21.0.12", "eclipse-temurin:21.0.12_8-jdk-noble@sha256:4d271cd5e0624598cf563342f47281b09cb364bc13acbbd7251f49f83470018d", false},
	{"java", "17", "17.0.20", "eclipse-temurin:17.0.20_8-jdk-noble@sha256:c2c0deda43a14dea9d5c6c5349d22538c385c7d678d03dc6a133fe0656ed0d64", false},
}

// pastBuilds are builds of a version Dockwell offered before its newest, by
// digest: an app on one keeps it until a person updates it (ADR 0058). None yet.
var pastBuilds []Runtime

// DefaultRuntimeUser is who an app from a folder runs as unless it says: the
// first user a server's installer makes.
const DefaultRuntimeUser = "1000:1000"

// Runtimes are the versions Dockwell knows, the newest build of each, newest
// first for each language, retired ones among them.
func Runtimes() []Runtime { return slices.Clone(runtimes) }

// LanguageOf is how a language runs an app, and whether Dockwell offers it.
func LanguageOf(language string) (Language, bool) {
	l, ok := languages[language]
	return l, ok
}

// RuntimeOf is the newest build of a language's version on offer, or of its
// newest version for "". A retired version isn't on offer.
func RuntimeOf(language, version string) (Runtime, bool) {
	for _, r := range runtimes {
		if r.Language == language && !r.Retired && (version == "" || r.Version == version) {
			return r, true
		}
	}
	return Runtime{}, false
}

// knownRuntime is the newest build of a version Dockwell knows, retired or not.
func knownRuntime(language, version string) (Runtime, bool) {
	for _, r := range runtimes {
		if r.Language == language && r.Version == version {
			return r, true
		}
	}
	return Runtime{}, false
}

// Retired reports a version Dockwell no longer offers a new app.
func Retired(language, version string) bool {
	r, ok := knownRuntime(language, version)
	return ok && r.Retired
}

// KnownBuild reports an image that's a build of a language's version Dockwell
// offers or offered: the newest, or one before it.
func KnownBuild(language, version, image string) bool {
	for _, r := range slices.Concat(runtimes, pastBuilds) {
		if r.Language == language && r.Version == version && r.Image == image {
			return true
		}
	}
	return false
}

// Offered says the versions of a language on offer, newest first, such as "26,
// 24 or 22".
func Offered(language string) string {
	var offered []string
	for _, r := range runtimes {
		if r.Language == language && !r.Retired {
			offered = append(offered, r.Version)
		}
	}
	if len(offered) < 2 {
		return strings.Join(offered, "")
	}
	return strings.Join(offered[:len(offered)-1], ", ") + " or " + offered[len(offered)-1]
}

// folderRE is what a folder's path may hold, as it goes into a mount and a plan,
// as a website's does.
var folderRE = lazyre.Regexp(`^(/[A-Za-z0-9._@+~-]+)+/?$`)

// checkRuntime checks what an app from a folder says, and writes what it leaves
// to Dockwell: its version, port and user. A retired version passes, as an app on
// it keeps changing; a new app on one is refused when it's planned (CheckNew).
func checkRuntime(rt *resourcev1.Runtime) error {
	lang, ok := languages[rt.GetLanguage()]
	if !ok {
		return errors.New("a runtime's language is node, python, go or java")
	}
	r, ok := RuntimeOf(rt.GetLanguage(), rt.GetVersion())
	if !ok && rt.GetVersion() != "" {
		r, ok = knownRuntime(rt.GetLanguage(), rt.GetVersion())
	}
	if !ok {
		return fmt.Errorf("an app from a folder runs %s %s, not %s", lang.Name, Offered(rt.GetLanguage()), rt.GetVersion())
	}
	rt.Version = r.Version
	folder := strings.TrimSpace(rt.GetFolder())
	switch {
	case folder == "" || !path.IsAbs(folder):
		return errors.New("a runtime's folder is a full path on the server, such as /srv/shop")
	case !folderRE().MatchString(folder):
		return fmt.Errorf("%q has a character a folder's path can't hold here. Use letters, digits, -, ., _, @, +, ~ and /", folder)
	}
	for _, part := range strings.Split(strings.Trim(folder, "/"), "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("%s has a %s in it. Give the folder it leads to", folder, part)
		}
	}
	if folder = path.Clean(folder); folder == "/" {
		return errors.New("a runtime's folder can't be the server's root. Use a folder of its own, such as /srv/shop")
	}
	rt.Folder = folder
	if rt.GetPort() == 0 {
		rt.Port = lang.Port
	}
	if rt.GetPort() > 65535 {
		return errors.New("a runtime's port is from 1 to 65535, such as 3000")
	}
	if strings.TrimSpace(rt.GetUser()) == "" {
		rt.User = DefaultRuntimeUser
	}
	uid, gid, err := resource.ParseOwner(rt.GetUser())
	if err != nil {
		return fmt.Errorf("a runtime's user is uid:gid, such as 1000:1000: %w", err)
	}
	if uid == 0 || gid == 0 {
		return errors.New("an app from a folder never runs as root, which could change anything on the server through it. Use the folder owner's uid:gid, such as 1000:1000")
	}
	rt.User = resource.FormatOwner(uid, gid)
	for _, cmd := range []string{rt.GetInstall(), rt.GetStart()} {
		if strings.ContainsRune(cmd, 0) || len(cmd) > 4096 {
			return errors.New("a runtime's commands are text, of up to 4096 characters")
		}
	}
	return nil
}

// writeEnv gives the app PORT, HOME and where its language's tools keep caches,
// and keeps in the runtime what it wrote. What's the person's stays theirs: a
// value the app sets itself, and a name the service takes from a variable or a
// secret, where the runtime's own value gives way. A value of the runtime's that
// comes back empty, as hidden values do in a whole spec sent back, is written
// again. PORT is the runtime's alone: one the app sets that isn't what the runtime
// wrote is refused, as one from a variable is.
func writeEnv(rt *resourcev1.Runtime, app *resourcev1.Container, lang Language, port string, taken func(string) bool) error {
	if app.Env == nil {
		app.Env = map[string]string{}
	}
	prev := rt.GetWrote()
	if v, ok := app.Env["PORT"]; ok && v != "" && v != port && v != prev["PORT"] {
		return fmt.Errorf("a runtime gives its app PORT itself, %s. Set the runtime's port instead of PORT in its environment", port)
	}
	app.Env["PORT"] = port
	wrote := map[string]string{"PORT": port}
	defaults := map[string]string{"HOME": "/tmp"}
	for k, v := range lang.Env {
		defaults[k] = v
	}
	for k, v := range defaults {
		cur, has := app.Env[k]
		was, ours := prev[k]
		ours = ours && cur == was
		if taken != nil && taken(k) {
			if has && (ours || cur == "" || cur == v) {
				delete(app.Env, k)
			}
			continue
		}
		if !has || cur == "" || ours {
			app.Env[k] = v
			wrote[k] = v
		}
	}
	rt.Wrote = wrote
	return nil
}

// WriteRuntime writes the parts of an app's container its runtime decides: its
// version's image, the folder at /app, installing, then starting, as its user,
// with PORT, the port exposed and a check on it, and every capability dropped
// (ADR 0058). It writes them each time, so the two never disagree. An app on a
// build of its version Dockwell knows keeps that build, until a person updates it
// (ChangeServiceRuntime) or changes its version. The rest of the container, such as
// its environment, ports and networks, it leaves as the app has it, with only what
// a language needs added where the app sets nothing, and taken says the names the
// service takes from variables and secrets, which it leaves to them. It returns
// app, or a new container when that's nil.
func WriteRuntime(rt *resourcev1.Runtime, app *resourcev1.Container, taken func(name string) bool) (*resourcev1.Container, error) {
	if err := checkRuntime(rt); err != nil {
		return nil, err
	}
	if app == nil {
		app = &resourcev1.Container{}
	}
	lang := languages[rt.GetLanguage()]
	r, _ := knownRuntime(rt.GetLanguage(), rt.GetVersion())
	install := rt.GetInstall()
	switch strings.TrimSpace(install) {
	case "":
		install = lang.Install
	case "-":
		install = ""
	}
	start := rt.GetStart()
	if strings.TrimSpace(start) == "" {
		start = lang.Start
	}
	script := []string{"set -e"}
	if lang.Path != "" {
		script = append(script, `export PATH="`+lang.Path+`:$PATH"`)
	}
	if install != "" {
		script = append(script, install)
	}
	script = append(script, "exec "+start)
	port := strconv.FormatUint(uint64(rt.GetPort()), 10)

	if !KnownBuild(rt.GetLanguage(), rt.GetVersion(), app.GetImage()) {
		app.Image = r.Image
	}
	app.Entrypoint, app.NoEntrypoint = nil, false
	app.Command = []string{"sh", "-c", strings.Join(script, "\n")}
	app.WorkingDir = "/app"
	app.User = rt.GetUser()
	if err := writeEnv(rt, app, lang, port, taken); err != nil {
		return nil, err
	}
	folder := &resourcev1.Mount{
		Source: &resourcev1.Mount_HostPath{HostPath: rt.GetFolder()}, Target: "/app", MakeOwner: rt.GetUser(),
	}
	app.Mounts = slices.DeleteFunc(app.Mounts, func(m *resourcev1.Mount) bool { return path.Clean(m.GetTarget()) == "/app" })
	app.Mounts = append([]*resourcev1.Mount{folder}, app.Mounts...)
	app.Expose = []string{port + "/tcp"}
	if app.GetRestart() == resourcev1.RestartPolicy_RESTART_POLICY_UNSPECIFIED {
		app.Restart = resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED
	}
	app.CapDrop = []string{"ALL"}
	// Installing at each start can take minutes before it listens. A check of
	// another kind that the app sets, it keeps.
	if hc := app.GetHealthcheck(); hc == nil || hc.GetTcp() != nil {
		app.Healthcheck = &resourcev1.Healthcheck{Tcp: &resourcev1.TcpCheck{Port: rt.GetPort()}, StartPeriod: "10m"}
	}
	return app, nil
}
