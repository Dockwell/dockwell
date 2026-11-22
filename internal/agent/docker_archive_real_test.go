// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
)

func TestArchiveRoundTripAndResumeOnRealDocker(t *testing.T) {
	d := dockertest.Real(t)
	ctx := t.Context()
	repo := cleanupRealRepo(t)
	tag := repo + ":source"
	file := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(file, []byte(repo), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.ImportFiles(ctx, tag, []docker.ImageFile{{Name: "archive-round-trip", Path: file, Mode: 0o600}}); err != nil {
		t.Fatal(err)
	}
	image, err := d.InspectImage(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRealImage(t, d, image.ID)
	spool, err := artifact.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	export, err := spool.Begin("test/session", artifact.Export, 0, "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	c := cleanupCalls(t, d)
	c.cfg.artifacts = spool
	ref := resource.Ref{Kind: host.Kind, Scope: "srv_cleanup", Name: host.Name}
	protection := &agentv1.DockerCleanupProtection{}
	alias, err := registry.Parse(tag)
	if err != nil {
		t.Fatal(err)
	}
	out := &v1.DockerMaintenance{ChangeId: id.New(id.Change), ImageAction: &v1.DockerImageAction{
		StagedSourceId: export.ArtifactId,
		Export:         &v1.DockerImageExport{SourceKey: &v1.DockerImageSourceKey{ServerId: ref.Scope, ImageId: image.ID}, Tags: []string{alias.String()}},
	}}
	if _, err := spool.Pin(&agentv1.DockerArtifactPin{ArtifactId: export.ArtifactId, ChangeId: out.GetChangeId()}, "test/session"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.applyArchiveAction(ctx, ref, out, protection); err != nil || len(got) != 1 || got[0].GetArtifactId() != export.ArtifactId {
		t.Fatalf("apply export: %v %v", got, err)
	}
	ready, err := spool.Status(export.ArtifactId, "test/session")
	if err != nil || ready.GetState() != artifact.Ready {
		t.Fatalf("export lease: %v %v", ready, err)
	}
	var b bytes.Buffer
	n := ready.SizeBytes / 2
	if err := spool.Send(ctx, ready.ArtifactId, "test/session", 0, n, &b); err != nil {
		t.Fatal(err)
	}
	if err := spool.Send(ctx, ready.ArtifactId, "test/session", n, ready.SizeBytes-n, &b); err != nil {
		t.Fatal(err)
	}
	data := b.Bytes()
	in, err := spool.Begin("test/session", artifact.Import, uint64(len(data)), ready.Sha256, false, "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Receive(ctx, in.ArtifactId, "test/session", 0, n, archiveTestFrames(data[:n])); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Receive(ctx, in.ArtifactId, "test/session", n, uint64(len(data))-n, archiveTestFrames(data[n:])); err != nil {
		t.Fatal(err)
	}
	sealed, err := spool.Seal(ctx, in.ArtifactId, "test/session")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveReviewedImage(ctx, image.ID); err != nil {
		t.Fatal(err)
	}
	reviewed, err := registry.Parse(repo + ":reviewed")
	if err != nil {
		t.Fatal(err)
	}
	imageID := sealed.Images[0].GetImageId()
	inPlan := &v1.DockerMaintenance{ChangeId: id.New(id.Change), ImageAction: &v1.DockerImageAction{
		StagedSourceId: sealed.ArtifactId, SourceSha256: sealed.Sha256,
		Load: &v1.DockerImageLoad{
			ArtifactId: sealed.ArtifactId, SelectedImageIds: []string{imageID},
			Tags: []*v1.DockerImageImportTag{{ImageId: imageID, Alias: reviewed.String()}},
		},
	}}
	if _, err := spool.Pin(&agentv1.DockerArtifactPin{ArtifactId: sealed.ArtifactId, ChangeId: inPlan.GetChangeId()}, "test/session"); err != nil {
		t.Fatal(err)
	}
	receipts, err := c.applyArchiveAction(ctx, ref, inPlan, protection)
	if err != nil || len(receipts) != 2 {
		t.Fatalf("apply load: %v %v", receipts, err)
	}
	loaded := receipts[0].GetImageKey().GetImageId()
	cleanupRealImage(t, d, loaded)
	if _, err := d.InspectImage(ctx, tag); !docker.IsNotFound(err) {
		t.Fatal("archive restored its original alias")
	}
	if tagged, err := d.InspectImage(ctx, reviewed.String()); err != nil || tagged.ID != loaded {
		t.Fatalf("reviewed tag: %v %v", tagged, err)
	}
	if got, err := c.applyArchiveAction(ctx, ref, inPlan, protection); err != nil || len(got) != 2 || got[0].GetImageKey().GetImageId() != loaded {
		t.Fatalf("retry load after staging removal: %v %v", got, err)
	}
}

func archiveTestFrames(data []byte) func() ([]byte, error) {
	return func() ([]byte, error) {
		if len(data) == 0 {
			return nil, io.EOF
		}
		part := data[:min(len(data), artifact.Frame)]
		data = data[len(part):]
		return part, nil
	}
}
