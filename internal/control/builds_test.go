// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/gittest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// gitBench is a control plane with one server on the fake engine, and a Git server
// beside them.
type gitBench struct {
	*bench
	git    *gittest.Server
	repo   *gittest.Repo
	builds dockwellv1connect.BuildServiceClient
}

func newGitBench(t *testing.T, backend store.Backend) *gitBench {
	t.Helper()
	b := newBench(t, backend)
	g := &gitBench{bench: b, git: gittest.Start(t), builds: dockwellv1connect.NewBuildServiceClient(b.b, b.b.origin, connect.WithProtoJSON())}
	g.repo = g.git.Repo("web")
	return g
}

const webDockerfile = "FROM alpine:3.23\nWORKDIR /app\nCOPY . .\nRUN echo \"installed for $NODE_ENV\"\nEXPOSE 8080\nCMD [\"serve\"]\n"

// buildEdit is a new app named web that builds from the bench's repository.
func (g *gitBench) buildEdit(ref string, secrets map[string]string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8080, ContainerPort: 8080}}}},
			Build: &resourcev1.ImageBuild{
				Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: g.repo.URL, Ref: ref}},
				Args:   map[string]string{"NODE_ENV": "production"}, Secrets: secrets,
			},
		}}},
	}
}

func (g *gitBench) propose(t *testing.T, edits ...*dockwellv1.Edit) *dockwellv1.Change {
	t.Helper()
	res, err := g.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Make web", Edits: edits}))
	if err != nil {
		t.Fatalf("proposing: %v", err)
	}
	return res.Msg.GetChange()
}

func buildStep(c *dockwellv1.Change) *dockwellv1.BuildPlan {
	for _, s := range c.GetPlan().GetServers() {
		for _, st := range s.GetSteps() {
			if st.GetBuild() != nil {
				return st.GetBuild()
			}
		}
	}
	return nil
}

func (g *gitBench) log(t *testing.T, id string) []string {
	t.Helper()
	res, err := g.builds.ReadBuildLog(t.Context(), connect.NewRequest(&dockwellv1.ReadBuildLogRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range res.Msg.GetLines() {
		out = append(out, l.GetText())
	}
	return out
}

// A new app from a repository: the plan pins the commit and the image, applying
// builds it on the server and runs what it built, and its revision says how the
// build went.
func TestAnAppBuildsFromGitAndRunsWhatItBuilt(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		commit := g.repo.Commit("main", "Add order export endpoint", map[string]string{"Dockerfile": webDockerfile, "app.js": "1"})

		plan := g.propose(t, g.buildEdit("main", nil))
		step := buildStep(plan)
		if step == nil || step.GetCommit() != commit || step.GetRef() != "refs/heads/main" || !docker.IsBuilt(step.GetImage()) {
			t.Fatalf("the plan builds %v, not %s of main into an image of its own", step, commit)
		}
		got := g.done(t, plan)
		if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the change ended %v", got.GetState())
		}

		web := g.service(t, "", "web")
		if img := web.GetSpec().GetApp().GetImage(); img != step.GetImage() {
			t.Errorf("the service names %s, not the image its plan built, %s", img, step.GetImage())
		}
		if web.GetSpec().GetBuild().GetGit().GetCommit() != commit {
			t.Errorf("the service's build is of %s, not %s", web.GetSpec().GetBuild().GetGit().GetCommit(), commit)
		}
		running := g.e.Newest(docker.Project(web.GetResource().GetScope()), "web")
		if running == nil || running.Config.Image != step.GetImage() || !running.State.Running {
			t.Fatalf("what runs is %+v, not the built image", running)
		}

		revs := g.revisions(t, web.GetId())
		b := revs[0].GetBuild()
		if b.GetState() != dockwellv1.BuildState_BUILD_STATE_DONE || b.GetCommitSubject() != "Add order export endpoint" || b.GetCommitAuthor() != "Leo Park" ||
			b.GetServerName() != "hz-1" || b.GetSteps() != 4 || b.GetCommit() != commit || b.GetRevision() != 1 {
			t.Errorf("the revision's build is %v", b)
		}
		if !slices.Contains(g.log(t, b.GetId()), "installed for production") {
			t.Errorf("the build's log doesn't have what its step printed: %q", g.log(t, b.GetId()))
		}
		list, err := g.builds.ListBuilds(t.Context(), connect.NewRequest(&dockwellv1.ListBuildsRequest{Id: "web"}))
		if err != nil || len(list.Msg.GetBuilds()) != 1 || list.Msg.GetBuilds()[0].GetId() != b.GetId() {
			t.Errorf("the service's builds are %v, %v", list.Msg.GetBuilds(), err)
		}

		// A follow of a build that ended sends its log, then how it ended, alone, last.
		stream, err := g.builds.WatchBuild(t.Context(), connect.NewRequest(&dockwellv1.WatchBuildRequest{Id: b.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		var followed []*dockwellv1.WatchBuildResponse
		for stream.Receive() {
			followed = append(followed, stream.Msg())
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		if n := len(followed); n < 2 || len(followed[n-1].GetLines()) != 0 || followed[n-1].GetBuild().GetState() != dockwellv1.BuildState_BUILD_STATE_DONE ||
			followed[0].GetBuild().GetState() != dockwellv1.BuildState_BUILD_STATE_BUILDING {
			t.Errorf("a follow of a build that ended sent %v", followed)
		}

		drift, err := g.c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
		if err != nil || len(drift.Msg.GetDrift()) != 0 {
			t.Errorf("a service built from Git reads back as drift: %v, %v", drift.Msg.GetDrift(), err)
		}

		// A deploy builds the newest commit of its branch, which takes what it can
		// from the cache, and says what it deploys.
		next := g.repo.Commit("main", "Cache product lookups", map[string]string{"app.js": "2"})
		res, err := g.s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: web.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		deploy := res.Msg.GetChange()
		if in := deploy.GetPlan().GetIntent(); in.GetCommit() != next || in.GetRef() != "refs/heads/main" || deploy.GetSummary() != "Deploy "+next[:7]+" to web" {
			t.Errorf("the deploy is for %v, %q", in, deploy.GetSummary())
		}
		g.done(t, deploy)
		second := g.revisions(t, web.GetId())[0].GetBuild()
		if second.GetCommit() != next || second.GetCachedSteps() != 2 || second.GetImage() == b.GetImage() {
			t.Errorf("the second build is %v, want %s with 2 steps from the cache, under a tag of its own", second, next)
		}

		// A deploy of an image to an app that builds is refused, with what to do.
		_, err = g.s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: web.GetId(), Image: "nginx:1.27"}))
		if err == nil || !strings.Contains(err.Error(), "builds its image from") || !strings.Contains(err.Error(), "remove its build to run nginx:1.27") {
			t.Errorf("deploying an image to an app that builds = %v", err)
		}
	})
}

