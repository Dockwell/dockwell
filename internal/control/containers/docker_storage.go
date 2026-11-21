// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	dockerNetworkKind = v1.DockerObjectKind_DOCKER_OBJECT_KIND_NETWORK
	dockerVolumeKind  = v1.DockerObjectKind_DOCKER_OBJECT_KIND_VOLUME
)

func storageKind(kind v1.DockerObjectKind) bool {
	return kind == dockerNetworkKind || kind == dockerVolumeKind
}

func (h *Handler) classifyDockerStorage(ctx context.Context, server string, rows []*v1.DockerObject) error {
	for _, row := range rows {
		row.Key.ServerId = server
	}
	if len(rows) == 0 {
		return nil
	}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		placed, err := tx.ListPlaced(ctx, server)
		if err != nil {
			return err
		}
		byName := make(map[string]store.Resource)
		volumes := make(map[string]store.Resource)
		for _, r := range placed {
			if r.Kind == string(network.Kind) {
				name := network.TargetOf(resource.Ref{Kind: network.Kind, Scope: r.Scope, Name: r.Name}).Name
				byName[name] = r
			} else if r.Kind == string(volume.Kind) {
				name := volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: r.Scope, Name: r.Name}).Name
				volumes[name] = r
			}
		}
		for _, row := range rows {
			held, placed := byName[row.GetName()]
			if row.GetVolume() != nil {
				held, placed = volumes[row.GetName()]
			}
			switch {
			case row.GetOwnership() == v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED && placed:
				ownDockerRow(row, held)
			case row.GetOwnership() != v1.DockerOwnership_DOCKER_OWNERSHIP_SYSTEM:
				row.Ownership = v1.DockerOwnership_DOCKER_OWNERSHIP_EXTERNAL
			}
		}
		return nil
	})
	if err != nil {
		return h.internal(ctx, err)
	}
	return nil
}

// CheckDockerNetwork reports live name and address conflicts without a write.
func (h *Handler) CheckDockerNetwork(ctx context.Context, req *connect.Request[v1.CheckDockerNetworkRequest]) (*connect.Response[v1.CheckDockerNetworkResponse], error) {
	candidate := req.Msg.GetCandidate()
	if err := (&network.Driver{}).CheckName(candidate.GetName()); err != nil {
		return nil, authapi.Invalid(err)
	}
	if candidate.GetSpec() == nil || candidate.GetEnvironmentId() == "" || len(candidate.GetSpec().GetIpam().GetPools()) > 2 {
		return nil, authapi.Invalid(errors.New("select an environment and enter up to two address pools"))
	}
	_, env, err := h.changes.Environment(ctx, "", candidate.GetEnvironmentId())
	if err != nil {
		return nil, err
	}
	servers, err := h.dockerServers(ctx, req.Msg.GetServerIds())
	if err != nil {
		return nil, err
	}
	name := network.TargetOf(resource.Ref{Kind: network.Kind, Name: candidate.GetName(), Scope: env.ID}).Name
	out := &v1.CheckDockerNetworkResponse{}
	findings := make([][]*v1.DockerNetworkFinding, len(servers))
	unavailable := make([]*v1.DockerUnavailable, len(servers))
	var group sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, server := range servers {
		slots <- struct{}{}
		group.Go(func() {
			defer func() { <-slots }()
			session, state := h.dockerAgent(server)
			if session != nil && session.DockerStorage() {
				if checked, err := session.CheckDockerNetwork(ctx, &agentv1.CheckDockerNetworkCall{
					EngineName: name, Spec: candidate.GetSpec(),
				}); err == nil {
					findings[i] = checked.GetFindings()
					for _, finding := range findings[i] {
						finding.ServerId = server.ID
					}
					return
				}
				state = v1.DockerReadState_DOCKER_READ_STATE_ENGINE_UNAVAILABLE
			} else if session != nil {
				state = v1.DockerReadState_DOCKER_READ_STATE_AGENT_OUTDATED
			}
			unavailable[i] = &v1.DockerUnavailable{ServerId: server.ID, State: state}
		})
	}
	group.Wait()
	for i := range servers {
		out.Findings = append(out.Findings, findings[i]...)
		if unavailable[i] != nil {
			out.Unavailable = append(out.Unavailable, unavailable[i])
		}
	}
	return connect.NewResponse(out), nil
}

