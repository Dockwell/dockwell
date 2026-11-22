// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/builds"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
)

// Folder staging may copy 4 GiB and walk 50,000 included entries on a slow disk.
const folderSnapshotTimeout = 10 * time.Minute

// SetBuilds connects the standalone log and Git resolution to the existing recorder.
func (h *Handler) SetBuilds(b *builds.Handler) { h.builds = b }

func (h *Handler) planDockerBuild(ctx context.Context, m *v1.PlanDockerMaintenanceRequest) (*connect.Response[v1.PlanDockerMaintenanceResponse], error) {
	if len(m.GetServerIds()) != 1 || len(m.GetSummary()) > 200 {
		return nil, authapi.Invalid(errors.New("choose one server for the image build"))
	}
	if h.builds == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("image builds are unavailable"))
	}
	server := m.GetServerIds()[0]
	a, err := h.artifactAccess(ctx, server)
	if err != nil {
		return nil, err
	}
	b := proto.CloneOf(m.GetBuild())
	raw, err := registry.Parse(b.GetOutput())
	if err != nil || raw.Digest != "" || raw.Registry == docker.BuiltRegistry || len(raw.Tag) > 96 {
		return nil, authapi.Invalid(errors.New("choose a local image repository and optional tag prefix"))
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, h.internal(ctx, err)
	}
	prefix := ""
	if raw.Tag != "" && raw.Tag != "latest" {
		prefix = raw.Tag + "-"
	}
	raw.Tag = prefix + "dockwell-" + hex.EncodeToString(nonce[:])
	b.Output = raw.String()
	if b.DockerfilePath == "" {
		b.DockerfilePath = "Dockerfile"
	}
	if !artifact.ContextPath(b.GetDockerfilePath()) || len(b.GetRegistryLoginIds()) > 32 {
		return nil, authapi.Invalid(errors.New("choose a Dockerfile inside the context and up to 32 registry logins"))
	}
	sources := 0
	for _, set := range []bool{b.GetFolderPath() != "", b.GetGit() != nil, b.GetContextArtifactId() != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return nil, authapi.Invalid(errors.New("choose one build source"))
	}
	action := &v1.DockerImageAction{Build: b, CredentialProject: b.GetCredentialProject()}
	if override := b.GetDockerfileOverrideArtifactId(); override != "" {
		staged, err := a.status(ctx, override)
		if err != nil || staged.GetKind() != artifact.Dockerfile || staged.GetState() != artifact.Ready {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("finish uploading the Dockerfile override before review"))
		}
	}
	ids := slices.Compact(slices.Sorted(slices.Values(b.GetRegistryLoginIds())))
	hosts := map[string]bool{}
	for _, key := range ids {
		ref, _, err := a.session.DockerBuildLogin(ctx, b.GetCredentialProject(), key)
		if err != nil || hosts[ref.GetHost()] {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("choose one eligible login per base-image registry"))
		}
		action.ApprovedLogins = append(action.ApprovedLogins, ref)
		hosts[ref.GetHost()] = true
	}
	b.RegistryLoginIds = ids
	var made string
	defer func() {
		if made != "" {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer done()
			_, _ = a.abort(cleanup, made)
		}
	}()
	switch {
	case b.GetGit() != nil:
		b.Git, action.GitCommit, err = h.builds.ResolveDockerGit(ctx, b.GetGit(), b.GetDockerfilePath())
		if err != nil {
			return nil, err
		}
	default:
		var staged *v1.DockerArtifactInfo
		if b.GetFolderPath() != "" {
			staged, err = artifactResult(a.session.Artifact(ctx, &agentv1.DockerArtifactControlCall{
				OwnerBinding: a.owner, Operation: &agentv1.DockerArtifactControlCall_SnapshotFolder{
					SnapshotFolder: &agentv1.DockerArtifactFolder{Path: b.GetFolderPath(), DockerfilePath: b.GetDockerfilePath(), OverrideArtifactId: b.GetDockerfileOverrideArtifactId()},
				},
			}, folderSnapshotTimeout))
			made = staged.GetArtifactId()
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("copying the build folder took more than 10 minutes. Exclude generated files or upload a context archive"))
			}
		} else {
			staged, err = a.status(ctx, b.GetContextArtifactId())
		}
		if err != nil {
			return nil, err
		}
		if staged.GetKind() != artifact.BuildContext || staged.GetState() != artifact.Ready {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("finish uploading the build context before review"))
		}
		action.StagedSourceId, action.SourceSha256 = staged.GetArtifactId(), staged.GetSha256()
	}
	ref := resource.Ref{Kind: host.Kind, Scope: server, Name: host.Name}
	prepared, err := a.session.PreviewDockerImage(ctx, ref, action, registry.Auth{})
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	action.BaseDescriptors, action.DockerfileSha256, b.Platform = prepared.GetBaseDescriptors(), prepared.GetDockerfileSha256(), prepared.GetBuild().GetPlatform()
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = "Build Docker image"
	}
	var pinned []*agentv1.DockerArtifactPin
	change, err := h.changes.PlanDockerArchive(ctx, summary, server, action, func(ctx context.Context, changeID string) error {
		for _, lease := range resource.DockerBuildLeases(action) {
			pin := &agentv1.DockerArtifactPin{ArtifactId: lease, ChangeId: changeID}
			_, err := artifactResult(a.session.Artifact(ctx, &agentv1.DockerArtifactControlCall{
				ArtifactId: lease, OwnerBinding: a.owner, Operation: &agentv1.DockerArtifactControlCall_Pin{Pin: pin},
			}, time.Minute))
			if err != nil {
				return err
			}
			pinned = append(pinned, pin)
		}
		return nil
	})
	if err != nil {
		for _, pin := range pinned {
			releaseArchivePin(ctx, a.session, pin)
		}
		return nil, err
	}
	made = ""
	return connect.NewResponse(&v1.PlanDockerMaintenanceResponse{Change: change}), nil
}

// WatchDockerImageBuild reuses the retained log stream with install-scoped access.
func (h *Handler) WatchDockerImageBuild(ctx context.Context, req *connect.Request[v1.WatchDockerImageBuildRequest], out *connect.ServerStream[v1.WatchBuildResponse]) error {
	if h.builds == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("image builds are unavailable"))
	}
	return h.builds.WatchImage(ctx, req.Msg, out.Send)
}

// CancelDockerImageBuild cancels only the selected standalone build.
func (h *Handler) CancelDockerImageBuild(ctx context.Context, req *connect.Request[v1.CancelDockerImageBuildRequest]) (*connect.Response[v1.CancelBuildResponse], error) {
	if h.builds == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("image builds are unavailable"))
	}
	return h.builds.CancelImage(ctx, req.Msg)
}
