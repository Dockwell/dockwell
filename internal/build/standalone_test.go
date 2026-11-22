// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

func TestReadsTheReviewedGitDockerfileAtRootAndInASubdirectory(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test",
			"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, data)
		}
		return strings.TrimSpace(string(data))
	}
	git("init", "-q")
	if err := os.Mkdir(filepath.Join(repo, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"Dockerfile": "FROM scratch\n# root\n", "nested/Dockerfile": "FROM scratch\n# nested\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "Fixture")
	commit := git("rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(repo, "nested", "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ context, want string }{{"", "root"}, {".", "root"}, {"nested", "nested"}} {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-ec", `say() { echo "$*"; }`+detectCommitDockerfile+`git cat-file blob "$dfobj"`)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "DW_CONTEXT="+tc.context, "DW_COMMIT="+commit, "DW_DOCKERFILE=Dockerfile")
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != "FROM scratch\n# "+tc.want+"\n" {
			t.Fatalf("context %q read %q: %v", tc.context, out, err)
		}
	}
}

func TestBuildExplainsAThreadedCgroupStartFailure(t *testing.T) {
	b, req := standaloneBench(t)
	b.engine.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/start") {
			return errors.New(`runc create failed: cannot enter cgroupv2 "/sys/fs/cgroup/docker" with domain controllers: it is in threaded mode`)
		}
		return nil
	})
	err := b.builder.Build(b.ctx(), req)
	if err == nil || resource.ReasonOf(err) != resource.ReasonBuild || !strings.Contains(b.final().Error, "Configure Docker to use a domain cgroup") {
		t.Fatalf("startup error = %v, build says %q", err, b.final().Error)
	}
}

func TestStandalonePolicyPinsOnlyTheReviewedNativeBases(t *testing.T) {
	index, manifest := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	const platform = "linux/amd64"
	refs, err := StandaloneReferences([]byte("FROM alpine:3 AS base\nFROM base\nCOPY --from=registry.example.test/app:review@" + index + " /payload /payload\n"))
	if err != nil || len(refs) != 2 {
		t.Fatalf("base images = %v, %v", refs, err)
	}
	bases := []*v1.DockerRegistryDescriptor{
		{Repository: "alpine", ManifestDigest: manifest, Platform: platform},
		{Repository: "registry.example.test/app", IndexDigest: index, ManifestDigest: manifest, Platform: platform},
	}
	policy, err := StandalonePolicy(refs, bases, platform)
	if err != nil {
		t.Fatal(err)
	}
	// Inspect the on-the-wire BuildKit policy, including order and match kind.
	type rule struct {
		action, match int
		from, to      string
	}
	var wire struct {
		Version int
		Rules   []struct {
			Action   int
			Selector struct {
				Identifier string
				Match      int `json:"match_type"`
			}
			Updates struct{ Identifier string }
		}
	}
	if err := json.Unmarshal([]byte(policy), &wire); err != nil || wire.Version != 1 {
		t.Fatalf("source policy: %s, %v", policy, err)
	}
	var got []rule
	for _, r := range wire.Rules {
		got = append(got, rule{r.Action, r.Selector.Match, r.Selector.Identifier, r.Updates.Identifier})
	}
	const hub, private = "docker-image://docker.io/library/alpine", "docker-image://registry.example.test/app"
	want := []rule{
		{1, 0, "docker-image://*", ""},
		{1, 0, "http*://*", ""},
		{1, 0, "git://*", ""},
		{2, 1, hub + ":3", hub + "@" + manifest},
		{0, 1, hub + "@" + manifest, ""},
		{2, 1, private + ":review@" + index, private + "@" + manifest},
		{2, 1, private + "@" + index, private + "@" + manifest},
		{0, 1, private + "@" + manifest, ""},
	}
	if !slices.Equal(want, got) {
		t.Fatalf("policy rules: %v", got)
	}
	for _, wrong := range []*v1.DockerRegistryDescriptor{
		{Repository: "registry.example.test/other", IndexDigest: index, ManifestDigest: manifest, Platform: platform},
		{Repository: "registry.example.test/app", IndexDigest: index, ManifestDigest: manifest, Platform: "linux/arm64"},
		{Repository: "registry.example.test/app", ManifestDigest: manifest, Platform: platform},
	} {
		if _, err := StandalonePolicy(refs, []*v1.DockerRegistryDescriptor{bases[0], wrong}, platform); err == nil {
			t.Fatalf("accepted unrelated base evidence: %v", wrong)
		}
	}
}

func TestStandaloneDockerfileRefusesUnsupportedSourcesAndEntitlements(t *testing.T) {
	for name, file := range map[string]string{
		"official external frontend": "# syntax=docker/dockerfile:1\nFROM scratch",
		"custom frontend":            "# syntax=registry.example.test/frontend\nFROM scratch",
		"remote context":             "FROM scratch\nADD https://registry.example.test/src.tar /src",
		"remote JSON context":        `FROM scratch` + "\n" + `ADD ["https://registry.example.test/src.tar","/src"]`,
		"remote Git context":         "FROM scratch\nADD git@registry.example.test:fixture.git /src",
		"host network":               "FROM scratch\nRUN --network=host true",
		"insecure security mode":     "FROM scratch\nRUN --security=insecure true",
		"device":                     "FROM scratch\nRUN --device=fixture true",
		"secret":                     "FROM scratch\nRUN --mount=type=secret,id=fixture true",
		"ssh":                        "FROM scratch\nRUN --mount=type=ssh true",
		"unresolved base":            "ARG BASE\nFROM $BASE",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := StandaloneReferences([]byte(file)); err == nil {
				t.Fatal("the source passed review")
			}
		})
	}
	if _, err := StandaloneReferences([]byte("FROM scratch\nADD ./files /src\nRUN --network=none --mount=type=cache,target=/cache true")); err != nil {
		t.Fatalf("a local source was refused: %v", err)
	}
}

