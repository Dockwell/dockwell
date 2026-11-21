// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"

	"google.golang.org/protobuf/proto"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// PlanDockerAttachment uses the saved spec and its checked base revision.
func (h *Handler) PlanDockerAttachment(ctx context.Context, ref resource.Ref, base int, server, name string, external bool, guard *v1.DockerNetworkAttachmentGuard) (*v1.Change, error) {
	return h.Planned(ctx, func(by revision.By) (*revision.Plan, error) {
		connect := guard.GetAction() == v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_CONNECT
		var spec proto.Message
		err := h.db.View(ctx, func(tx *store.Tx) error {
			var err error
			spec, err = h.engine.SpecWithNetworkMembership(ctx, tx, ref, base, server, name, external, connect)
			return err
		})
		if err != nil {
			return nil, err
		}
		summary := "Disconnect " + ref.Name + " from " + name
		if connect {
			summary = "Connect " + ref.Name + " to " + name
		}
		return h.engine.Propose(ctx, revision.Proposal{Summary: summary, Actor: by.Actor, Via: by.Via, Edits: []revision.Edit{{
			Ref: ref, Base: base, Spec: spec,
			DockerStorageGuard: &v1.DockerStorageGuard{Operation: &v1.DockerStorageGuard_Attachment{Attachment: guard}},
		}}})
	})
}
