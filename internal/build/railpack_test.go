// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// nodeApp is a Node app with no Dockerfile, as Railpack builds one.
var nodeApp = map[string]string{
	"package.json": `{"name":"storefront","scripts":{"build":"echo built the storefront","start":"node server.js"},"engines":{"node":"22"}}`,
	"server.js":    `require("http").createServer((q, s) => s.end("ok")).listen(process.env.PORT || 3000)`,
}

// withRailpack has the bench's registry serve Railpack's image too.
func withRailpack(b *bench) *bench {
	b.engine.Registry[RailpackFrontend] = dockertest.Image{}
	return b
}

// made is what was asked of each container of BuildKit's image that ran what, such
// as "dockwell-build" or "dockwell-detect".
func (b *bench) made(what string) []docker.CreateRequest {
	var out []docker.CreateRequest
	all := b.engine.Builders()
	for i := range all {
		if slices.Contains(all[i].Cmd, what) {
			out = append(out, all[i])
		}
	}
	return out
}

func TestAnAppWithNoDockerfileBuildsWithRailpack(t *testing.T) {
	b := withRailpack(newBench(t))
	commit := b.repo.Commit("main", "Add order export endpoint", nodeApp)
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	img, err := b.engine.Client().InspectImage(t.Context(), req.Image)
	if err != nil {
		t.Fatalf("Docker hasn't the image the build made: %v", err)
	}
	if !slices.Equal(img.Config.Entrypoint, []string{"/bin/bash", "-c"}) || !slices.Equal(img.Config.Cmd, []string{"npm run start"}) {
		t.Errorf("the image runs %v %v, not the start command Railpack found", img.Config.Entrypoint, img.Config.Cmd)
	}
	f := b.final()
	if f.State != resource.BuildDone {
		t.Fatalf("the build ended as %+v, not done", f)
	}
	// create mise config, install mise packages: node, copy package.json, npm
	// install and npm run build; none of BuildKit's own.
	if f.Steps != 5 {
		t.Errorf("the build says %d steps, not Railpack's 5", f.Steps)
	}
	d := f.Detection
	if d == nil || d.Builder != resource.BuilderRailpack || !slices.Equal(d.Providers, []string{"node"}) || d.Start != "npm run start" {
		t.Fatalf("the build found %+v, not Railpack's Node app that starts with npm run start", d)
	}
	if len(d.Packages) != 1 || d.Packages[0] != (resource.DetectedPackage{Name: "node", Requested: "22", Resolved: "22.23.4", Source: "package.json > engines > node"}) {
		t.Errorf("the build found the packages %+v, not Node 22 from package.json", d.Packages)
	}
	if !slices.Contains(b.lines(), "built the storefront") || !slices.Contains(b.lines(), "Railpack 0.43.0 read it: node") {
		t.Errorf("the log doesn't have what Railpack found and what the build printed: %q", b.lines())
	}
	// The builder, privileged, builds the plan the reading wrote, read-only, and
	// never mounts what Railpack runs from (the fake engine fails one that does).
	builders := b.made("dockwell-build")
	if len(builders) != 1 {
		t.Fatalf("%d builders ran", len(builders))
	}
	if slices.ContainsFunc(builders[0].HostConfig.Mounts, func(m docker.Mount) bool { return m.Source == RailpackVolume }) ||
		!slices.ContainsFunc(builders[0].HostConfig.Mounts, func(m docker.Mount) bool { return m.Target == planMount && m.ReadOnly }) {
		t.Errorf("the builder mounts %+v", builders[0].HostConfig.Mounts)
	}
	// The reading ran unprivileged, with Railpack's volume read-only, and all it
	// wrote in memory.
	// The first on a server reads twice: once to find there's no Dockerfile, and
	// again once Railpack is there.
	readings := b.made("dockwell-detect")
	if len(readings) == 0 || len(readings) > 2 {
		t.Fatalf("%d readings ran before the build", len(readings))
	}
	r := readings[len(readings)-1].HostConfig
	if r.Privileged || !slices.ContainsFunc(r.Mounts, func(m docker.Mount) bool { return m.Source == RailpackVolume && m.Target == railpackRO && m.ReadOnly }) ||
		r.Tmpfs[sourceDir] == "" || r.Tmpfs["/tmp"] == "" || r.Tmpfs["/work"] == "" {
		t.Errorf("the reading ran with %+v", r)
	}
	// Railpack came once, in a container of its own, and mise once, in another.
	if n := b.engine.RailpackCopies(); n != 1 {
		t.Errorf("Railpack was copied into its volume %d times, not once", n)
	}
	if n := b.engine.MiseSeeds(); n != 1 {
		t.Errorf("mise was fetched into Railpack's volume %d times, not once", n)
	}
	// A Railpack build's cache is keyed by the service, under the server's key, so
	// no Dockerfile can name it.
	key := ""
	for _, kv := range builders[0].Env {
		if v, ok := strings.CutPrefix(kv, "DW_CACHE_KEY="); ok {
			key = v
		}
	}
	if len(key) != 32 || strings.Contains(req.Image, key) || key != b.builder.cacheKey(req) {
		t.Errorf("the build's cache key is %q", key)
	}

	// The next build has it already.
	next := b.repo.Commit("main", "Batch inventory sync", map[string]string{"server.js": "// next"})
	if err := b.builder.Build(b.ctx(), b.request(next, "chg_01KBZ00000000000000000000B")); err != nil {
		t.Fatal(err)
	}
	if n := b.engine.RailpackCopies(); n != 1 || b.engine.MiseSeeds() != 1 {
		t.Errorf("a second build copied Railpack again: %d copies, %d of mise", n, b.engine.MiseSeeds())
	}
}

