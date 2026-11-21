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
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
)

type cleanupRead struct {
	targets     []*v1.DockerCleanupTarget
	pause       *v1.DockerCleanupPause
	unavailable *v1.DockerUnavailable
	err         error
}

func (h *Handler) readDockerCleanup(ctx context.Context, servers []Server, filters *v1.DockerCleanupFilters, fresh bool) (*v1.PreviewDockerCleanupResponse, []*v1.DockerCleanupTarget, error) {
	reads := make([]cleanupRead, len(servers))
	var group sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, s := range servers {
		slots <- struct{}{}
		group.Go(func() {
			defer func() { <-slots }()
			session, state := h.dockerAgent(s)
			if session != nil && !session.DockerCleanup() {
				session, state = nil, v1.DockerReadState_DOCKER_READ_STATE_AGENT_OUTDATED
			}
			if session != nil {
				held, err := revision.DockerCleanupProtection(ctx, h.db, s.ID, time.Now(), filters.GetNetworks())
				ask := &agentv1.DockerMaintenancePreviewCall{Filters: filters, ProtectedImages: held.Images, ProtectedNetworks: held.Networks}
				if err != nil {
					reads[i].err = err
					return
				}
				ask.Fresh = fresh
				if result, err := session.DockerCleanupPreview(ctx, ask); err == nil {
					reads[i].targets = result.GetTargets()
					reads[i].pause = result.GetPause()
					return
				} else if resource.ReasonOf(err) == resource.ReasonDiskMeasuring {
					// Present, with the default state, means the count is pending.
					reads[i].unavailable = &v1.DockerUnavailable{ServerId: s.ID}
					return
				}
				state = v1.DockerReadState_DOCKER_READ_STATE_ENGINE_UNAVAILABLE
			}
			reads[i].unavailable = &v1.DockerUnavailable{ServerId: s.ID, State: state}
		})
	}
	group.Wait()
	out := &v1.PreviewDockerCleanupResponse{}
	var all []*v1.DockerCleanupTarget
	hash := sha256.New()
	fmt.Fprintln(hash, filters.GetTaggedImages(), filters.GetNetworks())
	for i, read := range reads {
		if read.err != nil {
			return nil, nil, h.internal(ctx, read.err)
		}
		fmt.Fprintln(hash, servers[i].ID, read.unavailable.GetState())
		if read.pause != nil {
			read.pause.ServerId = servers[i].ID
			out.Pauses = append(out.Pauses, read.pause)
			fmt.Fprintln(hash, read.pause.GetToken())
		}
		if read.unavailable != nil {
			out.Unavailable = append(out.Unavailable, read.unavailable)
		}
		for _, target := range read.targets {
			target.ServerId = servers[i].ID
			all = append(all, target)
		}
	}
	// Eligible objects lead the pages so the defaults are visible before the
	// protected section, even on a busy host.
	slices.SortStableFunc(all, func(a, b *v1.DockerCleanupTarget) int {
		if ac, bc := a.GetEvidence() != "", b.GetEvidence() != ""; ac != bc {
			if ac {
				return -1
			}
			return 1
		}
		if a.GetKind() != b.GetKind() {
			return int(a.GetKind() - b.GetKind())
		}
		if order := strings.Compare(a.GetServerId(), b.GetServerId()); order != 0 {
			return order
		}
		if order := strings.Compare(a.GetEngineId(), b.GetEngineId()); order != 0 {
			return order
		}
		return strings.Compare(a.GetName(), b.GetName())
	})
	for _, target := range all {
		fmt.Fprintf(hash, "%q %d %q %q %d %q\n", target.GetServerId(), target.GetKind(), target.GetEngineId(), target.GetName(), target.GetReason(), target.GetEvidence())
	}
	out.Snapshot = hex.EncodeToString(hash.Sum(nil))
	return out, all, nil
}

type cleanupPage struct {
	Snapshot string
	Offset   int
}

func cleanupStale() error {
	return authapi.ReasonError(connect.CodeAborted, errors.New("the Docker cleanup candidates changed. Preview cleanup again"), "docker_cleanup_changed")
}

