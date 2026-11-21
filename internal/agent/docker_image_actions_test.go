// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
)

func TestReviewedAliasRechecksInfrastructureBeforeWriting(t *testing.T) {
	t.Parallel()
	c, engine := cleanupFixture(t)
	const source = "docker.io/fixture/source:latest"
	engine.AddLocalImage(source, dockertest.Image{})
	image, err := c.localImage(t.Context(), source)
	if err != nil || image == nil {
		t.Fatalf("fixture image: %v", err)
	}
	ref := resource.Ref{Kind: host.Kind, Scope: c.server, Name: host.Name}
	plan := func(target string) *v1.DockerMaintenance {
		return &v1.DockerMaintenance{ImageAction: &v1.DockerImageAction{Tag: &v1.DockerImageTag{
			SourceKey: &v1.DockerImageSourceKey{ServerId: c.server, ImageId: image.ID}, Destination: target, MoveExisting: true,
		}}}
	}
	if err := c.previewDockerImage(t.Context(), ref, plan("docker.io/moby/buildkit:fixture"), nil); !errors.Is(err, errImageInfrastructure) {
		t.Fatalf("infrastructure tag preview = %v", err)
	}
	const target = "docker.io/fixture/custom-ingress:current"
	engine.AddLocalImage(target, dockertest.Image{})
	old, err := c.localImage(t.Context(), target)
	if err != nil || old == nil {
		t.Fatalf("fixture alias: %v", err)
	}
	p := plan(target)
	if err := c.previewDockerImage(t.Context(), ref, p, nil); err != nil {
		t.Fatal(err)
	}
	p.ChangeId = id.New(id.Change)
	c.cfg.DockerIngressImage = target
	refused, err := c.applyImageAction(t.Context(), ref, p, nil, &agentv1.DockerCleanupProtection{})
	if !errors.Is(err, errImageInfrastructure) || refused.GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_PROTECTED {
		t.Fatalf("infrastructure override added between review and apply: %v, %v", refused, err)
	}
	if current, err := c.localImage(t.Context(), target); err != nil || imageID(current) != old.ID {
		t.Fatalf("the protected alias was retargeted: %v, %v", current, err)
	}
}

func TestCleanupDistinguishesContainerdDigestReferencesFromTags(t *testing.T) {
	t.Parallel()
	c, _ := cleanupFixture(t)
	imageID := "sha256:" + strings.Repeat("a", 64)
	digest := "fixture/image@" + imageID
	raw := docker.DiskUsageImage{ID: imageID, RepoTags: []string{digest}, RepoDigests: []string{digest}}
	got := c.imageCleanupTarget(raw, dockerOccupancy{}, nil)
	if got.GetReason() != reasonUnused || len(got.GetImageTags()) != 0 || len(got.GetImageDigests()) != 1 || got.GetImageDigests()[0] != digest {
		t.Fatalf("one canonical reference was counted twice: %v", got)
	}
	raw.RepoTags = append(raw.RepoTags, "fixture/image:tag")
	got = c.imageCleanupTarget(raw, dockerOccupancy{}, nil)
	if got.GetReason() != reasonUnavailable || got.GetEvidence() != "" {
		t.Fatalf("Engine cannot delete two independent records by ID without force: %v", got)
	}
}

func TestAnImageTagRetryChecksItsJournalBeforeWriting(t *testing.T) {
	t.Parallel()
	c, engine := cleanupFixture(t)
	c.cfg.images = newImages(c.cfg.Docker, ImageReads{}, time.Now)
	const source, target = "docker.io/fixture/journal:source", "docker.io/fixture/journal:target"
	engine.AddLocalImage(source, dockertest.Image{})
	image, err := c.localImage(t.Context(), source)
	if err != nil || image == nil {
		t.Fatalf("fixture image: %v", err)
	}
	ref := resource.Ref{Kind: host.Kind, Scope: c.cfg.Identity.ServerID, Name: host.Name}
	key := &v1.DockerImageSourceKey{ServerId: ref.Scope, ImageId: image.ID}
	plan := &v1.DockerMaintenance{ImageAction: &v1.DockerImageAction{Tag: &v1.DockerImageTag{SourceKey: key, Destination: target}}}
	if err := c.previewDockerImage(t.Context(), ref, plan, nil); err != nil {
		t.Fatal(err)
	}
	plan.ChangeId = id.New(id.Change)
	protected := &agentv1.DockerCleanupProtection{Images: []string{"fixture/journal:target"}}
	got, err := c.applyImageAction(t.Context(), ref, plan, nil, protected)
	if err == nil || got.GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_PROTECTED {
		t.Fatalf("the absent alias was reserved by a workload: %v, %v", got, err)
	}

	dir := filepath.Join(c.cfg.Dir, "docker-image-actions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, plan.GetChangeId()+".json")
	if err := saveImageJournal(file, &v1.DockerImageReceipt{Reference: target, ImageKey: key}); err != nil {
		t.Fatal(err)
	}
	apply := func() (*v1.DockerImageReceipt, error) {
		return c.applyImageAction(t.Context(), ref, plan, nil, &agentv1.DockerCleanupProtection{})
	}
	got, err = apply()
	if err == nil || got.GetOutcome() != v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN {
		t.Fatalf("an unfinished write was retried: %v, %v", got, err)
	}
	if current, err := c.localImage(t.Context(), target); current != nil || err != nil {
		t.Fatalf("retry wrote the absent alias: %v, %v", current, err)
	}
	if err := c.cfg.Docker.TagImage(t.Context(), image.ID, target); err != nil {
		t.Fatal(err)
	}
	got, err = apply()
	if err != nil || got.GetOutcome() != v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED {
		t.Fatalf("retry did not verify the earlier write: %v, %v", got, err)
	}
	if err := c.cfg.Docker.UntagImage(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	got, err = apply()
	if err != nil || got.GetOutcome() != v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED {
		t.Fatalf("the saved receipt did not survive a later alias change: %v, %v", got, err)
	}
	if current, err := c.localImage(t.Context(), target); current != nil || err != nil {
		t.Fatalf("reading a saved receipt wrote an alias: %v, %v", current, err)
	}
}