func TestABuildWithItsDockerfileGetsNoRailpack(t *testing.T) {
	b := withRailpack(newBench(t))
	commit := b.repo.Commit("main", "Something", map[string]string{"Dockerfile": nodeDockerfile, "package.json": nodeApp["package.json"]})
	if err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A")); err != nil {
		t.Fatal(err)
	}
	if n := b.engine.RailpackCopies(); n != 0 || b.engine.Pulls != 1 {
		t.Errorf("a build with a Dockerfile got Railpack: %d copies, %d pulls", n, b.engine.Pulls)
	}
	if d := b.final().Detection; d == nil || d.Builder != resource.BuilderDockerfile || d.Dockerfile != "Dockerfile" {
		t.Errorf("the build says it found %+v, not its Dockerfile", d)
	}
}

func TestARailpackBuildThatFailsSaysWhichStep(t *testing.T) {
	b := withRailpack(newBench(t))
	app := map[string]string{"package.json": `{"scripts":{"build":"echo compiling 214 modules && exit 2","start":"node server.js"}}`}
	commit := b.repo.Commit("main", "Move sessions to valkey", app)
	err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A"))
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, `The build failed at step 5 of 5, "npm run build": its log marks what it printed last`) {
		t.Errorf("a Railpack build that failed says %q", plain)
	}
	f := b.final()
	if f.StepName != "npm run build" || f.FailedLine != "compiling 214 modules" {
		t.Errorf("the build failed at %q saying %q", f.StepName, f.FailedLine)
	}
	if d := f.Detection; d == nil || !slices.Contains(d.Warnings, "No node package manager detected, using npm") {
		t.Errorf("the build doesn't say what Railpack warned of: %+v", d)
	}
}

