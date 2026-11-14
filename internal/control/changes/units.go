// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// serviceName is a system service's name: a systemd unit's, which may carry the
// \x2d escapes of an instance, or an OpenRC script's. The agent checks it again
// against what its init knows.
var serviceName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?:[A-Za-z0-9:_.@\\-]+\.(?:service|socket)|[A-Za-z0-9_][A-Za-z0-9_.@-]*)$`)
})

// ReasonGuarded is a refusal of what Dockwell won't do to a system service it needs.
const ReasonGuarded = "guarded"

// ReasonAlready is an action on a system service that would change nothing.
const ReasonAlready = "already"

// PlanSystemServiceAction plans an action on one of each server's system services:
// an edit of the server's host that makes no revision (ADR 0082). What a unit's
// guard refuses is refused here by its name, and on the server by every name it
// goes by.
func (h *Handler) PlanSystemServiceAction(ctx context.Context, req *connect.Request[dockwellv1.PlanSystemServiceActionRequest]) (*connect.Response[dockwellv1.PlanSystemServiceActionResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	name := m.GetName()
	if len(name) > 256 || !serviceName().MatchString(name) || strings.Contains(name, "@.") {
		return nil, authapi.Invalid(fmt.Errorf("%q isn't a system service's name, such as nginx.service", name))
	}
	action := resourcepb.UnitActionFrom(m.GetAction())
	if action == "" {
		return nil, authapi.Invalid(errors.New("say what to do to the system service: start, stop, restart, reload, enable, disable, mask or unmask"))
	}
	var edits []revision.Edit
	var set targets
	err = h.db.View(ctx, func(tx *store.Tx) (err error) {
		if set, err = h.targetsOf(ctx, tx, m.GetServerIds(), m.GetSelector(), m.GetRollout(), "act on"); err != nil {
			return err
		}
		for i := range set.servers {
			s := &set.servers[i]
			if why := resource.GuardOf(name).Refusal(name, s.Name, action); why != "" {
				return authapi.ReasonError(connect.CodeFailedPrecondition, errors.New(why), ReasonGuarded)
			}
			edits = append(edits, revision.Edit{
				Ref:  resource.Ref{Kind: host.Kind, Scope: s.ID, Name: host.Name},
				Unit: &resource.UnitAsked{Name: name, Action: action},
			})
		}
		return nil
	})
	names := set.names()
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return nil, cerr
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = unitSummary(action, name, names)
	}
	plan, err := h.engine.Propose(ctx, revision.Proposal{Summary: summary, Actor: who.Actor, Via: who.Via, Edits: edits, Notes: set.notes, Rollout: set.rollout})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	// Where the unit stands as asked on every server, there's nothing to do: say
	// so, rather than leave a change that does nothing.
	acts := false
	for i := range plan.Resources {
		acts = acts || plan.Resources[i].Unit != nil
	}
	if !acts {
		if err := h.engine.Discard(ctx, plan.Change, who); err != nil {
			h.logger.WarnContext(ctx, "can't discard a change that does nothing", slog.String("change", plan.Change), slog.String("error", err.Error()))
		}
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New(standsAsAsked(action, name, names)), ReasonAlready)
	}
	c, err := h.change(ctx, plan.Change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanSystemServiceActionResponse{Change: c}), nil
}

// unitSummary says what a change does to a unit, on which servers.
func unitSummary(action resource.UnitAction, name string, servers []string) string {
	where := human.List(servers)
	if len(servers) > 3 {
		where = fmt.Sprintf("%d servers", len(servers))
	}
	switch action {
	case resource.UnitEnable:
		return fmt.Sprintf("Start %s at boot on %s", name, where)
	case resource.UnitDisable:
		return fmt.Sprintf("Stop %s starting at boot on %s", name, where)
	case resource.UnitStart, resource.UnitStop, resource.UnitRestart, resource.UnitReload, resource.UnitMask, resource.UnitUnmask:
	}
	return fmt.Sprintf("%s %s on %s", action.Verb(), name, where)
}

// standsAsAsked says a unit already stands as the action asks, on every server.
func standsAsAsked(action resource.UnitAction, name string, servers []string) string {
	where := strings.Join(servers, " and ")
	if len(servers) > 2 {
		where = "every server named"
	}
	switch action {
	case resource.UnitStart:
		return fmt.Sprintf("%s already runs on %s, so there's nothing to start", name, where)
	case resource.UnitStop:
		return fmt.Sprintf("%s isn't running on %s, so there's nothing to stop", name, where)
	case resource.UnitEnable:
		return fmt.Sprintf("%s already starts at boot on %s", name, where)
	case resource.UnitDisable:
		return fmt.Sprintf("%s doesn't start at boot on %s", name, where)
	case resource.UnitMask:
		return fmt.Sprintf("%s is already masked on %s", name, where)
	case resource.UnitUnmask:
		return fmt.Sprintf("%s isn't masked on %s", name, where)
	case resource.UnitRestart, resource.UnitReload:
	}
	return fmt.Sprintf("there's nothing to %s for %s on %s", action, name, where)
}
