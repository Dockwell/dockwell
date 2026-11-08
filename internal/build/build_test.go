// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/gittest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/service"
)

var web = resource.Ref{Kind: "service", Scope: "env_01kbz0000000000000000000ab", Name: "web"}

// bench is a builder on the fake engine, with a Git server beside it.
type bench struct {
	t       *testing.T
	engine  *dockertest.Engine
	builder *Builder
	git     *gittest.Server
	repo    *gittest.Repo

	mu      sync.Mutex
	updates []resource.BuildUpdate
	said    []string
}

func newBench(t *testing.T) *bench {
	t.Helper()
	e := dockertest.New(t)
	e.Registry = map[string]dockertest.Image{Image: {}}
	b := &bench{t: t, engine: e, git: gittest.Start(t)}
	b.repo = b.git.Repo("web")
	b.builder = &Builder{
		Client:     e.Client(),
		SecretsDir: t.TempDir(),
		InMemory:   func(string) bool { return true },
		Memory:     func() (int64, int64, error) { return 4 << 30, 8 << 30, nil },
		CPUs:       func() int { return 4 },
		DiskSize:   func(string) (int64, error) { return 50 << 30, nil },
	}
	return b
}

// ctx is a step's context, which hears what the build says.
func (b *bench) ctx() context.Context {
	ctx := resource.WithBuilds(b.t.Context(), func(u resource.BuildUpdate) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.updates = append(b.updates, u)
	})
	return resource.WithProgress(ctx, func(s string) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.said = append(b.said, s)
	})
}

// request asks for a build of commit, as the step of change.
func (b *bench) request(commit, change string) service.BuildRequest {
	return service.BuildRequest{
		Service: web, Change: change, Image: docker.BuiltImage("svc_01KBZWEB", commit, change),
		URL: b.repo.URL, Ref: "refs/heads/main", Commit: commit,
	}
}

// lines is what the log said, in order.
func (b *bench) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for i := range b.updates {
		for _, l := range b.updates[i].Lines {
			out = append(out, l.Text)
		}
	}
	return out
}

// last is the last update that said where the build stands, with what it had found
// by then.
func (b *bench) final() resource.BuildUpdate {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out resource.BuildUpdate
	for i := range b.updates {
		u := &b.updates[i]
		if u.Subject != "" {
			out.Subject, out.Author = u.Subject, u.Author
		}
		if u.Steps > 0 {
			out.Steps, out.Cached = u.Steps, u.Cached
		}
		if u.State != 0 {
			out.State = u.State
		}
		if u.Error != "" {
			out.Error, out.Step, out.StepName, out.FailedLine = u.Error, u.Step, u.StepName, u.FailedLine
		}
		if u.Detection != nil {
			out.Detection = u.Detection
		}
	}
	return out
}

func (b *bench) states() []resource.BuildState {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []resource.BuildState
	for i := range b.updates {
		u := &b.updates[i]
		if len(out) == 0 || out[len(out)-1] != u.State {
			out = append(out, u.State)
		}
	}
	return out
}

const nodeDockerfile = `FROM alpine:3.23
WORKDIR /app
COPY . .
RUN echo "installed $NODE_ENV"
EXPOSE 8080
CMD ["serve"]
`