func TestASourceRailpackCantReadSaysSoAndBuildsNothing(t *testing.T) {
	b := withRailpack(newBench(t))
	commit := b.repo.Commit("main", "Docs only", map[string]string{"README.md": "# shop"})
	err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A"))
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, `Railpack can't tell how to build`) || !strings.Contains(plain, `"Railpack could not determine how to build the app."`) ||
		!strings.Contains(plain, "Add a Dockerfile, or set both a build command and a start command") {
		t.Errorf("a source Railpack can't read says %q", plain)
	}
	if n := len(b.made("dockwell-build")); n != 0 {
		t.Errorf("%d builders ran for a commit Railpack can't read", n)
	}
	if !slices.Contains(b.lines(), "Railpack could not determine how to build the app.") {
		t.Errorf("the log doesn't say what Railpack said: %q", b.lines())
	}
	if f := b.final(); f.State != resource.BuildFailed || f.Detection == nil || f.Detection.Error == "" {
		t.Errorf("the build ended as %+v", f)
	}
}

func TestARailpackBuildTakesArgumentsAndSecretsAsFiles(t *testing.T) {
	b := withRailpack(newBench(t))
	app := map[string]string{"package.json": `{"scripts":{"build":"echo token $NPM_TOKEN for $NODE_ENV","start":"node server.js"}}`}
	commit := b.repo.Commit("main", "Private packages", app)
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Builder = resource.BuilderRailpack
	req.Args = map[string]string{"NODE_ENV": "staging"}
	req.Secrets = map[string]string{"NPM_TOKEN": "npm_s3cretvalue"}
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(b.lines(), "token ••• for staging") {
		t.Errorf("the log doesn't have the step's words with the secret masked: %q", b.lines())
	}
	builder := b.made("dockwell-build")[0]
	all := strings.Join(append(slices.Clone(builder.Cmd), builder.Env...), " ")
	if strings.Contains(all, "npm_s3cretvalue") {
		t.Error("a secret's value is in the builder's command or environment")
	}
	// The reading has the secret's name, and a stand-in for its value.
	readings := b.made("dockwell-detect")
	reading := readings[len(readings)-1]
	if all := strings.Join(append(slices.Clone(reading.Cmd), reading.Env...), " "); strings.Contains(all, "npm_s3cretvalue") ||
		!slices.Contains(reading.Cmd, "NPM_TOKEN="+secretPlaceholder) {
		t.Errorf("the reading has %q", reading.Cmd)
	}
	hash := ""
	for _, kv := range builder.Env {
		if v, ok := strings.CutPrefix(kv, "DW_SECRETS_HASH="); ok {
			hash = v
		}
	}
	if len(hash) != 64 {
		t.Fatalf("the builder has no sum of its secrets: %q", hash)
	}
	// The same values sum the same, a changed one doesn't.
	if again := b.builder.secretsHash(req); again != hash {
		t.Error("the same secrets sum differently")
	}
	req.Secrets = map[string]string{"NPM_TOKEN": "npm_other"}
	if b.builder.secretsHash(req) == hash {
		t.Error("a changed secret sums the same, so its steps would come from the cache")
	}
}

func TestTheSumOfSecretsOutlivesTheAgent(t *testing.T) {
	dir := t.TempDir()
	r := service.BuildRequest{Args: map[string]string{"A": "1"}, Secrets: map[string]string{"S": "x"}}
	one, two := &Builder{StateDir: dir}, &Builder{StateDir: dir}
	if one.secretsHash(r) != two.secretsHash(r) {
		t.Error("an agent that restarts sums the same secrets differently, so a build's steps that use one build again")
	}
	if info, err := os.Stat(dir + "/build-key"); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the key is %v, %v, not a file only the agent reads", info, err)
	}
	if (&Builder{StateDir: t.TempDir()}).secretsHash(r) == one.secretsHash(r) {
		t.Error("two servers sum the same secrets the same")
	}
}

