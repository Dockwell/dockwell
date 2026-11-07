// SPDX-License-Identifier: AGPL-3.0-only

// Package backups serves dockwell.v1.BackupService: backing up a volume on its
// server, the record of what's been taken, and planning a restore. A backup is an
// event, and a restore a change with a plan. See
// docs/adr/0033-backups-of-volumes-and-databases.md.
package backups

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Page sizes.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// previewWait bounds a server's answer to what a backup would do.
const previewWait = 15 * time.Second

// What the audit log calls what becomes of a backup, each on the timeline.
const (
	ActionTaken      = "backup.taken"
	ActionFailed     = "backup.failed"
	ActionDeleted    = "backup.deleted"
	ActionCopyFailed = "backup.copy_failed"
	ActionFetched    = "backup.fetched"
	// ActionFetchFailed is a backup that couldn't be brought back from its destination.
	ActionFetchFailed = "backup.fetch_failed"
	// ActionServerCopyRemoved is a backup gone from its server, and kept on its destination.
	ActionServerCopyRemoved = "backup.server_copy_removed"
	// ActionCopyKept is a backup its destination wouldn't let go of.
	ActionCopyKept = "backup.copy_kept"
)

// Actions are all of them.
var Actions = []string{ActionTaken, ActionFailed, ActionDeleted, ActionCopyFailed, ActionFetched, ActionFetchFailed, ActionServerCopyRemoved, ActionCopyKept}