// A build that fails fails its step before anything is touched, so what ran runs
// on, and the revision says why, with the step and its last line.
func TestABuildThatFailsLeavesWhatRuns(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "Works", map[string]string{"Dockerfile": webDockerfile})
		g.done(t, g.propose(t, g.buildEdit("main", nil)))
		web := g.service(t, "", "web")
		before := g.e.Newest(docker.Project(web.GetResource().GetScope()), "web")

		g.repo.Commit("main", "Break the build", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo 'npm ERR! missing script: build' && exit 1\n"})
		res, err := g.s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: web.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		failed := applied(t, g.c, res.Msg.GetChange())
		if failed.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(failed.GetError(), "The build failed at step 2 of 2") {
			t.Fatalf("the change ended %v: %s", failed.GetState(), failed.GetError())
		}
		after := g.e.Newest(docker.Project(web.GetResource().GetScope()), "web")
		if after.ID != before.ID || !after.State.Running {
			t.Errorf("a failed build touched what runs: %s became %s", before.ID, after.ID)
		}
		var b *dockwellv1.Build
		for _, r := range g.revisions(t, web.GetId()) {
			if r.GetBuild().GetState() == dockwellv1.BuildState_BUILD_STATE_FAILED {
				b = r.GetBuild()
			}
		}
		if b == nil || b.GetStep() != 2 || b.GetFailedLine() != "npm ERR! missing script: build" || !strings.Contains(b.GetError(), "Fix it in the repository") {
			t.Errorf("the failed revision's build is %v", b)
		}
	})
}

// A branch the repository hasn't got is refused as the change is planned, with the
// branches it has.
func TestABuildOfABranchThatIsntThereIsRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "Works", map[string]string{"Dockerfile": webDockerfile})
		_, err := g.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Make web", Edits: []*dockwellv1.Edit{g.buildEdit("mian", nil)}}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "has no branch or tag mian. Its branches are main") {
			t.Errorf("a branch that isn't there = %v", err)
		}
	})
}

