// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

const dockerCallWait = 12 * time.Second

// DockerInventory reports whether this agent supports Docker inventory reads.
func (s *Session) DockerInventory() bool { return s.dockerCapabilities.GetInventory() }

// DockerComposeMembers reports whether the agent supports the lightweight membership scan.
func (s *Session) DockerComposeMembers() bool { return s.dockerCapabilities.GetComposeMembers() }

// DockerImages reports support for local images and their detail.
func (s *Session) DockerImages() bool { return s.dockerCapabilities.GetImageInventory() }

// DockerCleanup reports support for reviewed cleanup.
func (s *Session) DockerCleanup() bool { return s.dockerCapabilities.GetCleanup() }

// DockerArtifactCleanup reports support for image and network removals.
func (s *Session) DockerArtifactCleanup() bool { return s.dockerCapabilities.GetArtifactCleanup() }

func dockerProtection(ctx context.Context) *agentv1.DockerCleanupProtection {
	held, ok := resource.DockerProtectionOf(ctx)
	if !ok {
		return nil
	}
	return &agentv1.DockerCleanupProtection{Images: held.Images, Networks: held.Networks}
}

// DockerCleanupPreview reads Engine evidence without changing it.
func (s *Session) DockerCleanupPreview(ctx context.Context, ask *agentv1.DockerMaintenancePreviewCall) (*agentv1.DockerMaintenancePreviewResult, error) {
	call := &agentv1.Call{Call: &agentv1.Call_DockerMaintenancePreview{DockerMaintenancePreview: ask}}
	result, err := s.callOnceWithin(ctx, call, DiskUseTimeout)
	if err != nil {
		return nil, err
	}
	if out := result.GetDockerMaintenancePreview(); out != nil {
		return out, nil
	}
	return nil, errors.New("the agent didn't return Docker cleanup evidence")
}

// ExactTerminal reports whether this agent understands immutable IDs and argv.
func (s *Session) ExactTerminal() bool { return s.dockerCapabilities.GetExactTerminal() }

// DockerWorkloadControls reports whether the agent preserves saved Docker run intent.
func (s *Session) DockerWorkloadControls() bool { return s.dockerCapabilities.GetWorkloadControls() }

// DockerList reads a bounded inventory page or counters from this agent.
func (s *Session) DockerList(ctx context.Context, ask *agentv1.DockerInventoryListCall) (*agentv1.DockerInventoryListResult, error) {
	wait := dockerCallWait
	if ask.GetKind() == dockwellv1.DockerObjectKind_DOCKER_OBJECT_KIND_IMAGE {
		wait = DiskUseTimeout
	}
	r, err := s.callOnceWithin(ctx, &agentv1.Call{Call: &agentv1.Call_DockerInventoryList{DockerInventoryList: ask}}, wait)
	if err != nil {
		return nil, err
	}
	if out := r.GetDockerInventoryList(); out != nil {
		return out, nil
	}
	return nil, errors.New("the agent didn't return Docker inventory")
}

// DockerDetail inspects the exact instance through this agent.
func (s *Session) DockerDetail(ctx context.Context, ask *agentv1.DockerInventoryDetailCall) (*agentv1.DockerInventoryDetailResult, error) {
	wait := dockerCallWait
	if ask.GetImageId() != "" {
		wait = DiskUseTimeout
	}
	r, err := s.callOnceWithin(ctx, &agentv1.Call{Call: &agentv1.Call_DockerInventoryDetail{DockerInventoryDetail: ask}}, wait)
	if err != nil {
		return nil, err
	}
	if out := r.GetDockerInventoryDetail(); out != nil {
		return out, nil
	}
	return nil, errors.New("the agent didn't return Docker details")
}

func (s *Session) callOnceWithin(ctx context.Context, call *agentv1.Call, timeout time.Duration) (*agentv1.CallResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.callOnce(ctx, call)
}

// WatchDocker keeps a renewable subscription on this stream. Updates are
// coalesced here when a slow public client is unable to consume them.
func (s *Session) WatchDocker(ctx context.Context, visible []string, emit func(*agentv1.DockerInventoryChanged) error) error {
	id := rand.Text()
	inbox := make(chan *agentv1.DockerInventoryChanged, 1)

	s.mu.Lock()
	if len(s.dockerWatches) >= 8 {
		s.mu.Unlock()
		return errors.New("this server has eight Docker inventory watches open")
	}
	if s.dockerWatches == nil {
		s.dockerWatches = make(map[string]chan *agentv1.DockerInventoryChanged)
	}
	s.dockerWatches[id] = inbox
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.dockerWatches, id)
		s.mu.Unlock()
	}()

	renew := func(ctx context.Context, stop bool) error {
		ask := &agentv1.DockerInventoryWatchCall{SubscriptionId: id, VisibleIds: visible, Stop: stop}
		reply, err := s.callOnceWithin(ctx, &agentv1.Call{Call: &agentv1.Call_DockerInventoryWatch{DockerInventoryWatch: ask}}, 3*time.Second)
		if err == nil && reply.GetDockerInventoryWatch() == nil {
			return errors.New("the agent didn't confirm the Docker subscription")
		}
		return err
	}
	defer func() {
		if s.isDone() {
			return
		}
		_ = renew(context.WithoutCancel(ctx), true)
	}()
	if err := renew(ctx, false); err != nil {
		return err
	}
	tick := time.NewTicker(45 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return errors.New("the agent disconnected")
		case <-tick.C:
			if err := renew(ctx, false); err != nil {
				return err
			}
		case msg := <-inbox:
			if err := emit(msg); err != nil {
				return err
			}
		}
	}
}

