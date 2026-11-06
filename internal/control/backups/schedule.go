// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// ScheduleEvery is how often the schedule is looked at.
const ScheduleEvery = 30 * time.Second

// ConnectGrace is how long servers have to connect after the control plane starts,
// before a run that fell due while it was down fails for want of one.
const ConnectGrace = 2 * time.Minute

// KeepRunning runs each policy's backups as they fall due, until ctx ends.
func (h *Handler) KeepRunning(ctx context.Context) {
	t := time.NewTicker(ScheduleEvery)
	defer t.Stop()
	for {
		if err := h.RunDue(ctx); err != nil && ctx.Err() == nil {
			h.logger.WarnContext(ctx, "can't read the schedule of backups; trying again soon", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// policy is a schedule as it stands.
type policy struct {
	id, name, environment string
	spec                  *resourcev1.BackupPolicy
	// madeAt is when its latest revision was applied: nothing is due from before.
	madeAt time.Time
}

// policies reads the schedules, by name.
func (h *Handler) policies(ctx context.Context, tx *store.Tx) ([]policy, error) {
	stored, err := tx.ListKind(ctx, string(backuppolicy.Kind))
	if err != nil {
		return nil, err
	}
	out := make([]policy, 0, len(stored))
	for i := range stored {
		r := &stored[i]
		spec, err := h.engine.Kinds().Decode(backuppolicy.Kind, r.Spec)
		if err != nil {
			return nil, fmt.Errorf("can't read the backup policy %s: %w", r.Name, err)
		}
		rev, err := tx.GetRevision(ctx, r.ID, r.Revision)
		if err != nil {
			return nil, err
		}
		out = append(out, policy{id: r.ID, name: r.Name, environment: r.Scope, spec: spec.(*resourcev1.BackupPolicy), madeAt: rev.CreatedAt})
	}
	slices.SortFunc(out, func(a, b policy) int { return cmp.Compare(a.name, b.name) })
	return out, nil
}

// due is one run of a policy on one server.
type due struct {
	policy policy
	volume store.Resource
	server store.Server
}

// RunDue starts what's due now. A run missed while the control plane was down
// happens once, when it's back. A server that can't be reached fails its run where
// someone can see it, and the policy goes on from there.
func (h *Handler) RunDue(ctx context.Context) error {
	now := h.now()
	var runs []due
	err := h.db.View(ctx, func(tx *store.Tx) error {
		list, err := h.policies(ctx, tx)
		if err != nil {
			return err
		}
		for i := range list {
			p := list[i]
			v, err := tx.GetResource(ctx, string(volume.Kind), p.environment, p.spec.GetVolume())
			if err != nil || v.Spec == nil {
				continue // the volume's gone: a change would have had to delete the policy first
			}
			placed, err := tx.Placement(ctx, v.ID, v.Revision)
			if err != nil {
				return err
			}
			for _, serverID := range placed {
				server, err := tx.GetServer(ctx, serverID)
				if err != nil || server.RevokedAt != nil {
					continue
				}
				ready, err := isDue(ctx, tx, p, serverID, now)
				if err != nil {
					return err
				}
				if ready {
					runs = append(runs, due{p, v, server})
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := range runs {
		h.start(ctx, runs[i])
	}
	return nil
}

// isDue reports a policy that's due on a server, and has no run going there.
func isDue(ctx context.Context, tx *store.Tx, p policy, serverID string, now time.Time) (bool, error) {
	list, err := tx.ListBackups(ctx, store.BackupFilter{PolicyID: p.id, ServerID: serverID, Limit: 1})
	if err != nil {
		return false, err
	}
	since := p.madeAt
	if len(list) > 0 {
		if list[0].State == store.BackupRunning {
			return false, nil
		}
		if list[0].StartedAt.After(since) {
			since = list[0].StartedAt
		}
	}
	return !backuppolicy.Next(p.spec, since).After(now), nil
}

// start runs one policy's backup on one server, as Dockwell's own.
func (h *Handler) start(ctx context.Context, run due) {
	s, ok := h.agents.Session(run.server.ID)
	if !ok && h.now().Sub(h.began) < ConnectGrace {
		return // a later look at the schedule finds it due still
	}
	b := store.Backup{
		ID: id.New(id.Backup), EnvironmentID: run.volume.Scope, Volume: run.volume.Name, VolumeID: run.volume.ID, ServerID: run.server.ID,
		State: store.BackupRunning, Actor: revision.SystemActor, StartedAt: h.now(), PolicyID: run.policy.id, Destination: run.policy.spec.GetDestination(),
	}
	if err := h.db.Update(ctx, func(tx *store.Tx) error { return tx.CreateBackup(ctx, b) }); err != nil {
		h.logger.ErrorContext(ctx, "can't record a backup", slog.String("error", err.Error()))
		return
	}
	if !ok {
		h.finish(ctx, b, nil, fmt.Errorf("%s wasn't connected when %s fell due, so this backup didn't run. It runs again on schedule", run.server.Name, run.policy.name), store.SurfaceAutomatic)
		return
	}
	destination, err := h.destination(ctx, b.Destination)
	if err != nil {
		h.finish(ctx, b, nil, fmt.Errorf("%s names the destination %s, and it can't be read: %w. Choose another", run.policy.name, b.Destination, err), store.SurfaceAutomatic)
		return
	}
	ref := resource.Ref{Kind: volume.Kind, Scope: run.volume.Scope, Name: run.volume.Name}
	keep := int(run.policy.spec.GetKeep())
	stop := run.policy.spec.GetStop()
	turn := h.turnOn(run.server.ID)
	h.wg.Go(func() {
		select {
		case turn <- struct{}{}:
			defer func() { <-turn }()
			h.run(s, b, ref, stop, store.SurfaceAutomatic, keep, destination)
		case <-h.base.Done():
			h.finish(context.WithoutCancel(h.base), b, nil, h.base.Err(), store.SurfaceAutomatic)
		}
	})
}

// turnOn is held by the scheduled run under way on a server. Runs that fall due
// together go one at a time, or each would look for room in the same free space.
func (h *Handler) turnOn(serverID string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.turns == nil {
		h.turns = map[string]chan struct{}{}
	}
	turn, ok := h.turns[serverID]
	if !ok {
		turn = make(chan struct{}, 1)
		h.turns[serverID] = turn
	}
	return turn
}

// prune removes a policy's oldest backups past what it keeps, on a server, and the
// record of failures older than the oldest it keeps. It runs before a run, and after
// one that's whole: it never goes below what the policy keeps, so a run of failures
// never takes away what's good. A backup a restore is planned from or running from
// stays until that's over.
func (h *Handler) prune(ctx context.Context, s *agentapi.Session, b store.Backup, keep int) {
	restoring, err := h.engine.BackupsRestoring(ctx)
	if err != nil {
		h.logger.ErrorContext(ctx, "can't read which backups are being restored, so nothing goes this time", slog.String("error", err.Error()))
		return
	}
	var all []store.Backup
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		all, err = tx.ListBackups(ctx, store.BackupFilter{PolicyID: b.PolicyID, ServerID: b.ServerID})
		return err
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "can't read what a policy keeps", slog.String("error", err.Error()))
		return
	}
	kept := 0
	for i := range all {
		old := &all[i]
		switch {
		case old.State == store.BackupRunning:
			continue
		case old.State == store.BackupDone && kept < keep:
			kept++
			continue
		case old.State == store.BackupFailed && kept < keep:
			continue
		case restoring[old.ID]:
			continue
		}
		if old.State == store.BackupDone {
			if err := h.deleteOn(ctx, s, old); err != nil {
				h.logger.WarnContext(ctx, "can't remove a backup past what its policy keeps; the next run tries again", slog.String("backup", old.ID), slog.String("error", err.Error()))
				continue
			}
		}
		if err := h.forget(ctx, old, revision.SystemActor, store.SurfaceAutomatic); err != nil {
			h.logger.ErrorContext(ctx, "can't record that a backup went", slog.String("backup", old.ID), slog.String("error", err.Error()))
		}
	}
}

// ListBackupPolicies returns the schedules, with when each next runs.
func (h *Handler) ListBackupPolicies(ctx context.Context, _ *connect.Request[dockwellv1.ListBackupPoliciesRequest]) (*connect.Response[dockwellv1.ListBackupPoliciesResponse], error) {
	out := &dockwellv1.ListBackupPoliciesResponse{}
	now := h.now()
	err := h.db.View(ctx, func(tx *store.Tx) error {
		list, err := h.policies(ctx, tx)
		if err != nil {
			return err
		}
		look := h.names.In(ctx, tx)
		for i := range list {
			p := &list[i]
			info := &dockwellv1.BackupPolicyInfo{Id: p.id, Name: p.name, EnvironmentId: p.environment, Spec: p.spec}
			backups, err := tx.ListBackups(ctx, store.BackupFilter{PolicyID: p.id})
			if err != nil {
				return err
			}
			since := p.madeAt
			for j := range backups {
				b := &backups[j]
				if j == 0 {
					info.Last = h.protoIn(look, b)
					if b.StartedAt.After(since) {
						since = b.StartedAt
					}
				}
				if b.State == store.BackupDone {
					info.Kept++
					info.KeptBytes += b.SizeBytes
				}
			}
			info.NextRun = timestamppb.New(max2(backuppolicy.Next(p.spec, since), now))
			out.Policies = append(out.Policies, info)
		}
		return nil
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(out), nil
}

func max2(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