func TestDetectSaysADockerfileAndWhatItStartsFrom(t *testing.T) {
	b := withRailpack(newBench(t))
	dockerfile := "FROM node:22-alpine AS build\nRUN npm ci\nFROM build AS test\nFROM --platform=linux/amd64 nginx:1.27 AS web\nCOPY --from=build /app /usr/share/nginx/html\n"
	commit := b.repo.Commit("main", "Something", map[string]string{"Dockerfile": dockerfile})
	d, err := b.builder.Detect(t.Context(), b.request(commit, ""))
	if err != nil {
		t.Fatal(err)
	}
	if d.Builder != resource.BuilderDockerfile || d.Dockerfile != "Dockerfile" || d.BaseImage != "nginx:1.27" {
		t.Errorf("detecting found %+v, not the Dockerfile and nginx:1.27", d)
	}
	if n := b.engine.RailpackCopies(); n != 0 {
		t.Errorf("reading a commit with a Dockerfile got Railpack: %d copies", n)
	}
	detector := b.made("dockwell-detect")[0]
	if detector.HostConfig.Privileged || detector.HostConfig.Memory > detectMemory || detector.HostConfig.Memory == 0 {
		t.Errorf("the detect container runs with %+v, not unprivileged within its memory", detector.HostConfig)
	}
	if left := b.engine.Containers(); len(left) != 0 {
		t.Errorf("the detect container is still there: %v", left)
	}
}

