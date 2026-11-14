// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// Reasons PlanAgentUpgrade refuses with.
const (
	// ReasonDevelopmentBuild is a control plane that isn't a release, which has
	// none to upgrade agents to.
	ReasonDevelopmentBuild = "development_build"
	// ReasonControlServer is an upgrade of the control server's agent alone, which
	// is the control plane, upgraded by the install script on it.
	ReasonControlServer = "control_server"
	// ReasonOlderAgent is an agent from before fleet upgrades, which the install
	// script on its server upgrades once.
	ReasonOlderAgent = "older_agent"
)

// PlanAgentUpgrade plans upgrading servers' agents to the control plane's own
// release (ADR 0094): an edit of each server's host that makes no revision, as
// good as root, whose step runs the control plane's install script on the server
// and passes once its agent connects at that version. The control server is left
// out, and a server whose agent runs the version already takes no step.
func (h *Handler) PlanAgentUpgrade(ctx context.Context, req *connect.Request[dockwellv1.PlanAgentUpgradeRequest]) (*connect.Response[dockwellv1.PlanAgentUpgradeResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	version, sum, err := h.engine.AgentReleaseOf()
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, err, ReasonDevelopmentBuild)
	}
	m := req.Msg
	var edits []revision.Edit
	var set targets
	err = h.db.View(ctx, func(tx *store.Tx) (err error) {
		set, err = h.targetsOf(ctx, tx, m.GetServerIds(), m.GetSelector(), m.GetRollout(), "upgrade the agents of")
		if err != nil {
			return err
		}
		set.leaveOutControlServer(h.engine.ControlServer())
		if len(set.servers) == 0 {
			return authapi.ReasonError(connect.CodeFailedPrecondition,
				errors.New("the control server's agent is the control plane, so a change doesn't upgrade it. Upgrade it by running the install script on the control server"), ReasonControlServer)
		}
		if older := set.leaveOutOlderAgents(h.engine.UpgradesItself); len(set.servers) == 0 {
			return authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("the agent on %s is older than fleet upgrades, so it can't upgrade itself. Upgrade it by running the install script on the server once; later upgrades can come from here", human.List(older)), ReasonOlderAgent)
		}
		for i := range set.servers {
			edits = append(edits, revision.Edit{
				Ref:          resource.Ref{Kind: host.Kind, Scope: set.servers[i].ID, Name: host.Name},
				UpgradeAgent: &resource.AgentUpgradeAsked{Version: version, ScriptSHA256: sum},
			})
		}
		return nil
	})
	if cerr := (*connect.Error)(nil); errors.As(err, &cerr) {
		return nil, cerr
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		agents := "agents"
		if len(set.servers) == 1 {
			agents = "agent"
		}
		summary = fmt.Sprintf("Upgrade the %s on %s to %s", agents, set.on(m.GetSelector()), version)
	}
	plan, err := h.engine.Propose(ctx, revision.Proposal{Summary: summary, Actor: who.Actor, Via: who.Via, Edits: edits, Notes: set.notes, Rollout: set.rollout})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if !slices.ContainsFunc(plan.Resources, func(r revision.ResourcePlan) bool { return r.AgentUpgrade != nil }) {
		if err := h.engine.Discard(ctx, plan.Change, who); err != nil {
			h.logger.WarnContext(ctx, "can't discard a change that does nothing", slog.String("change", plan.Change), slog.String("error", err.Error()))
		}
		said := fmt.Sprintf("the agent on %s runs %s already, so there's nothing to upgrade", set.on(m.GetSelector()), version)
		if len(set.servers) > 1 {
			said = fmt.Sprintf("every agent named runs %s already, so there's nothing to upgrade", version)
		}
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New(said), ReasonAlready)
	}
	c, err := h.change(ctx, plan.Change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanAgentUpgradeResponse{Change: c}), nil
}

// leaveOutOlderAgents takes out of the targets each server whose agent can't upgrade
// itself, being from before fleet upgrades, and says so in a note when others are
// left. It returns their names.
func (t *targets) leaveOutOlderAgents(upgrades func(server string) bool) []string {
	var older []string
	t.servers = slices.DeleteFunc(t.servers, func(s store.Server) bool {
		if upgrades(s.ID) {
			return false
		}
		older = append(older, s.Name)
		if t.rollout != nil {
			t.rollout.Servers = slices.DeleteFunc(t.rollout.Servers, func(id string) bool { return id == s.ID })
		}
		return true
	})
	if len(older) > 0 && len(t.servers) > 0 {
		message := human.List(older) + "'s agent is older than fleet upgrades, so it isn't in this change. Upgrade it by running the install script on it once"
		if len(older) > 1 {
			message = human.List(older) + " have agents older than fleet upgrades, so they aren't in this change. Upgrade each by running the install script on it once"
		}
		t.notes = append(t.notes, revision.Note{Reason: ReasonOlderAgent, Names: older, Named: true, Message: message})
	}
	return older
}

// leaveOutControlServer takes the control server out of the targets, and says so
// in a note when others are left: its agent is the control plane, which upgrades by
// the install script on it (ADR 0094).
func (t *targets) leaveOutControlServer(control string) {
	i := slices.IndexFunc(t.servers, func(s store.Server) bool { return s.ID == control })
	if control == "" || i < 0 {
		return
	}
	name := t.servers[i].Name
	t.servers = slices.Delete(t.servers, i, i+1)
	if t.rollout != nil {
		t.rollout.Servers = slices.DeleteFunc(t.rollout.Servers, func(s string) bool { return s == control })
	}
	if len(t.servers) > 0 {
		t.notes = append(t.notes, revision.Note{
			Reason: ReasonControlServer, Names: []string{name}, Named: true,
			Message: name + " is the control server, whose agent is the control plane, so it isn't in this change. Upgrade it by running the install script on it",
		})
	}
}