// Told is what an audit event says of a backup, in its detail, which the timeline
// reads. It holds no path and nothing of what's in the backup.
type Told struct {
	ID     string `json:"id"`
	Volume string `json:"volume"`
	Method string `json:"method,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Error  string `json:"error,omitempty"`
	// Destination is where it went or didn't, by name.
	Destination string `json:"destination,omitempty"`
}

// ReadTold reads an audit event's detail.
func ReadTold(detail string) (Told, bool) {
	var t Told
	err := json.Unmarshal([]byte(detail), &t)
	return t, err == nil && t.ID != ""
}

// Handler implements BackupService.
type Handler struct {
	changes *changes.Handler
	engine  *revision.Engine
	agents  *agentapi.Agents
	db      *store.DB
	keeper  *secrets.Keeper
	logger  *slog.Logger
	names   *actors.Names

	// base outlives each request that starts a backup, and Stop ends it.
	base   context.Context //nolint:containedctx // what every backup under way runs under
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// began is when this control plane started, as servers need a moment to connect.
	began time.Time

	mu       sync.Mutex
	progress map[string]string        // how each backup under way is going
	turns    map[string]chan struct{} // whose turn it is on each server, for scheduled runs
}

var _ dockwellv1connect.BackupServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents.
func New(ch *changes.Handler, engine *revision.Engine, agents *agentapi.Agents, db *store.DB, keeper *secrets.Keeper, logger *slog.Logger) *Handler {
	base, cancel := context.WithCancel(context.Background())
	return &Handler{
		changes: ch, engine: engine, agents: agents, db: db, keeper: keeper, logger: logger, names: actors.New(),
		base: base, cancel: cancel, progress: map[string]string{},
	}
}

// Stop ends the backups under way, which their servers stop, and waits for
// their records.
func (h *Handler) Stop() {
	h.cancel()
	h.wg.Wait()
}

// Recover fails what was running when the control plane last stopped, as nothing
// waits for those any more. It runs before anything else can start one. What it
// can't read stays as it is until the next start, as a person needs the panel to
// find out why.
func (h *Handler) Recover(ctx context.Context) {
	h.began = h.now()
	var running []store.Backup
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		running, err = tx.ListBackups(ctx, store.BackupFilter{})
		return err
	}); err != nil {
		if ctx.Err() == nil {
			h.logger.ErrorContext(ctx, "can't read the backups that were running, so they show as running until the next start", slog.String("error", err.Error()))
		}
		return
	}
	for i := range running {
		b := running[i]
		switch {
		case b.State == store.BackupRunning:
			h.finish(ctx, b, nil, errors.New("the control plane stopped while this ran, so it isn't known to be whole, and it doesn't count. Take another"), store.SurfaceAutomatic)
		case sending(&b):
			// It's whole, and it was on its way when the control plane stopped.
			b.CopyError = copyCutShort(b.Destination)
			told := Told{ID: b.ID, Volume: b.Volume, Destination: b.Destination, Error: b.CopyError}
			if err := h.say(ctx, &b, ActionCopyFailed, told, b.Actor, store.SurfaceAutomatic, func(tx *store.Tx) error { return tx.SetBackupCopy(ctx, b) }); err != nil {
				h.logger.ErrorContext(ctx, "can't record what became of a backup's copy", slog.String("backup", b.ID), slog.String("error", err.Error()))
			}
		}
	}
}

// Wait waits for what's under way to end: runs, what they send, and fetches.
func (h *Handler) Wait() { h.wg.Wait() }

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// volumeOf reads a volume by ID, and where it is.
func (h *Handler) volumeOf(ctx context.Context, volumeID, serverName string) (resource.Ref, store.Server, error) {
	var ref resource.Ref
	var server store.Server
	err := h.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(ctx, volumeID)
		if errors.Is(err, store.ErrNotFound) || err == nil && (r.Spec == nil || r.Kind != string(volume.Kind)) {
			return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no volume %s. List the resources to find its ID", volumeID), "not_found")
		}
		if err != nil {
			return err
		}
		ref = resource.Ref{Kind: volume.Kind, Scope: r.Scope, Name: r.Name}
		placed, err := tx.Placement(ctx, r.ID, r.Revision)
		if err != nil {
			return err
		}
		server, err = chooseServer(ctx, tx, ref.Name, placed, serverName)
		return err
	})
	return ref, server, err
}

// chooseServer finds the server a request names among those that have a volume,
// or the only one that does.
func chooseServer(ctx context.Context, tx *store.Tx, volumeName string, placed []string, want string) (store.Server, error) {
	var servers []store.Server
	var names []string
	for _, p := range placed {
		s, err := tx.GetServer(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return store.Server{}, err
		}
		if s.RevokedAt == nil {
			servers = append(servers, s)
			names = append(names, s.Name)
		}
	}
	slices.Sort(names)
	switch {
	case len(servers) == 0:
		return store.Server{}, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't on any server yet. Deploy what uses it first", volumeName), "not_placed")
	case want == "" && len(servers) > 1:
		return store.Server{}, authapi.Invalid(fmt.Errorf("%s is on %s. Say which to back up, with server", volumeName, human.List(names)))
	case want == "":
		return servers[0], nil
	}
	if i := slices.IndexFunc(servers, func(s store.Server) bool { return s.ID == want || s.Name == want }); i >= 0 {
		return servers[i], nil
	}
	return store.Server{}, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("%s isn't on %s. It's on %s", volumeName, want, human.List(names)), "not_found")
}

// session finds a server's connection, or says why there's none.
func (h *Handler) session(server store.Server) (*agentapi.Session, error) {
	s, ok := h.agents.Session(server.ID)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s isn't connected, and a backup runs on it. Try again once its agent reconnects", server.Name), "unavailable")
	}
	return s, nil
}

// PreviewBackup says what backing up a volume would do.
func (h *Handler) PreviewBackup(ctx context.Context, req *connect.Request[dockwellv1.PreviewBackupRequest]) (*connect.Response[dockwellv1.PreviewBackupResponse], error) {
	ref, server, err := h.volumeOf(ctx, req.Msg.GetVolumeId(), req.Msg.GetServer())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	s, err := h.session(server)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, previewWait)
	defer cancel()
	p, err := s.BackupPreview(ctx, ref)
	if err != nil {
		return nil, h.agentError(server, err)
	}
	return connect.NewResponse(&dockwellv1.PreviewBackupResponse{
		Method: p.GetMethod(), Database: p.GetDatabase(), Running: p.GetRunning(),
		EstimateBytes: p.GetEstimateBytes(), FreeBytes: p.GetFreeBytes(), ReserveBytes: p.GetReserveBytes(),
	}), nil
}

// CreateBackup starts a backup, and returns it running.
func (h *Handler) CreateBackup(ctx context.Context, req *connect.Request[dockwellv1.CreateBackupRequest]) (*connect.Response[dockwellv1.CreateBackupResponse], error) {
	m := req.Msg
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	client, _ := authapi.ClientFrom(ctx)
	ref, server, err := h.volumeOf(ctx, m.GetVolumeId(), m.GetServer())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	s, err := h.session(server)
	if err != nil {
		return nil, err
	}
	destination, err := h.destination(ctx, m.GetDestination())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	b := store.Backup{
		ID: id.New(id.Backup), EnvironmentID: ref.Scope, Volume: ref.Name, VolumeID: m.GetVolumeId(), ServerID: server.ID,
		State: store.BackupRunning, Actor: p.Actor(), StartedAt: h.now(), Destination: m.GetDestination(),
	}
	if err := h.db.Update(ctx, func(tx *store.Tx) error { return tx.CreateBackup(ctx, b) }); err != nil {
		return nil, h.fail(ctx, err)
	}
	h.wg.Go(func() { h.run(s, b, ref, m.GetStop(), client.Surface(p), 0, destination) })
	out, err := h.proto(ctx, b)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.CreateBackupResponse{Backup: out}), nil
}

// run takes a backup on its server, and records how that went.
func (h *Handler) run(s *agentapi.Session, b store.Backup, ref resource.Ref, stop bool, surface string, keep int, destination map[string]string) {
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
	// A schedule cut back holds more than it keeps now, which can be what keeps this
	// run from fitting, and only a run that's whole would take those away otherwise.
	if b.PolicyID != "" {
		h.prune(ctx, s, b, keep)
	}
	got, err := s.Backup(ctx, &agentv1.BackupCall{Id: b.ID, Volume: resourcepb.Ref(ref), Stop: stop})
	h.finish(context.WithoutCancel(ctx), b, got, err, surface)
	// It's on record and the volume is free before this starts, however long it takes.
	if err == nil && destination != nil {
		h.send(ctx, s, b, ref, destination, surface)
	}
	// Only once it's whole: a run that fails takes nothing away.
	if err == nil && b.PolicyID != "" {
		h.prune(context.WithoutCancel(ctx), s, b, keep)
	}
}

// send sends a whole backup to its destination, and records what came of that. The
// backup is done whatever happens here: what's on its server is what counts.
func (h *Handler) send(ctx context.Context, s *agentapi.Session, b store.Backup, ref resource.Ref, destination map[string]string, surface string) {
	got, err := s.SendBackup(ctx, &agentv1.SendBackupCall{Id: b.ID, Volume: resourcepb.Ref(ref)}, destination)
	record := context.WithoutCancel(ctx)
	action, told := "", Told{ID: b.ID, Volume: b.Volume, Destination: b.Destination}
	if err != nil {
		action = ActionCopyFailed
		told.Error, _ = resource.Explain(err)
		if errors.Is(err, context.Canceled) {
			told.Error = copyCutShort(b.Destination)
		}
		b.CopyError = told.Error
	} else {
		b.CopyKey, b.CopyBytes, b.SealedWith = got.GetKey(), got.GetStoredBytes(), h.sealedWith(destination)
	}
	save := func(tx *store.Tx) error { return tx.SetBackupCopy(record, b) }
	if action != "" {
		err = h.say(record, &b, action, told, b.Actor, surface, save)
	} else {
		err = h.db.Update(record, save)
	}
	if err != nil {
		h.logger.ErrorContext(record, "can't record what became of a backup's copy", slog.String("backup", b.ID), slog.String("error", err.Error()))
	}
}

// sending says a whole backup is on its way to its destination: nothing has said how
// that ended.
func sending(b *store.Backup) bool {
	return b.State == store.BackupDone && b.Destination != "" && b.CopyKey == "" && b.CopyError == ""
}

// copyCutShort says a copy didn't get there, as the control plane stopped first.
func copyCutShort(destination string) string {
	return "The control plane stopped while this was sent, so it isn't on " + destination + ". It's safe on its server. Take another to send one"
}

// finish records how a backup ended, and says so on the timeline.
func (h *Handler) finish(ctx context.Context, b store.Backup, got *agentv1.BackupResult, failed error, surface string) {
	now := h.now()
	b.FinishedAt = &now
	action := ActionTaken
	if failed != nil {
		b.State, action = store.BackupFailed, ActionFailed
		plain, _ := resource.Explain(failed)
		b.Error = plain
		if errors.Is(failed, context.Canceled) {
			b.Error = "The control plane stopped while this ran, so its server stopped it too. Take another."
		}
	} else {
		b.State = store.BackupDone
		b.Method, b.Database = resourcepb.BackupMethodFrom(got.GetMethod()), got.GetDatabase()
		b.SizeBytes, b.SHA256, b.FilesChanged = got.GetSizeBytes(), got.GetSha256(), got.GetFilesChanged()
	}
	detail, err := json.Marshal(Told{ID: b.ID, Volume: b.Volume, Method: b.Method, Size: b.SizeBytes, Error: b.Error})
	if err != nil {
		h.logger.ErrorContext(ctx, "can't record a backup", slog.String("backup", b.ID), slog.String("error", err.Error()))
		return
	}
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.FinishBackup(ctx, b); err != nil {
			return err
		}
		if err := tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: b.Actor, Action: action, Subject: b.VolumeID, Target: b.ServerID, Detail: string(detail), Surface: surface,
		}); err != nil {
			return err
		}
		if b.CopyError == "" {
			return nil
		}
		said, err := json.Marshal(Told{ID: b.ID, Volume: b.Volume, Destination: b.Destination, Error: b.CopyError})
		if err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: b.Actor, Action: ActionCopyFailed, Subject: b.VolumeID, Target: b.ServerID, Detail: string(said), Surface: surface,
		})
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "can't record how a backup ended", slog.String("backup", b.ID), slog.String("error", err.Error()))
		return
	}
	if failed != nil {
		h.logger.WarnContext(ctx, "a backup failed", slog.String("backup", b.ID), slog.String("volume", b.Volume), slog.String("error", failed.Error()))
	}
}

// GetBackup returns one backup.
func (h *Handler) GetBackup(ctx context.Context, req *connect.Request[dockwellv1.GetBackupRequest]) (*connect.Response[dockwellv1.GetBackupResponse], error) {
	b, err := h.backup(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	out, err := h.proto(ctx, b)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.GetBackupResponse{Backup: out}), nil
}

func (h *Handler) backup(ctx context.Context, backupID string) (store.Backup, error) {
	var b store.Backup
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		b, err = tx.GetBackup(ctx, backupID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return b, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no backup %s. List the backups to find its ID", backupID), "not_found")
	}
	return b, err
}

// ListBackups returns a page of backups, newest first.
func (h *Handler) ListBackups(ctx context.Context, req *connect.Request[dockwellv1.ListBackupsRequest]) (*connect.Response[dockwellv1.ListBackupsResponse], error) {
	m := req.Msg
	size, err := paging.Size(m.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	cursor, more, err := paging.Read(m.GetPageToken(), "ListBackups")
	if err != nil {
		return nil, err
	}
	f := store.BackupFilter{VolumeID: m.GetVolumeId(), EnvironmentID: m.GetEnvironmentId(), Limit: size + 1}
	if more {
		f.AfterAt, f.AfterID = &cursor.At, cursor.ID
	}
	out := &dockwellv1.ListBackupsResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListBackups(ctx, f)
		if err != nil {
			return err
		}
		if len(list) > size {
			list = list[:size]
			last := list[size-1]
			out.NextPageToken = paging.Token(paging.Cursor{At: last.StartedAt, ID: last.ID})
		}
		look := h.names.In(ctx, tx)
		for i := range list {
			out.Backups = append(out.Backups, h.protoIn(look, &list[i]))
		}
		return nil
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(out), nil
}

// DeleteBackup removes a backup from its server and from the record.
func (h *Handler) DeleteBackup(ctx context.Context, req *connect.Request[dockwellv1.DeleteBackupRequest]) (*connect.Response[dockwellv1.DeleteBackupResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	client, _ := authapi.ClientFrom(ctx)
	b, err := h.backup(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if b.State == store.BackupRunning {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is still running. Wait for it to end, then delete it", b.ID), "running")
	}
	if sending(&b) {
		// What's on its way would land with no record of it, for nobody to delete.
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is on its way to %s. Wait for that to end, then delete it", b.ID, b.Destination), "sending")
	}
	actor, surface := p.Actor(), client.Surface(p)
	serverOnly := req.Msg.GetServerOnly()
	if serverOnly {
		switch {
		case b.State != store.BackupDone || b.OffServer:
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't on its server, so there's nothing to remove there", b.ID), "not_on_server")
		case b.CopyKey == "":
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is only on its server, so removing it from there would lose it. Delete it instead", b.ID), "not_stored")
		}
	}
	if b.State == store.BackupDone {
		kept, err := h.removeFromServer(ctx, b, serverOnly)
		if err != nil {
			return nil, err
		}
		if kept != "" {
			if err := h.keptOnDestination(ctx, &b, ActionCopyKept, kept, actor, surface); err != nil {
				return nil, h.fail(ctx, err)
			}
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("it's gone from its server, and %s kept it: %s. Delete it again to try again", b.Destination, kept), "copy_kept")
		}
		if serverOnly {
			if err := h.keptOnDestination(ctx, &b, ActionServerCopyRemoved, "", actor, surface); err != nil {
				return nil, h.fail(ctx, err)
			}
			return connect.NewResponse(&dockwellv1.DeleteBackupResponse{}), nil
		}
	}
	if err := h.forget(ctx, &b, actor, surface); err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.DeleteBackupResponse{}), nil
}

// keptOnDestination records a backup gone from its server that stays on its destination.
func (h *Handler) keptOnDestination(ctx context.Context, b *store.Backup, action, why, actor, surface string) error {
	told := Told{ID: b.ID, Volume: b.Volume, Destination: b.Destination, Error: why}
	return h.say(ctx, b, action, told, actor, surface, func(tx *store.Tx) error { return tx.SetBackupOffServer(ctx, b.ID) })
}

// forget takes a backup off the record, and says so on the timeline.
func (h *Handler) forget(ctx context.Context, b *store.Backup, actor, surface string) error {
	told := Told{ID: b.ID, Volume: b.Volume, Method: b.Method, Size: b.SizeBytes}
	return h.say(ctx, b, ActionDeleted, told, actor, surface, func(tx *store.Tx) error { return tx.DeleteBackup(ctx, b.ID) })
}

// say records what became of a backup, with what goes with it, on the timeline.
func (h *Handler) say(ctx context.Context, b *store.Backup, action string, told Told, actor, surface string, also func(*store.Tx) error) error {
	detail, err := json.Marshal(told)
	if err != nil {
		return err
	}
	return h.db.Update(ctx, func(tx *store.Tx) error {
		if err := also(tx); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: h.now(), Actor: actor, Action: action, Subject: b.VolumeID, Target: b.ServerID, Detail: string(detail), Surface: surface,
		})
	})
}

// removeFromServer removes a backup's bytes. A server that's been removed from
// the fleet took them with it, as far as Dockwell can reach.
func (h *Handler) removeFromServer(ctx context.Context, b store.Backup, serverOnly bool) (kept string, err error) {
	var server store.Server
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		server, err = tx.GetServer(ctx, b.ServerID)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", nil
	case err != nil:
		return "", h.fail(ctx, err)
	case server.RevokedAt != nil:
		return "", nil
	}
	s, err := h.session(server)
	if err != nil {
		return "", err
	}
	kept, err = h.deleteOn(ctx, s, &b, serverOnly)
	if err != nil {
		return "", h.agentError(server, err)
	}
	return kept, nil
}

// deleteOn removes a backup from a server's disk, and unless serverOnly, from its
// destination if it went to one that's still there. It says why the destination kept
// it, if it did.
func (h *Handler) deleteOn(ctx context.Context, s *agentapi.Session, b *store.Backup, serverOnly bool) (string, error) {
	ref := resource.Ref{Kind: volume.Kind, Scope: b.EnvironmentID, Name: b.Volume}
	call := &agentv1.DeleteBackupCall{Id: b.ID, Volume: resourcepb.Ref(ref)}
	var destination map[string]string
	if b.CopyKey != "" && !serverOnly {
		var err error
		destination, err = h.destination(ctx, b.Destination)
		var cerr *connect.Error
		switch {
		case errors.As(err, &cerr) && cerr.Code() == connect.CodeNotFound:
			// The destination was deleted, so what's there is the bucket's now.
		case err != nil:
			return "", err
		default:
			call.Key = b.CopyKey
			defer clear(destination)
		}
	}
	got, err := s.DeleteBackup(ctx, call, destination)
	if err != nil {
		return "", err
	}
	return got.GetCopyError(), nil
}

// RestoreBackup plans restoring a backup, as a change.
func (h *Handler) RestoreBackup(ctx context.Context, req *connect.Request[dockwellv1.RestoreBackupRequest]) (*connect.Response[dockwellv1.RestoreBackupResponse], error) {
	b, err := h.backup(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if b.State != store.BackupDone {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is %s, so there's nothing to restore. Choose one that finished", b.ID, b.State), "not_restorable")
	}
	c, err := h.changes.Planned(ctx, func(who revision.By) (*revision.Plan, error) {
		ref := resource.Ref{Kind: volume.Kind, Scope: b.EnvironmentID, Name: b.Volume}
		source := resource.RestoreSource{Backup: b.ID, Server: b.ServerID}
		return h.engine.Restore(ctx, ref, source, who, strings.TrimSpace(req.Msg.GetSummary()))
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.RestoreBackupResponse{Change: c}), nil
}

var states = map[string]dockwellv1.BackupState{
	store.BackupRunning: dockwellv1.BackupState_BACKUP_STATE_RUNNING,
	store.BackupDone:    dockwellv1.BackupState_BACKUP_STATE_DONE,
	store.BackupFailed:  dockwellv1.BackupState_BACKUP_STATE_FAILED,
}

func (h *Handler) proto(ctx context.Context, b store.Backup) (*dockwellv1.Backup, error) {
	var out *dockwellv1.Backup
	err := h.db.View(ctx, func(tx *store.Tx) error {
		out = h.protoIn(h.names.In(ctx, tx), &b)
		return nil
	})
	return out, err
}

func (h *Handler) protoIn(look actors.Lookup, b *store.Backup) *dockwellv1.Backup {
	copied := dockwellv1.BackupCopy_BACKUP_COPY_UNSPECIFIED
	switch {
	case b.Destination == "" || b.State == store.BackupFailed:
	case b.CopyKey != "":
		copied = dockwellv1.BackupCopy_BACKUP_COPY_STORED
	case b.CopyError != "":
		copied = dockwellv1.BackupCopy_BACKUP_COPY_FAILED
	default:
		copied = dockwellv1.BackupCopy_BACKUP_COPY_PENDING
	}
	out := &dockwellv1.Backup{
		Destination: b.Destination, Copy: copied, CopyError: b.CopyError, OnServer: b.State == store.BackupDone && !b.OffServer,
		FetchError: b.FetchError,
		Id:         b.ID, EnvironmentId: b.EnvironmentID, Volume: b.Volume, VolumeId: b.VolumeID, ServerId: b.ServerID, ServerName: look.Name(b.ServerID),
		State: states[b.State], Method: resourcepb.BackupMethod(b.Method), Database: b.Database, SizeBytes: b.SizeBytes, Sha256: b.SHA256,
		StartedAt: timestamppb.New(b.StartedAt), Actor: look.Actor(b.Actor), Error: b.Error, FilesChanged: b.FilesChanged, PolicyId: b.PolicyID,
	}
	if b.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*b.FinishedAt)
	}
	h.mu.Lock()
	out.Progress = h.progress[b.ID] // a run's, and a send's or a fetch's after one
	h.mu.Unlock()
	return out
}

// agentError says what a server said, or that it's gone.
func (h *Handler) agentError(server store.Server, err error) error {
	switch {
	case errors.Is(err, resource.ErrNotConnected), errors.Is(err, resource.ErrLost):
		return authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s isn't connected, and a backup runs on it. Try again once its agent reconnects", server.Name), "unavailable")
	case errors.Is(err, context.DeadlineExceeded):
		return authapi.ReasonError(connect.CodeDeadlineExceeded, fmt.Errorf("%s didn't answer in time. Try again", server.Name), "unavailable")
	}
	plain, _ := resource.Explain(err)
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s: %s", server.Name, plain))
}

// fail turns what went wrong into what the API says. An error it made for the
// caller passes through as it is.
func (h *Handler) fail(ctx context.Context, err error) error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "a backup call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its backups; try again, and check its logs if it keeps failing"))
}
