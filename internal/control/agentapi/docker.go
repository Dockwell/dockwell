// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
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
func (s *Session) WatchDocker(ctx context.Context, kind dockwellv1.DockerObjectKind, visible []string, emit func(*agentv1.DockerInventoryChanged) error) error {
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
		ask := &agentv1.DockerInventoryWatchCall{SubscriptionId: id, VisibleIds: visible, Stop: stop, Kind: kind}
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
	DockerPushLogin(context.Context, string, string, string) (*dockwellv1.DockerImageLoginRef, registry.Auth, error)
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

// DockerPushLogin reopens a saved registry login approved for push.
func (s *Session) DockerPushLogin(ctx context.Context, project, reference, id string) (*dockwellv1.DockerImageLoginRef, registry.Auth, error) {
	if l, ok := s.agents.logins.(dockerImageLogins); ok {
		return l.DockerPushLogin(ctx, project, reference, id)
	}
	return nil, registry.Auth{}, ErrNoLogins
}

// PreviewDockerImage uses the existing read-only planning call to freeze Docker
// and registry evidence. It doesn't hold a change or assign a change ID.
func (s *Session) PreviewDockerImage(ctx context.Context, ref resource.Ref, image *dockwellv1.DockerImageAction, login registry.Auth) (*dockwellv1.DockerImageAction, error) {
	plan := &dockwellv1.DockerMaintenance{ImageAction: image}
	wanted := &agentv1.DesiredResource{Resource: resourcepb.Ref(ref), DockerMaintenance: plan}
	wait := dockerCallWait
	if image.GetBuild() != nil {
		wait = 3 * time.Minute
	}
	reply, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Plan{Plan: &agentv1.PlanCall{Desired: []*agentv1.DesiredResource{wanted}}}}, wait,
		func(session *Session) error {
			if image.GetBuild() != nil {
				return session.sealDockerBuild(ctx, ref, wanted)
			}
			return session.imageOperationAuth(wanted, ref, login)
		})
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
	wait, reference := s.agents.applyTimeout, image.GetPull().GetReference()
	if push := image.GetPush(); push != nil {
		wait += registry.PushTimeout + 30*time.Minute
		reference = push.GetDestination()
	}
	if image.GetBuild() != nil {
		wait += 2*time.Hour + resource.BuildBudget
	}
	_, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_Apply{Apply: call}}, wait, func(session *Session) error {
		if image.GetBuild() != nil {
			return session.sealDockerBuild(ctx, step.Ref, wanted)
		}
		var login registry.Auth
		if refs := image.GetApprovedLogins(); len(refs) > 0 {
			open := session.DockerImageLogin
			if image.GetPush() != nil {
				open = session.DockerPushLogin
			}
			saved, auth, err := open(ctx, image.GetCredentialProject(), reference, refs[0].GetId())
			if err != nil || !proto.Equal(saved, refs[0]) {
				return errors.New("the registry login changed. Review the image action again")
			}
			login = auth
		}
		return session.imageOperationAuth(wanted, step.Ref, login)
	})
	if push := image.GetPush(); push != nil && (errors.Is(err, ErrLost) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
		resource.Report(ctx, resource.Outcome{DockerImageReceipts: []*dockwellv1.DockerImageReceipt{{
			Outcome:  dockwellv1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN,
			Reason:   dockwellv1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED,
			ImageKey: push.GetSourceKey(), Reference: reference, Digest: image.GetDescriptor_().GetManifestDigest(),
		}}})
		// A lost apply can already have published the manifest. Returning
		// ErrLost here would queue another apply when the agent reconnects.
		return resource.PlainlyBecause("docker_push_unknown", "The registry result is unknown. Check the destination before reviewing again", registry.ErrPushUnknown)
	}
	return err
}

// DockerStorage reports support for storage inventory and guarded storage plans.
func (s *Session) DockerStorage() bool { return s.dockerCapabilities.GetNetworkVolumeInventory() }

// CheckDockerNetwork asks this Engine to validate a candidate without changing it.
func (s *Session) CheckDockerNetwork(ctx context.Context, ask *agentv1.CheckDockerNetworkCall) (*agentv1.CheckDockerNetworkResult, error) {
	result, err := s.callOnceWithin(ctx, &agentv1.Call{Call: &agentv1.Call_CheckDockerNetwork{CheckDockerNetwork: ask}}, dockerCallWait)
	if err != nil {
		return nil, err
	}
	if out := result.GetCheckDockerNetwork(); out != nil {
		return out, nil
	}
	return nil, errors.New("the agent did not check the Docker network")
}

