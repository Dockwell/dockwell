// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
)

func (h *Handler) planDockerImage(ctx context.Context, req *v1.PlanDockerMaintenanceRequest) (*connect.Response[v1.PlanDockerMaintenanceResponse], error) {
	p, _ := authapi.PrincipalFrom(ctx)
	if p.Token != nil || p.Session.ID == "" {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("sign in to review an image action"), authapi.ReasonNeedsSession)
	}
	if len(req.GetServerIds()) != 1 || len(req.GetSummary()) > 200 {
		return nil, authapi.Invalid(errors.New("choose one server for the image action"))
	}
	servers, err := h.dockerServers(ctx, req.GetServerIds())
	if err != nil {
		return nil, err
	}
	server := servers[0]
	session, _ := h.dockerAgent(server)
	if session == nil || !session.DockerImageOperations() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("connect or update this server's agent to review an image action"))
	}
	ref := resource.Ref{Kind: host.Kind, Scope: server.ID, Name: host.Name}
	action := &v1.DockerImageAction{}
	var input, summary string
	if pull := req.GetPull(); pull != nil {
		input, summary = pull.GetReference(), "Pull image"
		action.CredentialProject = pull.GetCredentialProject()
		action.Pull = proto.CloneOf(pull)
	} else {
		tag := req.GetTag()
		if tag.GetSourceKey().GetServerId() != server.ID || !docker.IsImageID(tag.GetSourceKey().GetImageId()) {
			return nil, authapi.Invalid(errors.New("select the full image ID on this server"))
		}
		input, summary = tag.GetDestination(), "Set image tag"
		action.Tag = proto.CloneOf(tag)
	}
	target, err := registry.Parse(input)
	if err != nil || target.Digest != "" && (target.Tag != "" || action.GetTag() != nil) ||
		target.Registry == docker.BuiltRegistry {
		return nil, authapi.Invalid(errors.New("enter a repository and tag, or a digest to pull"))
	}
	if target.Digest == "" {
		target.Tag = target.Reference()
	}
	canonical := target.String()
	if pull := action.GetPull(); pull != nil {
		pull.Reference = canonical
	} else {
		action.GetTag().Destination = canonical
	}
	var prepared *v1.DockerImageAction
	if action.GetTag() != nil || action.GetPull().GetLoginId() == "" {
		prepared, err = session.PreviewDockerImage(ctx, ref, action, registry.Auth{})
	}
	if pull := action.GetPull(); pull != nil && prepared == nil {
		login, auth, loginErr := session.DockerImageLogin(ctx, pull.GetCredentialProject(), canonical, pull.GetLoginId())
		if loginErr != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("choose a registry login that this operation may use"))
		}
		if login != nil {
			action.ApprovedLogins = []*v1.DockerImageLoginRef{login}
			prepared, err = session.PreviewDockerImage(ctx, ref, action, auth)
		}
	}
	if err != nil {
		if reason := resource.ReasonOf(err); reason != "" {
			plain, _ := resource.Explain(err)
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New(plain), reason)
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if len(prepared.GetBindings()) != 1 || prepared.GetBindings()[0].GetReference() != canonical {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent could not verify the local image binding"))
	}
	action.Bindings = prepared.GetBindings()
	binding := action.Bindings[0]
	if binding.GetExpectAbsent() != (binding.GetImageId() == "") || binding.GetImageId() != "" && !docker.IsImageID(binding.GetImageId()) {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent returned an invalid image ID"))
	}
	out := &v1.PlanDockerMaintenanceResponse{}
	if action.GetPull() != nil {
		descriptor := prepared.GetDescriptor_()
		if descriptor.GetRepository() != target.Name() || !registry.IsDigest(descriptor.GetConfigDigest()) ||
			!registry.IsDigest(descriptor.GetManifestDigest()) || descriptor.GetPlatform() == "" ||
			descriptor.GetIndexDigest() != "" && !registry.IsDigest(descriptor.GetIndexDigest()) {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent could not verify the registry digest"))
		}
		action.Descriptor_ = descriptor
		current := binding.GetLocal()
		if current != nil {
			if current.GetRepository() != target.Name() || binding.GetExpectAbsent() ||
				current.GetConfigDigest() != "" && current.GetConfigDigest() != binding.GetImageId() ||
				current.GetManifestDigest() != "" && !registry.IsDigest(current.GetManifestDigest()) ||
				current.GetIndexDigest() != "" && !registry.IsDigest(current.GetIndexDigest()) {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent could not verify the installed image digest"))
			}
		}
		out.DigestCheck = &v1.DockerImageDigestCheck{Candidate: descriptor, Current: current, SameContent: resource.DockerImageMatchesDigest(current, descriptor)}
	}
	if out.GetDigestCheck().GetSameContent() || action.GetTag() != nil && !resource.DockerImageRetargetsAlias(action) {
		return connect.NewResponse(out), nil
	}
	if resource.DockerImageRetargetsAlias(action) {
		protection, err := revision.DockerCleanupProtection(ctx, h.db, server.ID, time.Now(), false, false)
		if err != nil {
			return nil, h.internal(ctx, err)
		}
		if resource.DockerAliasProtected(canonical, protection.Images) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a kept workload uses this tag. Choose another tag"))
		}
	}
	if custom := strings.TrimSpace(req.GetSummary()); custom != "" {
		summary = custom
	}
	out.Change, err = h.changes.PlanDockerMaintenance(ctx, summary, map[string]*v1.DockerMaintenance{server.ID: {ImageAction: action}})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}
