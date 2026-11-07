// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
)

// destinations reads the destinations, by name, with what signs in to each. What it
// returns goes no further than a call to an agent, sealed.
func (h *Handler) destinations(ctx context.Context, tx *store.Tx) (map[string]*resourcev1.BackupDestination, map[string]string, error) {
	stored, err := tx.ListKind(ctx, string(backuppolicy.DestinationKind))
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]*resourcev1.BackupDestination, len(stored))
	ids := make(map[string]string, len(stored))
	for i := range stored {
		r := &stored[i]
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, nil, fmt.Errorf("can't read the backup destination %s: %w", r.Name, err)
		}
		out[r.Name], ids[r.Name] = spec.(*resourcev1.BackupDestination), r.ID
	}
	return out, ids, nil
}

// destination reads one destination, as a call carries it. An empty name is none.
func (h *Handler) destination(ctx context.Context, name string) (map[string]string, error) {
	if name == "" {
		return nil, nil
	}
	var found map[string]*resourcev1.BackupDestination
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		found, _, err = h.destinations(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	d, ok := found[name]
	if !ok {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no backup destination %s. List them to find its name", name), "not_found")
	}
	return map[string]string{
		"endpoint": d.GetEndpoint(), "region": d.GetRegion(), "bucket": d.GetBucket(), "prefix": d.GetPrefix(),
		"access_key": d.GetAccessKey(), "secret_key": d.GetSecretKey(), "passphrase": d.GetPassphrase(),
	}, nil
}

// sealedWith says which passphrase a destination seals with, as a keyed hash that says
// nothing of it without the install's key. A destination can go and come back under
// its name with another passphrase, and this tells that from the one a copy needs. It
// stays off the API, as a value anyone could compare would confirm a guess.
func (h *Handler) sealedWith(destination map[string]string) string {
	return hex.EncodeToString(h.keeper.Digest("backup passphrase", []byte(destination["passphrase"])))
}

// ListBackupDestinations returns the destinations, and never what signs in to one.
func (h *Handler) ListBackupDestinations(ctx context.Context, _ *connect.Request[dockwellv1.ListBackupDestinationsRequest]) (*connect.Response[dockwellv1.ListBackupDestinationsResponse], error) {
	out := &dockwellv1.ListBackupDestinationsResponse{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		found, ids, err := h.destinations(ctx, tx)
		if err != nil {
			return err
		}
		all, err := tx.ListBackups(ctx, store.BackupFilter{})
		if err != nil {
			return err
		}
		names := make([]string, 0, len(found))
		for name := range found {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			d := found[name]
			info := &dockwellv1.BackupDestinationInfo{Id: ids[name], Name: name, Endpoint: d.GetEndpoint(), Bucket: d.GetBucket(), Prefix: d.GetPrefix()}
			for i := range all {
				b := &all[i]
				if b.Destination != name || b.CopyKey == "" {
					continue
				}
				info.Backups++
				info.UsedBytes += b.CopyBytes
				if info.LastUpload == nil && b.FinishedAt != nil {
					info.LastUpload = timestamppb.New(*b.FinishedAt)
				}
			}
			out.Destinations = append(out.Destinations, info)
		}
		return nil
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(out), nil
}

// FetchBackup starts bringing a backup back from its destination.
func (h *Handler) FetchBackup(ctx context.Context, req *connect.Request[dockwellv1.FetchBackupRequest]) (*connect.Response[dockwellv1.FetchBackupResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	client, _ := authapi.ClientFrom(ctx)
	b, err := h.backup(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if b.CopyKey == "" {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't on a destination, so there's nothing to fetch", b.ID), "not_stored")
	}
	_, server, err := h.volumeOf(ctx, b.VolumeID, req.Msg.GetServer())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	s, err := h.session(server)
	if err != nil {
		return nil, err
	}
	destination, err := h.destination(ctx, b.Destination)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	surface := client.Surface(p)
	actor := p.Actor()
	if err := h.db.Update(ctx, func(tx *store.Tx) error { return tx.SetBackupFetchError(ctx, b.ID, "") }); err != nil {
		return nil, h.fail(ctx, err)
	}
	// From here, not from the server's first word, so a page can tell it's under way.
	h.mu.Lock()
	h.progress[b.ID] = "Fetching " + b.Volume
	h.mu.Unlock()
	h.wg.Go(func() { h.fetch(s, b, server.ID, destination, actor, surface) })
	out, err := h.proto(ctx, b)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.FetchBackupResponse{Backup: out}), nil
}

// fetch brings a backup back, and says so.
func (h *Handler) fetch(s *agentapi.Session, b store.Backup, server string, destination map[string]string, actor, surface string) {
	defer clear(destination)
	ctx := resource.WithProgress(h.base, func(message string) {
		h.mu.Lock()
		h.progress[b.ID] = message
		h.mu.Unlock()
	})
	defer func() {
		h.mu.Lock()
		delete(h.progress, b.ID)
		h.mu.Unlock()
	}()
	ref := resource.Ref{Kind: volume.Kind, Scope: b.EnvironmentID, Name: b.Volume}
	_, err := s.FetchBackup(ctx, &agentv1.FetchBackupCall{Id: b.ID, Volume: resourcepb.Ref(ref), Key: b.CopyKey, Sha256: b.SHA256}, destination)
	record := context.WithoutCancel(ctx)
	action, told := ActionFetched, Told{ID: b.ID, Volume: b.Volume, Destination: b.Destination}
	if err != nil {
		action = ActionFetchFailed
		told.Error, _ = resource.Explain(err)
		if b.SealedWith != "" && b.SealedWith != h.sealedWith(destination) {
			told.Error += ". This copy was sealed with the passphrase " + b.Destination + " had before, so today's can't open it. Add " + b.Destination + " again with that passphrase, or open the file with dockwell open-backup"
		}
	}
	if werr := h.say(record, &b, action, told, actor, surface, func(tx *store.Tx) error {
		if err != nil {
			return tx.SetBackupFetchError(record, b.ID, told.Error)
		}
		return tx.MoveBackup(record, b.ID, server)
	}); werr != nil {
		h.logger.ErrorContext(record, "can't record that a backup was fetched", "backup", b.ID, "error", werr.Error())
	}
}
