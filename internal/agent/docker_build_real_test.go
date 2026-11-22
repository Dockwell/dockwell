// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"net/http/cgi"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/build"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

func TestBuildImageFromSealedContextOnRealDocker(t *testing.T) {
	d := dockertest.Real(t)
	ctx, stop := context.WithTimeout(t.Context(), 12*time.Minute)
	defer stop()
	c := cleanupCalls(t, d)
	var logMu sync.Mutex
	logs := map[string][]string{}
	c.send = func(m *agentv1.ConnectRequest) error {
		u := m.GetBuild()
		if u != nil {
			logMu.Lock()
			defer logMu.Unlock()
			for _, line := range u.GetLines() {
				logs[u.GetChangeId()] = append(logs[u.GetChangeId()], line.GetText())
			}
		}
		return nil
	}
	spool, err := artifact.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.artifacts, c.cfg.images = spool, newImages(d, ImageReads{}, time.Now)
	builder := &build.Builder{Client: d, StateDir: c.cfg.Dir, SecretsDir: t.TempDir()}
	c.cfg.Builds = builder
	c.cfg.Files = files.New(c.cfg.Dir, nil, nil)
	const owner = "test-build/session"
	ref := resource.Ref{Kind: host.Kind, Scope: c.cfg.Identity.ServerID, Name: host.Name}
	repo := cleanupRealRepo(t)
	network := realBuildNetwork(t, d)
	gateway := network.IPAM.Config[0].Gateway
	remote := startImageRegistry(ctx, t)
	address := exposeBuildRegistry(t, gateway, remote)
	builder.PlainRegistry = address
	c.cfg.images.newClient = func(o registry.Options) *registry.Client {
		o.Insecure = func(host string) bool { return host == address }
		return registry.New(o)
	}
	base := address + "/fixture/build-base:review"
	seedBuildBase(t, d, remote, "review", "original base contents")
	seedBuildBase(t, d, remote, "next", "replacement base contents")
	// The real test keeps its shared public cache by design. Its fixture images
	// are unique and removed even if an assertion fails after publishing them.
	run := func(t *testing.T, a *v1.DockerImageAction) {
		t.Helper()
		out := a.GetBuild().GetOutput()
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			current, err := d.InspectImage(cleanup, out)
			if err == nil {
				_, _ = d.RemoveUnusedImage(cleanup, current.ID)
			}
		})
		plan := &v1.DockerMaintenance{ImageAction: a}
		desired := &agentv1.DesiredResource{Resource: resourcepb.Ref(ref), DockerMaintenance: plan}
		if err := c.previewImageBuild(ctx, ref, desired); err != nil {
			plain, detail := resource.Explain(err)
			t.Fatalf("reviewing %s: %s (%s)", out, plain, detail)
		}
		if a.GetDockerfileSha256() == "" || a.GetBuild().GetPlatform() == "" {
			t.Fatal("the reviewed build has no Dockerfile or platform evidence")
		}
		if a.GetBuild().GetFolderPath() != "" {
			if len(a.GetBaseDescriptors()) != 1 || !registry.IsDigest(a.GetBaseDescriptors()[0].GetManifestDigest()) {
				t.Fatalf("the review didn't pin its registry base: %v", a.GetBaseDescriptors())
			}
			reviewedDigest := a.GetBaseDescriptors()[0].GetManifestDigest()
			remote.moveManifest(ctx, t, "fixture/build-base", "next", "review")
			baseRef, err := registry.Parse(base)
			if err != nil {
				t.Fatal(err)
			}
			moved, err := c.resolveDockerImage(ctx, baseRef)
			if err != nil || moved.GetManifestDigest() == reviewedDigest {
				t.Fatalf("fixture tag did not move: %v, %v", moved, err)
			}
		}
		plan.ChangeId = id.New(id.Change)
		for _, lease := range resource.DockerBuildLeases(a) {
			if _, err := spool.Pin(&agentv1.DockerArtifactPin{ArtifactId: lease, ChangeId: plan.ChangeId}, owner); err != nil {
				t.Fatal(err)
			}
		}
		receipt, err := c.applyImageBuild(ctx, ref, desired, &agentv1.DockerCleanupProtection{})
		if err != nil || !docker.IsImageID(receipt.GetImageKey().GetImageId()) || receipt.GetReference() != out {
			logMu.Lock()
			lines := strings.Join(logs[plan.ChangeId], "\n")
			logMu.Unlock()
			if err != nil {
				plain, detail := resource.Explain(err)
				t.Logf("Runtime error: %s (%s)", plain, detail)
			}
			t.Fatalf("reviewed build: %v\nBuildKit log:\n%s", receipt, lines)
		}
		image, err := d.InspectImage(ctx, out)
		if err != nil || image.ID != receipt.GetImageKey().GetImageId() {
			t.Fatalf("published image: %v %v", image, err)
		}
		if a.GetBuild().GetFolderPath() != "" {
			requireOriginalBuildBase(ctx, t, remote, out)
		}
	}
	const sourceFile = "FROM scratch\nCOPY payload /payload\n"
	t.Run("folder snapshot", func(t *testing.T) {
		dir := t.TempDir()
		for name, body := range map[string]string{"Dockerfile": "FROM " + base + "\nCOPY payload /snapshot\n", "payload": "folder snapshot", ".dockerignore": "excluded\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("/proc/self/environ", filepath.Join(dir, "excluded")); err != nil {
			t.Fatal(err)
		}
		staged, err := c.cfg.Files.SnapshotContext(ctx, spool, owner, dir, "Dockerfile")
		if err != nil || staged.GetState() != artifact.Ready {
			t.Fatalf("snapshot: %v %v", staged, err)
		}
		if err := os.Remove(filepath.Join(dir, "payload")); err != nil {
			t.Fatal(err)
		}
		run(t, &v1.DockerImageAction{
			StagedSourceId: staged.ArtifactId, SourceSha256: staged.Sha256,
			Build: &v1.DockerImageBuild{FolderPath: dir, DockerfilePath: "Dockerfile", Output: repo + ":dockwell-folder"},
		})
	})
	t.Run("uploaded context", func(t *testing.T) {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, body := range map[string]string{"Dockerfile": sourceFile, "payload": "uploaded context"} {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o600}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		staged, err := spool.Begin(owner, artifact.BuildContext, uint64(buf.Len()), "", false, "context.tar")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := spool.Receive(ctx, staged.ArtifactId, owner, 0, uint64(buf.Len()), archiveTestFrames(buf.Bytes())); err != nil {
			t.Fatal(err)
		}
		staged, err = spool.Seal(ctx, staged.ArtifactId, owner)
		if err != nil {
			t.Fatal(err)
		}
		run(t, &v1.DockerImageAction{
			StagedSourceId: staged.ArtifactId, SourceSha256: staged.Sha256,
			Build: &v1.DockerImageBuild{ContextArtifactId: staged.ArtifactId, DockerfilePath: "Dockerfile", Output: repo + ":dockwell-upload"},
		})
	})
	t.Run("Git commit", func(t *testing.T) {
		repoURL, commit := realBuildGit(t, gateway, sourceFile)
		for _, tc := range []struct{ name, folder string }{{"root", ""}, {"nested", "nested"}} {
			t.Run(tc.name, func(t *testing.T) {
				run(t, &v1.DockerImageAction{GitCommit: commit, Build: &v1.DockerImageBuild{
					Git:            &v1.DockerImageGitSource{Repository: repoURL, Ref: "refs/heads/main", Subdirectory: tc.folder},
					DockerfilePath: "Dockerfile", Output: repo + ":dockwell-git-" + tc.name,
				}})
			})
		}
	})
}

