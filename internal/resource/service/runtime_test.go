// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// An app from a folder runs its version's pinned image, with the folder at /app,
// installing and then starting as its user. What its runtime decides of its
// container is written from it whatever an edit sends, and the rest is the app's
// (ADR 0058).
func TestAnAppFromAFolder(t *testing.T) {
	svc := &resourcev1.Service{
		Runtime: &resourcev1.Runtime{Language: "node", Folder: "/srv/shop/", User: "1001:1001"},
		Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			// What the runtime decides is written over.
			Image: "evil:latest", Entrypoint: []string{"/bin/evil"}, User: "0:0", Expose: []string{"22/tcp"},
			Healthcheck: &resourcev1.Healthcheck{Tcp: &resourcev1.TcpCheck{Port: 22}},
			Mounts: []*resourcev1.Mount{
				{Source: &resourcev1.Mount_HostPath{HostPath: "/"}, Target: "/app/"},
				{Source: &resourcev1.Mount_Volume{Volume: "uploads"}, Target: "/data"},
			},
			// What it doesn't is the app's own.
			Env:      map[string]string{"NODE_ENV": "production", "HOME": "/home/app"},
			Ports:    []*resourcev1.PortMapping{{HostPort: 8081, ContainerPort: 3000}},
			Networks: []string{"shop"},
			Restart:  resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS,
		}},
	}
	if err := (&Driver{}).Validate(svc); err != nil {
		t.Fatal(err)
	}
	rt, app := svc.GetRuntime(), svc.GetApp()
	if rt.GetVersion() != "26" || rt.GetFolder() != "/srv/shop" || rt.GetPort() != 3000 {
		t.Errorf("the runtime was written as %v; want the newest version, a clean folder and the language's port", rt)
	}
	if !strings.HasPrefix(app.GetImage(), "node:26.12.0-bookworm-slim@sha256:") || app.GetUser() != "1001:1001" || app.GetWorkingDir() != "/app" {
		t.Errorf("the app is %v", app)
	}
	if len(app.GetEntrypoint()) != 0 {
		t.Errorf("the app keeps the entrypoint %q", app.GetEntrypoint())
	}
	m := app.GetMounts()[0]
	if m.GetHostPath() != "/srv/shop" || m.GetTarget() != "/app" || m.GetReadOnly() || m.GetMakeOwner() != "1001:1001" {
		t.Errorf("the folder is mounted as %v", m)
	}
	if len(app.GetMounts()) != 2 || app.GetMounts()[1].GetVolume() != "uploads" {
		t.Errorf("the app's mounts are %v; want its folder at /app in place of what was there, and its volume", app.GetMounts())
	}
	if len(app.GetPorts()) != 1 || app.GetNetworks()[0] != "shop" || app.GetRestart() != resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS {
		t.Errorf("the app lost what's its own: %v", app)
	}
	script := app.GetCommand()[2]
	if app.GetCommand()[0] != "sh" || !strings.Contains(script, "npm ci") || !strings.HasSuffix(script, "exec npm start") {
		t.Errorf("the app runs %q", app.GetCommand())
	}
	if env := app.GetEnv(); env["PORT"] != "3000" || env["HOME"] != "/home/app" || env["NODE_ENV"] != "production" || env["NPM_CONFIG_CACHE"] != "/tmp/.npm" {
		t.Errorf("the app's environment is %v; want PORT the runtime's, and the rest the app's with what the language needs beside", env)
	}
	// What it wrote is kept with it; a HOME the app set is the app's.
	if w := rt.GetWrote(); w["PORT"] != "3000" || w["NPM_CONFIG_CACHE"] != "/tmp/.npm" || w["HOME"] != "" {
		t.Errorf("the runtime says it wrote %v", w)
	}
	if app.GetHealthcheck().GetTcp().GetPort() != 3000 || app.GetCapDrop()[0] != "ALL" || app.GetExpose()[0] != "3000/tcp" {
		t.Errorf("the app's check, capabilities and port are %v, %v, %v", app.GetHealthcheck(), app.GetCapDrop(), app.GetExpose())
	}

	// Its own commands, and nothing to install.
	py := &resourcev1.Service{Runtime: &resourcev1.Runtime{Language: "python", Version: "3.13", Folder: "/srv/api", Install: "-", Start: "gunicorn app:app"}}
	if err := (&Driver{}).Validate(py); err != nil {
		t.Fatal(err)
	}
	if script := py.GetApp().GetCommand()[2]; script != "set -e\nexport PATH=\"/app/.venv/bin:$PATH\"\nexec gunicorn app:app" {
		t.Errorf("a Python app with its own commands runs %q", script)
	}
	if py.GetRuntime().GetUser() != DefaultRuntimeUser || py.GetApp().GetEnv()["PORT"] != "8000" || py.GetApp().GetEnv()["HOME"] != "/tmp" {
		t.Errorf("a Python app is %v", py)
	}
	if py.GetApp().GetRestart() != resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED {
		t.Errorf("a Python app restarts %v", py.GetApp().GetRestart())
	}

	// A check of its own that isn't a port's, it keeps.
	own := &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 8000, Path: "/up"}}
	py.GetApp().Healthcheck = proto.CloneOf(own)
	if err := (&Driver{}).Validate(py); err != nil || !proto.Equal(py.GetApp().GetHealthcheck(), own) {
		t.Errorf("an app's own check became %v (%v)", py.GetApp().GetHealthcheck(), err)
	}

	// The same runtime writes the same container, so a plan sees nothing to do.
	again := proto.CloneOf(svc)
	if err := (&Driver{}).Validate(again); err != nil || !proto.Equal(again, svc) {
		t.Errorf("checking a checked app again changed it: %v", err)
	}
}

