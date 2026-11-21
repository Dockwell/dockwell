// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	dockerImageKind  = v1.DockerObjectKind_DOCKER_OBJECT_KIND_IMAGE
	dockerPageSize   = 24
	dockerMaxServers = 32
)

var _ dockwellv1connect.DockerServiceHandler = (*Handler)(nil)

func (h *Handler) dockerServers(ctx context.Context, asked []string) ([]Server, error) {
	if len(asked) == 0 {
		s, err := h.server(ctx, "")
		return []Server{s}, err
	}
	if len(asked) > dockerMaxServers {
		return nil, authapi.Invalid(errors.New("select at most 32 servers to read their Docker inventory"))
	}
	var servers []store.Server
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		servers, err = tx.ListServers(ctx)
		return err
	}); err != nil {
		return nil, h.internal(ctx, err)
	}
	found := make(map[string]Server, 2*len(servers))
	for i := range servers {
		server := &servers[i]
		s := Server{ID: server.ID, Name: server.Name}
		found[s.Name], found[s.ID] = s, s
	}
	selected := make([]Server, 0, len(asked))
	seen := make(map[string]bool, len(asked))
	for _, choice := range asked {
		s, ok := found[choice]
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("a selected server is no longer in the fleet. Select its replacement in Servers"))
		}
		if !seen[s.ID] {
			selected = append(selected, s)
			seen[s.ID] = true
		}
	}
	return selected, nil
}

func (h *Handler) dockerAgent(s Server) (*agentapi.Session, v1.DockerReadState) {
	if h.agents != nil {
		if session, ok := h.agents.Session(s.ID); ok {
			if session.DockerInventory() {
				return session, 0
			}
			return nil, v1.DockerReadState_DOCKER_READ_STATE_AGENT_OUTDATED
		}
	}
	return nil, v1.DockerReadState_DOCKER_READ_STATE_OFFLINE
}

// GetDockerOverview reads counts and availability for the selected servers.
func (h *Handler) GetDockerOverview(ctx context.Context, req *connect.Request[v1.GetDockerOverviewRequest]) (*connect.Response[v1.GetDockerOverviewResponse], error) {
	servers, err := h.dockerServers(ctx, req.Msg.GetServerIds())
	if err != nil {
		return nil, err
	}
	summaries := make([]*v1.DockerServerSummary, len(servers))
	unavailable := make([]*v1.DockerUnavailable, len(servers))
	var reads sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, s := range servers {
		slots <- struct{}{}
		reads.Go(func() {
			defer func() { <-slots }()
			session, state := h.dockerAgent(s)
			if session != nil {
				res, err := session.DockerList(ctx, &agentv1.DockerInventoryListCall{Overview: true})
				if summary := res.GetOverview(); err == nil && summary != nil {
					summary.ServerId, summary.ExactTerminal = s.ID, session.ExactTerminal()
					for _, row := range summary.GetAttention() {
						row.Key = &v1.DockerObjectKey{ServerId: s.ID, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER, EngineId: row.GetKey().GetEngineId()}
						row.ManagedResource, row.ManagedResourceId, row.ManagedRevision = nil, "", 0
					}
					summaries[i] = summary
					return
				}
				state = v1.DockerReadState_DOCKER_READ_STATE_ENGINE_UNAVAILABLE
			}
			unavailable[i] = &v1.DockerUnavailable{ServerId: s.ID, State: state}
		})
	}
	reads.Wait()
	return connect.NewResponse(&v1.GetDockerOverviewResponse{
		ServerSummaries: slices.DeleteFunc(summaries, func(s *v1.DockerServerSummary) bool { return s == nil }),
		Unavailable:     slices.DeleteFunc(unavailable, func(s *v1.DockerUnavailable) bool { return s == nil }),
	}), nil
}

type dockerCursor struct {
	Selection string
	Server    int
	After     string
	Snapshot  string
}