func realBuildNetwork(t *testing.T, d *docker.Client) *docker.Network {
	t.Helper()
	ctx := t.Context()
	network, err := d.NetworkNamed(ctx, build.Network)
	if docker.IsNotFound(err) || err == nil && network == nil {
		if _, err := d.CreateNetwork(ctx, &docker.NetworkCreate{
			Name: build.Network, Driver: "bridge", CheckDuplicate: true,
			Options: map[string]string{"com.docker.network.bridge.enable_icc": "false"},
			Labels:  map[string]string{docker.LabelBuilder: "network"},
		}); err != nil && !docker.IsConflict(err) {
			t.Fatal(err)
		}
		network, err = d.NetworkNamed(ctx, build.Network)
	}
	if err != nil || network == nil || len(network.IPAM.Config) == 0 || network.IPAM.Config[0].Gateway == "" {
		t.Fatalf("builder bridge has no host gateway: %v, %v", network, err)
	}
	return network
}

// BuildKit reaches the fixture over its bridge. The test proxy presents only
// that registry, injecting its fixture credentials toward the TLS upstream.
func exposeBuildRegistry(t *testing.T, gateway string, remote *imageRegistryFixture) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", net.JoinHostPort(gateway, "0"))
	if err != nil {
		t.Fatal(err)
	}
	proxy := &httputil.ReverseProxy{Transport: remote.http.Transport, Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(&url.URL{Scheme: "https", Host: remote.address})
		r.Out.SetBasicAuth(remote.auth.Username, remote.auth.Password)
	}}
	srv := httptest.NewUnstartedServer(proxy)
	_ = srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)
	return listener.Addr().String()
}