// PreviewDockerCleanup reports eligible and protected objects in a bounded page.
func (h *Handler) PreviewDockerCleanup(ctx context.Context, req *connect.Request[v1.PreviewDockerCleanupRequest]) (*connect.Response[v1.PreviewDockerCleanupResponse], error) {
	servers, err := h.dockerServers(ctx, req.Msg.GetServerIds())
	if err != nil {
		return nil, err
	}
	out, targets, err := h.readDockerCleanup(ctx, servers, req.Msg.GetFilters(), false)
	if err != nil {
		return nil, err
	}
	var page cleanupPage
	if token := req.Msg.GetPageToken(); token != "" {
		if len(token) > 160 {
			return nil, cleanupStale()
		}
		data, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || json.Unmarshal(data, &page) != nil || page.Snapshot != out.Snapshot || page.Offset < 0 || page.Offset > len(targets) {
			return nil, cleanupStale()
		}
	}
	size := int(min(req.Msg.GetPageSize(), 48))
	if size == 0 {
		size = 48
	}
	end := min(page.Offset+size, len(targets))
	for _, target := range targets[page.Offset:end] {
		if target.GetEvidence() == "" {
			out.Exclusions = append(out.Exclusions, target)
		} else {
			out.Candidates = append(out.Candidates, target)
		}
	}
	if end < len(targets) {
		token, err := json.Marshal(cleanupPage{Snapshot: out.Snapshot, Offset: end})
		if err != nil {
			return nil, h.internal(ctx, err)
		}
		out.NextPageToken = base64.RawURLEncoding.EncodeToString(token)
	}
	return connect.NewResponse(out), nil
}

// PlanDockerMaintenance freezes the selected candidate evidence in a regular change.
func (h *Handler) PlanDockerMaintenance(ctx context.Context, req *connect.Request[v1.PlanDockerMaintenanceRequest]) (*connect.Response[v1.PlanDockerMaintenanceResponse], error) {
	m := req.Msg
	if _, ok := m.GetOperation().(*v1.PlanDockerMaintenanceRequest_ResumeCleanupToken); ok {
		return h.planResumeDockerCleanup(ctx, m)
	}
	if m.GetPull() != nil || m.GetTag() != nil {
		return h.planDockerImage(ctx, m)
	}
	cleanup := m.GetCleanup()
	selected := cleanup.GetTargets()
	if len(m.GetServerIds()) == 0 || len(m.GetExpectedSnapshot()) != 64 || len(selected) == 0 || len(selected) > 12 || len(m.GetSummary()) > 200 {
		return nil, authapi.Invalid(errors.New("select up to 12 cleanup candidates on explicitly selected servers"))
	}
	servers, err := h.dockerServers(ctx, m.GetServerIds())
	if err != nil {
		return nil, err
	}
	preview, all, err := h.readDockerCleanup(ctx, servers, cleanup.GetFilters(), true)
	if err != nil {
		return nil, err
	}
	if preview.GetSnapshot() != m.GetExpectedSnapshot() {
		return nil, cleanupStale()
	}
	byServer := make(map[string]*v1.DockerMaintenance)
	seen := make(map[*v1.DockerCleanupTarget]bool)
	for _, reviewed := range selected {
		i := slices.IndexFunc(all, func(fresh *v1.DockerCleanupTarget) bool {
			return fresh.GetEvidence() != "" && proto.Equal(reviewed, fresh)
		})
		if i < 0 || seen[all[i]] {
			return nil, cleanupStale()
		}
		fresh := all[i]
		seen[fresh] = true
		server := fresh.GetServerId()
		if byServer[server] == nil {
			byServer[server] = &v1.DockerMaintenance{}
		}
		byServer[server].Targets = append(byServer[server].Targets, fresh)
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = "Clean up Docker artifacts"
	}
	change, err := h.changes.PlanDockerMaintenance(ctx, summary, byServer)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.PlanDockerMaintenanceResponse{Change: change}), nil
}

func (h *Handler) planResumeDockerCleanup(ctx context.Context, req *v1.PlanDockerMaintenanceRequest) (*connect.Response[v1.PlanDockerMaintenanceResponse], error) {
	token := req.GetResumeCleanupToken()
	if len(req.GetServerIds()) != 1 || !docker.IsFullID(token) {
		return nil, authapi.Invalid(errors.New("select one paused server to re-enable Docker cleanup"))
	}
	servers, err := h.dockerServers(ctx, req.GetServerIds())
	if err != nil {
		return nil, err
	}
	server := servers[0]
	session, _ := h.dockerAgent(server)
	if session == nil || !session.DockerCleanup() {
		return nil, cleanupStale()
	}
	preview, err := session.DockerCleanupPreview(ctx, &agentv1.DockerMaintenancePreviewCall{})
	if err != nil || preview.GetPause().GetToken() != token {
		return nil, cleanupStale()
	}
	change, err := h.changes.PlanDockerMaintenance(ctx, "Re-enable Docker cleanup", map[string]*v1.DockerMaintenance{
		server.ID: {ResumeCleanupToken: token},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.PlanDockerMaintenanceResponse{Change: change}), nil
}
