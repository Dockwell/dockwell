// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/build"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/secrets"
)

func TestStandaloneCacheScopeSeparatesLoginRevisionsAndPublic(t *testing.T) {
	login := func(revision int64) []*v1.DockerImageLoginRef {
		return []*v1.DockerImageLoginRef{{Id: "rg_fixture", Name: "fixture", Host: "registry.example.test", Revision: revision}}
	}
	public, first, second := buildCacheScope(nil), buildCacheScope(login(1)), buildCacheScope(login(2))
	if public != "public" || first == public || second == public || first == second || first != buildCacheScope(login(1)) {
		t.Fatalf("cache scopes aren't isolated and stable: %q %q %q", public, first, second)
	}
}

func TestStandaloneBuildRefusesStaleLoginAtQueueExit(t *testing.T) {
	d := dockertest.New(t).Client()
	c := cleanupCalls(t, d)
	builder := &build.Builder{Client: d}
	c.cfg.Builds = builder
	c.cfg.Files = files.New(c.cfg.Dir, nil, nil)
	spool, err := artifact.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.artifacts = spool
	c.delivery, err = delivery.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	const registryHost, owner = "registry.example.test", "fixture/session"
	login := registry.Auth{Name: "fixture", Registry: registryHost, Username: "fixture", Password: "fixture-password"}
	var stale atomic.Bool
	var staleSignIns atomic.Int32
	config := `{"os":"linux","architecture":"amd64"}`
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(config)))
	manifest := fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":%q},"layers":[]}`, configDigest)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			if u, p, ok := r.BasicAuth(); ok && u == login.Username && p == login.Password {
				if !stale.Load() {
					w.WriteHeader(http.StatusOK)
					return
				}
				staleSignIns.Add(1)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifests/review"):
			_, _ = io.WriteString(w, manifest)
		case strings.HasSuffix(r.URL.Path, "/blobs/"+configDigest):
			_, _ = io.WriteString(w, config)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	c.cfg.images = imagesAt(t, remote)
	// Transport is local while the registry client sees an HTTPS login origin.
	c.cfg.images.o.Test = "https://" + registryHost
	ctx := t.Context()
	ref := resource.Ref{Kind: host.Kind, Name: host.Name, Scope: c.server}
	stage := func(base string) *agentv1.DesiredResource {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+base+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := c.cfg.Files.SnapshotContext(ctx, spool, owner, dir, "Dockerfile")
		if err != nil {
			t.Fatal(err)
		}
		return &agentv1.DesiredResource{DockerMaintenance: &v1.DockerMaintenance{ImageAction: &v1.DockerImageAction{
			ApprovedLogins: []*v1.DockerImageLoginRef{{Id: "rg_fixture", Name: login.Name, Host: login.Registry, Revision: 1}},
			StagedSourceId: info.GetArtifactId(), SourceSha256: info.GetSha256(),
			Build: &v1.DockerImageBuild{FolderPath: dir, DockerfilePath: "Dockerfile", Output: "fixture/output:dockwell-fixture"},
		}}}
	}
	seal := func(wanted *agentv1.DesiredResource) {
		t.Helper()
		purpose := resource.DockerImageAuthRef(ref, wanted.GetDockerMaintenance())
		data, err := secrets.SealValues(c.delivery.Public(), c.server, purpose, registry.AuthsValues([]registry.Auth{login}))
		if err != nil {
			t.Fatal(err)
		}
		wanted.DockerRegistryAuth = data
	}
	bad := stage(registryHost + "/fixture/base:missing")
	seal(bad)
	if err := c.previewImageBuild(ctx, ref, bad); err == nil || len(bad.GetDockerMaintenance().GetImageAction().GetBaseDescriptors()) != 0 {
		t.Fatalf("a credentialed build accepted an unverifiable source: %v", err)
	}
	wanted := stage(registryHost + "/fixture/base:review")
	seal(wanted)
	if err := c.previewImageBuild(ctx, ref, wanted); err != nil {
		t.Fatal(err)
	}
	p := wanted.GetDockerMaintenance()
	a := p.GetImageAction()
	if got, want := a.GetBaseDescriptors()[0].GetManifestDigest(), fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(manifest))); got != want {
		t.Fatalf("base digest = %s, want %s", got, want)
	}
	p.ChangeId = id.New(id.Change)
	seal(wanted)
	if _, err := spool.Pin(&agentv1.DockerArtifactPin{ArtifactId: a.GetStagedSourceId(), ChangeId: p.ChangeId}, owner); err != nil {
		t.Fatal(err)
	}
	queued := make(chan struct{}, 1)
	c.send = func(m *agentv1.ConnectRequest) error {
		if m.GetBuild().GetState() == v1.BuildState_BUILD_STATE_QUEUED {
			queued <- struct{}{}
		}
		return nil
	}
	parked := make(chan struct{})
	resume := make(chan struct{})
	resumeQueue := sync.OnceFunc(func() { close(resume) })
	defer resumeQueue()
	holder := make(chan error, 1)
	go func() {
		holder <- builder.Build(ctx, service.BuildRequest{Change: "hold-slot", Service: ref, Standalone: &service.StandaloneBuild{
			Prepare: func(ctx context.Context, _ *service.BuildRequest, _ func() error) error {
				close(parked)
				select {
				case <-resume:
					return errors.New("the fixture released the queue")
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		}})
	}()
	<-parked
	done := make(chan error, 1)
	go func() {
		_, err := c.applyImageBuild(ctx, ref, wanted, &agentv1.DockerCleanupProtection{})
		done <- err
	}()
	select {
	case <-queued:
	case err := <-done:
		t.Fatalf("build never queued: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("build never queued")
	}
	stale.Store(true)
	resumeQueue()
	if err := <-holder; err == nil {
		t.Error("the queue holder returned success without building")
	}
	if err := <-done; !errors.Is(err, errImageAuthRefused) || staleSignIns.Load() != 1 {
		t.Fatalf("queue-exit login recheck: %v, sign-ins %d", err, staleSignIns.Load())
	}
	if _, err := d.InspectImage(ctx, a.GetBuild().GetOutput()); !docker.IsNotFound(err) {
		t.Fatalf("a stale login reached publication: %v", err)
	}
}
