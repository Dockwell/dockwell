// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// nodeApp is a Node app with no Dockerfile, which Railpack builds.
var nodeApp = map[string]string{
	"package.json": `{"name":"storefront","scripts":{"build":"echo built the storefront","start":"node server.js"},"engines":{"node":"22"}}`,
	"server.js":    `require("http").createServer((q, s) => s.end("ok")).listen(8080)`,
}

// detects counts the detect containers the engine has run.
func (g *gitBench) detects() int {
	n := 0
	builders := g.e.Builders()
	for i := range builders {
		if slices.Contains(builders[i].Cmd, "dockwell-detect") {
			n++
		}
	}
	return n
}

// An app with no Dockerfile builds with Railpack, and its plan says what Railpack
// found before anything builds.
func TestAnAppWithNoDockerfilePlansWhatRailpackFound(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		commit := g.repo.Commit("main", "Add order export endpoint", nodeApp)

		plan := g.propose(t, g.buildEdit("main", nil))
		step := buildStep(plan)
		d := step.GetDetection()
		if step.GetCommit() != commit || d.GetBuilder() != resourcev1.ImageBuild_BUILDER_RAILPACK || !slices.Equal(d.GetProviders(), []string{"node"}) {
			t.Fatalf("the plan builds %v", step)
		}
		if d.GetStartCommand() != "npm run start" || len(d.GetPackages()) != 1 || d.GetPackages()[0].GetRequested() != "22" {
			t.Errorf("the plan says Railpack found %v", d)
		}
		if !slices.ContainsFunc(d.GetSteps(), func(s *dockwellv1.DetectedStep) bool {
			return s.GetName() == "build" && slices.Equal(s.GetCommands(), []string{"npm run build"})
		}) {
			t.Errorf("the plan doesn't say what the build runs: %v", d.GetSteps())
		}
		if got := g.done(t, plan); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the change ended %v: %s", got.GetState(), got.GetError())
		}
		web := g.service(t, "", "web")
		running := g.e.Newest(docker.Project(web.GetResource().GetScope()), "web")
		if running == nil || running.Config.Image != step.GetImage() || !running.State.Running {
			t.Fatalf("what runs is %+v, not what Railpack built", running)
		}
		b := g.revisions(t, web.GetId())[0].GetBuild()
		if b.GetDetection().GetBuilder() != resourcev1.ImageBuild_BUILDER_RAILPACK || b.GetDetection().GetStartCommand() != "npm run start" {
			t.Errorf("the build says it found %v", b.GetDetection())
		}
		if !slices.Contains(g.log(t, b.GetId()), "built the storefront") {
			t.Errorf("the build's log doesn't have what its build step printed: %q", g.log(t, b.GetId()))
		}
	})
}

// A form reads how a new app's repository builds before anyone plans it, and the
// plan that follows reads it from what the form read.
func TestDetectBuildReadsANewAppsRepository(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		commit := g.repo.Commit("main", "Add order export endpoint", nodeApp)
		ask := func() *dockwellv1.DetectBuildResponse {
			t.Helper()
			res, err := g.builds.DetectBuild(t.Context(), connect.NewRequest(&dockwellv1.DetectBuildRequest{Build: &resourcev1.ImageBuild{
				Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: g.repo.URL, Ref: "main"}},
				Args:   map[string]string{"NODE_ENV": "production"},
			}}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg
		}
		got := ask()
		if got.GetCommit() != commit || got.GetRef() != "refs/heads/main" || got.GetServerName() == "" || got.GetDetection().GetBuilder() != resourcev1.ImageBuild_BUILDER_RAILPACK {
			t.Fatalf("detecting said %v", got)
		}
		// The first asks for Railpack, which the server then gets, and reads again.
		first := g.detects()
		if first != 2 || g.e.RailpackCopies() != 1 {
			t.Fatalf("reading once ran %d detect containers and copied Railpack %d times", first, g.e.RailpackCopies())
		}
		ask()
		g.propose(t, g.buildEdit("main", nil))
		if n := g.detects(); n != first {
			t.Errorf("reading again and planning ran %d more detect containers, where what was read first would do", n-first)
		}
	})
}

