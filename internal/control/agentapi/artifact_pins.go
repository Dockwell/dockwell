// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/store"
)

// CheckDockerArtifactPins reconciles only archived work on the caller's server.
func (h *Handler) CheckDockerArtifactPins(ctx context.Context, req *connect.Request[agentv1.CheckDockerArtifactPinsRequest]) (*connect.Response[agentv1.CheckDockerArtifactPinsResponse], error) {
	peer, err := requireServer(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := h.auth.CheckConnection(ctx, peer.ServerID, peer.Serial); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if h.artifactPlans == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("archive plans are unavailable"))
	}
	pins := req.Msg.GetPins()
	if len(pins) > 64 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("too many artifact pins"))
	}
	changes := make([]string, 0, len(pins))
	for _, pin := range pins {
		if _, err := id.Parse(id.Change, pin.GetChangeId()); err != nil || !artifact.ValidID(pin.GetArtifactId()) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid artifact pin"))
		}
		changes = append(changes, pin.GetChangeId())
	}
	var records map[string]store.Change
	if err := h.artifactPlans.View(ctx, func(tx *store.Tx) error {
		var err error
		records, err = tx.GetChanges(ctx, changes)
		return err
	}); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	out := &agentv1.CheckDockerArtifactPinsResponse{}
	for _, pin := range pins {
		r, exists := records[pin.GetChangeId()]
		type archiveResource struct {
			Ref               resource.Ref
			DockerMaintenance *v1.DockerMaintenance
		}
		var plan struct{ Resources []archiveResource }
		if exists {
			if err := json.Unmarshal(r.Plan, &plan); err != nil {
				return nil, h.rpcError(ctx, err)
			}
		}
		exact := slices.ContainsFunc(plan.Resources, func(item archiveResource) bool {
			action := item.DockerMaintenance.GetImageAction()
			return item.Ref.Kind == host.Kind && item.Ref.Scope == peer.ServerID &&
				(action.GetLoad() != nil || action.GetExport() != nil) && action.GetStagedSourceId() == pin.GetArtifactId()
		})
		if !exact {
			out.Absent = append(out.Absent, pin)
		} else if r.State != store.ChangePlanned && r.State != store.ChangeApplying {
			out.Ended = append(out.Ended, pin)
		}
	}
	return connect.NewResponse(out), nil
}