// PlanDockerNetworkAttachment edits the recorded workload, with observed evidence.
func (h *Handler) PlanDockerNetworkAttachment(ctx context.Context, req *connect.Request[v1.PlanDockerNetworkAttachmentRequest]) (*connect.Response[v1.PlanDockerNetworkAttachmentResponse], error) {
	m := req.Msg
	netKey, ctrKey := m.GetNetwork(), m.GetContainer()
	connecting := m.GetAction() == v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_CONNECT
	if !connecting && m.GetAction() != v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_DISCONNECT ||
		netKey.GetKind() != dockerNetworkKind || ctrKey.GetKind() != v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER ||
		netKey.GetServerId() == "" || netKey.GetServerId() != ctrKey.GetServerId() ||
		!docker.IsFullID(netKey.GetEngineId()) || !docker.IsFullID(ctrKey.GetEngineId()) || m.GetBaseRevision() <= 0 {
		return nil, authapi.Invalid(errors.New("select an exact network and container on one server and its reviewed revision"))
	}
	read := func(key *v1.DockerObjectKey) (*v1.DockerObject, error) {
		r, err := h.GetDockerObject(ctx, connect.NewRequest(&v1.GetDockerObjectRequest{Key: key}))
		if err != nil {
			return nil, err
		}
		return r.Msg.GetObject(), nil
	}
	net, err := read(netKey)
	if err != nil {
		return nil, err
	}
	ctr, err := read(ctrKey)
	if err != nil {
		return nil, err
	}
	if net == nil || ctr == nil {
		return nil, authapi.Invalid(errors.New("the network or container changed. Refresh and review again"))
	}
	if ctr.GetOwnership() != v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED || ctr.GetManagedRevision() != m.GetBaseRevision() ||
		ctr.GetContainer().GetOneOff() || ctr.GetContainer().GetDetailsTruncated() {
		return nil, authapi.Invalid(errors.New("select the current single managed workload. Take on an external container first"))
	}
	if net.GetNetwork().GetDriver() != "bridge" || net.GetNetwork().GetScope() != "local" || net.GetNetwork().GetProtection() != 0 {
		return nil, authapi.Invalid(errors.New("only ordinary local bridge networks can be attached here"))
	}
	name, external := net.GetName(), net.GetOwnership() != v1.DockerOwnership_DOCKER_OWNERSHIP_MANAGED
	ref := resource.Ref{Kind: resource.Kind(ctr.GetManagedResource().GetKind()), Scope: ctr.GetManagedResource().GetScope(), Name: ctr.GetManagedResource().GetName()}
	if !external && ref.Kind != container.Kind {
		if net.GetManagedResource().GetScope() != ref.Scope {
			return nil, authapi.Invalid(errors.New("use the owning plan for a network in another environment"))
		}
		name = net.GetManagedResource().GetName()
	}
	var evidence string
	for _, ep := range ctr.GetContainer().GetEndpoints() {
		if docker.MatchesReviewedNetwork(ep.GetNetwork(), ep.GetNetworkId(), net.GetName(), netKey.GetEngineId()) {
			if ep.GetNetworkId() != "" && ep.GetNetworkId() != netKey.GetEngineId() {
				return nil, authapi.Invalid(errors.New("the network attachment refers to a replaced network"))
			}
			evidence = netKey.GetEngineId()
			break
		}
	}
	if connecting == (evidence != "") {
		return nil, authapi.Invalid(errors.New("the observed attachment already has that state"))
	}
	change, err := h.changes.PlanDockerAttachment(ctx, ref, int(m.GetBaseRevision()), netKey.GetServerId(), name, external,
		&v1.DockerNetworkAttachmentGuard{
			NetworkId: netKey.GetEngineId(), NetworkName: net.GetName(),
			ContainerId: ctrKey.GetEngineId(), ExpectedEndpointId: evidence, Action: m.GetAction(),
		})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.PlanDockerNetworkAttachmentResponse{Change: change}), nil
}