func (s *Session) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Session) dockerChanged(msg *agentv1.DockerInventoryChanged) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inbox := s.dockerWatches[msg.GetSubscriptionId()]
	if inbox == nil {
		return
	}
	select {
	case inbox <- msg:
	default:
		select {
		case prev := <-inbox:
			msg.Resync = msg.GetResync() || prev.GetResync()
			for _, update := range prev.GetHealthUpdates() {
				if !slices.ContainsFunc(msg.GetHealthUpdates(), func(next *dockwellv1.DockerHealthUpdate) bool {
					return next.GetKey().GetEngineId() == update.GetKey().GetEngineId()
				}) && len(msg.GetHealthUpdates()) < 16 {
					msg.HealthUpdates = append(msg.HealthUpdates, update)
				}
			}
		default:
		}
		inbox <- msg
	}
}

func (s *Session) applyDockerCleanup(ctx context.Context, step resource.Step) error {
	call := &agentv1.ApplyCall{
		Step:                    resourcepb.Step(step, ""),
		DockerCleanupProtection: dockerProtection(ctx),
		Desired:                 &agentv1.DesiredResource{Resource: resourcepb.Ref(step.Ref), DockerMaintenance: step.DockerMaintenance},
	}
	_, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Apply{Apply: call}}, s.agents.applyTimeout,
		func(session *Session) error {
			if !session.DockerCleanup() || !session.DockerArtifactCleanup() && slices.ContainsFunc(step.DockerMaintenance.GetTargets(), func(t *dockwellv1.DockerCleanupTarget) bool {
				return t.GetKind() != dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE
			}) {
				return fault.New(ErrOlderAgent, "update the agent to apply Docker cleanup")
			}
			return nil
		})
	return err
}

type dockerImageLogins interface {
	DockerImageLogin(context.Context, string, string, string) (*dockwellv1.DockerImageLoginRef, registry.Auth, error)
}

// DockerImageOperations reports whether this connection understands reviewed image actions.
func (s *Session) DockerImageOperations() bool { return s.dockerCapabilities.GetImageOperations() }

// DockerImageLogin picks or reopens a saved registry connection.
func (s *Session) DockerImageLogin(ctx context.Context, project, reference, id string) (*dockwellv1.DockerImageLoginRef, registry.Auth, error) {
	if l, ok := s.agents.logins.(dockerImageLogins); ok {
		return l.DockerImageLogin(ctx, project, reference, id)
	}
	if id != "" {
		return nil, registry.Auth{}, ErrNoLogins
	}
	return nil, registry.Auth{}, nil
}

// PreviewDockerImage uses the existing read-only planning call to freeze Docker
// and registry evidence. It doesn't hold a change or assign a change ID.
func (s *Session) PreviewDockerImage(ctx context.Context, ref resource.Ref, image *dockwellv1.DockerImageAction, login registry.Auth) (*dockwellv1.DockerImageAction, error) {
	plan := &dockwellv1.DockerMaintenance{ImageAction: image}
	wanted := &agentv1.DesiredResource{Resource: resourcepb.Ref(ref), DockerMaintenance: plan}
	reply, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Plan{Plan: &agentv1.PlanCall{Desired: []*agentv1.DesiredResource{wanted}}}}, dockerCallWait,
		func(session *Session) error { return session.imageOperationAuth(wanted, ref, login) })
	if err != nil {
		return nil, err
	}
	steps := reply.GetPlan().GetSteps()
	if len(steps) != 1 || steps[0].GetDockerMaintenance().GetImageAction() == nil {
		return nil, ErrOlderAgent
	}
	return steps[0].GetDockerMaintenance().GetImageAction(), nil
}

func (s *Session) imageOperationAuth(wanted *agentv1.DesiredResource, ref resource.Ref, login registry.Auth) error {
	if !s.DockerImageOperations() {
		return fault.New(ErrOlderAgent, "update the agent to use Docker image actions")
	}
	if login.Registry == "" {
		return nil
	}
	sealed, err := s.sealLogin(resource.DockerImageAuthRef(ref, wanted.GetDockerMaintenance()), login)
	wanted.DockerRegistryAuth = sealed
	return err
}

func (s *Session) applyDockerImage(ctx context.Context, step resource.Step) error {
	plan := step.DockerMaintenance
	image := plan.GetImageAction()
	wanted := &agentv1.DesiredResource{Resource: resourcepb.Ref(step.Ref), DockerMaintenance: plan}
	call := &agentv1.ApplyCall{Step: resourcepb.Step(step, ""), DockerCleanupProtection: dockerProtection(ctx), Desired: wanted}
	_, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Apply{Apply: call}}, s.agents.applyTimeout, func(session *Session) error {
		var login registry.Auth
		if refs := image.GetApprovedLogins(); len(refs) > 0 {
			saved, auth, err := session.DockerImageLogin(ctx, image.GetCredentialProject(), image.GetPull().GetReference(), refs[0].GetId())
			if err != nil || !proto.Equal(saved, refs[0]) {
				return errors.New("the registry login changed. Review the image action again")
			}
			login = auth
		}
		return session.imageOperationAuth(wanted, step.Ref, login)
	})
	return err
}
