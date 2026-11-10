// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/store"
)

// ActionDrilled is a restore drill that ended, however it ended (ADR 0068).
const ActionDrilled = "backup.drill"

// tokenDrillEvery is how often an API token may start a drill of one volume.
const tokenDrillEvery = time.Hour

// drillResults are what a drill's stored result says on the API.
var drillResults = map[string]dockwellv1.DrillResult{
	store.DrillRunning:    dockwellv1.DrillResult_DRILL_RESULT_RUNNING,
	store.DrillPassed:     dockwellv1.DrillResult_DRILL_RESULT_PASSED,
	store.DrillFailed:     dockwellv1.DrillResult_DRILL_RESULT_FAILED,
	store.DrillNotChecked: dockwellv1.DrillResult_DRILL_RESULT_NOT_CHECKED,
}

// storedResult is a drill's result as it's stored.
func storedResult(r dockwellv1.DrillResult) string {
	for stored, api := range drillResults {
		if api == r {
			return stored
		}
	}
	return store.DrillNotChecked
}

// DrillBackup checks that a backup on its server restores, in a container and a
// volume of its own that it throws away (ADR 0068). It returns the drill running.
func (h *Handler) DrillBackup(ctx context.Context, req *connect.Request[dockwellv1.DrillBackupRequest]) (*connect.Response[dockwellv1.DrillBackupResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	client, _ := authapi.ClientFrom(ctx)
	b, err := h.backup(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if b.State != store.BackupDone {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s didn't finish, so there's nothing of it to check", b.ID), "not_done")
	}
	here, err := h.restorable(ctx, &b)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	var server store.Server
	err = h.db.View(ctx, func(tx *store.Tx) (err error) {
		server, err = tx.GetServer(ctx, b.ServerID)
		return err
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if !here {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't on a connected server that has its volume, so it can't be checked there", b.ID), "not_on_server")
	}
	s, err := h.session(server)
	if err != nil {
		return nil, err
	}
	if !s.Drills() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s's agent is too old to check a restore. Update the agent on %s, then try again", server.Name, server.Name), "agent_too_old")
	}
	now := h.now()
	d := store.Drill{
		ID: id.New(id.Drill), BackupID: b.ID, EnvironmentID: b.EnvironmentID, VolumeID: b.VolumeID, ServerID: b.ServerID,
		Result: store.DrillRunning, Actor: p.Actor(), StartedAt: now,
	}
	// One at a time on a server, and a token at most one of a volume an hour.
	h.mu.Lock()
	if h.drilling == nil {
		h.drilling = map[string]string{}
	}
	busy := h.drilling[b.ServerID]
	if busy == "" {
		h.drilling[b.ServerID] = b.ID
	}
	h.mu.Unlock()
	if busy != "" {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("a restore check is running on %s, which checks one at a time. Check this one once it ends", server.Name), "drill_running")
	}
	release := func() {
		h.mu.Lock()
		delete(h.drilling, b.ServerID)
		h.mu.Unlock()
	}
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if p.Token != nil {
			since := now.Add(-tokenDrillEvery)
			recent, err := tx.ListDrills(ctx, store.DrillFilter{VolumeID: b.VolumeID, Since: &since})
			if err != nil {
				return err
			}
			if len(recent) > 0 {
				return authapi.ReasonError(connect.CodeResourceExhausted, fmt.Errorf("%s's volume was checked in the last hour, and an API token checks one at most once an hour. Try again later", b.ID), "too_often")
			}
		}
		return tx.CreateDrill(ctx, d)
	})
	if err != nil {
		release()
		return nil, h.fail(ctx, err)
	}
	ref, _, err := h.volumeOf(ctx, b.VolumeID, b.ServerID)
	if err != nil {
		// Its volume's record went between the checks: what was started ends as such.
		h.finishDrill(context.WithoutCancel(ctx), d, &b, nil, err, client.Surface(p))
		release()
		return nil, h.fail(ctx, err)
	}
	surface := client.Surface(p)
	h.wg.Go(func() {
		defer release()
		got, err := s.DrillBackup(h.base, &agentv1.DrillBackupCall{Id: d.ID, Backup: b.ID, Volume: resourcepb.Ref(ref)})
		h.finishDrill(context.WithoutCancel(h.base), d, &b, got, err, surface)
	})
	var look *rowFacts
	err = h.db.View(ctx, func(tx *store.Tx) (err error) {
		look, err = h.facts(ctx, tx)
		return err
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.DrillBackupResponse{Drill: drillProto(&d, look)}), nil
}

// finishDrill records how a drill ended, and says so on the timeline. A drill whose
// call didn't come back wasn't checked: that says nothing of the backup.
func (h *Handler) finishDrill(ctx context.Context, d store.Drill, b *store.Backup, got *agentv1.DrillBackupResult, failed error, surface string) {
	now := h.now()
	d.FinishedAt = &now
	switch {
	case failed != nil:
		d.Result = store.DrillNotChecked
		plain, _ := resource.Explain(failed)
		d.Why = plain
		if errors.Is(failed, context.Canceled) {
			d.Why = "The control plane stopped while it ran."
		}
	default:
		d.Result = storedResult(got.GetResult())
		d.Why, d.Detail, d.RestoredBytes, d.Tables, d.UnpackedOnly = got.GetWhy(), got.GetDetail(), got.GetRestoredBytes(), int64(got.GetTables()), got.GetUnpackedOnly()
	}
	detail, err := json.Marshal(Told{ID: b.ID, Volume: b.Volume, Method: b.Method, Size: b.SizeBytes, Error: d.Why, Drill: d.Result})
	if err != nil {
		h.logger.ErrorContext(ctx, "can't record a restore drill", slog.String("drill", d.ID), slog.String("error", err.Error()))
		return
	}
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.FinishDrill(ctx, d); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: d.Actor, Action: ActionDrilled, Subject: d.VolumeID, Target: d.ServerID, Detail: string(detail), Surface: surface,
		})
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "can't record how a restore drill ended", slog.String("drill", d.ID), slog.String("error", err.Error()))
		return
	}
	if d.Result != store.DrillPassed {
		h.logger.WarnContext(ctx, "a restore drill didn't pass", slog.String("drill", d.ID), slog.String("backup", b.ID), slog.String("result", d.Result), slog.String("why", d.Why))
	}
}