func seedBuildBase(t *testing.T, d *docker.Client, remote *imageRegistryFixture, tag, marker string) {
	t.Helper()
	ref := remote.address + "/fixture/build-base:" + tag
	file := writeCleanupPayload(t, t.TempDir(), marker)
	if err := d.ImportFiles(t.Context(), ref, []docker.ImageFile{{Name: "/base-marker", Path: file, Mode: 0o600}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = d.RemoveUnusedImage(ctx, ref)
	})
	remote.run(t.Context(), t, "push", ref)
}

func requireOriginalBuildBase(ctx context.Context, t *testing.T, remote *imageRegistryFixture, image string) {
	t.Helper()
	created, err := remote.command(ctx, "create", image, "/unused")
	if err != nil {
		t.Fatalf("create the built image for inspection: %s: %v", created, err)
	}
	containerID := strings.TrimSpace(string(created))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = remote.command(ctx, "rm", "-fv", containerID)
	})
	data, err := remote.command(ctx, "cp", containerID+":/base-marker", "-")
	if err != nil {
		t.Fatalf("copy the base marker: %s: %v", data, err)
	}
	tr := tar.NewReader(bytes.NewReader(data))
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(tr)
	if err != nil || string(contents) != "original base contents" {
		t.Fatalf("the output used a different base: %q, %v", contents, err)
	}
}

// realBuildGit serves a fixture from the builder bridge's host gateway. Both
// the runner's Engine and the private CI daemons use the runner's host network.
func realBuildGit(t *testing.T, gateway, dockerfile string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "fixture.git")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
			"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.com")
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, data)
		}
		return strings.TrimSpace(string(data))
	}
	run("init", "-q", "-b", "main")
	if err := os.Mkdir(filepath.Join(repo, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"Dockerfile": dockerfile, "payload": "Git commit",
		"nested/Dockerfile": dockerfile, "nested/payload": "nested Git context",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-qm", "Build fixture")
	commit := run("rev-parse", "HEAD")
	backend := filepath.Join(run("--exec-path"), "git-http-backend")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", net.JoinHostPort(gateway, "0"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(&cgi.Handler{Path: backend, Dir: dir, Env: []string{"GIT_PROJECT_ROOT=" + dir, "GIT_HTTP_EXPORT_ALL=1"}})
	_ = srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL + "/fixture.git", commit
}