func TestDetectAsksRailpackWithNoDockerfile(t *testing.T) {
	b := withRailpack(newBench(t))
	commit := b.repo.Commit("main", "Something", nodeApp)
	req := b.request(commit, "")
	req.Args = map[string]string{"RAILPACK_NODE_VERSION": "20"}
	req.BuildCommand = "npm run build:prod"
	d, err := b.builder.Detect(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Builder != resource.BuilderRailpack || len(d.Packages) != 1 || d.Packages[0].Requested != "20" || d.Packages[0].Source != "RAILPACK_NODE_VERSION" {
		t.Errorf("detecting found %+v, not Node 20 from the build argument", d)
	}
	if !slices.ContainsFunc(d.Steps, func(s resource.DetectedStep) bool {
		return s.Name == "build" && slices.Equal(s.Commands, []string{"npm run build:prod"})
	}) {
		t.Errorf("detecting found the steps %+v, not the build command it was given", d.Steps)
	}
	if n := b.engine.RailpackCopies(); n != 1 {
		t.Errorf("Railpack was copied %d times, not once", n)
	}
}

// Railpack is asked to refuse a commit with nothing that starts the app, unless the
// service starts with its own command: an image with no start command runs bash,
// which exits at once.
func TestDetectRefusesACommitWithNothingToStart(t *testing.T) {
	b := withRailpack(newBench(t))
	commit := b.repo.Commit("main", "A library", map[string]string{"package.json": `{"name":"lib","scripts":{"build":"tsc"}}`})
	req := b.request(commit, "")
	d, err := b.builder.Detect(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Problem != resource.ProblemNoStart || !strings.Contains(resource.RailpackRefusal(d, "web"), "Set the build's start command") {
		t.Errorf("a commit with nothing to start reads as %+v", d)
	}
	req.OwnCommand = true
	if d, err := b.builder.Detect(t.Context(), req); err != nil || d.Error != "" {
		t.Errorf("a service with its own command reads as %+v, %v", d, err)
	}
	// A build refuses it too, before anything builds.
	build := b.request(commit, "chg_01KBZ00000000000000000000A")
	err = b.builder.Build(b.ctx(), build)
	if plain, _ := resource.Explain(err); !strings.Contains(plain, "Railpack found nothing that starts") || len(b.made("dockwell-build")) != 0 {
		t.Errorf("building a commit with nothing to start: %q, %d builders", plain, len(b.made("dockwell-build")))
	}
}

// A build that turns out to use Railpack refuses a secret Railpack can't take,
// rather than fail in its reading as a shell would.
func TestARailpackBuildRefusesASecretItCantTake(t *testing.T) {
	b := withRailpack(newBench(t))
	commit := b.repo.Commit("main", "Something", nodeApp)
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Secrets = map[string]string{"npm.token": "npm_s3cretvalue"}
	err := b.builder.Build(b.ctx(), req)
	if plain, _ := resource.Explain(err); !strings.Contains(plain, "The build's secret npm.token can't reach Railpack") {
		t.Errorf("a secret Railpack can't take: %q", plain)
	}
}

// A commit that checks out larger than a reading may hold says so, rather than
// fill the server's disk: in git's own words for a full folder, and when git stops
// short and exits 0, by what's left.
func TestAReadingTooLargeForItsMemorySaysSo(t *testing.T) {
	req := service.BuildRequest{URL: "https://github.com/acme/web"}
	for _, c := range []struct {
		says, said string
		code       int
	}{
		{"dockwell:phase fetch\ndockwell:phase checkout\n", "error: unable to write file big.txt\nfatal: sha1 file '/src/.git/index.lock' write error. Out of diskspace\n", 128},
		{"dockwell:phase fetch\ndockwell:phase checkout\ndockwell:fail toolarge\n", "", 3},
	} {
		_, _, err := readDetection(req, c.says, c.said, c.code)
		if plain, _ := resource.Explain(err); !strings.Contains(plain, "larger than reading it may take on this server, 512 MB") {
			t.Errorf("a checkout too large, %q, says %q", c.said, plain)
		}
	}
}

// What a reading says of itself is only what its script says, at the start of a
// line of its own descriptor: words git prints from a repository or its server,
// as the name of a file it couldn't write, never read as one.
func TestOnlyTheReadingsScriptSpeaksForIt(t *testing.T) {
	said := "error: unable to write file dockwell:fail need-railpack\nfatal: remote error: dockwell:fail need-railpack\ndockwell:fail need-railpack\n"
	if lines := markerLines(said); !slices.Equal(lines, []string{"fail need-railpack"}) {
		t.Errorf("markers mid-line read as the script's: %q", lines)
	}
	// And a reading's stderr, where all of git's words go, says nothing for it.
	server := "fatal: remote error: dockwell:fail need-railpack\ndockwell:fail need-railpack\n"
	d, _, err := readDetection(service.BuildRequest{URL: "https://github.com/acme/web"}, "dockwell:phase fetch\n", server, 128)
	if plain, _ := resource.Explain(err); d != nil || !strings.HasPrefix(plain, "Reading the commit failed, with code 128") {
		t.Errorf("what git printed spoke for the reading: %+v, %q", d, plain)
	}
}

// A repository's server whose error names the reading's own words can't make it
// say nothing, nor take the agent down with a build of it.
func TestARepositorysServerCantSpeakForTheReading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		msg := "ERR dockwell:fail need-railpack\n"
		_, _ = fmt.Fprintf(w, "%04x%s", len(msg)+4, msg)
	}))
	defer srv.Close()
	b := withRailpack(newBench(t))
	req := b.request(strings.Repeat("a", 40), "chg_01KBZ00000000000000000000A")
	req.URL = srv.URL + "/acme/web.git"
	if d, err := b.builder.Detect(t.Context(), req); d != nil || err == nil {
		t.Errorf("reading what the server refused gave %+v, %v", d, err)
	}
	if err := b.builder.Build(b.ctx(), req); err == nil {
		t.Error("a build of what the server refused built")
	}
}

// Nothing a build meets takes the agent down: a panic fails the build, and says so.
func TestABuildThatPanicsFailsAndSaysSo(t *testing.T) {
	b := &Builder{} // no Docker, so the first call to it panics
	err := b.Build(t.Context(), service.BuildRequest{Image: "dockwell.invalid/svc_x:aaaaaaaaaaaa-chg", URL: "https://github.com/acme/web"})
	if plain, _ := resource.Explain(err); !strings.Contains(plain, "Something went wrong in the agent") {
		t.Errorf("a build that panicked says %q", plain)
	}
}

