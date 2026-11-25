// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"log/slog"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// beginAction reserves the receipt before changing Docker. If it cannot be kept,
// wait until there is room, rather than make a change a restart cannot account for.
func (r *reconciler) beginAction(ref resource.Ref, what, reason, target string) (desired.Entry, bool) {
	if r.cfg.Journal == nil {
		return desired.Entry{Ref: ref, What: what, Reason: reason, At: r.now()}, true
	}
	e, err := r.cfg.Journal.Begin(ref, what, reason, target, r.now())
	if err != nil {
		r.journal(ref, desired.Waiting, fmt.Sprintf("The agent can't keep its pending action (%v), so it leaves Docker alone. Free room in the agent's data folder; it tries again in a minute", err))
		return e, false
	}
	return e, true
}

func (r *reconciler) endAction(e desired.Entry, what, reason string) {
	if r.cfg.Journal == nil {
		r.journal(e.Ref, what, reason)
		return
	}
	if err := r.cfg.Journal.Finish(e.ID, what, reason); err != nil {
		r.cfg.Logger.Warn("can't keep the outcome of a local action; its pending receipt stays on disk and the outcome is handed back from memory",
			slog.String("resource", e.Ref.String()), slog.String("error", err.Error()))
	}
}

// recoverActions only reads Docker, even while connected. A creation finished
// before a kill can be handed back on this stream without another outage.
func (r *reconciler) recoverActions(ctx context.Context) {
	if r.cfg.Journal == nil {
		return
	}
	entries := r.cfg.Journal.Doing()
	if len(entries) == 0 {
		return
	}
	if c := r.cfg.Docker; c != nil {
		ping, cancel := context.WithTimeout(ctx, lookTimeout)
		err := c.Ping(ping)
		cancel()
		if err != nil {
			return
		}
	}
	for i := range entries {
		e := &entries[i]
		if ctx.Err() != nil {
			return
		}
		mark, ok := r.held.claim(e.Ref)
		if !ok {
			continue
		}
		r.recoverAction(ctx, *e)
		r.held.end(mark)
	}
}

func (r *reconciler) recoverAction(ctx context.Context, e desired.Entry) {
	rec, ok, err := r.cfg.Desired.Read(e.Ref)
	if err != nil {
		return
	}
	if !ok {
		r.endAction(e, desired.Waiting, "The agent stopped during this action and no longer holds its desired record. Check the resource from the control plane")
		return
	}
	if rec.ScopeName != "" {
		ctx = resource.WithScopeNames(ctx, map[string]string{e.Ref.Scope: rec.ScopeName})
	}
	ctx, cancel := context.WithTimeout(ctx, lookTimeout)
	defer cancel()
	if e.What == desired.Paused && r.cfg.Docker != nil {
		c, err := r.cfg.Docker.InspectContainer(ctx, e.Target)
		switch {
		case err == nil && c.State.Paused:
			r.endAction(e, desired.Paused, e.Reason)
		case err == nil || docker.IsNotFound(err):
			r.endAction(e, desired.Waiting, "The earlier pause could not be confirmed. The agent checks the workload again")
		}
		return
	}
	if rv := r.local.Reviver(e.Ref); rv != nil {
		if st, err := rv.RunState(ctx, e.Ref); err == nil {
			if !st.Exists && e.What == desired.Started {
				r.endAction(e, desired.Waiting, "The agent stopped during a start and that container is now gone. Check the resource from the control plane")
			} else if st.Exists {
				r.finishAction(ctx, rv, e.Ref, st, false)
			}
		}
		return
	}
	if seen, err := r.local.Observe(ctx, []resource.Ref{e.Ref}); err == nil && len(seen) == 1 && seen[0] != nil {
		r.endAction(e, desired.Waiting, "The agent stopped during creation and the resource exists now. Check it from the control plane; the agent leaves it alone")
	}
}

// finishAction accounts for a creation or start interrupted by an agent kill.
// Only a container carrying this intent's label, never started by anyone, may
// finish its first start. An exited container is never started by this path.
func (r *reconciler) finishAction(ctx context.Context, rv resource.Reviver, ref resource.Ref, st resource.RunState, act bool) bool {
	if r.cfg.Journal == nil {
		return false
	}
	e, ok := r.cfg.Journal.Action(ref)
	if !ok {
		return false
	}
	switch {
	case e.What == desired.Made && st.Reconcile == e.ID:
		if st.Created {
			if !act || !r.canStart(ref, st.Failed) {
				return true
			}
			if why := r.tryStart(ctx, rv, ref); why != "" {
				// The creation's receipt stays: its container is there, made
				// and never started, and the next look tries its first start again.
				r.journal(ref, desired.Waiting, why)
				return true
			}
			st.Running = true
			st.Failed = ""
		}
		r.endAction(e, desired.Made, e.Reason)
		r.seen(ref, st, r.now())
		return true
	case e.What == desired.Started && st.ID == e.Target:
		if st.Running {
			r.endAction(e, desired.Started, e.Reason)
			return true
		}
		// It may have been stopped after starting. Leave it as it stands; its
		// normal Docker-error check decides whether another start is allowed.
		r.endAction(e, desired.Waiting, "The agent stopped during a start and the container is stopped now. Check it from the control plane; the agent leaves it as it stands")
	default:
		r.endAction(e, desired.Waiting, "The agent stopped during this action and another container is there now. Check it from the control plane; the agent leaves it alone")
	}
	return false
}

// restorePauses runs from the shared Docker inventory scan, including while the
// control plane is connected. An operator has already approved this run state.
func (r *reconciler) restorePauses(ctx context.Context, ref resource.Ref, ids []string) {
	if !r.presence.active() {
		return
	}
	mark, ok := r.held.claim(ref)
	if !ok {
		return
	}
	defer r.held.end(mark)
	if r.cfg.Journal != nil {
		if _, pending := r.cfg.Journal.Action(ref); pending {
			return
		}
	}
	rec, ok, err := r.cfg.Desired.Read(ref)
	if err != nil || !ok {
		return
	}
	spec, err := r.cfg.Drivers.Decode(ref.Kind, rec.Spec)
	if err != nil || resource.RunIntent(spec) != resourcev1.DesiredRunState_DESIRED_RUN_STATE_PAUSED {
		return
	}
	look, cancel := context.WithTimeout(ctx, lookTimeout)
	defer cancel()
	for _, id := range ids {
		err := r.cfg.RunControl.WithDesiredRunState(id, func(state resourcev1.DesiredRunState) error {
			if state != resourcev1.DesiredRunState_DESIRED_RUN_STATE_PAUSED {
				return nil
			}
			c, err := r.cfg.Docker.InspectContainer(look, id)
			if docker.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !c.State.Running || c.State.Paused || c.State.Restarting {
				return nil
			}
			const note = "Docker was running a workload its revision had paused. The agent paused it again"
			entry, ok := r.beginAction(ref, desired.Paused, note, id)
			if !ok {
				return nil
			}
			if err := r.cfg.Docker.PauseContainer(look, id); err != nil {
				// The Engine may have paused it before losing its reply. The
				// next pass checks the existing receipt before trying again.
				return err
			}
			r.endAction(entry, desired.Paused, note)
			return nil
		})
		if err != nil {
			select {
			case r.lost <- struct{}{}:
			default:
			}
			r.journal(ref, desired.Waiting, "The agent could not restore the approved pause. It checks again on the next Docker scan")
			break
		}
	}
}