func inventoryCursor(servers []Server, request *v1.ListDockerObjectsRequest) (dockerCursor, error) {
	h := sha256.New()
	h.Write([]byte{byte(request.GetKind())}) //nolint:gosec // inventory kinds fit in one byte.
	for _, server := range servers {
		h.Write([]byte(server.ID))
		h.Write([]byte{0})
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(request.GetFilter())
	if err != nil {
		return dockerCursor{}, err
	}
	fmt.Fprintln(h, request.GetKind())
	h.Write(encoded)
	selection := hex.EncodeToString(h.Sum(nil))
	token := request.GetPageToken()
	if token == "" {
		return dockerCursor{Selection: selection}, nil
	}
	var cursor dockerCursor
	if len(token) <= 512 {
		data, err := base64.RawURLEncoding.DecodeString(token)
		if err == nil && json.Unmarshal(data, &cursor) == nil && cursor.Selection == selection &&
			cursor.Server >= 0 && cursor.Server < len(servers) &&
			(cursor.After == "" || dockerKeyID(request.GetKind(), cursor.After) && len(cursor.Snapshot) == 64) {
			return cursor, nil
		}
	}
	return dockerCursor{}, authapi.Invalid(errors.New("the Docker page has a different selection. Refresh the list"))
}

// ListDockerObjects reads a page of Docker inventory across the selection.
func (h *Handler) ListDockerObjects(ctx context.Context, req *connect.Request[v1.ListDockerObjectsRequest]) (*connect.Response[v1.ListDockerObjectsResponse], error) {
	m := req.Msg
	if len(m.GetFilter().GetSearch()) > 255 || len(m.GetFilter().GetComposeProject()) > 255 || !containerKind(m.GetKind()) && m.GetKind() != dockerImageKind && m.GetKind() != composeKind {
		return nil, authapi.Invalid(errors.New("select a supported Docker inventory and use a search of at most 255 bytes"))
	}
	servers, err := h.dockerServers(ctx, m.GetServerIds())
	if err != nil {
		return nil, err
	}
	cursor, err := inventoryCursor(servers, m)
	if err != nil {
		return nil, err
	}
	out := &v1.ListDockerObjectsResponse{}
	page := int(min(m.GetPageSize(), dockerPageSize))
	if page == 0 {
		page = dockerPageSize
	}
	if m.GetKind() == v1.DockerObjectKind_DOCKER_OBJECT_KIND_COMPOSE {
		if err := h.listDockerComposes(ctx, servers, m.GetFilter(), &cursor, out, page); err != nil {
			return nil, err
		}
	} else if err := h.listDockerContainers(ctx, servers, m, &cursor, out, page); err != nil {
		return nil, err
	}
	if cursor.Server < len(servers) {
		token, err := json.Marshal(cursor)
		if err != nil {
			return nil, h.internal(ctx, err)
		}
		out.NextPageToken = base64.RawURLEncoding.EncodeToString(token)
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) listDockerContainers(ctx context.Context, servers []Server, m *v1.ListDockerObjectsRequest, cursor *dockerCursor, out *v1.ListDockerObjectsResponse, page int) error {
	filter := m.GetFilter()
	agentFilter := filter
	scope := ""
	if selected, ok := strings.CutPrefix(filter.GetComposeProject(), "managed:"); ok && filter.GetOwnership() == v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED {
		if _, err := id.Parse(id.Environment, selected); err != nil {
			return authapi.Invalid(errors.New("select a managed Compose environment"))
		}
		// A project taken on without a restart still has its old Engine
		// labels. For a managed stack, select by confirmed control-plane
		// ownership after reading the inventory.
		scope = selected
		agentFilter = proto.CloneOf(filter)
		agentFilter.ComposeProject = ""
	}
	// Bound work even with a selective ownership filter on a large fleet. A
	// next token can remain when a page found no matches in its budget.
	for visits := 0; len(out.Objects) < page && cursor.Server < len(servers) && visits < 6; visits++ {
		s := servers[cursor.Server]
		session, state := h.dockerAgent(s)
		if session != nil && m.GetKind() == dockerImageKind && !session.DockerImages() {
			session, state = nil, v1.DockerReadState_DOCKER_READ_STATE_AGENT_OUTDATED
		}
		if session == nil {
			out.Unavailable = append(out.Unavailable, &v1.DockerUnavailable{ServerId: s.ID, State: state})
		} else {
			res, err := session.DockerList(ctx, &agentv1.DockerInventoryListCall{
				Kind: m.GetKind(), Filter: agentFilter, PageSize: uint32(min(max(0, page-len(out.Objects)), dockerPageSize)), AfterId: cursor.After, Snapshot: cursor.Snapshot,
			})
			if err != nil {
				out.Unavailable = append(out.Unavailable, &v1.DockerUnavailable{ServerId: s.ID, State: v1.DockerReadState_DOCKER_READ_STATE_ENGINE_UNAVAILABLE})
			} else {
				if res.GetResync() {
					return authapi.ReasonError(connect.CodeAborted, errors.New("the Docker inventory changed. Refresh the list"), "docker_resync")
				}
				items := res.GetObjects()
				if err := h.classifyDockerObjects(ctx, s.ID, m.GetKind(), items); err != nil {
					return err
				}
				for _, row := range items {
					if scope != "" && (row.GetManagedResource().GetKind() != "service" || row.GetManagedResource().GetScope() != scope) {
						continue
					}
					if ownership := m.GetFilter().GetOwnership(); m.GetKind() == dockerImageKind || ownership == 0 || row.GetOwnership() == ownership {
						out.Objects = append(out.Objects, row)
					}
				}
				out.Snapshot = res.GetSnapshot()
				if next := res.GetNextId(); next != "" && next > cursor.After {
					cursor.After, cursor.Snapshot = next, out.Snapshot
					continue
				}
			}
		}
		cursor.Server++
		cursor.After, cursor.Snapshot = "", ""
	}
	return nil
}

func containerKind(kind v1.DockerObjectKind) bool {
	return kind == 0 || kind == v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER
}

func dockerKeyID(kind v1.DockerObjectKind, engineID string) bool {
	if kind == composeKind {
		return len(engineID) <= 263 && composeInventoryID.MatchString(engineID)
	}
	if kind == dockerImageKind {
		return docker.IsImageID(engineID)
	}
	return docker.IsFullID(engineID)
}

func (h *Handler) classifyDockerObjects(ctx context.Context, server string, kind v1.DockerObjectKind, rows []*v1.DockerObject) error {
	if kind != dockerImageKind {
		return h.classifyDocker(ctx, server, rows)
	}
	for _, row := range rows {
		row.Key.ServerId = server
		for _, container := range row.GetImage().GetContainers() {
			if key := container.GetKey(); key != nil {
				key.ServerId = server
			}
		}
	}
	return nil
}

// GetDockerObject reads an immutable Engine instance through its agent.
func (h *Handler) GetDockerObject(ctx context.Context, req *connect.Request[v1.GetDockerObjectRequest]) (*connect.Response[v1.GetDockerObjectResponse], error) {
	key := req.Msg.GetKey()
	if key.GetKind() == v1.DockerObjectKind_DOCKER_OBJECT_KIND_COMPOSE {
		return h.getDockerCompose(ctx, key)
	}
	if (!containerKind(key.GetKind()) && key.GetKind() != dockerImageKind) || !dockerKeyID(key.GetKind(), key.GetEngineId()) || len(req.Msg.GetLastKnownName()) > 255 {
		return nil, authapi.Invalid(errors.New("a Docker object detail needs its full Engine ID"))
	}
	s, err := h.server(ctx, key.GetServerId())
	if err != nil {
		return nil, err
	}
	session, state := h.dockerAgent(s)
	if session == nil {
		if state == v1.DockerReadState_DOCKER_READ_STATE_AGENT_OUTDATED {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("this agent needs an update to inspect Docker containers"), "agent_outdated")
		}
		return nil, notConnected(s)
	}
	ask := &agentv1.DockerInventoryDetailCall{ContainerId: key.GetEngineId(), LastKnownName: req.Msg.GetLastKnownName()}
	if key.GetKind() == dockerImageKind {
		if !session.DockerImages() {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("this agent needs an update to inspect local images"), "agent_outdated")
		}
		ask = &agentv1.DockerInventoryDetailCall{ImageId: key.GetEngineId()}
	}
	res, err := session.DockerDetail(ctx, ask)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeUnavailable, errors.New("the Engine could not inspect this object. Try again when it answers"), authapi.ReasonUnavailable)
	}
	if res.GetObject() != nil {
		if err := h.classifyDockerObjects(ctx, s.ID, key.GetKind(), []*v1.DockerObject{res.GetObject()}); err != nil {
			return nil, err
		}
	}
	if image := res.GetObject().GetImage(); image != nil {
		if err := h.imageDeployments(ctx, s.ID, key.GetEngineId(), image); err != nil {
			return nil, h.internal(ctx, err)
		}
	}
	if replacement := res.GetReplacementKey(); replacement != nil {
		replacement.ServerId = s.ID
	}
	return connect.NewResponse(&v1.GetDockerObjectResponse{Object: res.GetObject(), ReplacementKey: res.GetReplacementKey()}), nil
}