func standaloneBench(t *testing.T) (*bench, service.BuildRequest) {
	t.Helper()
	b := newBench(t)
	b.builder.DiskFree = func(string) (int64, error) { return 40 << 30, nil }
	req := service.BuildRequest{
		Service: web, Change: "fixture-change", Image: "fixture/output:dockwell-build", Builder: resource.BuilderDockerfile,
		Standalone: &service.StandaloneBuild{
			Context: t.TempDir(), Scratch: t.TempDir(), Platform: "linux/amd64", CacheScope: "public", Dockerfile: "FROM scratch\n",
			Prepare: func(_ context.Context, _ *service.BuildRequest, run func() error) error { return run() },
			Publish: func(ctx context.Context, begin func() error, load func(context.Context) error) error {
				finishing, done := resource.Finishing(ctx, 3*time.Second)
				defer done()
				if err := begin(); err != nil {
					return err
				}
				return load(finishing)
			},
		},
	}
	return b, req
}

func TestStandalonePublicationProtectsTheDiskFloor(t *testing.T) {
	t.Run("refuse before loading", func(t *testing.T) {
		b, req := standaloneBench(t)
		b.builder.DiskFree = func(string) (int64, error) { return floor(50<<30) + (64 << 20), nil }
		var loads atomic.Int32
		b.engine.Intercept(func(r *http.Request) error {
			if strings.HasSuffix(r.URL.Path, "/images/load") {
				loads.Add(1)
			}
			return nil
		})
		if err := b.builder.Build(b.ctx(), req); resource.ReasonOf(err) != resource.ReasonDisk || loads.Load() != 0 {
			t.Fatalf("disk preflight: %v; %d loads", err, loads.Load())
		}
	})
	t.Run("stop a started load", func(t *testing.T) {
		b, req := standaloneBench(t)
		var free atomic.Int64
		free.Store(40 << 30)
		b.builder.DiskFree = func(string) (int64, error) { return free.Load(), nil }
		b.builder.WatchEvery = 5 * time.Millisecond
		cancelled := make(chan struct{})
		b.engine.Intercept(func(r *http.Request) error {
			if !strings.HasSuffix(r.URL.Path, "/images/load") {
				return nil
			}
			_, _ = io.Copy(io.Discard, r.Body)
			free.Store(floor(50<<30) - 1)
			select {
			case <-r.Context().Done():
				close(cancelled)
			case <-time.After(2 * time.Second):
			}
			return errors.New("the fixture interrupted the load")
		})
		if err := b.builder.Build(b.ctx(), req); resource.ReasonOf(err) != resource.ReasonDisk {
			t.Fatalf("a disk drop during load: %v", err)
		}
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("the load request stayed open under the disk floor")
		}
	})
}

func TestStandaloneCancelRespectsPublication(t *testing.T) {
	for _, started := range []bool{false, true} {
		name := "before handoff"
		if started {
			name = "while Docker takes the image"
		}
		t.Run(name, func(t *testing.T) {
			b, req := standaloneBench(t)
			atGate, continueGate := make(chan struct{}), make(chan struct{})
			defer close(continueGate)
			publish := req.Standalone.Publish
			req.Standalone.Publish = func(ctx context.Context, begin func() error, load func(context.Context) error) error {
				wait := func() error {
					close(atGate)
					select {
					case <-continueGate:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				if !started {
					if err := wait(); err != nil {
						return err
					}
				}
				return publish(ctx, begin, func(ctx context.Context) error {
					if started {
						close(atGate)
						<-continueGate
					}
					return load(ctx)
				})
			}
			done := make(chan error, 1)
			go func() { done <- b.builder.Build(b.ctx(), req) }()
			select {
			case <-atGate:
			case err := <-done:
				t.Fatalf("build never reached publication: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("build never reached publication")
			}
			stopping, err := b.builder.CancelWithStatus(req.Change, req.Service, "Fixture")
			if started {
				if stopping || resource.ReasonOf(err) != resource.ReasonBuildPublishing {
					t.Fatalf("cancel after handoff = %v, %v", stopping, err)
				}
				// Let publication finish before this test checks the tag.
				continueGate <- struct{}{}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if _, err := b.engine.Client().InspectImage(t.Context(), req.Image); err != nil {
					t.Fatal(err)
				}
				return
			}
			if !stopping || err != nil {
				t.Fatalf("cancel before handoff = %v, %v", stopping, err)
			}
			if err := <-done; resource.ReasonOf(err) != resource.ReasonCancelled || b.final().State != resource.BuildCancelled {
				t.Fatalf("cancelled build = %v, %v", b.final().State, err)
			}
			if _, err := b.engine.Client().InspectImage(t.Context(), req.Image); !docker.IsNotFound(err) {
				t.Fatalf("the cancelled build reached Docker: %v", err)
			}
		})
	}
}
