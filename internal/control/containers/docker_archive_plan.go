// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
)

func (h *Handler) planDockerArchive(ctx context.Context, m *v1.PlanDockerMaintenanceRequest) (*connect.Response[v1.PlanDockerMaintenanceResponse], error) {
	if len(m.GetServerIds()) != 1 || len(m.GetSummary()) > 200 {
		return nil, authapi.Invalid(errors.New("choose one server for the image archive"))
	}
	server := m.GetServerIds()[0]
	a, err := h.artifactAccess(ctx, server)
	if err != nil {
		return nil, err
	}
	ref := resource.Ref{Kind: host.Kind, Scope: server, Name: host.Name}
	action := &v1.DockerImageAction{}
	var summary string
	if load := m.GetLoad(); load != nil {
		info, err := a.status(ctx, load.GetArtifactId())
		if err != nil {
			return nil, err
		}
		if info.GetKind() != artifact.Import || info.GetState() != artifact.Ready || len(load.GetSelectedImageIds()) == 0 ||
			len(load.GetSelectedImageIds()) > 64 || len(load.GetTags()) > 128 {
			return nil, authapi.Invalid(artifact.ErrArchive)
		}
		load = proto.CloneOf(load)
		protection, err := revision.DockerCleanupProtection(ctx, h.db, server, time.Now(), false)
		if err != nil {
			return nil, h.internal(ctx, err)
		}
		for _, tag := range load.GetTags() {
			name, err := archiveAlias(tag.GetAlias())
			if err != nil {
				return nil, authapi.Invalid(err)
			}
			if resource.DockerAliasProtected(name, protection.Images) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a kept workload uses this tag. Choose another tag"))
			}
			tag.Alias = name
		}
		action.Load, action.StagedSourceId, action.SourceSha256 = load, info.GetArtifactId(), info.GetSha256()
		summary = "Import Docker image archive"
	} else {
		out := proto.CloneOf(m.GetExport())
		if out.GetSourceKey().GetServerId() != server || !docker.IsImageID(out.GetSourceKey().GetImageId()) || len(out.GetTags()) > 128 {
			return nil, authapi.Invalid(errors.New("choose one local image by full ID"))
		}
		for i, name := range out.GetTags() {
			out.Tags[i], err = archiveAlias(name)
			if err != nil {
				return nil, authapi.Invalid(err)
			}
		}
		out.Tags = slices.Compact(slices.Sorted(slices.Values(out.Tags)))
		info, err := a.begin(ctx, artifact.Export, 0, "", false, "")
		if err != nil {
			return nil, err
		}
		action.Export, action.StagedSourceId = out, info.GetArtifactId()
		summary = "Export Docker image"
	}
	prepared, err := a.session.PreviewDockerImage(ctx, ref, action, registry.Auth{})
	if err != nil {
		if action.GetExport() != nil {
			discardExport(ctx, a.session, action.GetStagedSourceId())
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if action.GetExport() != nil {
		// The Engine's native platform is part of the reviewed identity.
		action.Export.Platform = prepared.GetExport().GetPlatform()
	}
	if custom := strings.TrimSpace(m.GetSummary()); custom != "" {
		summary = custom
	}
	change, err := h.changes.PlanDockerArchive(ctx, summary, server, action, func(ctx context.Context, changeID string) error {
		lease := action.GetStagedSourceId()
		_, err := artifactResult(a.session.Artifact(ctx, &agentv1.DockerArtifactControlCall{
			ArtifactId: lease, OwnerBinding: a.owner, Operation: &agentv1.DockerArtifactControlCall_Pin{
				Pin: &agentv1.DockerArtifactPin{ArtifactId: lease, ChangeId: changeID},
			},
		}, time.Minute))
		return err
	})
	if err != nil {
		if action.GetExport() != nil {
			discardExport(ctx, a.session, action.GetStagedSourceId())
		}
		return nil, err
	}
	return connect.NewResponse(&v1.PlanDockerMaintenanceResponse{Change: change}), nil
}

// DiscardDockerArchives drops staging after the change engine records a discard.
func (h *Handler) DiscardDockerArchives(ctx context.Context, change *v1.Change) {
	for _, item := range change.GetPlan().GetResources() {
		action := item.GetDockerMaintenance().GetImageAction()
		if action.GetExport() == nil && action.GetLoad() == nil && action.GetBuild() == nil {
			continue
		}
		session, _ := h.dockerAgent(Server{ID: item.GetResource().GetScope()})
		if session == nil || !session.ArtifactTransfers() {
			continue
		}
		leases := resource.DockerBuildLeases(action)
		if action.GetBuild() == nil {
			leases = []string{action.GetStagedSourceId()}
		}
		for _, lease := range leases {
			releaseArchivePin(ctx, session, &agentv1.DockerArtifactPin{ArtifactId: lease, ChangeId: change.GetId()})
		}
	}
}

func releaseArchivePin(ctx context.Context, session *agentapi.Session, pin *agentv1.DockerArtifactPin) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_, _ = session.Artifact(ctx, &agentv1.DockerArtifactControlCall{
		ArtifactId: pin.GetArtifactId(), Operation: &agentv1.DockerArtifactControlCall_ReleasePin{ReleasePin: pin},
	}, 3*time.Second)
}

func discardExport(ctx context.Context, session *agentapi.Session, lease string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_, _ = session.Artifact(ctx, &agentv1.DockerArtifactControlCall{
		ArtifactId: lease, Operation: &agentv1.DockerArtifactControlCall_DiscardExport{DiscardExport: &agentv1.DockwellDockerArtifactEmpty{}},
	}, 3*time.Second)
}

func archiveAlias(name string) (string, error) {
	ref, err := registry.Parse(name)
	if err != nil || ref.Digest != "" || ref.Tag == "" {
		return "", errors.New("choose a repository and tag")
	}
	return ref.String(), nil
}