// WatchDockerObjects follows invalidations and live samples across the selection.
func (h *Handler) WatchDockerObjects(ctx context.Context, req *connect.Request[v1.WatchDockerObjectsRequest], stream *connect.ServerStream[v1.WatchDockerObjectsResponse]) error {
	m := req.Msg
	kind := m.GetSelection().GetKind()
	compose := kind == composeKind
	if len(m.GetVisibleKeys()) > 16 || len(m.GetAfterSnapshot()) > 128 ||
		!compose && kind != v1.DockerObjectKind_DOCKER_OBJECT_KIND_IMAGE && !containerKind(kind) || !containerKind(kind) && len(m.GetVisibleKeys()) > 0 {
		return authapi.Invalid(errors.New("watch Docker images, Compose summaries or containers, with at most 16 visible container IDs"))
	}
	servers, err := h.dockerServers(ctx, m.GetSelection().GetServerIds())
	if err != nil {
		return err
	}
	visible := make(map[string][]string, len(servers))
	for _, server := range servers {
		visible[server.ID] = nil
	}
	for _, key := range m.GetVisibleKeys() {
		if _, ok := visible[key.GetServerId()]; !ok || !docker.IsFullID(key.GetEngineId()) || !containerKind(key.GetKind()) {
			return authapi.Invalid(errors.New("watch only full container IDs on the selected servers"))
		}
		visible[key.GetServerId()] = append(visible[key.GetServerId()], key.GetEngineId())
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	out := make(chan *v1.WatchDockerObjectsResponse, len(servers))
	emit := func(frame *v1.WatchDockerObjectsResponse) error {
		select {
		case out <- frame:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, s := range servers {
		keys := visible[s.ID]
		workers.Go(func() { h.watchDockerServer(ctx, s, keys, compose, emit) })
	}
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeat.C:
			if err := stream.Send(&v1.WatchDockerObjectsResponse{}); err != nil {
				return err
			}
		case frame := <-out:
			if err := stream.Send(frame); err != nil {
				return err
			}
		}
	}
}

func (h *Handler) watchDockerServer(ctx context.Context, s Server, visible []string, compose bool, emit func(*v1.WatchDockerObjectsResponse) error) {
	backoff := 5 * time.Second
	var live *bool
	report := func(next bool) error {
		if live != nil && *live == next {
			return nil
		}
		live = &next
		return emit(&v1.WatchDockerObjectsResponse{WatchStatus: &v1.DockerWatchStatus{ServerId: s.ID, Live: next}})
	}
	for ctx.Err() == nil {
		session, _ := h.dockerAgent(s)
		if session != nil {
			_ = session.WatchDocker(ctx, visible, func(msg *agentv1.DockerInventoryChanged) error {
				if msg.EventStreamReady != nil {
					ready := msg.GetEventStreamReady()
					if ready {
						backoff = 5 * time.Second
					}
					if err := report(ready); err != nil {
						return err
					}
				}
				frame := &v1.WatchDockerObjectsResponse{Resync: msg.GetResync()}
				for _, sample := range msg.GetSamples() {
					if key := sample.GetKey(); slices.Contains(visible, key.GetEngineId()) {
						key.ServerId = s.ID
						frame.Samples = append(frame.Samples, sample)
					}
				}
				for _, update := range msg.GetHealthUpdates() {
					if key := update.GetKey(); slices.Contains(visible, key.GetEngineId()) || compose && docker.IsFullID(key.GetEngineId()) {
						key.ServerId = s.ID
						frame.HealthUpdates = append(frame.HealthUpdates, update)
					}
				}
				if frame.Resync || len(frame.Samples) != 0 || len(frame.HealthUpdates) != 0 {
					return emit(frame)
				}
				return nil
			})
		}
		if ctx.Err() != nil || report(false) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, time.Minute)
		}
	}
}