func TestBuildsACommitIntoAnImageDockerRuns(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "Add order export endpoint", map[string]string{"Dockerfile": nodeDockerfile, "app.js": "1"})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Args = map[string]string{"NODE_ENV": "production"}
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	img, err := b.engine.Client().InspectImage(t.Context(), req.Image)
	if err != nil {
		t.Fatalf("Docker hasn't the image the build made: %v", err)
	}
	if _, ok := img.Config.ExposedPorts["8080/tcp"]; !ok || !slices.Equal(img.Config.Cmd, []string{"serve"}) {
		t.Errorf("the image runs %v and exposes %v, not what its Dockerfile says", img.Config.Cmd, img.Config.ExposedPorts)
	}
	if len(img.RepoDigests) != 0 {
		t.Errorf("a built image has no registry's digest, and this has %v", img.RepoDigests)
	}
	f := b.final()
	if f.State != resource.BuildDone || f.Subject != "Add order export endpoint" || f.Author != "Leo Park" {
		t.Errorf("the build ended as %+v, not done with its commit's subject and author", f)
	}
	if f.Steps != 4 || f.Cached != 0 {
		t.Errorf("the build says %d steps, %d from the cache, not 4 and 0", f.Steps, f.Cached)
	}
	if got := b.states(); !slices.Equal(got, []resource.BuildState{resource.BuildQueued, resource.BuildBuilding, resource.BuildDone}) {
		t.Errorf("the build went %v, not queued, building, done", got)
	}
	if !slices.Contains(b.lines(), "installed production") {
		t.Errorf("the log doesn't have what the step printed, with its build argument: %q", b.lines())
	}
	builder := b.engine.Builders()[0]
	if !builder.HostConfig.Privileged || builder.HostConfig.LogConfig == nil || builder.HostConfig.LogConfig.Type != "none" {
		t.Errorf("the builder runs with %+v, not privileged with its logging off", builder.HostConfig)
	}
	if !slices.Contains(builder.Cmd, "build-arg:NODE_ENV=production") {
		t.Errorf("the builder wasn't given the build argument: %v", builder.Cmd)
	}
	if left := b.engine.Containers(); len(left) != 0 {
		t.Errorf("the builder is still there once the build ended: %v", left)
	}
}

func TestBuildingAgainTakesWhatItCanFromTheCache(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "First", map[string]string{"Dockerfile": nodeDockerfile})
	if err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A")); err != nil {
		t.Fatal(err)
	}
	b.updates = nil
	next := b.repo.Commit("main", "Second", map[string]string{"app.js": "2"})
	if err := b.builder.Build(b.ctx(), b.request(next, "chg_01KBZ00000000000000000000B")); err != nil {
		t.Fatal(err)
	}
	if f := b.final(); f.Cached != 2 {
		t.Errorf("a build after a change to the app took %d steps from the cache, not the 2 before its COPY", f.Cached)
	}
}

func TestABuildThatFailsSaysWhichStepAndWhatItSaidLast(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "Break it", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo 'npm ERR! missing script: build' && exit 1\nCMD [\"serve\"]\n"})
	err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A"))
	if resource.ReasonOf(err) != resource.ReasonBuild {
		t.Fatalf("the build failed with %v, not as a build", err)
	}
	plain, _ := resource.Explain(err)
	// What the step said last is the build's failed line: the error, which anyone
	// reading changes reads, says only which step.
	if !strings.Contains(plain, `step 2 of 2, "RUN echo 'npm ERR! missing script: build' && exit 1": its log marks what it printed last. Fix it`) {
		t.Errorf("the failure says %q, not the step, quoted", plain)
	}
	f := b.final()
	if f.State != resource.BuildFailed || f.Step != 2 || f.FailedLine != "npm ERR! missing script: build" {
		t.Errorf("the build ended as %+v, not failed at step 2 with its last line", f)
	}
	if has, _ := b.builder.Has(t.Context(), b.request(commit, "chg_01KBZ00000000000000000000A").Image); has {
		t.Error("a build that failed left an image")
	}
}

func TestABuildWithNoDockerfileSaysWhereItLooked(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "No Dockerfile", map[string]string{"web/index.js": "1"})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Context, req.Builder = "web", resource.BuilderDockerfile
	err := b.builder.Build(b.ctx(), req)
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, "has no Dockerfile at web/Dockerfile") {
		t.Errorf("a build with no Dockerfile says %q", plain)
	}
}

func TestABuildOfACommitTheRepositoryHasntGotSaysSo(t *testing.T) {
	b := newBench(t)
	b.repo.Commit("main", "Something", map[string]string{"Dockerfile": nodeDockerfile})
	err := b.builder.Build(b.ctx(), b.request("0123456789abcdef0123456789abcdef01234567", "chg_01KBZ00000000000000000000A"))
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, "0123456 isn't on refs/heads/main of") || !strings.Contains(plain, "Deploy again to build what refs/heads/main names now") {
		t.Errorf("a commit that isn't there says %q", plain)
	}
}

