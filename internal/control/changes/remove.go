// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// appsOnRemoval is what the API's choice for a server's apps is to the engine.
var appsOnRemoval = map[dockwellv1.AppsOnRemoval]string{
	dockwellv1.AppsOnRemoval_APPS_ON_REMOVAL_UNSPECIFIED: revision.AppsLeave,
	dockwellv1.AppsOnRemoval_APPS_ON_REMOVAL_LEAVE:       revision.AppsLeave,
	dockwellv1.AppsOnRemoval_APPS_ON_REMOVAL_MOVE:        revision.AppsMove,
	dockwellv1.AppsOnRemoval_APPS_ON_REMOVAL_STOP:        revision.AppsStop,
}

// PlanRemoveServer plans taking a server out of the fleet (ADR 0090): a change that
// moves, stops or lets go of what runs there, says what becomes of its certificates
// by DNS and its backups, and revokes it.
func (h *Handler) PlanRemoveServer(ctx context.Context, req *connect.Request[dockwellv1.PlanRemoveServerRequest]) (*connect.Response[dockwellv1.PlanRemoveServerResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetServerId() == "" {
		return nil, authapi.Invalid(errors.New("name the server to remove, by ID"))
	}
	apps, ok := appsOnRemoval[m.GetApps()]
	if !ok {
		return nil, authapi.Invalid(fmt.Errorf("%s isn't what can happen to a server's apps as it's removed: leave, move or stop", m.GetApps()))
	}
	var name string
	var notes []revision.Note
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		srv, err := tx.GetServer(ctx, m.GetServerId())
		if errors.Is(err, store.ErrNotFound) {
			return nil // the engine says so
		}
		if err != nil {
			return err
		}
		name = srv.Name
		notes, err = backupNotes(ctx, tx, srv.ID, name)
		return err
	}); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if h.removalNotes != nil && name != "" {
		more, err := h.removalNotes(ctx, m.GetServerId(), name)
		if err != nil {
			return nil, h.rpcError(ctx, err)
		}
		notes = append(more, notes...)
	}
	plan, err := h.engine.RemoveServer(ctx, revision.RemoveServer{
		Server: m.GetServerId(), Apps: apps, MoveTo: m.GetMoveTo(), Compromised: m.GetCompromised(), Summary: m.GetSummary(), Notes: notes,
	}, who)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	c, err := h.change(ctx, plan.Change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanRemoveServerResponse{Change: c}), nil
}

// backupsListed bounds how many of a server's backups its removal's plan reads.
const backupsListed = 1000

// backupNotes say what becomes of a server's backups as it's removed: those on its
// disk alone go with it, and those with a copy in storage stay there, to fetch onto
// a server that has the volume (ADR 0033).
func backupNotes(ctx context.Context, tx *store.Tx, server, name string) ([]revision.Note, error) {
	list, err := tx.ListBackups(ctx, store.BackupFilter{ServerID: server, Limit: backupsListed})
	if err != nil {
		return nil, err
	}
	var gone, kept int
	var goneVolumes, destinations []string
	for i := range list {
		b := &list[i]
		if b.OffServer || b.State != store.BackupDone {
			continue
		}
		if b.CopyKey != "" {
			kept++
			if !slices.Contains(destinations, b.Destination) {
				destinations = append(destinations, b.Destination)
			}
			continue
		}
		gone++
		if !slices.Contains(goneVolumes, b.Volume) {
			goneVolumes = append(goneVolumes, b.Volume)
		}
	}
	var out []revision.Note
	if gone > 0 {
		out = append(out, revision.Note{
			Reason: "backups_go", Names: goneVolumes,
			Message: fmt.Sprintf("%s on %s's disk alone %s with it, of %s", backups(gone), name, goOrGoes(gone), human.List(goneVolumes)),
		})
	}
	if kept > 0 {
		out = append(out, revision.Note{
			Reason: "backups_stay", Names: destinations,
			Message: fmt.Sprintf("%s %s in %s, to fetch onto a server with the volume", backups(kept), stayOrStays(kept), human.List(destinations)),
		})
	}
	return out, nil
}

func backups(n int) string {
	if n == 1 {
		return "1 backup"
	}
	return fmt.Sprintf("%d backups", n)
}

func goOrGoes(n int) string {
	if n == 1 {
		return "goes"
	}
	return "go"
}

func stayOrStays(n int) string {
	if n == 1 {
		return "stays"
	}
	return "stay"
}
