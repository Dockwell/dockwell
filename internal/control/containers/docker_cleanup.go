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
	"strconv"
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

const (
	maxCleanupTargetsPerServer = 200
	maxCleanupTargetsTotal     = 500
	legacyCleanupTargets       = 12
	defaultCleanupPageSize     = 200
	maxCleanupPageSize         = 250
)

type cleanupRead struct {
	targets         []*v1.DockerCleanupTarget
	pause           *v1.DockerCleanupPause
	unavailable     *v1.DockerUnavailable
	limits          *v1.DockerCleanupServerLimits
	volumeCleanup   bool
	extendedCleanup bool
	err             error
}

func cleanupFilterWants(filters *v1.DockerCleanupFilters, kind v1.DockerCleanupKind) bool {
	kinds := filters.GetKinds()
	return len(kinds) == 0 || slices.Contains(kinds, kind)
}

func (h *Handler) readDockerCleanup(ctx context.Context, servers []Server, filters *v1.DockerCleanupFilters, fresh bool) (*v1.PreviewDockerCleanupResponse, []*v1.DockerCleanupTarget, map[string]cleanupRead, error) {
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
				reads[i].volumeCleanup = session.DockerVolumeCleanup()
				reads[i].extendedCleanup = session.DockerExtendedCleanup()
				capForServer := uint32(legacyCleanupTargets)
				if reads[i].extendedCleanup {
					capForServer = maxCleanupTargetsPerServer
				}
				supported := []v1.DockerCleanupKind{v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE}
				if session.DockerArtifactCleanup() {
					supported = append(supported,
						v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
						v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK,
					)
				}
				if reads[i].volumeCleanup {
					supported = append(supported, v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME)
				}
				reads[i].limits = &v1.DockerCleanupServerLimits{
					ServerId:       s.ID,
					MaxTargets:     capForServer,
					SupportedKinds: supported,
				}
				wantImages := cleanupFilterWants(filters, v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE)
				wantNetworks := cleanupFilterWants(filters, v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK) && filters.GetNetworks()
				wantVolumes := cleanupFilterWants(filters, v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME) && filters.GetVolumes() && reads[i].volumeCleanup
				held, err := revision.DockerCleanupProtectionKinds(ctx, h.db, s.ID, time.Now(), wantImages, wantNetworks, wantVolumes)
				if err != nil {
					reads[i].err = err
					return
				}
				callFilters := filters
				if filters != nil && filters.GetVolumes() && !reads[i].volumeCleanup {
					callFilters = proto.CloneOf(filters)
					callFilters.Volumes = false
				}
				ask := &agentv1.DockerMaintenancePreviewCall{
					Filters:           callFilters,
					ProtectedImages:   held.Images,
					ProtectedNetworks: held.Networks,
					ProtectedVolumes:  held.Volumes,
					BackupVolumes:     held.BackupVolumes,
					RollbackVolumes:   held.RollbackVolumes,
					Fresh:             fresh,
				}
				if result, err := session.DockerCleanupPreview(ctx, ask); err == nil {
					targets := result.GetTargets()
					if kinds := filters.GetKinds(); len(kinds) > 0 && !reads[i].extendedCleanup {
						targets = slices.DeleteFunc(slices.Clone(targets), func(t *v1.DockerCleanupTarget) bool {
							return !slices.Contains(kinds, t.GetKind())
						})
					}
					reads[i].targets = targets
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
	out := &v1.PreviewDockerCleanupResponse{
		MaxTargetsTotal: maxCleanupTargetsTotal,
	}
	var all []*v1.DockerCleanupTarget
	byServerRead := make(map[string]cleanupRead, len(servers))
	hash := sha256.New()
	fmt.Fprintln(hash, filters.GetTaggedImages(), filters.GetNetworks(), filters.GetVolumes(), filters.GetKinds())
	for i, read := range reads {
		if read.err != nil {
			return nil, nil, nil, h.internal(ctx, read.err)
		}
		byServerRead[servers[i].ID] = read
		fmt.Fprintln(hash, servers[i].ID, read.unavailable.GetState())
		if read.limits != nil {
			out.ServerLimits = append(out.ServerLimits, read.limits)
			fmt.Fprintln(hash, read.limits.GetMaxTargets(), read.limits.GetSupportedKinds())
		}
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
		fmt.Fprintf(hash, "%q %d %q %q %d %q\n", target.GetServerId(), target.GetKind(), target.GetEngineId(), target.GetName(), target.GetReason(), cleanupSnapshotEvidence(target))
	}
	out.Snapshot = hex.EncodeToString(hash.Sum(nil))
	return out, all, byServerRead, nil
}

func cleanupSnapshotEvidence(target *v1.DockerCleanupTarget) string {
	evidence := target.GetEvidence()
	if target.GetKind() == v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME {
		evidence, _, _ = strings.Cut(evidence, ".")
	}
	return evidence
}

type cleanupPage struct {
	Snapshot string
	Offset   int
}

func cleanupStale() error {
	return authapi.ReasonError(connect.CodeAborted, errors.New("the Docker cleanup candidates changed. Preview cleanup again"), "docker_cleanup_changed")
}

// sameReviewedCleanupTarget checks that the reviewed target matches the fresh
// projection. A newly created empty volume can be unmeasured in a cached preview
// and report 0 bytes once PlanDockerMaintenance refreshes disk usage, and a
// volume's embedded agent preview timestamp advances between preview and plan.
func sameReviewedCleanupTarget(reviewed, fresh *v1.DockerCleanupTarget) bool {
	if proto.Equal(reviewed, fresh) {
		return true
	}
	adjusted := proto.CloneOf(reviewed)
	if adjusted.EstimatedBytes == nil && fresh.EstimatedBytes != nil && fresh.GetEstimatedBytes() == 0 {
		adjusted.EstimatedBytes = fresh.EstimatedBytes
	}
	if adjusted.GetKind() == v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME && adjusted.GetEvidence() != fresh.GetEvidence() {
		reviewedKey, reviewedTime, _ := strings.Cut(adjusted.GetEvidence(), ".")
		freshKey, freshTime, _ := strings.Cut(fresh.GetEvidence(), ".")
		reviewedAt, reviewedErr := strconv.ParseInt(reviewedTime, 10, 64)
		freshAt, freshErr := strconv.ParseInt(freshTime, 10, 64)
		if reviewedKey != freshKey || reviewedErr != nil || freshErr != nil || reviewedAt <= 0 || reviewedAt > freshAt {
			return false
		}
		adjusted.Evidence = fresh.GetEvidence()
	}
	return proto.Equal(adjusted, fresh)
}

// PreviewDockerCleanup reports eligible and protected objects in a bounded page.
func (h *Handler) PreviewDockerCleanup(ctx context.Context, req *connect.Request[v1.PreviewDockerCleanupRequest]) (*connect.Response[v1.PreviewDockerCleanupResponse], error) {
	servers, err := h.dockerServers(ctx, req.Msg.GetServerIds())
	if err != nil {
		return nil, err
	}
	out, targets, _, err := h.readDockerCleanup(ctx, servers, req.Msg.GetFilters(), false)
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
	size := int(min(req.Msg.GetPageSize(), maxCleanupPageSize))
	if size == 0 {
		size = defaultCleanupPageSize
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
	if m.GetBuild() != nil {
		return h.planDockerBuild(ctx, m)
	}
	if m.GetLoad() != nil || m.GetExport() != nil {
		return h.planDockerArchive(ctx, m)
	}
	if m.GetPush() != nil {
		return h.planDockerPush(ctx, m)
	}
	if m.GetPull() != nil || m.GetTag() != nil {
		return h.planDockerImage(ctx, m)
	}
	cleanup := m.GetCleanup()
	selected := cleanup.GetTargets()
	if len(m.GetServerIds()) == 0 || len(m.GetExpectedSnapshot()) != 64 || len(selected) == 0 || len(selected) > maxCleanupTargetsTotal || len(m.GetSummary()) > 200 {
		return nil, authapi.Invalid(fmt.Errorf("select up to %d cleanup candidates on explicitly selected servers", maxCleanupTargetsTotal))
	}
	servers, err := h.dockerServers(ctx, m.GetServerIds())
	if err != nil {
		return nil, err
	}
	preview, all, reads, err := h.readDockerCleanup(ctx, servers, cleanup.GetFilters(), true)
	if err != nil {
		return nil, err
	}
	if preview.GetSnapshot() != m.GetExpectedSnapshot() {
		return nil, cleanupStale()
	}
	index := make(map[string]*v1.DockerCleanupTarget, len(all))
	for _, fresh := range all {
		if fresh.GetEvidence() == "" {
			continue
		}
		key := fmt.Sprintf("%s\x00%d\x00%s", fresh.GetServerId(), fresh.GetKind(), fresh.GetEngineId())
		index[key] = fresh
	}
	byServer := make(map[string]*v1.DockerMaintenance)
	seen := make(map[*v1.DockerCleanupTarget]bool, len(selected))
	for _, reviewed := range selected {
		key := fmt.Sprintf("%s\x00%d\x00%s", reviewed.GetServerId(), reviewed.GetKind(), reviewed.GetEngineId())
		fresh := index[key]
		if fresh == nil || seen[fresh] || !sameReviewedCleanupTarget(reviewed, fresh) {
			return nil, cleanupStale()
		}
		seen[fresh] = true
		server := fresh.GetServerId()
		read := reads[server]
		if fresh.GetKind() == v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME && !read.volumeCleanup {
			return nil, authapi.Invalid(errors.New("update the agent on this server to remove unused Docker volumes"))
		}
		if byServer[server] == nil {
			byServer[server] = &v1.DockerMaintenance{}
		}
		if fresh.GetEvidence() != reviewed.GetEvidence() {
			// Keep the time from the preview the operator reviewed.
			fresh = proto.CloneOf(fresh)
			fresh.Evidence = reviewed.GetEvidence()
		}
		byServer[server].Targets = append(byServer[server].Targets, fresh)
		capForServer := maxCleanupTargetsPerServer
		if !read.extendedCleanup {
			capForServer = legacyCleanupTargets
		}
		if len(byServer[server].Targets) > capForServer {
			if !read.extendedCleanup {
				return nil, authapi.Invalid(errors.New("this agent can remove at most 12 Docker artifacts per plan. Update the agent to remove more at once"))
			}
			return nil, authapi.Invalid(fmt.Errorf("select up to %d cleanup candidates per server", maxCleanupTargetsPerServer))
		}
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = cleanupSummary(selected)
	}
	change, err := h.changes.PlanDockerMaintenance(ctx, summary, byServer)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.PlanDockerMaintenanceResponse{Change: change}), nil
}

func cleanupSummary(targets []*v1.DockerCleanupTarget) string {
	var kind v1.DockerCleanupKind
	for _, t := range targets {
		if kind == 0 {
			kind = t.GetKind()
		} else if kind != t.GetKind() {
			return "Clean up Docker artifacts"
		}
	}
	switch kind {
	case v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE:
		return "Remove unused Docker images"
	case v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME:
		return "Remove unused Docker volumes"
	case v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK:
		return "Remove unused Docker networks"
	case v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE:
		return "Remove Docker build cache"
	default:
		return "Clean up Docker artifacts"
	}
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