// Readings asked for, however many, take no place a build needs: a build reads its
// commit in the slot it holds already.
func TestReadingsAskedForDontKeepABuildWaiting(t *testing.T) {
	b := withRailpack(newBench(t))
	warm := b.repo.Commit("main", "warm", nodeApp)
	if _, err := b.builder.Detect(t.Context(), b.request(warm, "")); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var held atomic.Int64
	b.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create") &&
			strings.HasPrefix(r.URL.Query().Get("name"), "dockwell-detect-") && held.Add(1) <= detectsAtOnce {
			<-release
		}
		return nil
	})
	defer close(release)
	for i := range detectsAtOnce {
		c := b.repo.Commit("main", fmt.Sprintf("held %d", i), map[string]string{"held": strconv.Itoa(i)})
		go func() { _, _ = b.builder.Detect(context.WithoutCancel(t.Context()), b.request(c, "")) }()
	}
	for held.Load() < detectsAtOnce {
		time.Sleep(10 * time.Millisecond)
	}
	commit := b.repo.Commit("main", "real build", nodeApp)
	ctx, cancel := context.WithTimeout(b.ctx(), 20*time.Second)
	defer cancel()
	if err := b.builder.Build(ctx, b.request(commit, "chg_01KBZ00000000000000000000A")); err != nil {
		t.Errorf("a build behind %d held readings: %v", detectsAtOnce, err)
	}
}