// A commit that isn't on the branch the build names, as one a fork pushed, which
// GitHub serves through its parent's URL, isn't built.
func TestABuildOfACommitThatIsntOnItsBranchIsRefused(t *testing.T) {
	b := newBench(t)
	b.repo.Commit("main", "Something", map[string]string{"Dockerfile": nodeDockerfile})
	elsewhere := b.repo.Commit("fork", "Something else", map[string]string{"Dockerfile": nodeDockerfile, "evil.sh": "1"})
	err := b.builder.Build(b.ctx(), b.request(elsewhere, "chg_01KBZ00000000000000000000A"))
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, elsewhere[:7]+" isn't on refs/heads/main of") || !strings.Contains(plain, "a build's commit has to be on the branch or tag it names") {
		t.Errorf("a commit from elsewhere says %q", plain)
	}
	// Named as the ref itself, it is fetched as it is: who may name one is a plan's
	// to say (WriterOf).
	req := b.request(elsewhere, "chg_01KBZ00000000000000000000B")
	req.Ref = elsewhere
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Errorf("a commit named as the ref = %v", err)
	}
	// A pull request's head, which a fork pushes, by its full name or Git's short
	// one, which would reach it, isn't built.
	b.repo.Ref("refs/pull/7/head", elsewhere)
	for _, ref := range []string{"refs/pull/7/head", "pull/7/head", "fork"} {
		req := b.request(elsewhere, "chg_01KBZ00000000000000000000C")
		req.Ref = ref
		err := b.builder.Build(b.ctx(), req)
		if plain, _ := resource.Explain(err); !strings.Contains(plain, "builds only a branch or a tag by its full name") {
			t.Errorf("a build of %q says %q", ref, plain)
		}
	}
}

func TestABuildsSecretsReachItAsFilesAndNeverItsLog(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "Use a token", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN --mount=type=secret,id=NPM_TOKEN cat /run/secrets/NPM_TOKEN\n"})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Secrets = map[string]string{"NPM_TOKEN": "npm_s3cr3t-value"}
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	for _, l := range b.lines() {
		if strings.Contains(l, "npm_s3cr3t-value") {
			t.Fatalf("the build's log holds its secret: %q", l)
		}
	}
	if !slices.Contains(b.lines(), binding.Hide("npm_s3cr3t-value", []string{"npm_s3cr3t-value"})) {
		t.Errorf("the step that printed the secret doesn't show it masked: %q", b.lines())
	}
	builder := b.engine.Builders()[0]
	for _, kv := range builder.Env {
		if strings.Contains(kv, "npm_s3cr3t-value") {
			t.Errorf("the secret is in the builder's environment: %s", kv)
		}
	}
	if strings.Contains(strings.Join(builder.Cmd, " "), "npm_s3cr3t-value") {
		t.Error("the secret is in the builder's command")
	}
	entries, err := os.ReadDir(b.builder.SecretsDir)
	if err != nil || len(entries) != 0 {
		t.Errorf("the build's secrets are still on the server once it ended: %v %v", entries, err)
	}
}

// A step that prints a secret and fails puts it in none of what keeps a failure:
// the step's error, the change's, or the line the build says it said last.
func TestABuildThatFailsAfterPrintingASecretKeepsItMasked(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "Use a token", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN --mount=type=secret,id=NPM_TOKEN cat /run/secrets/NPM_TOKEN && exit 1\n"})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Secrets = map[string]string{"NPM_TOKEN": "npm_s3cr3t-value"}
	err := b.builder.Build(b.ctx(), req)
	if err == nil {
		t.Fatal("a step that exits 1 built")
	}
	plain, detail := resource.Explain(err)
	f := b.final()
	for what, s := range map[string]string{"the error": plain, "its detail": detail, "the build's error": f.Error, "the failing line": f.FailedLine, "the step": f.StepName} {
		if strings.Contains(s, "npm_s3cr3t") {
			t.Errorf("%s holds the secret: %q", what, s)
		}
	}
	if f.FailedLine != binding.Masked {
		t.Errorf("the build says its step printed %q last, not the masked line", f.FailedLine)
	}
	if resource.ReasonOf(err) != resource.ReasonBuild {
		t.Errorf("a masked failure lost its reason: %q", resource.ReasonOf(err))
	}
}

