// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"log/slog"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// HostsEvery is how often the control plane gives a server that has none its
// host: what makes one for a server that has just joined.
var HostsEvery = time.Minute

// HostsSummary is what the change that makes servers' hosts says.
const HostsSummary = "Add each server's host"

// Hosts gives every server of the fleet its host resource, kind "server.host",
// with an empty spec, by Dockwell's own change: at start for the servers that
// joined before, and for each server as it joins (ADR 0064).
type Hosts struct {
	DB     *store.DB
	Engine *revision.Engine
	// Agents are the servers' agents: a host is made for a server whose agent is
	// connected and runs the kind, so an older agent, or one offline, waits for
	// its own and holds up no other server's.
	Agents revision.Agents
	Logger *slog.Logger
	// Fleet, when set, reads servers' labels again as changes move on, so the
	// fleet's stream shows a change of them at once (ADR 0091).
	Fleet *fleet.Hub
}

// FollowLabels has the fleet read servers' labels again each time a change moves
// on, until ctx ends. A change of labels alone applies at once, so this is what
// shows it.
func (h *Hosts) FollowLabels(ctx context.Context) {
	if h.Fleet == nil {
		return
	}
	updates := h.Engine.WatchAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case u, ok := <-updates:
			if !ok {
				if ctx.Err() != nil {
					return
				}
				// Dropped for falling behind: watch again, and read what was missed.
				updates = h.Engine.WatchAll(ctx)
			} else if u.State == "" {
				continue
			}
		}
		// What else came meanwhile is read with this.
		for drained := false; !drained; {
			select {
			case <-updates:
			default:
				drained = true
			}
		}
		if err := h.Fleet.ReadLabels(ctx); err != nil && ctx.Err() == nil {
			h.Logger.WarnContext(ctx, "can't read servers' labels again; the fleet shows them as they were until the next change", slog.String("error", err.Error()))
		}
	}
}

// runner is an agent that says which kinds it runs, as agentapi's sessions do.
type runner interface {
	Runs(kind resource.Kind) bool
}

// Run makes what's missing now, again as each agent connects, and every
// HostsEvery, until ctx ends.
func (h *Hosts) Run(ctx context.Context) {
	connected := make(chan struct{}, 1)
	if c, ok := h.Agents.(interface{ OnConnect(func(string)) }); ok {
		c.OnConnect(func(string) {
			select {
			case connected <- struct{}{}:
			default:
			}
		})
	}
	t := time.NewTicker(HostsEvery)
	defer t.Stop()
	for {
		if err := h.Make(ctx); err != nil && ctx.Err() == nil {
			h.Logger.WarnContext(ctx, "can't add a server's host; it's tried again in a minute", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-connected:
		}
	}
}

// runsHosts reports whether a server's agent is connected and runs hosts.
func (h *Hosts) runsHosts(server string) bool {
	if h.Agents == nil {
		return true
	}
	a, online := h.Agents.Agent(server)
	if !online {
		return false
	}
	r, ok := a.(runner)
	return !ok || r.Runs(host.Kind)
}

// Make makes the hosts the fleet's servers don't have, in one change.
func (h *Hosts) Make(ctx context.Context) error {
	var edits []revision.Edit
	if err := h.DB.View(ctx, func(tx *store.Tx) error {
		servers, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		hosts, err := tx.ListKind(ctx, string(host.Kind))
		if err != nil {
			return err
		}
		has := map[string]bool{}
		for i := range hosts {
			has[hosts[i].Scope] = true
		}
		var labels map[string]map[string]string
		for i := range servers {
			s := &servers[i]
			if s.RevokedAt != nil || has[s.ID] || !h.runsHosts(s.ID) {
				continue
			}
			// A new host has the labels its server joined with (ADR 0091).
			if labels == nil {
				if labels, err = tx.JoinLabels(ctx); err != nil {
					return err
				}
			}
			edits = append(edits, revision.Edit{Ref: resource.Ref{Kind: host.Kind, Scope: s.ID, Name: host.Name}, Spec: &resourcev1.ServerHost{Labels: labels[s.ID]}})
		}
		return nil
	}); err != nil || len(edits) == 0 {
		return err
	}
	plan, err := h.Engine.Propose(ctx, revision.Proposal{Summary: HostsSummary, Actor: revision.SystemActor, Via: revision.ViaAutomatic, Edits: edits})
	if err != nil {
		return err
	}
	if err := h.Engine.Apply(ctx, plan.Change); err != nil {
		// A plan that didn't apply, as an agent that went between the two, isn't
		// left behind: the next look makes another.
		_ = h.Engine.Discard(context.WithoutCancel(ctx), plan.Change, revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic})
		return err
	}
	return nil
}