// A build's secret reaches its build as a BuildKit secret, and never its log.
func TestABuildsSecretStaysOutOfItsLog(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "Install private packages", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN --mount=type=secret,id=NPM_TOKEN cat /run/secrets/NPM_TOKEN\n"})
		secret := &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Name: "NPM"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "npm_s3cr3t-value"}}},
		}
		plan := g.propose(t, secret, g.buildEdit("main", map[string]string{"NPM_TOKEN": "NPM"}))
		g.done(t, plan)
		web := g.service(t, "", "web")
		b := g.revisions(t, web.GetId())[0].GetBuild()
		lines := g.log(t, b.GetId())
		for _, l := range lines {
			if strings.Contains(l, "npm_s3cr3t-value") {
				t.Fatalf("the build's log holds its secret: %q", l)
			}
		}
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "•") }) {
			t.Errorf("the step that printed the secret doesn't show it masked: %q", lines)
		}
		running := g.e.Newest(docker.Project(web.GetResource().GetScope()), "web")
		for _, kv := range running.Config.Env {
			if strings.Contains(kv, "npm_s3cr3t-value") || strings.HasPrefix(kv, "build:") {
				t.Errorf("the build's secret reached what runs: %s", kv)
			}
		}
	})
}

// Someone can cancel a build under way: its change fails, and what ran runs on.
func TestSomeoneCanCancelABuildUnderWay(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "Works", map[string]string{"Dockerfile": webDockerfile})
		g.done(t, g.propose(t, g.buildEdit("main", nil)))
		web := g.service(t, "", "web")
		before := g.e.Newest(docker.Project(web.GetResource().GetScope()), "web")

		g.repo.Commit("main", "Slow", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo working && sleep 30\n"})
		res, err := g.s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: web.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		change := res.Msg.GetChange()
		w := watch(t, g.c, change.GetId())
		if _, err := g.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()})); err != nil {
			t.Fatal(err)
		}
		var building *dockwellv1.Build
		deadline := time.Now().Add(15 * time.Second)
		for building == nil && time.Now().Before(deadline) {
			list, err := g.builds.ListBuilds(t.Context(), connect.NewRequest(&dockwellv1.ListBuildsRequest{Id: "web"}))
			if err != nil {
				t.Fatal(err)
			}
			if bs := list.Msg.GetBuilds(); len(bs) > 0 && bs[0].GetChangeId() == change.GetId() && bs[0].GetState() == dockwellv1.BuildState_BUILD_STATE_BUILDING {
				building = bs[0]
			}
			time.Sleep(50 * time.Millisecond)
		}
		if building == nil {
			t.Fatal("the build never got under way")
		}
		cancelled, err := g.builds.CancelBuild(t.Context(), connect.NewRequest(&dockwellv1.CancelBuildRequest{Id: building.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if cancelled.Msg.GetBuild().GetCancelledBy() == "" {
			t.Error("the build doesn't say who cancelled it")
		}
		ended := waitWatch(t, w).last()
		if ended.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(ended.GetError(), "was cancelled by") {
			t.Fatalf("a cancelled build's change ended %v: %s", ended.GetState(), ended.GetError())
		}
		got, err := g.builds.GetBuild(t.Context(), connect.NewRequest(&dockwellv1.GetBuildRequest{Id: building.GetId()}))
		if err != nil || got.Msg.GetBuild().GetState() != dockwellv1.BuildState_BUILD_STATE_CANCELLED {
			t.Errorf("the cancelled build reads %v, %v", got.Msg.GetBuild(), err)
		}
		after := g.e.Newest(docker.Project(web.GetResource().GetScope()), "web")
		if after.ID != before.ID || !after.State.Running {
			t.Error("a cancelled build touched what runs")
		}
		if _, err := g.builds.CancelBuild(t.Context(), connect.NewRequest(&dockwellv1.CancelBuildRequest{Id: building.GetId()})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("cancelling a build that ended = %v", err)
		}
	})
}