// A key of more than one line, stored with a newline at its end, as a PEM file is,
// is masked line by line, as a log prints it.
func TestAKeyOfManyLinesIsMaskedLineByLine(t *testing.T) {
	b := newBench(t)
	key := "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\nbm90IGEgcmVhbCBrZXkgYXQgYWxs\n-----END PRIVATE KEY-----\n"
	commit := b.repo.Commit("main", "Use a key", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN --mount=type=secret,id=KEY cat /run/secrets/KEY\n"})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Secrets = map[string]string{"KEY": key}
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	for _, l := range b.lines() {
		if strings.Contains(l, "MIIEvQ") || strings.Contains(l, "bm90IGE") {
			t.Fatalf("the build's log holds a line of the key: %q", l)
		}
	}
}

func TestABuildWithSecretsNeedsSomewhereInMemoryForThem(t *testing.T) {
	b := newBench(t)
	b.builder.InMemory = func(string) bool { return false }
	commit := b.repo.Commit("main", "Use a token", map[string]string{"Dockerfile": nodeDockerfile})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	req.Secrets = map[string]string{"NPM_TOKEN": "x"}
	err := b.builder.Build(b.ctx(), req)
	if plain, _ := resource.Explain(err); !strings.Contains(plain, "nowhere in memory") {
		t.Errorf("a server with nowhere in memory for secrets says %q", plain)
	}
	if len(b.engine.Builders()) != 0 {
		t.Error("a build ran with nowhere to keep its secrets")
	}
}

func TestSomeoneCanCancelABuild(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "Slow", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo working && sleep 30\n"})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	done := make(chan error, 1)
	go func() { done <- b.builder.Build(b.ctx(), req) }()
	waitFor(t, func() bool { return slices.Contains(b.lines(), "working") })
	if !b.builder.Cancel(req.Change, web, "Mira Chen") {
		t.Fatal("the build under way couldn't be cancelled")
	}
	select {
	case err := <-done:
		if resource.ReasonOf(err) != resource.ReasonCancelled {
			t.Fatalf("a cancelled build failed with %v", err)
		}
		if plain, _ := resource.Explain(err); !strings.Contains(plain, "cancelled by Mira Chen") {
			t.Errorf("a cancelled build says %q", plain)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled build went on")
	}
	if f := b.final(); f.State != resource.BuildCancelled {
		t.Errorf("a cancelled build ended as %v", f.State)
	}
	if b.builder.Cancel(req.Change, web, "Mira Chen") {
		t.Error("a build that ended can still be cancelled")
	}
}

func TestOneBuildAtATimeOnAServer(t *testing.T) {
	b := newBench(t)
	slow := b.repo.Commit("main", "Slow", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo first && sleep 2\n"})
	first := make(chan error, 1)
	go func() { first <- b.builder.Build(b.ctx(), b.request(slow, "chg_01KBZ00000000000000000000A")) }()
	waitFor(t, func() bool { return slices.Contains(b.lines(), "first") })
	if err := b.builder.Build(b.ctx(), b.request(slow, "chg_01KBZ00000000000000000000B")); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	said := slices.Clone(b.said)
	b.mu.Unlock()
	if !slices.Contains(said, "Waiting for another build on this server to finish") {
		t.Errorf("a second build didn't say it waited for the first: %q", said)
	}
}

func TestABuildThatRunsOutOfMemorySaysSo(t *testing.T) {
	b := newBench(t)
	b.builder.Memory = func() (int64, int64, error) { return 600 << 20, 1 << 30, nil }
	commit := b.repo.Commit("main", "Big", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN make oom\n"})
	err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A"))
	plain, _ := resource.Explain(err)
	if !strings.Contains(plain, "ran out of the 512 MB it may use") {
		t.Errorf("a build that ran out of memory says %q", plain)
	}
}

func TestABuildsLimitsLeaveRoomForWhatRuns(t *testing.T) {
	for _, c := range []struct {
		name               string
		available          int64
		cpus               int
		wantMemory, wantNs int64
	}{
		{"a server of 1 GB", 600 << 20, 1, 512 << 20, 1e9},
		{"a server of 4 GB", 3 << 30, 2, 3<<30 - 256<<20, 1e9},
		{"a large server", 60 << 30, 16, 8 << 30, 15e9},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t)
			b.engine.NCPU = 16
			b.builder.Memory = func() (int64, int64, error) { return c.available, 2 * c.available, nil }
			b.builder.CPUs = func() int { return c.cpus }
			commit := b.repo.Commit("main", "x", map[string]string{"Dockerfile": nodeDockerfile})
			if err := b.builder.Build(b.ctx(), b.request(commit, "chg_01KBZ00000000000000000000A")); err != nil {
				t.Fatal(err)
			}
			h := b.engine.Builders()[0].HostConfig
			if h.Memory != c.wantMemory || h.MemorySwap != c.wantMemory || h.NanoCPUs != c.wantNs {
				t.Errorf("the builder may use %d bytes, %d with swap, and %d nano-CPUs; want %d, no swap, and %d", h.Memory, h.MemorySwap, h.NanoCPUs, c.wantMemory, c.wantNs)
			}
			if h.OomScoreAdj != 1000 || h.CPUShares != 256 || h.PidsLimit != 4096 {
				t.Errorf("the builder runs with %+v, not first to go, at a quarter of the weight, and 4096 processes", h)
			}
		})
	}
}

func TestAServiceKeepsItsNewestBuiltImages(t *testing.T) {
	b := newBench(t)
	var images []string
	for i := range 6 {
		commit := b.repo.Commit("main", "Change "+string(rune('a'+i)), map[string]string{"Dockerfile": nodeDockerfile, "n": string(rune('a' + i))})
		req := b.request(commit, "chg_01KBZ0000000000000000000"+string(rune('A'+i))+"A")
		if err := b.builder.Build(b.ctx(), req); err != nil {
			t.Fatal(err)
		}
		images = append(images, req.Image)
		time.Sleep(10 * time.Millisecond)
	}
	for i, img := range images {
		has, err := b.builder.Has(t.Context(), img)
		if err != nil {
			t.Fatal(err)
		}
		if want := i >= 2; has != want {
			t.Errorf("build %d's image is kept: %v, want %v", i+1, has, want)
		}
	}
}

func TestAnOldBuiltImageAContainerRunsStays(t *testing.T) {
	b := newBench(t)
	c := b.engine.Client()
	var images []string
	for i := range 6 {
		commit := b.repo.Commit("main", "Change "+string(rune('a'+i)), map[string]string{"Dockerfile": nodeDockerfile, "n": string(rune('a' + i))})
		req := b.request(commit, "chg_01KBZ0000000000000000000"+string(rune('A'+i))+"A")
		if err := b.builder.Build(b.ctx(), req); err != nil {
			t.Fatal(err)
		}
		images = append(images, req.Image)
		if i == 0 {
			// The first runs, as after a rollback to it.
			if _, err := c.CreateContainer(t.Context(), "web-1", &docker.CreateRequest{Config: docker.Config{Image: req.Image}}); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if has, _ := b.builder.Has(t.Context(), images[0]); !has {
		t.Error("an old built image a container runs was removed")
	}
	if has, _ := b.builder.Has(t.Context(), images[1]); has {
		t.Error("an old built image nothing runs stayed")
	}
}

func TestAnImageTheServerHasIsntBuiltAgain(t *testing.T) {
	b := newBench(t)
	commit := b.repo.Commit("main", "x", map[string]string{"Dockerfile": nodeDockerfile})
	req := b.request(commit, "chg_01KBZ00000000000000000000A")
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	if err := b.builder.Build(b.ctx(), req); err != nil {
		t.Fatal(err)
	}
	if n := len(b.engine.Builders()); n != 1 {
		t.Errorf("building an image the server has ran %d builders, not 1", n)
	}
}

func TestCleanRemovesWhatACrashLeft(t *testing.T) {
	b := newBench(t)
	c := b.engine.Client()
	b.engine.AddLocalImage(Image, dockertest.Image{})
	if _, err := c.CreateContainer(t.Context(), "dockwell-build-left", &docker.CreateRequest{Config: docker.Config{Image: Image, Labels: map[string]string{docker.LabelBuilder: "chg_x"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b.builder.SecretsDir+"/build-123", 0o700); err != nil {
		t.Fatal(err)
	}
	b.builder.Clean(t.Context())
	if left := b.engine.Containers(); len(left) != 0 {
		t.Errorf("a builder a crash left is still there: %v", left)
	}
	if _, err := os.Stat(b.builder.SecretsDir + "/build-123"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a build's secrets a crash left are still there: %v", err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("it never happened")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
