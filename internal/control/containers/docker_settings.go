// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/dockerdaemon"
	"github.com/dockwell/dockwell/internal/store"
)

func (h *Handler) settingsServer(ctx context.Context, id string) (Server, *agentapi.Session, error) {
	if id == "" {
		return Server{}, nil, authapi.Invalid(errors.New("select one server"))
	}
	s, err := h.server(ctx, id)
	if err != nil {
		return s, nil, err
	}
	a, state := h.dockerAgent(s)
	if a == nil {
		if state == v1.DockerReadState_DOCKER_READ_STATE_AGENT_OUTDATED {
			return s, nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("update the agent to read Docker settings"))
		}
		return s, nil, notConnected(s)
	}
	return s, a, nil
}

// GetDockerSettings reads effective settings on one server.
func (h *Handler) GetDockerSettings(ctx context.Context, req *connect.Request[v1.GetDockerSettingsRequest]) (*connect.Response[v1.GetDockerSettingsResponse], error) {
	s, a, err := h.settingsServer(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	res, err := a.DockerSettings(ctx, &agentv1.DockerSettingsCall{Input: &agentv1.DockerSettingsCall_Read{Read: true}, LoggingSummary: req.Msg.GetLoggingSummary()})
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	row := res.GetSettings()
	if row == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent did not return Docker settings"))
	}
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResource(ctx, string(dockerdaemon.Kind), s.ID, dockerdaemon.Name)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		row.ManagedRevision = int64(r.Revision)
		return err
	}); err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&v1.GetDockerSettingsResponse{Settings: row}), nil
}

// CheckDockerSettings validates a draft through its target agent.
func (h *Handler) CheckDockerSettings(ctx context.Context, req *connect.Request[v1.CheckDockerSettingsRequest]) (*connect.Response[v1.CheckDockerSettingsResponse], error) {
	_, a, err := h.settingsServer(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetDraft() == nil {
		return nil, authapi.Invalid(errors.New("enter a Docker settings draft"))
	}
	res, err := a.DockerSettings(ctx, &agentv1.DockerSettingsCall{Input: &agentv1.DockerSettingsCall_Check{Check: req.Msg.GetDraft()}})
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if res.GetChecked() == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent did not validate the Docker settings"))
	}
	return connect.NewResponse(res.GetChecked()), nil
}

// PlanDockerContainerLogging starts a reviewed revision for the selected workload.
func (h *Handler) PlanDockerContainerLogging(ctx context.Context, req *connect.Request[v1.PlanDockerContainerLoggingRequest]) (*connect.Response[v1.PlanDockerContainerLoggingResponse], error) {
	m := req.Msg
	key := m.GetContainer()
	if key.GetKind() != v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER || !docker.IsFullID(key.GetEngineId()) || m.GetBaseRevision() <= 0 || m.GetLogging() == nil {
		return nil, authapi.Invalid(errors.New("select an exact managed container and its current revision"))
	}
	s, a, err := h.settingsServer(ctx, key.GetServerId())
	if err != nil {
		return nil, err
	}
	if !a.DockerContainerLogging() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("update the agent to review log rotation"))
	}
	object, err := h.GetDockerObject(ctx, connect.NewRequest(&v1.GetDockerObjectRequest{Key: key}))
	if err != nil {
		return nil, err
	}
	row := object.Msg.GetObject()
	if row.GetOwnership() != v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED || row.GetManagedRevision() != m.GetBaseRevision() || row.GetContainer().GetOneOff() {
		return nil, authapi.Invalid(errors.New("select the current managed workload. Take on an external container first"))
	}
	ref := row.GetManagedResource()
	change, err := h.changes.PlanDockerLogging(ctx, resource.Ref{Kind: resource.Kind(ref.GetKind()), Scope: ref.GetScope(), Name: ref.GetName()}, int(m.GetBaseRevision()), s.ID, key.GetEngineId(), m.GetLogging())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.PlanDockerContainerLoggingResponse{Change: change}), nil
}
