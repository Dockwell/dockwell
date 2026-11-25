// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
)

func (h *Handler) planDockerLifecycle(ctx context.Context, selection *v1.DockerLifecycleSelection) (*connect.Response[v1.PlanDockerMaintenanceResponse], error) {
	principal, ok := authapi.PrincipalFrom(ctx)
	if !ok || principal.Token != nil || principal.Session.ID == "" {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("sign in with an install admin account to act on external containers"), authapi.ReasonNeedsSession)
	}
	if len(selection.GetKeys()) == 0 || len(selection.GetKeys()) > 128 {
		return nil, authapi.Invalid(errors.New("select up to 128 containers or Compose projects"))
	}
	action := selection.GetAction()
	verb, ok := strings.CutPrefix(action.String(), "DOCKER_CONTAINER_ACTION_")
	if !ok || action < v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_START || action > v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_RENAME {
		return nil, authapi.Invalid(errors.New("select a Docker container action"))
	}
	keys := make(map[string][]*v1.DockerObjectKey)
	for _, key := range selection.GetKeys() {
		if key.GetServerId() == "" {
			return nil, authapi.Invalid(errors.New("select a server for each target"))
		}
		keys[key.GetServerId()] = append(keys[key.GetServerId()], key)
	}
	servers, err := h.dockerServers(ctx, slices.Sorted(maps.Keys(keys)))
	if err != nil {
		return nil, err
	}
	plans := make(map[string]*v1.DockerMaintenance)
	var total int
	for _, server := range servers {
		session, _ := h.dockerAgent(server)
		if session == nil {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("a selected server couldn't answer. Reconnect it and review again"))
		}
		serverKeys := keys[server.ID]
		if len(serverKeys) == 0 {
			return nil, authapi.Invalid(errors.New("use exact server IDs from Docker inventory"))
		}
		preview, err := session.DockerLifecyclePreview(ctx, &agentv1.DockerLifecyclePreviewCall{
			Keys: serverKeys, Action: action, NewName: strings.TrimSpace(selection.GetNewName()),
			RemoveAnonymousVolumes: selection.GetRemoveAnonymousVolumes(),
		})
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		targets := preview.GetTargets()
		if err := h.checkExternalTargets(ctx, server.ID, serverKeys, targets); err != nil {
			return nil, err
		}
		if len(targets) == 0 {
			continue
		}
		total += len(targets)
		if total > 128 {
			return nil, authapi.Invalid(errors.New("select up to 128 container targets across the fleet"))
		}
		plans[server.ID] = &v1.DockerMaintenance{Lifecycle: &v1.DockerContainerLifecycle{
			Action: action, Signal: strings.ToUpper(strings.TrimSpace(selection.GetSignal())),
			TimeoutSeconds: selection.GetTimeoutSeconds(), RemoveAnonymousVolumes: selection.GetRemoveAnonymousVolumes(), Targets: targets,
		}}
	}
	if total == 0 {
		return nil, authapi.Invalid(errors.New("none of the selected containers can perform this action in their current state. Refresh and choose an available action"))
	}
	verb = verb[:1] + strings.ToLower(verb[1:])
	change, err := h.changes.PlanDockerMaintenance(ctx, verb+" Docker containers", plans)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.PlanDockerMaintenanceResponse{Change: change}), nil
}

func (h *Handler) checkExternalTargets(ctx context.Context, server string, keys []*v1.DockerObjectKey, targets []*v1.DockerContainerTarget) error {
	rows := make([]*v1.DockerObject, 0, len(targets))
	for _, target := range targets {
		rows = append(rows, &v1.DockerObject{Key: &v1.DockerObjectKey{EngineId: target.GetEngineId()}})
	}
	if err := h.classifyDocker(ctx, server, rows); err != nil {
		return err
	}
	refused := errors.New("a selected container or Compose project is managed by Dockwell. Use its workload controls")
	for _, row := range rows {
		if row.GetOwnership() != v1.DockerOwnership_DOCKER_OWNERSHIP_EXTERNAL {
			return connect.NewError(connect.CodeFailedPrecondition, refused)
		}
	}
	// Health can be absent or stale. Compare the reviewed Compose identities
	// directly against placed resources as well.
	var overlaps bool
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		placed, err := tx.ListPlaced(ctx, server)
		if err != nil {
			return err
		}
		for _, held := range placed {
			if held.Kind != string(container.Kind) && held.Kind != string(service.Kind) {
				continue
			}
			project := docker.Project(held.Scope)
			for _, key := range keys {
				overlaps = overlaps || key.GetKind() == composeKind && key.GetEngineId() == "outside:"+project
			}
			for _, target := range targets {
				overlaps = overlaps || target.GetComposeProject() == project && target.GetComposeService() == held.Name
			}
		}
		return nil
	}); err != nil {
		return h.internal(ctx, err)
	}
	if overlaps {
		return connect.NewError(connect.CodeFailedPrecondition, refused)
	}
	return nil
}
