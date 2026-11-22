// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
)

func (h *Handler) planDockerPush(ctx context.Context, m *v1.PlanDockerMaintenanceRequest) (*connect.Response[v1.PlanDockerMaintenanceResponse], error) {
	if len(m.GetServerIds()) != 1 || len(m.GetSummary()) > 200 {
		return nil, authapi.Invalid(errors.New("choose one server for the image push"))
	}
	server := m.GetServerIds()[0]
	access, err := h.artifactAccess(ctx, server)
	if err != nil {
		return nil, err
	}
	push := proto.CloneOf(m.GetPush())
	key := push.GetSourceKey()
	target, err := registry.Parse(push.GetDestination())
	if err != nil || target.Registry == docker.BuiltRegistry || target.Tag != "" && target.Digest != "" ||
		key != nil && (key.GetServerId() != server || !docker.IsImageID(key.GetImageId()) || target.Digest != "" || push.GetDigestOnly() && target.Tag != "") ||
		key == nil && push.GetDigestOnly() {
		return nil, authapi.Invalid(errors.New("choose a registry repository and a tag or publish by digest"))
	}
	if target.Digest == "" && !push.GetDigestOnly() {
		target.Tag = target.Reference()
	}
	open := access.session.DockerImageLogin
	if key != nil {
		open = access.session.DockerPushLogin
	}
	login, auth, err := open(ctx, push.GetCredentialProject(), target.String(), push.GetLoginId())
	if err != nil || login == nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("choose a saved login that allows this registry action"), "docker_push_login")
	}
	push.LoginId = login.GetId()
	action := &v1.DockerImageAction{Push: push, CredentialProject: push.GetCredentialProject(), ApprovedLogins: []*v1.DockerImageLoginRef{login}}
	made := ""
	defer func() {
		if made != "" {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer done()
			_, _ = access.abort(cleanup, made)
		}
	}()
	if key != nil {
		staged, err := access.session.Artifact(ctx, &agentv1.DockerArtifactControlCall{
			OwnerBinding: access.owner,
			Operation:    &agentv1.DockerArtifactControlCall_PreparePush{PreparePush: &v1.DockerImageExport{SourceKey: key}},
		}, 30*time.Minute)
		info, err := artifactResult(staged, err)
		if err != nil {
			return nil, err
		}
		made = info.GetArtifactId()
		d := staged.GetDescriptor_()
		if info.GetKind() != artifact.PushSource || info.GetState() != artifact.Ready || !artifact.ValidID(made) || len(info.GetSha256()) != 64 ||
			!docker.IsImageID(d.GetManifestDigest()) || !docker.IsImageID(d.GetConfigDigest()) || d.GetPlatform() == "" {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent could not verify the staged image"))
		}
		action.StagedSourceId, action.SourceSha256 = made, info.GetSha256()
		action.Descriptor_ = proto.CloneOf(d)
		action.Descriptor_.Repository = target.Name()
		if push.GetDigestOnly() {
			target.Digest = d.GetManifestDigest()
		}
	}
	push.Destination = target.String()
	ref := resource.Ref{Kind: host.Kind, Scope: server, Name: host.Name}
	prepared, err := access.session.PreviewDockerImage(ctx, ref, action, auth)
	if err != nil {
		if reason := resource.ReasonOf(err); reason != "" {
			plain, _ := resource.Explain(err)
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New(plain), reason)
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	bindings := prepared.GetBindings()
	if len(bindings) != 1 || bindings[0].GetReference() != push.GetDestination() || bindings[0].GetImageId() != "" || bindings[0].GetLocal() != nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent could not verify the destination"))
	}
	if key == nil {
		out := &v1.DockerImageDigestCheck{}
		if !bindings[0].GetExpectAbsent() {
			d := prepared.GetDescriptor_()
			if d.GetRepository() != target.Name() || !registry.IsDigest(d.GetManifestDigest()) {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent could not verify the destination digest"))
			}
			out.Current = &v1.DockerRegistryDescriptor{Repository: target.Name(), ManifestDigest: d.GetManifestDigest()}
		}
		return connect.NewResponse(&v1.PlanDockerMaintenanceResponse{DigestCheck: out}), nil
	}
	if !proto.Equal(prepared.GetDescriptor_(), action.GetDescriptor_()) {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent returned different image evidence"))
	}
	action.Bindings = bindings
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = "Push Docker image"
	}
	change, err := h.changes.PlanDockerArchive(ctx, summary, server, action, func(ctx context.Context, changeID string) error {
		pin := &agentv1.DockerArtifactPin{ArtifactId: made, ChangeId: changeID}
		_, err := artifactResult(access.session.Artifact(ctx, &agentv1.DockerArtifactControlCall{
			ArtifactId: made, OwnerBinding: access.owner, Operation: &agentv1.DockerArtifactControlCall_Pin{Pin: pin},
		}, time.Minute))
		return err
	})
	if err != nil {
		return nil, err
	}
	made = ""
	return connect.NewResponse(&v1.PlanDockerMaintenanceResponse{Change: change}), nil
}