// What a failing step printed last is log content: a person reads it, and so does a
// token with the logs grant, and a token without it reads that the build failed, at
// which step, and nothing it printed.
func TestAFailedBuildsLastLineNeedsTheLogsGrant(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		git := gittest.Start(t)
		repo := git.Repo("web")
		repo.Commit("main", "Works", map[string]string{"Dockerfile": webDockerfile})
		c := changeClient(b, b.origin)
		applyConfirmed(t, c, proposeEdit(t, c, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Build: &resourcev1.ImageBuild{Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: repo.URL, Ref: "main"}}},
			}}},
		}))
		repo.Commit("main", "Break it", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo 'npm ERR! missing script: build' && exit 1\n"})
		deployed, err := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON()).DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "web"}))
		if err != nil {
			t.Fatal(err)
		}
		ch := deployed.Msg.GetChange()
		w := watch(t, c, ch.GetId())
		if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err != nil {
			t.Fatal(err)
		}
		ended := waitWatch(t, w).last()
		if ended.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || strings.Contains(ended.GetError(), "missing script") && !strings.Contains(ended.GetError(), `RUN echo`) {
			t.Fatalf("the change ended %v: %s", ended.GetState(), ended.GetError())
		}
		if strings.Contains(ended.GetError(), `: "npm ERR!`) {
			t.Errorf("the change's error, which any reader reads, holds what the step printed: %s", ended.GetError())
		}
		tokens := dockwellv1connect.NewTokenServiceClient(b, b.origin, connect.WithProtoJSON())
		reader := func(grants ...dockwellv1.TokenGrant) dockwellv1connect.BuildServiceClient {
			res, err := tokens.CreateToken(t.Context(), connect.NewRequest(&dockwellv1.CreateTokenRequest{
				Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
				Name:  "CI", Scope: dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, Grants: grants,
			}))
			if err != nil {
				t.Fatal(err)
			}
			return dockwellv1connect.NewBuildServiceClient(bearer{token: res.Msg.GetSecret(), rt: h2cTransport(t, f.api)}, "http://"+f.api, connect.WithProtoJSON())
		}
		lastLine := func(client dockwellv1connect.BuildServiceClient) string {
			t.Helper()
			list, err := client.ListBuilds(t.Context(), connect.NewRequest(&dockwellv1.ListBuildsRequest{Id: "web"}))
			if err != nil || len(list.Msg.GetBuilds()) != 2 {
				t.Fatalf("ListBuilds = %v, %v", list, err)
			}
			got, err := client.GetBuild(t.Context(), connect.NewRequest(&dockwellv1.GetBuildRequest{Id: list.Msg.GetBuilds()[0].GetId()}))
			if err != nil {
				t.Fatal(err)
			}
			if got.Msg.GetBuild().GetFailedLine() != list.Msg.GetBuilds()[0].GetFailedLine() {
				t.Error("GetBuild and ListBuilds say different lines")
			}
			return got.Msg.GetBuild().GetFailedLine()
		}
		const said = "npm ERR! missing script: build"
		if got := lastLine(dockwellv1connect.NewBuildServiceClient(b, b.origin, connect.WithProtoJSON())); got != said {
			t.Errorf("a person reads the last line as %q", got)
		}
		if got := lastLine(reader(dockwellv1.TokenGrant_TOKEN_GRANT_LOGS)); got != said {
			t.Errorf("a token with the logs grant reads the last line as %q", got)
		}
		if got := lastLine(reader()); got != "" {
			t.Errorf("a token without the logs grant reads the last line %q", got)
		}
	})
}

// A service that builds runs only what its own builds made: another service's build
// is that service's code.
func TestABuildCantRunAnotherServicesImage(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "Works", map[string]string{"Dockerfile": webDockerfile})
		g.done(t, g.propose(t, g.buildEdit("main", nil)))
		theirs := g.service(t, "", "web").GetSpec().GetApp().GetImage()
		edit := g.buildEdit("main", nil)
		edit.Resource.Name = "copy"
		edit.GetSpec().GetService().GetApp().Image = theirs
		edit.GetSpec().GetService().GetBuild().GetGit().Commit = strings.Repeat("a", 40)
		_, err := g.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Copy web", Edits: []*dockwellv1.Edit{edit}}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "copy builds its own image, so it can't run "+theirs) {
			t.Errorf("running another service's build = %v", err)
		}
	})
}

// A build follows a branch or a tag, and not what else a repository advertises,
// such as a pull request's head, which anyone with a fork pushes.
func TestABuildOfAPullRequestsHeadIsRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "Works", map[string]string{"Dockerfile": webDockerfile})
		fork := g.repo.Commit("fork", "Evil", map[string]string{"Dockerfile": webDockerfile, "evil.sh": "1"})
		g.repo.Ref("refs/pull/7/head", fork)
		for ref, says := range map[string]string{
			"refs/pull/7/head": "such as a pull request's head",
			"pull/7/head":      "has no branch or tag pull/7/head",
		} {
			_, err := g.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Make web", Edits: []*dockwellv1.Edit{g.buildEdit(ref, nil)}}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), says) {
				t.Errorf("a build of %s = %v", ref, err)
			}
		}
	})
}
