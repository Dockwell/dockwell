// SPDX-License-Identifier: AGPL-3.0-only

// Package volumes serves dockwell.v1.VolumeService: a volume as its own page shows
// it, with what it holds on each server and the apps that mount it. Making and
// deleting one are ChangeService's, as for any resource.
package volumes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// DiskUser asks a server's agent what takes its disk (agentapi.Session.DiskUse).
type DiskUser interface {
	DiskUse(ctx context.Context) (*dockwellv1.ServerDiskUse, error)
}

// Handler implements VolumeService.
type Handler struct {
	db     *store.DB
	keeper *secrets.Keeper
	// session is the agent of a server that's connected now, or false.
	session func(ctx context.Context, server string) (DiskUser, bool)
	logger  *slog.Logger
}

var _ dockwellv1connect.VolumeServiceHandler = (*Handler)(nil)

// New returns a handler that reads db, opens sealed specs with keeper, and asks the
// agents agents has what volumes hold.
func New(db *store.DB, keeper *secrets.Keeper, agents *agentapi.Agents, logger *slog.Logger) *Handler {
	return &Handler{db: db, keeper: keeper, logger: logger, session: func(ctx context.Context, server string) (DiskUser, bool) {
		if agents == nil {
			return nil, false
		}
		s, ok := agents.SessionPastMove(ctx, server, agentapi.MoveGrace)
		if !ok || s == nil {
			return nil, false
		}
		return s, true
	}}
}

// GetVolume returns one volume: its servers and what it holds on each, which their
// agents add up now, at once, and the apps of its environment that mount it.
func (h *Handler) GetVolume(ctx context.Context, req *connect.Request[dockwellv1.GetVolumeRequest]) (*connect.Response[dockwellv1.GetVolumeResponse], error) {
	out := &dockwellv1.VolumeInfo{}
	var servers []string
	names := map[string]string{}
	var target volume.Target
	err := h.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(ctx, req.Msg.GetId())
		if errors.Is(err, store.ErrNotFound) || err == nil && (r.Kind != string(volume.Kind) || r.Spec == nil) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		v, _ := spec.(*resourcev1.Volume)
		ref := resource.Ref{Kind: volume.Kind, Scope: r.Scope, Name: r.Name}
		target = volume.TargetOf(ref)
		out.Id, out.Revision, out.ChangeId = r.ID, int64(r.Revision), r.ChangeID
		out.Resource = &dockwellv1.ResourceRef{Kind: r.Kind, Scope: r.Scope, Name: r.Name}
		out.Driver, out.HasOptions = cmp.Or(v.GetDriver(), "local"), len(v.GetOptions()) > 0
		if rev, err := tx.GetRevision(ctx, r.ID, r.Revision); err == nil {
			out.UpdateTime = timestamppb.New(rev.CreatedAt)
		}
		if servers, err = tx.Placement(ctx, r.ID, r.Revision); err != nil {
			return err
		}
		list, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		for i := range list {
			names[list[i].ID] = list[i].Name
		}
		out.MountedBy, err = h.mountedBy(ctx, tx, r.Scope, r.Name)
		return err
	})
	if errors.Is(err, errNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no volume %s. List volumes with dockwell resources list --kind volume", req.Msg.GetId()), "not_found")
	}
	if err != nil {
		h.logger.ErrorContext(ctx, "can't read a volume", slog.String("volume", req.Msg.GetId()), slog.String("error", err.Error()))
		return nil, connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read this volume; try again, and check its logs if it keeps failing"))
	}
	out.Servers = h.onServers(ctx, servers, names, target.Name)
	return connect.NewResponse(&dockwellv1.GetVolumeResponse{Volume: out}), nil
}

var errNotFound = errors.New("no such volume")

// mountedBy are the services of env that mount the volume name, by name.
func (h *Handler) mountedBy(ctx context.Context, tx *store.Tx, env, name string) ([]*dockwellv1.VolumeMount, error) {
	list, err := tx.ListResources(ctx, env, string(service.Kind))
	if err != nil {
		return nil, err
	}
	var out []*dockwellv1.VolumeMount
	for i := range list {
		r := list[i]
		if r.Spec == nil {
			continue
		}
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, err
		}
		svc, _ := spec.(*resourcev1.Service)
		for _, m := range svc.GetApp().GetMounts() {
			if m.GetVolume() == name && !m.GetExternal() {
				out = append(out, &dockwellv1.VolumeMount{ServiceId: r.ID, Service: r.Name, Target: m.GetTarget(), ReadOnly: m.GetReadOnly()})
			}
		}
	}
	slices.SortFunc(out, func(a, b *dockwellv1.VolumeMount) int {
		return cmp.Compare(a.GetService()+a.GetTarget(), b.GetService()+b.GetTarget())
	})
	return out, nil
}

// onServers asks each server's agent, at once, what the volume, Docker's name,
// holds there. One that can't say says why, and the rest answer.
func (h *Handler) onServers(ctx context.Context, servers []string, names map[string]string, name string) []*dockwellv1.VolumeOnServer {
	out := make([]*dockwellv1.VolumeOnServer, len(servers))
	var wg sync.WaitGroup
	for i, id := range servers {
		on := &dockwellv1.VolumeOnServer{ServerId: id, ServerName: cmp.Or(names[id], id)}
		out[i] = on
		session, ok := h.session(ctx, id)
		if !ok {
			on.Unread = on.GetServerName() + " isn't connected, so it can't say what this holds there now"
			continue
		}
		wg.Go(func() {
			use, err := session.DiskUse(ctx)
			if err != nil {
				var se *resource.ServerError
				if errors.As(err, &se) {
					err = se.Err
				}
				on.Unread = fmt.Sprintf("%s can't say what this holds now: %v", on.GetServerName(), err)
				return
			}
			i := slices.IndexFunc(use.GetVolumeUse(), func(v *dockwellv1.VolumeDiskUse) bool { return v.GetName() == name })
			switch {
			case i < 0:
				on.Unread = fmt.Sprintf("%s has no volume %s now, so it holds nothing there", on.GetServerName(), name)
			case use.GetVolumeUse()[i].GetSizeBytes() < 0:
				on.Unread = "its driver keeps it somewhere Docker doesn't add up"
				on.Path, on.Containers = use.GetVolumeUse()[i].GetMountpoint(), use.GetVolumeUse()[i].GetContainers()
			default:
				v := use.GetVolumeUse()[i]
				on.SizeBytes, on.Path, on.Containers = v.GetSizeBytes(), v.GetMountpoint(), v.GetContainers()
			}
		})
	}
	wg.Wait()
	return out
}