// A service's build reads what its branch names now, on the server it runs on.
func TestDetectBuildOfAServiceReadsItsBranchNow(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "Works", map[string]string{"Dockerfile": webDockerfile})
		g.done(t, g.propose(t, g.buildEdit("main", nil)))
		next := g.repo.Commit("main", "Next", map[string]string{"Dockerfile": "FROM node:22-alpine AS build\nRUN echo hi\nFROM nginx:1.27\n"})
		res, err := g.builds.DetectBuild(t.Context(), connect.NewRequest(&dockwellv1.DetectBuildRequest{Id: "web"}))
		if err != nil {
			t.Fatal(err)
		}
		d := res.Msg.GetDetection()
		if res.Msg.GetCommit() != next || d.GetBuilder() != resourcev1.ImageBuild_BUILDER_DOCKERFILE || d.GetBaseImage() != "nginx:1.27" || d.GetDockerfile() != "Dockerfile" {
			t.Errorf("detecting web said %v", res.Msg)
		}
	})
}

// A commit Railpack can't build is refused as it's planned, before anything runs.
func TestAPlanRailpackCantBuildIsRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "Docs only", map[string]string{"README.md": "# storefront"})
		_, err := g.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Make web", Edits: []*dockwellv1.Edit{g.buildEdit("main", nil)}}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), `Railpack can't tell how to build`) ||
			!strings.Contains(err.Error(), `"Railpack could not determine how to build the app."`) || !strings.Contains(err.Error(), "Add a Dockerfile, or set both a build command and a start command") {
			t.Errorf("planning what Railpack can't build = %v", err)
		}
		if n := len(g.e.Containers()); n != 0 {
			t.Errorf("a refused plan left %d containers", n)
		}
	})
}

// What can't be read is refused with what to do.
func TestDetectBuildSaysWhatItCantRead(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "Works", nodeApp)
		detect := func(b *resourcev1.ImageBuild) error {
			_, err := g.builds.DetectBuild(t.Context(), connect.NewRequest(&dockwellv1.DetectBuildRequest{Build: b}))
			return err
		}
		git := func(url, ref string) *resourcev1.ImageBuild {
			return &resourcev1.ImageBuild{Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: url, Ref: ref}}}
		}
		if err := detect(git(g.repo.URL, "mian")); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "has no branch or tag mian. Its branches are main") {
			t.Errorf("a branch that isn't there = %v", err)
		}
		if err := detect(git("git@example.com:acme/web.git", "main")); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("a URL Dockwell can't fetch = %v", err)
		}
		b := git(g.repo.URL, "main")
		b.Builder = resourcev1.ImageBuild_BUILDER_DOCKERFILE
		if err := detect(b); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "has no Dockerfile at Dockerfile") {
			t.Errorf("a Dockerfile build with none = %v", err)
		}
		if err := detect(nil); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("nothing to read = %v", err)
		}
	})
}