// A build's plan and arguments with nowhere in /run go to a folder made for them,
// never to one someone else made in the temporary folder, or pointed elsewhere.
func TestTheTemporaryFolderTakesNothingItFinds(t *testing.T) {
	tmp, elsewhere := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if err := os.Symlink(elsewhere, filepath.Join(tmp, "dockwell-builds")); err != nil {
		t.Fatal(err)
	}
	b := &Builder{SecretsDir: "/proc/dockwell-builds", InMemory: func(string) bool { return true }}
	dir, cleanup, err := b.writeSecrets(map[string]string{planFile: "{}"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	resolved, _ := filepath.EvalSymlinks(dir)
	parent, _ := filepath.EvalSymlinks(tmp)
	if filepath.Dir(resolved) != parent || !strings.HasPrefix(filepath.Base(dir), "dockwell-build-") {
		t.Errorf("the plan went to %s, which is %s", dir, resolved)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the plan's folder is %v, %v", info, err)
	}
	if _, _, err := b.writeSecrets(map[string]string{"s-X": "x"}, true); err == nil {
		t.Error("a secret went outside memory")
	}
}

func TestDetectSaysWhatRailpackCantRead(t *testing.T) {
	b := withRailpack(newBench(t))
	commit := b.repo.Commit("main", "Docs only", map[string]string{"README.md": "# shop"})
	d, err := b.builder.Detect(t.Context(), b.request(commit, ""))
	if err != nil {
		t.Fatal(err)
	}
	if d.Builder != resource.BuilderRailpack || !strings.HasPrefix(d.Error, "Railpack could not determine how to build the app.") {
		t.Errorf("detecting found %+v, not what Railpack said it couldn't do", d)
	}
}

func TestDetectWithTheDockerfileBuilderNeedsOne(t *testing.T) {
	b := withRailpack(newBench(t))
	commit := b.repo.Commit("main", "Something", nodeApp)
	req := b.request(commit, "")
	req.Builder = resource.BuilderDockerfile
	_, err := b.builder.Detect(t.Context(), req)
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, "has no Dockerfile at Dockerfile") {
		t.Errorf("detecting a Dockerfile build with no Dockerfile says %q", plain)
	}
}

func TestReadsWhatRailpackWrites(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// What Railpack 0.43.0 wrote for a Node app with a build and a start script.
	d := railpackDetection(read("railpack-node-info.json"), read("railpack-node-plan.json"))
	if d.Railpack != "0.43.0" || !slices.Equal(d.Providers, []string{"node"}) || d.Start != "npm run start" || d.Error != "" {
		t.Errorf("read %+v", d)
	}
	if len(d.Packages) != 1 || d.Packages[0].Name != "node" || d.Packages[0].Requested != "22" || !strings.HasPrefix(d.Packages[0].Resolved, "22.") {
		t.Errorf("read the packages %+v", d.Packages)
	}
	want := []resource.DetectedStep{{Name: "install", Commands: []string{"npm install"}}, {Name: "build", Commands: []string{"npm run build"}}}
	if !slices.EqualFunc(d.Steps, want, func(a, b resource.DetectedStep) bool { return a.Name == b.Name && slices.Equal(a.Commands, b.Commands) }) {
		t.Errorf("read the steps %+v, not %+v", d.Steps, want)
	}
	if !slices.Equal(d.Warnings, []string{"No node package manager detected, using npm"}) {
		t.Errorf("read the warnings %q", d.Warnings)
	}
	// And for a folder it can't read.
	none := railpackDetection(read("railpack-none-info.json"), nil)
	if !strings.HasPrefix(none.Error, "Railpack could not determine how to build the app.") {
		t.Errorf("read the error %q", none.Error)
	}
}

func TestBaseImageFollowsStages(t *testing.T) {
	for from, want := range map[string]string{
		"FROM node:22-alpine\n":                                          "node:22-alpine",
		"FROM node:22 AS build\nFROM build\n":                            "node:22",
		"from golang:1.25 as b\nfrom gcr.io/distroless/static:nonroot\n": "gcr.io/distroless/static:nonroot",
		"ARG BASE=node:22\nFROM $BASE\n":                                 "",
		"FROM golang:1.25 AS b\nFROM scratch\n":                          "",
		"":                                                               "",
	} {
		if got := baseImage(from); got != want {
			t.Errorf("baseImage(%q) = %q, not %q", from, got, want)
		}
	}
}

// A reading of a commit that isn't on its branch says so, as a build does.
func TestAReadingOfACommitOffItsBranchSaysSo(t *testing.T) {
	says := "dockwell:phase fetch\ndockwell:fail notonref\n"
	_, _, err := readDetection(service.BuildRequest{URL: "https://github.com/acme/web", Ref: "refs/heads/main", Commit: strings.Repeat("a", 40)}, says, "", 3)
	if plain, _ := resource.Explain(err); !strings.Contains(plain, "isn't on refs/heads/main of github.com/acme/web, so Dockwell won't build it") {
		t.Errorf("a commit off its branch reads %q", plain)
	}
}

// What a build keeps of its reading has its secrets masked, as its log does.
func TestABuildsReadingKeepsNoSecret(t *testing.T) {
	r := newReport(t.Context(), service.BuildRequest{Secrets: map[string]string{"NPM_TOKEN": "npm_s3cretvalue"}}, time.Now)
	d := r.hideDetection(&resource.BuildDetection{
		Builder: resource.BuilderRailpack, Start: "node server.js --token=npm_s3cretvalue",
		Steps:    []resource.DetectedStep{{Name: "build", Commands: []string{"echo npm_s3cretvalue"}}},
		Warnings: []string{"npm_s3cretvalue"},
	})
	if strings.Contains(d.Start+d.Steps[0].Commands[0]+d.Warnings[0], "npm_s3cretvalue") {
		t.Errorf("the reading kept a secret: %+v", d)
	}
}

// An agent that can't write where secrets go, as one that doesn't run as root,
// still builds with Railpack what has none: its plan and arguments aren't secret.
func TestARailpackBuildWithNoSecretNeedsNoRoot(t *testing.T) {
	b := withRailpack(newBench(t))
	b.builder.SecretsDir = "/proc/dockwell-builds"
	commit := b.repo.Commit("main", "Add order export endpoint", nodeApp)
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Args = map[string]string{"NODE_ENV": "production"}
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	req.Change, req.Secrets = "chg_01KBZ00000000000000000000B", map[string]string{"NPM_TOKEN": "npm_s3cretvalue"}
	req.Image += "x"
	if plain, _ := resource.Explain(b.builder.Build(b.ctx(), req)); !strings.Contains(plain, "nowhere in memory") {
		t.Errorf("a build with a secret and nowhere in memory for it says %q", plain)
	}
}

// runReading runs the reading's own script, as a reading's container would, with
// this machine's shell and git, in a folder of its own, and gives what it said of
// itself, what else it printed, and its code.
func runReading(t *testing.T, env ...string) (says, said string, code int, src string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git here")
	}
	dir := t.TempDir()
	src = filepath.Join(dir, "src")
	script := strings.NewReplacer(sourceDir, src, "/tmp/", dir+"/t/", "/work", dir+"/work").Replace(detectScript)
	if err := os.MkdirAll(dir+"/t", 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script, "dockwell-detect")
	cmd.Env = append(os.Environ(), env...)
	var out, errs strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errs
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errs.String(), code, src
}

