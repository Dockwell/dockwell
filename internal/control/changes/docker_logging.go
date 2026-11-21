// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"

	"google.golang.org/protobuf/proto"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// PlanDockerLogging changes only logging on the workload’s current revision.
func (h *Handler) PlanDockerLogging(ctx context.Context, ref resource.Ref, base int, server, engineID string, logging *spec.ContainerLogging) (*v1.Change, error) {
	return h.Planned(ctx, func(who revision.By) (*revision.Plan, error) {
		var next proto.Message
		err := h.db.View(ctx, func(tx *store.Tx) error {
			var err error
			next, err = h.engine.SpecWithDockerLogging(ctx, tx, ref, base, server, logging)
			return err
		})
		if err != nil {
			return nil, err
		}
		return h.engine.Propose(ctx, revision.Proposal{
			Summary: "Set Docker log rotation", Actor: who.Actor, Via: who.Via,
			Edits: []revision.Edit{{Ref: ref, Spec: next, Base: base, DockerLoggingTarget: &revision.DockerLoggingTarget{Server: server, Container: engineID}}},
		})
	})
}