func (h *Handler) classifyDocker(ctx context.Context, server string, list []*v1.DockerObject) error {
	// Inventory is about the reporting server. Resolve ownership from its placed
	// resources and the independently observed full instance ID.
	for _, row := range list {
		row.Key = &v1.DockerObjectKey{ServerId: server, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER, EngineId: row.GetKey().GetEngineId()}
		row.ManagedResource, row.ManagedResourceId, row.ManagedRevision = nil, "", 0
		if row.GetOwnership() != v1.DockerOwnership_DOCKER_OWNERSHIP_SYSTEM {
			row.Ownership = v1.DockerOwnership_DOCKER_OWNERSHIP_EXTERNAL
		}
	}
	if len(list) == 0 || h.agents == nil {
		return nil
	}
	report, _ := h.agents.Health(server)
	byID := make(map[string]*v1.InstanceHealth, len(report.Instances))
	for _, item := range report.Instances {
		byID[item.GetContainerId()] = item
	}
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		listed, err := tx.ListPlaced(ctx, server)
		if err != nil || len(listed) == 0 {
			return err
		}
		byProject := make(map[string]store.Resource)
		placed := make(map[string]store.Resource)
		for _, held := range listed {
			if held.Kind == string(service.Kind) || held.Kind == string(container.Kind) {
				byProject[docker.Project(held.Scope)+"/"+held.Name] = held
				placed[held.ID] = held
			}
		}
		var unresolved []*v1.DockerObject
		for _, row := range list {
			if row.GetOwnership() == v1.DockerOwnership_DOCKER_OWNERSHIP_SYSTEM {
				continue
			}
			instance := byID[row.GetKey().GetEngineId()]
			if held, ok := byProject[instance.GetProject()+"/"+instance.GetService()]; ok {
				ownDockerRow(row, held)
			} else {
				unresolved = append(unresolved, row)
			}
		}
		if len(unresolved) == 0 || len(placed) == 0 {
			return nil
		}
		adopted, err := tx.PlacedAdoptions(ctx, server)
		if err != nil {
			return err
		}
		for _, row := range unresolved {
			if held, ok := placed[adopted[row.GetKey().GetEngineId()]]; ok {
				ownDockerRow(row, held)
			}
		}
		return nil
	}); err != nil {
		return h.internal(ctx, err)
	}
	return nil
}

func ownDockerRow(row *v1.DockerObject, held store.Resource) {
	row.Ownership = v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED
	row.ManagedResource = &v1.ResourceRef{Kind: held.Kind, Scope: held.Scope, Name: held.Name}
	row.ManagedResourceId, row.ManagedRevision = held.ID, int64(held.Revision)
}