// A plan refuses, with what to do, what Railpack would refuse once it built: a
// commit with nothing that starts the app, unless the service starts with its own
// command; build and start commands a Dockerfile makes do nothing; and a secret
// Railpack can't take, for a build that turns out to use Railpack.
func TestAPlanRefusesWhatARailpackBuildWouldFailOn(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		propose := func(e *dockwellv1.Edit) error {
			_, err := g.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Make web", Edits: []*dockwellv1.Edit{e}}))
			return err
		}
		g.repo.Commit("main", "A library", map[string]string{"package.json": `{"name":"lib","scripts":{"build":"tsc"}}`})
		if err := propose(g.buildEdit("main", nil)); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "Railpack found nothing that starts") ||
			!strings.Contains(err.Error(), "Set the build's start command") {
			t.Errorf("a commit with nothing to start = %v", err)
		}
		own := g.buildEdit("main", nil)
		own.GetSpec().GetService().GetApp().Command = []string{"node", "lib.js"}
		if err := propose(own); err != nil {
			t.Errorf("a service with its own command = %v", err)
		}

		g.repo.Commit("main", "A Dockerfile", map[string]string{"Dockerfile": webDockerfile})
		commands := g.buildEdit("main", nil)
		commands.GetSpec().GetService().GetBuild().StartCommand = "node server.js"
		if err := propose(commands); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "its build and start commands would do nothing") {
			t.Errorf("a start command with a Dockerfile = %v", err)
		}

		// Another repository, as a commit keeps what the one before it had.
		api := g.git.Repo("api")
		api.Commit("main", "No Dockerfile", nodeApp)
		npm := &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Name: "NPM"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "npm_s3cr3t-value"}}},
		}
		_, err := g.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Make web", Edits: []*dockwellv1.Edit{npm, func() *dockwellv1.Edit {
				e := g.buildEdit("main", map[string]string{"npm.token": "NPM"})
				e.GetSpec().GetService().GetBuild().GetGit().Url = api.URL
				return e
			}()},
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "builds with Railpack, and the build's secret npm.token can't reach Railpack") {
			t.Errorf("a secret Railpack can't take = %v", err)
		}
	})
}

// The same reading asked for at once, as a form asks while you type, reads the
// commit once; and with more than one server, a reading waits to be told which
// builds the app, rather than read where it may never run.
func TestReadingsOfACommitAreFewAndWhereItBuilds(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "Works", nodeApp)
		build := &resourcev1.ImageBuild{Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: g.repo.URL, Ref: "main"}}}
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Go(func() {
				_, errs[i] = g.builds.DetectBuild(t.Context(), connect.NewRequest(&dockwellv1.DetectBuildRequest{Build: build}))
			})
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		// The first on a server reads twice: once to find there's no Dockerfile,
		// and again once Railpack is there.
		if n := g.detects(); n > 2 {
			t.Errorf("four readings of one commit at once read it %d times", n)
		}

		g.f.addDockerServer(t)
		_, err := g.builds.DetectBuild(t.Context(), connect.NewRequest(&dockwellv1.DetectBuildRequest{Build: build}))
		if reasonOf(err) != "choose_server" || !strings.Contains(err.Error(), "there are 2 servers, so say which builds the app") {
			t.Errorf("a reading with two servers and none named = %v", err)
		}
	})
}

// One caller has at most two readings under way: a server reads few at once for
// everyone, so no one keeps it reading.
func TestACallerHasFewReadingsUnderWay(t *testing.T) {
	g := newGitBench(t, store.SQLite)
	g.repo.Commit("main", "Works", nodeApp)
	release := make(chan struct{})
	var held atomic.Int64
	g.e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create") && strings.HasPrefix(r.URL.Query().Get("name"), "dockwell-detect-") {
			held.Add(1)
			<-release
		}
		return nil
	})
	read := func(n int) error {
		b := &resourcev1.ImageBuild{
			Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: g.repo.URL, Ref: "main"}},
			Args:   map[string]string{"N": strconv.Itoa(n)},
		}
		_, err := g.builds.DetectBuild(t.Context(), connect.NewRequest(&dockwellv1.DetectBuildRequest{Build: b}))
		return err
	}
	var wg sync.WaitGroup
	for n := range 2 {
		wg.Go(func() { _ = read(n) })
	}
	for held.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if err := read(3); reasonOf(err) != "too_many_readings" || !strings.Contains(err.Error(), "you have 2 readings under way already") {
		t.Errorf("a third reading at once = %v", err)
	}
	close(release)
	wg.Wait()
	if err := read(4); err != nil {
		t.Errorf("a reading once the others ended = %v", err)
	}
}
