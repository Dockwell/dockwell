// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

const composeKind = v1.DockerObjectKind_DOCKER_OBJECT_KIND_COMPOSE

var composeInventoryID = regexp.MustCompile(`^(?:env_[a-zA-Z0-9]+|outside:[a-zA-Z0-9._-]{1,255})$`)

type composeRow struct {
	object    *v1.DockerObject
	services  map[string]v1.ContainerState
	attention map[string]bool
	dirs      map[string]bool
}

func newComposeRow(server, id, name string, ownership v1.DockerOwnership) *composeRow {
	return &composeRow{
		object: &v1.DockerObject{
			Key:  &v1.DockerObjectKey{ServerId: server, Kind: composeKind, EngineId: id},
			Name: name, Ownership: ownership, Detail: &v1.DockerObject_Compose{Compose: &v1.DockerCompose{}},
		},
		services: map[string]v1.ContainerState{}, attention: map[string]bool{}, dirs: map[string]bool{},
	}
}

func (r *composeRow) observe(name string, row *v1.DockerObject) {
	c := row.GetContainer()
	state := row.GetState()
	previous := r.services[name]
	if previous != v1.ContainerState_CONTAINER_STATE_RUNNING && (previous != v1.ContainerState_CONTAINER_STATE_RESTARTING || state == v1.ContainerState_CONTAINER_STATE_RUNNING) {
		r.services[name] = state
	}
	if c.GetHealth() == v1.DockerHealth_DOCKER_HEALTH_UNHEALTHY || c.GetExitCode() != 0 || state == v1.ContainerState_CONTAINER_STATE_RESTARTING {
		r.attention[name] = true
	}
	r.dirs[c.GetComposeDirectory()] = true
	if r.object.GetOwnership() == v1.DockerOwnership_DOCKER_OWNERSHIP_EXTERNAL {
		if at := c.GetCreatedAt(); at != nil && (r.object.ObservedAt == nil || at.AsTime().After(r.object.ObservedAt.AsTime())) {
			r.object.ObservedAt = at
		}
	}
}

func (r *composeRow) finish(available bool) *v1.DockerObject {
	c := r.object.GetCompose()
	c.TotalServices = uint32(len(r.services))             //nolint:gosec // bounded inventory.
	c.ServicesNeedingAttention = uint32(len(r.attention)) //nolint:gosec // bounded inventory.
	for _, state := range r.services {
		if state == v1.ContainerState_CONTAINER_STATE_RUNNING {
			c.RunningServices++
		}
	}
	c.ObservationUnavailable = !available
	switch {
	case !available || c.GetChecking():
	case c.ServicesNeedingAttention != 0:
		r.object.State = v1.ContainerState_CONTAINER_STATE_RESTARTING
	case c.RunningServices == c.TotalServices:
		r.object.State = v1.ContainerState_CONTAINER_STATE_RUNNING
	case c.RunningServices == 0:
		r.object.State = v1.ContainerState_CONTAINER_STATE_EXITED
	}
	if len(r.dirs) == 1 {
		for dir := range r.dirs {
			c.SourceDirectory = dir
		}
	}
	return r.object
}