// recoverDrills says the drills that were running when the control plane stopped
// weren't checked, as nothing waits for them any more.
func (h *Handler) recoverDrills(ctx context.Context) {
	var running []store.Drill
	backups := map[string]store.Backup{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if running, err = tx.ListDrills(ctx, store.DrillFilter{Result: store.DrillRunning}); err != nil {
			return err
		}
		for i := range running {
			b, err := tx.GetBackup(ctx, running[i].BackupID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			backups[running[i].BackupID] = b
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			h.logger.ErrorContext(ctx, "can't read the restore drills that were running, so they show as running until the next start", slog.String("error", err.Error()))
		}
		return
	}
	for i := range running {
		b := backups[running[i].BackupID]
		lost := resource.Plainly("The control plane stopped while it ran.", errors.New("the control plane stopped while this drill ran"))
		h.finishDrill(ctx, running[i], &b, nil, lost, store.SurfaceAutomatic)
	}
}

// drillsRunning are the backups a drill is checking now, by ID.
func drillsRunning(ctx context.Context, tx *store.Tx) (map[string]bool, error) {
	running, err := tx.ListDrills(ctx, store.DrillFilter{Result: store.DrillRunning})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range running {
		out[running[i].BackupID] = true
	}
	return out, nil
}

// drillProto is a drill as the API has it.
func drillProto(d *store.Drill, f *rowFacts) *dockwellv1.BackupDrill {
	out := &dockwellv1.BackupDrill{
		Id: d.ID, Result: drillResults[d.Result], Why: d.Why, Detail: d.Detail, RestoredBytes: d.RestoredBytes,
		Tables: int32(min(d.Tables, 1<<31-1)), UnpackedOnly: d.UnpackedOnly, //nolint:gosec // bounded above
		StartedAt: timestamppb.New(d.StartedAt), Actor: f.look.Actor(d.Actor),
	}
	if d.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*d.FinishedAt)
	}
	return out
}

// DrillResult is what a drill's stored result says on the API, for the timeline.
// Empty is unspecified.
func DrillResult(stored string) dockwellv1.DrillResult { return drillResults[stored] }