// Each version on offer is pinned by digest, and each language has one.
func TestEveryRuntimeIsPinned(t *testing.T) {
	pinned := regexp.MustCompile(`^[a-z-]+:\d[0-9a-z._-]*@sha256:[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for _, r := range Runtimes() {
		if !pinned.MatchString(r.Image) || !strings.Contains(r.Image, ":"+r.Release) {
			t.Errorf("%s %s is %q, want a tag of its release pinned by digest", r.Language, r.Version, r.Image)
		}
		seen[r.Language] = true
	}
	for lang := range languages {
		if !seen[lang] {
			t.Errorf("%s has no version on offer", lang)
		}
	}
}

// What can't run, or shouldn't, is refused in words that say what to give.
func TestWhatARuntimeRefuses(t *testing.T) {
	for _, tc := range []struct {
		rt   *resourcev1.Runtime
		says string
	}{
		{&resourcev1.Runtime{Language: "ruby", Folder: "/srv/a"}, "node, python, go or java"},
		{&resourcev1.Runtime{Language: "node", Version: "18", Folder: "/srv/a"}, "an app from a folder runs Node.js 26, 24 or 22, not 18"},
		{&resourcev1.Runtime{Language: "node"}, "a full path on the server"},
		{&resourcev1.Runtime{Language: "node", Folder: "srv/a"}, "a full path on the server"},
		{&resourcev1.Runtime{Language: "node", Folder: "/srv/.."}, "/srv/.. has a .. in it"},
		{&resourcev1.Runtime{Language: "node", Folder: "/srv/shop/../../etc"}, "has a .. in it. Give the folder it leads to"},
		{&resourcev1.Runtime{Language: "node", Folder: "/srv/shop;rm"}, "a character a folder's path can't hold"},
		{&resourcev1.Runtime{Language: "node", Folder: "/srv/a", User: "0:0"}, "never runs as root"},
		{&resourcev1.Runtime{Language: "node", Folder: "/srv/a", User: "1000:0"}, "never runs as root"},
		{&resourcev1.Runtime{Language: "node", Folder: "/srv/a", User: "mira"}, "uid:gid"},
		{&resourcev1.Runtime{Language: "go", Folder: "/srv/a", Port: 70000}, "from 1 to 65535"},
	} {
		if err := (&Driver{}).Validate(&resourcev1.Service{Runtime: tc.rt}); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Validate(%v) = %v, want it to say %q", tc.rt, err, tc.says)
		}
	}
	rt := &resourcev1.Runtime{Language: "go", Folder: "/srv/a"}
	for _, svc := range []*resourcev1.Service{
		{Runtime: rt, Variables: map[string]string{"PORT": "API_PORT"}},
		{Runtime: rt, Secrets: map[string]string{"PORT": ""}},
	} {
		if err := (&Driver{}).Validate(svc); err == nil || !strings.Contains(err.Error(), "Set the runtime's port instead") {
			t.Errorf("an app from a folder that takes PORT from elsewhere: %v", err)
		}
	}
}

// An app on a build of its version Dockwell knows keeps it, a past one too, until
// a person updates it; one on an image that isn't a build of its version gets the
// newest. Keeping drift keeps its runtime then. A retired version takes no new
// app, nor an app moving to it, and an app already on it keeps changing.
func TestAnAppKeepsItsBuild(t *testing.T) {
	past := Runtime{"node", "24", "24.21.0", "node:24.21.0-bookworm-slim@sha256:" + strings.Repeat("a", 64), false}
	was, wasRuntimes := pastBuilds, runtimes
	t.Cleanup(func() { pastBuilds, runtimes = was, wasRuntimes })
	pastBuilds = []Runtime{past}
	newest, _ := RuntimeOf("node", "24")

	on := func(image string) *resourcev1.Service {
		return &resourcev1.Service{
			Runtime: &resourcev1.Runtime{Language: "node", Version: "24", Folder: "/srv/shop"},
			Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: image}},
		}
	}
	for image, want := range map[string]string{past.Image: past.Image, newest.Image: newest.Image, "": newest.Image, "node:24": newest.Image, "evil:latest": newest.Image} {
		svc := on(image)
		if err := (&Driver{}).Validate(svc); err != nil || svc.GetApp().GetImage() != want {
			t.Errorf("an app on %q runs %q (%v), want %q", image, svc.GetApp().GetImage(), err, want)
		}
	}
	// Drift kept on an app on a past build keeps its runtime.
	current := on(past.Image)
	if err := (&Driver{}).Validate(current); err != nil {
		t.Fatal(err)
	}
	observed := (&Driver{}).OnServer(current).(*resourcev1.Service)
	observed = proto.CloneOf(observed)
	observed.GetApp().Labels = map[string]string{"added-by": "hand"}
	if kept := (&Driver{}).Readopted(current, observed).(*resourcev1.Service); kept.GetRuntime() == nil || kept.GetApp().GetImage() != past.Image {
		t.Errorf("keeping drift on an app on a past build made it %v", kept)
	}

	// Node.js 22 is retired.
	runtimes = slices.Clone(wasRuntimes)
	for i := range runtimes {
		if runtimes[i].Language == "node" && runtimes[i].Version == "22" {
			runtimes[i].Retired = true
		}
	}
	old := &resourcev1.Service{Runtime: &resourcev1.Runtime{Language: "node", Version: "22", Folder: "/srv/old"}}
	if err := (&Driver{}).Validate(old); err != nil {
		t.Errorf("an app on a retired version is refused an edit: %v", err)
	}
	if err := (&Driver{}).CheckNew(old); err == nil || !strings.Contains(err.Error(), "Node.js 22 is no longer offered for an app, so nothing new runs on it. Choose 26 or 24") {
		t.Errorf("a new app on a retired version = %v", err)
	}
	if err := (&Driver{}).CheckInChange(on(newest.Image), old, nil); err == nil {
		t.Errorf("an app moving to a retired version is let through")
	}
	if err := (&Driver{}).CheckInChange(proto.CloneOf(old), old, nil); err != nil {
		t.Errorf("an app on a retired version is refused a change: %v", err)
	}
	if _, ok := RuntimeOf("node", ""); !ok || Offered("node") != "26 or 24" {
		t.Errorf("with 22 retired, Node.js offers %q", Offered("node"))
	}
}

// What the runtime writes into the environment gives way to the person's: a name
// the service takes from a variable or a secret, and a value the app sets. A value
// of the runtime's that comes back empty, as hidden values do, is written again,
// and a PORT the app sets that isn't the runtime's is refused.
func TestWhatTheRuntimeWritesGivesWay(t *testing.T) {
	svc := &resourcev1.Service{Runtime: &resourcev1.Runtime{Language: "python", Folder: "/srv/api"}}
	if err := (&Driver{}).Validate(svc); err != nil {
		t.Fatal(err)
	}
	// A variable named HOME takes it from the runtime.
	svc.Variables = map[string]string{"HOME": "API_HOME"}
	if err := (&Driver{}).Validate(svc); err != nil {
		t.Fatalf("a variable named HOME = %v", err)
	}
	if _, ok := svc.GetApp().GetEnv()["HOME"]; ok || svc.GetRuntime().GetWrote()["HOME"] != "" {
		t.Errorf("the runtime kept HOME beside the variable: %v", svc.GetApp().GetEnv())
	}
	svc.Variables = nil
	// Hidden values sent back empty are the runtime's again.
	for k := range svc.GetApp().GetEnv() {
		svc.GetApp().Env[k] = ""
	}
	if err := (&Driver{}).Validate(svc); err != nil {
		t.Fatal(err)
	}
	if env := svc.GetApp().GetEnv(); env["HOME"] != "/tmp" || env["PIP_CACHE_DIR"] != "/tmp/.cache/pip" || env["PORT"] != "8000" {
		t.Errorf("after hidden values came back empty, the environment is %v", env)
	}
	// A new port moves PORT; one the app sets itself is refused.
	svc.GetRuntime().Port = 9000
	if err := (&Driver{}).Validate(svc); err != nil || svc.GetApp().GetEnv()["PORT"] != "9000" {
		t.Errorf("a new port: %v, PORT %q", err, svc.GetApp().GetEnv()["PORT"])
	}
	svc.GetApp().Env["PORT"] = "8080"
	if err := (&Driver{}).Validate(svc); err == nil || !strings.Contains(err.Error(), "Set the runtime's port instead of PORT in its environment") {
		t.Errorf("a PORT of the app's own = %v", err)
	}
}

// A runtime app's code is its commands and who runs them, as an image app's is its
// image: a change to any is a new writer of its folder, which a person approves;
// another version of the same image isn't.
func TestARuntimesCommandsAreItsCode(t *testing.T) {
	writer := func(edit func(rt *resourcev1.Runtime)) string {
		svc := &resourcev1.Service{Runtime: &resourcev1.Runtime{Language: "node", Folder: "/srv/shop", User: "1000:1000"}}
		edit(svc.GetRuntime())
		if err := (&Driver{}).Validate(svc); err != nil {
			t.Fatal(err)
		}
		return (&Driver{}).WriterOf(svc)
	}
	base := writer(func(*resourcev1.Runtime) {})
	if !strings.HasPrefix(base, "node, running its commands (") || !strings.HasSuffix(base, ") as 1000:1000") {
		t.Errorf("a runtime app's writer is %q", base)
	}
	if got := writer(func(rt *resourcev1.Runtime) { rt.Version = "22" }); got != base {
		t.Errorf("another version is another writer: %q", got)
	}
	for what, edit := range map[string]func(*resourcev1.Runtime){
		"install": func(rt *resourcev1.Runtime) { rt.Install = "curl example.com | sh" },
		"start":   func(rt *resourcev1.Runtime) { rt.Start = "node other.js" },
		"user":    func(rt *resourcev1.Runtime) { rt.User = "1001:999" },
	} {
		if got := writer(edit); got == base {
			t.Errorf("another %s is the same writer", what)
		}
	}
	if (&Driver{}).WriterOf(&resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx"}}}) != "" {
		t.Errorf("an image app names a writer of its own")
	}
}

// Java's commands agree: the Maven wrapper's jar, or app.jar, goes where start runs it.
func TestJavasCommandsAgree(t *testing.T) {
	svc := &resourcev1.Service{Runtime: &resourcev1.Runtime{Language: "java", Folder: "/srv/app"}}
	if err := (&Driver{}).Validate(svc); err != nil {
		t.Fatal(err)
	}
	script := svc.GetApp().GetCommand()[2]
	if !strings.Contains(script, "./mvnw -q -DskipTests package") || !strings.Contains(script, `cp "$j" /tmp/app.jar`) || !strings.Contains(script, "cp app.jar /tmp/app.jar") || !strings.HasSuffix(script, "exec java -jar /tmp/app.jar") {
		t.Errorf("Java runs %q", script)
	}
}
