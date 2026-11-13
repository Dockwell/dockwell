// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
)

// PlanPanelStop plans stopping the own containers and services of the panel an
// import came from on a server: an edit of the server's host that makes no
// revision, as an action on a system service is (ADR 0089). What its agent can do
// is the caller's to check. Where nothing of the panel runs, it says so, and leaves
// no change that does nothing.
func (h *Handler) PlanPanelStop(ctx context.Context, server *dockwellv1.Server, kind, summary string) (*dockwellv1.Change, error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	name := resource.PanelName(kind)
	if summary == "" {
		summary = fmt.Sprintf("Stop %s on %s", name, server.GetName())
	}
	plan, err := h.engine.Propose(ctx, revision.Proposal{
		Summary: summary, Actor: who.Actor, Via: who.Via,
		Edits: []revision.Edit{{
			Ref:   resource.Ref{Kind: host.Kind, Scope: server.GetId(), Name: host.Name},
			Spec:  &resourcev1.ServerHost{},
			Panel: &resource.PanelAsked{Kind: kind},
		}},
	})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	acts := false
	for i := range plan.Resources {
		acts = acts || plan.Resources[i].Panel != nil
	}
	if !acts {
		if err := h.engine.Discard(ctx, plan.Change, who); err != nil {
			h.logger.WarnContext(ctx, "can't discard a change that does nothing", slog.String("change", plan.Change), slog.String("error", err.Error()))
		}
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's own containers are stopped on %s already, so there's nothing to stop", name, server.GetName()), ReasonAlready)
	}
	return h.change(ctx, plan.Change)
}

// errNoPanel is a panel this method doesn't know.
var errNoPanel = errors.New("name the panel to stop: Coolify or Dokploy")

// PanelKindOf is the panel's word for its kind in the API, or an error a caller
// returns as it is.
func PanelKindOf(k dockwellv1.PanelKind) (string, error) {
	switch k {
	case dockwellv1.PanelKind_PANEL_KIND_COOLIFY:
		return resource.PanelCoolify, nil
	case dockwellv1.PanelKind_PANEL_KIND_DOKPLOY:
		return resource.PanelDokploy, nil
	case dockwellv1.PanelKind_PANEL_KIND_UNSPECIFIED:
	}
	return "", authapi.Invalid(errNoPanel)
}