// A reading finds a Dockerfile in the commit's trees, following symlinks within the
// commit and none out of it, and checks nothing out: a Dockerfile build of a
// repository however large reads only its Dockerfile.
func TestAReadingFindsTheDockerfileWithoutACheckout(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "Something", map[string]string{
		"web/Dockerfile": "FROM node:22-alpine AS build\nRUN npm ci\nFROM nginx:1.27\n",
		"web/big.txt":    strings.Repeat("a line of the catalogue\n", 4096),
		"Dockerfile":     "",
	})
	env := []string{"DW_URL=" + b.repo.URL, "DW_REF=refs/heads/main", "DW_COMMIT=" + commit, "DW_DOCKERFILE=Dockerfile", "DW_BUILDER="}
	says, said, code, src := runReading(t, append(env, "DW_CONTEXT=web")...)
	d, _, err := readDetection(service.BuildRequest{Context: "web"}, says, said, code)
	if err != nil || d.Builder != resource.BuilderDockerfile || d.BaseImage != "nginx:1.27" {
		t.Fatalf("the reading found %+v, %v, saying %q and %q", d, err, says, said)
	}
	entries, _ := os.ReadDir(src)
	for _, e := range entries {
		if e.Name() != ".git" {
			t.Errorf("the reading checked out %s", e.Name())
		}
	}
	if strings.Contains(said, marker) {
		t.Errorf("the script spoke on stderr: %q", said)
	}
}

// A Dockerfile that is a symlink out of the commit isn't one, as a build's checkout
// finds it isn't: the reading asks for Railpack, which this machine hasn't.
func TestAReadingFollowsNoSymlinkOutOfTheCommit(t *testing.T) {
	b := newBench(t)
	b.repo.Commit("main", "Something", map[string]string{"package.json": "{}"})
	work := t.TempDir()
	for _, args := range [][]string{
		{"clone", "-q", b.repo.URL, work},
		{"-C", work, "checkout", "-q", "main"},
	} {
		if out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(work, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", work, "add", "Dockerfile"},
		{"-C", work, "-c", "user.name=Leo Park", "-c", "user.email=leo@example.com", "commit", "-q", "-m", "Point the Dockerfile out"},
		{"-C", work, "push", "-q", "origin", "main"},
	} {
		if out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, _ := exec.CommandContext(t.Context(), "git", "-C", work, "rev-parse", "HEAD").Output()
	commit := strings.TrimSpace(string(out))
	says, _, code, _ := runReading(t, "DW_URL="+b.repo.URL, "DW_REF=refs/heads/main", "DW_COMMIT="+commit, "DW_DOCKERFILE=Dockerfile", "DW_BUILDER=", "DW_CONTEXT=")
	if !slices.Contains(markerLines(says), "fail need-railpack") || code != 4 {
		t.Errorf("a Dockerfile that leads out of the commit read as %q, code %d", says, code)
	}
}