func (h *Handler) composeRows(ctx context.Context, server Server) ([]*v1.DockerObject, v1.DockerReadState, error) {
	groups := map[string]*composeRow{}
	byEngine := map[string]*composeRow{}
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		placed, err := tx.ListPlaced(ctx, server.ID)
		if err != nil {
			return err
		}
		changeTimes, err := tx.PlacedRevisionTimes(ctx, server.ID, string(service.Kind))
		if err != nil {
			return err
		}
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, held := range placed {
			if held.Kind != string(service.Kind) {
				continue
			}
			row := groups[held.Scope]
			if row == nil {
				project, env, err := tree.Resolve("", held.Scope)
				if err != nil {
					continue
				}
				name := project.DisplayName
				if name == "" {
					name = project.Name
				}
				if len(project.Environments) > 1 {
					name += " / " + env.Name
				}
				row = newComposeRow(server.ID, env.ID, name, v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED)
				if at, ok := changeTimes[env.ID]; ok {
					row.object.ObservedAt = timestamppb.New(at)
				}
				row.object.GetCompose().EnvironmentId = env.ID
				row.object.GetCompose().EngineProject = docker.Project(env.ID)
				groups[env.ID] = row
				byEngine[docker.Project(env.ID)] = row
			}
			row.services[held.Name] = v1.ContainerState_CONTAINER_STATE_UNSPECIFIED
		}
		return nil
	}); err != nil {
		return nil, 0, h.internal(ctx, err)
	}
	session, state := h.dockerAgent(server)
	if session != nil && !session.DockerComposeMembers() {
		session, state = nil, v1.DockerReadState_DOCKER_READ_STATE_AGENT_OUTDATED
	}
	if session != nil {
		var observed []*v1.DockerObject
		after, snapshot := "", ""
		complete := false
		for range 64 {
			res, err := session.DockerList(ctx, &agentv1.DockerInventoryListCall{
				ComposeMembers: true, AfterId: after, Snapshot: snapshot,
			})
			if err != nil || res.GetResync() {
				break
			}
			observed = append(observed, res.GetObjects()...)
			if res.GetNextId() == "" {
				complete = true
				break
			}
			after, snapshot = res.GetNextId(), res.GetSnapshot()
		}
		if !complete {
			state = v1.DockerReadState_DOCKER_READ_STATE_ENGINE_UNAVAILABLE
		} else {
			if err := h.classifyDocker(ctx, server.ID, observed); err != nil {
				return nil, 0, err
			}
			for _, object := range observed {
				body := object.GetContainer()
				if body.GetOneOff() || object.GetOwnership() == v1.DockerOwnership_DOCKER_OWNERSHIP_SYSTEM {
					continue
				}
				ref := object.GetManagedResource()
				if group := groups[ref.GetScope()]; group != nil && ref.GetKind() == string(service.Kind) {
					group.observe(ref.GetName(), object)
					continue
				}
				project, name := body.GetComposeProject(), body.GetComposeService()
				if group := byEngine[project]; group != nil {
					if _, placed := group.services[name]; placed && group.services[name] == 0 {
						group.object.GetCompose().Checking = true
					}
					continue
				}
				if project == "" || name == "" || ref != nil {
					continue
				}
				key := "outside:" + project
				if groups[key] == nil {
					groups[key] = newComposeRow(server.ID, key, project, v1.DockerOwnership_DOCKER_OWNERSHIP_EXTERNAL)
					groups[key].object.GetCompose().EngineProject = project
				}
				groups[key].observe(name, object)
			}
		}
	}
	out := make([]*v1.DockerObject, 0, len(groups))
	for _, id := range slices.Sorted(maps.Keys(groups)) {
		out = append(out, groups[id].finish(state == 0))
	}
	return out, state, nil
}

func (h *Handler) listDockerComposes(ctx context.Context, servers []Server, filter *v1.DockerObjectFilter, cursor *dockerCursor, out *v1.ListDockerObjectsResponse, page int) error {
	search := strings.ToLower(strings.TrimSpace(filter.GetSearch()))
	for visits := 0; visits < 6 && len(out.Objects) < page && cursor.Server < len(servers); visits++ {
		server := servers[cursor.Server]
		rows, state, err := h.composeRows(ctx, server)
		if err != nil {
			return err
		}
		if state != 0 {
			out.Unavailable = append(out.Unavailable, &v1.DockerUnavailable{ServerId: server.ID, State: state})
		}
		digest := sha256.New()
		for _, row := range rows {
			// Paging follows membership. Health and state can change between reads.
			fmt.Fprintf(digest, "%s\x00%s\x00%d\x00", row.GetKey().GetEngineId(), row.GetName(), row.GetOwnership())
		}
		out.Snapshot = hex.EncodeToString(digest.Sum(nil))
		if cursor.Snapshot != "" && cursor.Snapshot != out.Snapshot {
			return authapi.ReasonError(connect.CodeAborted, errors.New("the Compose inventory changed. Refresh the list"), "docker_resync")
		}
		for _, row := range rows {
			id := row.GetKey().GetEngineId()
			if id <= cursor.After || filter.GetOwnership() != 0 && filter.GetOwnership() != row.GetOwnership() ||
				!strings.Contains(strings.ToLower(row.GetName()), search) {
				continue
			}
			if len(out.Objects) == page {
				return nil
			}
			out.Objects = append(out.Objects, row)
			cursor.After, cursor.Snapshot = id, out.Snapshot
		}
		cursor.Server++
		cursor.After, cursor.Snapshot = "", ""
	}
	return nil
}

func (h *Handler) getDockerCompose(ctx context.Context, key *v1.DockerObjectKey) (*connect.Response[v1.GetDockerObjectResponse], error) {
	if !dockerKeyID(composeKind, key.GetEngineId()) {
		return nil, authapi.Invalid(errors.New("select a Compose project from Docker inventory"))
	}
	server, err := h.server(ctx, key.GetServerId())
	if err != nil {
		return nil, err
	}
	rows, state, err := h.composeRows(ctx, server)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.GetKey().GetEngineId() == key.GetEngineId() {
			return connect.NewResponse(&v1.GetDockerObjectResponse{Object: row}), nil
		}
	}
	if state != 0 {
		return nil, authapi.ReasonError(connect.CodeUnavailable, errors.New("the Engine could not list Compose projects. Try again when it answers"), authapi.ReasonUnavailable)
	}
	return nil, authapi.ReasonError(connect.CodeNotFound, errors.New("this Compose project is no longer on the server"), "not_found")
}