func (s *Session) checkDockerStoragePlan(ctx context.Context, d *resource.Desired) error {
	n, _ := d.Spec.(*resourcev1.Network)
	if !s.DockerStorage() && (d.DockerStorageGuard != nil || n.GetIpam() != nil) {
		return fault.New(ErrOlderAgent, "update the agent to review this Docker storage plan")
	}
	if guard := d.DockerStorageGuard.GetCreate(); n != nil && (guard != nil || n.GetIpam() != nil) {
		name := network.TargetOf(d.Ref).Name
		result, err := s.CheckDockerNetwork(ctx, &agentv1.CheckDockerNetworkCall{EngineName: name, Spec: n})
		if err != nil {
			return err
		}
		for _, finding := range result.GetFindings() {
			switch finding.GetCode() {
			case dockwellv1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_SUBNET_OVERLAP:
				if finding.GetConflictingNetwork() == name {
					continue
				}
				return fmt.Errorf("the subnet overlaps %s on %s", finding.GetConflictingSubnet(), finding.GetConflictingNetwork())
			case dockwellv1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_NAME_IN_USE:
				// A fresh plan checks ActionCreate. A resumed create then
				// checks its creation token on the agent, before any write.
			default:
				return fmt.Errorf("the Engine refused the network's %s", finding.GetFieldPath())
			}
		}
	}
	return nil
}

func (s *Session) checkDockerAttachmentPlan(ctx context.Context, g *dockwellv1.DockerNetworkAttachmentGuard) error {
	net, err := s.DockerDetail(ctx, &agentv1.DockerInventoryDetailCall{NetworkId: g.GetNetworkId()})
	if err != nil || net.GetObject().GetKey().GetEngineId() != g.GetNetworkId() || net.GetObject().GetName() != g.GetNetworkName() {
		return errors.New("the reviewed network changed. Review the attachment again")
	}
	ctr, err := s.DockerDetail(ctx, &agentv1.DockerInventoryDetailCall{ContainerId: g.GetContainerId()})
	if err != nil || ctr.GetObject().GetKey().GetEngineId() != g.GetContainerId() {
		return errors.New("the reviewed container changed. Review the attachment again")
	}
	for _, ep := range ctr.GetObject().GetContainer().GetEndpoints() {
		if !docker.MatchesReviewedNetwork(ep.GetNetwork(), ep.GetNetworkId(), g.GetNetworkName(), g.GetNetworkId()) {
			continue
		}
		if g.GetAction() == dockwellv1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_DISCONNECT &&
			(ep.GetNetworkId() == g.GetNetworkId() || ep.GetNetworkId() == "") &&
			g.GetNetworkId() == g.GetExpectedEndpointId() {
			return nil
		}
		return errors.New("the reviewed network attachment changed. Review it again")
	}
	if g.GetAction() != dockwellv1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_CONNECT ||
		ctr.GetObject().GetContainer().GetDetailsTruncated() || g.GetExpectedEndpointId() != "" {
		return errors.New("the reviewed network attachment changed. Review it again")
	}
	return nil
}

// DockerSettings reads or validates one daemon configuration through its agent.
func (s *Session) DockerSettings(ctx context.Context, ask *agentv1.DockerSettingsCall) (*agentv1.DockerSettingsResult, error) {
	if !s.dockerCapabilities.GetDaemonSettings() {
		return nil, fault.New(ErrOlderAgent, "update the agent to read Docker settings")
	}
	res, err := s.callOnceWithin(ctx, &agentv1.Call{Call: &agentv1.Call_DockerSettings{DockerSettings: ask}}, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if res.GetDockerSettings() == nil {
		return nil, errors.New("the agent did not return Docker settings")
	}
	return res.GetDockerSettings(), nil
}

// DockerContainerLogging reports whether the agent supports typed container log settings.
func (s *Session) DockerContainerLogging() bool { return s.dockerCapabilities.GetContainerLogging() }

// CheckDockerLoggingTarget checks the reviewed Engine identity before a log policy revision.
func (s *Session) CheckDockerLoggingTarget(ctx context.Context, id string) (bool, error) {
	if !s.DockerContainerLogging() {
		return false, fault.New(ErrOlderAgent, "update the agent to review log rotation")
	}
	res, err := s.DockerDetail(ctx, &agentv1.DockerInventoryDetailCall{ContainerId: id})
	return res.GetObject().GetKey().GetEngineId() == id, err
}
