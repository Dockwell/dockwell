// SPDX-License-Identifier: AGPL-3.0-only

// Package backups serves dockwell.v1.BackupService: backing up a volume on its
// server, the record of what's been taken, and planning a restore. A backup is an
// event, and a restore a change with a plan. See
// docs/adr/0033-backups-of-volumes-and-databases.md.
package backups

import (
	"cmp"
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
	"github.com/dockwell/dockwell/internal/database"
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
var Actions = []string{ActionTaken, ActionFailed, ActionDeleted, ActionCopyFailed, ActionFetched, ActionFetchFailed, ActionServerCopyRemoved, ActionCopyKept, ActionDrilled}

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
	// Drill is how a restore drill of it went, for ActionDrilled: passed, failed or
	// not_checked, with Error saying why (ADR 0068).
	Drill string `json:"drill,omitempty"`
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
	// drilling is the backup each server's one restore drill checks, by server ID.
	drilling map[string]string
}

var _ dockwellv1connect.BackupServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents.
func New(ch *changes.Handler, engine *revision.Engine, agents *agentapi.Agents, db *store.DB, keeper *secrets.Keeper, logger *slog.Logger) *Handler {
	base, cancel := context.WithCancel(context.Background())
	h := &Handler{
		changes: ch, engine: engine, agents: agents, db: db, keeper: keeper, logger: logger, names: actors.New(),
		base: base, cancel: cancel, progress: map[string]string{},
	}
	engine.OnBackup(h.recordStep)
	return h
}

// recordStep puts a backup a change's step took on the record. A step resumed after
// its answer went unheard reports what's on the disk again, and this says it once.
func (h *Handler) recordStep(ctx context.Context, step revision.BackupStep) {
	taken := step.Taken
	now := h.now()
	actor := cmp.Or(step.Change.AppliedBy, step.Change.Actor)
	b := store.Backup{
		ID: taken.ID, EnvironmentID: step.Volume.Scope, Volume: step.Volume.Name, VolumeID: step.VolumeID, ServerID: step.Server,
		State: store.BackupDone, Method: taken.Method, Database: taken.Database, SizeBytes: taken.Size, SHA256: taken.SHA256, FilesChanged: taken.Changed,
		Actor: actor, StartedAt: now, FinishedAt: &now, OneDatabase: step.One,
	}
	if link := step.For; link != (resource.Ref{}) {
		// Taken for an app before its update: its page lists it (ADR 0084).
		_ = h.db.View(ctx, func(tx *store.Tx) error {
			r, err := tx.GetResource(ctx, string(link.Kind), link.Scope, link.Name)
			if err == nil {
				b.ForLink = r.ID
			}
			return err
		})
	}
	told := Told{ID: b.ID, Volume: b.Volume, Method: b.Method, Size: b.SizeBytes}
	err := h.say(ctx, &b, ActionTaken, told, actor, store.SurfaceAutomatic, func(tx *store.Tx) error {
		if err := tx.CreateBackup(ctx, b); err != nil {
			return err
		}
		return tx.FinishBackup(ctx, b)
	})
	if err != nil {
		var seen store.Backup
		if lookErr := h.db.View(ctx, func(tx *store.Tx) error {
			var err error
			seen, err = tx.GetBackup(ctx, b.ID)
			return err
		}); lookErr == nil && seen.ID == b.ID {
			return
		}
		h.logger.ErrorContext(ctx, "can't record a backup a change took", slog.String("backup", b.ID), slog.String("error", err.Error()))
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
	h.recoverDrills(ctx)
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

// CreateBackup starts a backup of a whole volume, and returns it running.
func (h *Handler) CreateBackup(ctx context.Context, req *connect.Request[dockwellv1.CreateBackupRequest]) (*connect.Response[dockwellv1.CreateBackupResponse], error) {
	m := req.Msg
	out, err := h.begin(ctx, taking{volumeID: m.GetVolumeId(), server: m.GetServer(), destination: m.GetDestination(), stop: m.GetStop()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.CreateBackupResponse{Backup: out}), nil
}

// taking is a backup to start: of a whole volume, or of one database on it, taken
// for an app's link.
type taking struct {
	volumeID, server, destination string
	stop                          bool
	// one is the one database to dump, and forLink the link, by ID, of the app it's
	// taken for (ADR 0084).
	one, forLink string
}

// begin starts a backup, and returns it running.
func (h *Handler) begin(ctx context.Context, t taking) (*dockwellv1.Backup, error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	client, _ := authapi.ClientFrom(ctx)
	ref, server, err := h.volumeOf(ctx, t.volumeID, t.server)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	s, err := h.session(server)
	if err != nil {
		return nil, err
	}
	if t.one != "" {
		if err := database.CheckSQLName("database", t.one); err != nil {
			return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
		}
		if database.SystemDatabase(t.one) {
			return nil, authapi.ReasonError(connect.CodeInvalidArgument, fmt.Errorf("%s is a database its server keeps for itself. Back up the whole volume to keep it", t.one), "invalid")
		}
	}
	destination, err := h.destination(ctx, t.destination)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	b := store.Backup{
		ID: id.New(id.Backup), EnvironmentID: ref.Scope, Volume: ref.Name, VolumeID: t.volumeID, ServerID: server.ID,
		State: store.BackupRunning, Actor: p.Actor(), StartedAt: h.now(), Destination: t.destination, OneDatabase: t.one, ForLink: t.forLink,
	}
	if err := h.db.Update(ctx, func(tx *store.Tx) error { return tx.CreateBackup(ctx, b) }); err != nil {
		return nil, h.fail(ctx, err)
	}
	h.wg.Go(func() { h.run(s, b, ref, t.stop, client.Surface(p), 0, destination) })
	out, err := h.proto(ctx, b)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return out, nil
}

// BackUpOne starts a backup of one database alone on a volume, an app's on a server
// it may share, taken for the app's link, by ID, whose project's role the caller has
// been checked for, and returns it running (ADR 0084).
func (h *Handler) BackUpOne(ctx context.Context, volumeID, one, forLink, destination string) (*dockwellv1.Backup, error) {
	return h.begin(ctx, taking{volumeID: volumeID, destination: destination, one: one, forLink: forLink})
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
	got, err := s.Backup(ctx, &agentv1.BackupCall{Id: b.ID, Volume: resourcepb.Ref(ref), Stop: stop, Database: b.OneDatabase})
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
	told := Told{ID: b.ID, Volume: b.Volume, Destination: b.Destination}
	var action string // what to put on the timeline, if anything
	if err != nil {
		action = ActionCopyFailed
		told.Error, _ = resource.Explain(err)
		if errors.Is(err, context.Canceled) {
			told.Error = copyCutShort(b.Destination)
		}
		b.CopyError = told.Error
	} else {
		b.CopyKey, b.CopyBytes, b.SealedWith = got.GetKey(), got.GetStoredBytes(), h.sealedWith(destination["passphrase"])
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
	out, err := h.list(ctx, store.BackupFilter{VolumeID: m.GetVolumeId(), EnvironmentID: m.GetEnvironmentId(), OneDatabase: m.GetDatabaseName()}, m.GetPageSize(), m.GetPageToken(), "ListBackups")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// ListFor lists the backups taken for an app's link, by ID, whose project's role the
// caller has been checked for (ADR 0084).
func (h *Handler) ListFor(ctx context.Context, forLink string, pageSize int32, pageToken string) (*dockwellv1.ListBackupsResponse, error) {
	return h.list(ctx, store.BackupFilter{ForLink: forLink}, pageSize, pageToken, "ListCatalogNeedBackups")
}

// list is a page of the backups f keeps, newest first.
func (h *Handler) list(ctx context.Context, f store.BackupFilter, pageSize int32, pageToken, call string) (*dockwellv1.ListBackupsResponse, error) {
	size, err := paging.Size(pageSize, DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	cursor, more, err := paging.Read(pageToken, call)
	if err != nil {
		return nil, err
	}
	f.Limit = size + 1
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
		f, err := h.facts(ctx, tx)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(list))
		for i := range list {
			ids = append(ids, list[i].ID)
		}
		if f.drills, err = tx.NewestDrills(ctx, ids); err != nil {
			return err
		}
		for i := range list {
			row, err := h.protoIn(ctx, tx, f, &list[i])
			if err != nil {
				return err
			}
			out.Backups = append(out.Backups, row)
		}
		return nil
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return out, nil
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
	var drilled map[string]bool
	if err := h.db.View(ctx, func(tx *store.Tx) (err error) {
		drilled, err = drillsRunning(ctx, tx)
		return err
	}); err != nil {
		return nil, h.fail(ctx, err)
	}
	if drilled[b.ID] {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("a restore check of %s is running. Delete it once that ends", b.ID), "drill_running")
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
		earlier, err := h.sealedEarlier(ctx, &b)
		if err != nil {
			return nil, h.fail(ctx, err)
		}
		if earlier {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s was sealed with an earlier passphrase, so removing it from its server would leave only a copy Dockwell can't open. Add %s again with that passphrase first, or leave it on its server", b.ID, b.Destination), "sealed_earlier")
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

// RestoreBackup plans restoring a backup of a whole volume, as a change. A database
// server other projects' apps share takes a role in each, or an admin, its name
// typed, and a plan that names what each app loses (ADR 0084).
func (h *Handler) RestoreBackup(ctx context.Context, req *connect.Request[dockwellv1.RestoreBackupRequest]) (*connect.Response[dockwellv1.RestoreBackupResponse], error) {
	b, err := h.backup(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if b.OneDatabase != "" {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s holds %s alone, an app's database, which restores from that app's page. Restore it there", b.ID, b.OneDatabase), "restore_from_app")
	}
	ref := resource.Ref{Kind: volume.Kind, Scope: b.EnvironmentID, Name: b.Volume}
	notes, err := h.wholeServer(ctx, ref, &b, req.Msg.GetConfirm())
	if err != nil {
		return nil, err
	}
	c, err := h.restore(ctx, &b, ref, revision.RestoreAs{Notes: notes}, strings.TrimSpace(req.Msg.GetSummary()))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.RestoreBackupResponse{Change: c}), nil
}

// RestoreOne plans bringing back one database alone from a backup taken for an app's
// link, by its ID and as its ref, whose project's role the caller has been checked
// for: the engine checks it again, where the link is (ADR 0084).
func (h *Handler) RestoreOne(ctx context.Context, backupID, forLink string, link resource.Ref, summary string) (*dockwellv1.Change, error) {
	b, err := h.backup(ctx, backupID)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if b.ForLink != forLink || b.OneDatabase == "" {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no backup %s of this app's database. List its backups to find one", backupID), "not_found")
	}
	ref := resource.Ref{Kind: volume.Kind, Scope: b.EnvironmentID, Name: b.Volume}
	return h.restore(ctx, &b, ref, revision.RestoreAs{For: link}, cmp.Or(strings.TrimSpace(summary), "Restore "+b.OneDatabase))
}

// restore plans restoring b into ref, as as says.
func (h *Handler) restore(ctx context.Context, b *store.Backup, ref resource.Ref, as revision.RestoreAs, summary string) (*dockwellv1.Change, error) {
	if b.State != store.BackupDone {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is %s, so there's nothing to restore. Choose one that finished", b.ID, b.State), "not_restorable")
	}
	if b.CopyKey != "" {
		can, err := h.restorable(ctx, b)
		if err != nil {
			return nil, h.fail(ctx, err)
		}
		if !can {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't on a server that can restore it now. Fetch it from %s onto a server that has %s, then restore it", b.ID, b.Destination, b.Volume), "fetch_first")
		}
	}
	return h.changes.Planned(ctx, func(who revision.By) (*revision.Plan, error) {
		source := resource.RestoreSource{Backup: b.ID, Server: b.ServerID}
		return h.engine.RestoreWith(ctx, ref, source, as, who, summary)
	})
}

var states = map[string]dockwellv1.BackupState{
	store.BackupRunning: dockwellv1.BackupState_BACKUP_STATE_RUNNING,
	store.BackupDone:    dockwellv1.BackupState_BACKUP_STATE_DONE,
	store.BackupFailed:  dockwellv1.BackupState_BACKUP_STATE_FAILED,
}

func (h *Handler) proto(ctx context.Context, b store.Backup) (*dockwellv1.Backup, error) {
	var out *dockwellv1.Backup
	err := h.db.View(ctx, func(tx *store.Tx) error {
		f, err := h.facts(ctx, tx)
		if err != nil {
			return err
		}
		out, err = h.protoIn(ctx, tx, f, &b)
		return err
	})
	return out, err
}

// rowFacts is what a backup's row says beyond the backup, read once for a page of them.
type rowFacts struct {
	look actors.Lookup
	// connected names the servers in the fleet that are connected now, by ID.
	connected map[string]string
	// sealing says what each destination seals with now, by name (sealedWith).
	sealing map[string]string
	// placed is the servers each volume read so far is on, by ID, and nil for one
	// that's deleted.
	placed map[string][]string
	// drills are each backup's newest drill, by its ID, read for a page. Nil reads
	// each row's own.
	drills map[string]store.Drill
}

func (h *Handler) facts(ctx context.Context, tx *store.Tx) (*rowFacts, error) {
	f := &rowFacts{look: h.names.In(ctx, tx), connected: map[string]string{}, sealing: map[string]string{}, placed: map[string][]string{}}
	servers, err := tx.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range servers {
		if _, ok := h.agents.Session(servers[i].ID); ok {
			f.connected[servers[i].ID] = servers[i].Name
		}
	}
	found, _, err := h.destinations(ctx, tx)
	if err != nil {
		return nil, err
	}
	for name, d := range found {
		f.sealing[name] = h.sealedWith(d.GetPassphrase())
	}
	return f, nil
}

// opens says a backup's copy opens with what its destination has now. One that doesn't
// is no way back for Dockwell, whatever the bucket holds.
func (f *rowFacts) opens(b *store.Backup) bool {
	return b.CopyKey != "" && b.SealedWith == f.sealing[b.Destination]
}

// placement is the servers a backup's volume is on, and nil once it's deleted.
func (f *rowFacts) placement(ctx context.Context, tx *store.Tx, b *store.Backup) ([]string, error) {
	if placed, ok := f.placed[b.VolumeID]; ok {
		return placed, nil
	}
	var placed []string
	r, err := tx.GetResourceByID(ctx, b.VolumeID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil:
	case err != nil:
		return nil, err
	default:
		if placed, err = tx.Placement(ctx, r.ID, r.Revision); err != nil {
			return nil, err
		}
	}
	f.placed[b.VolumeID] = placed
	return placed, nil
}

// here says a backup can be restored where it is now: its server has it, is in the fleet
// and connected, and still has its volume. What a volume that's deleted needs first, a
// restore says itself.
func (f *rowFacts) here(ctx context.Context, tx *store.Tx, b *store.Backup) (bool, error) {
	if _, connected := f.connected[b.ServerID]; !connected || b.State != store.BackupDone || b.OffServer {
		return false, nil
	}
	placed, err := f.placement(ctx, tx, b)
	return placed == nil || slices.Contains(placed, b.ServerID), err
}

// onto names the connected servers a backup can be fetched onto: those its volume is on.
func (f *rowFacts) onto(ctx context.Context, tx *store.Tx, b *store.Backup) ([]string, error) {
	placed, err := f.placement(ctx, tx, b)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, server := range placed {
		if name, ok := f.connected[server]; ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// sealedEarlier says a backup's copy won't open with what its destination has now.
func (h *Handler) sealedEarlier(ctx context.Context, b *store.Backup) (bool, error) {
	var opens bool
	err := h.db.View(ctx, func(tx *store.Tx) error {
		f, err := h.facts(ctx, tx)
		if err == nil {
			opens = f.opens(b)
		}
		return err
	})
	return !opens, err
}

// restorable says a backup can be restored where it is now (rowFacts.here).
func (h *Handler) restorable(ctx context.Context, b *store.Backup) (bool, error) {
	var can bool
	err := h.db.View(ctx, func(tx *store.Tx) error {
		f, err := h.facts(ctx, tx)
		if err != nil {
			return err
		}
		can, err = f.here(ctx, tx, b)
		return err
	})
	return can, err
}

func (h *Handler) protoIn(ctx context.Context, tx *store.Tx, f *rowFacts, b *store.Backup) (*dockwellv1.Backup, error) {
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
	here, err := f.here(ctx, tx, b)
	if err != nil {
		return nil, err
	}
	out := &dockwellv1.Backup{
		Destination: b.Destination, Copy: copied, CopyError: b.CopyError, OnServer: here,
		FetchError: b.FetchError, SealedEarlier: b.CopyKey != "" && !f.opens(b),
		Id: b.ID, EnvironmentId: b.EnvironmentID, Volume: b.Volume, VolumeId: b.VolumeID, ServerId: b.ServerID, ServerName: f.look.Name(b.ServerID),
		State: states[b.State], Method: resourcepb.BackupMethod(b.Method), Database: b.Database, DatabaseName: b.OneDatabase, SizeBytes: b.SizeBytes, Sha256: b.SHA256,
		StartedAt: timestamppb.New(b.StartedAt), Actor: f.look.Actor(b.Actor), Error: b.Error, FilesChanged: b.FilesChanged, PolicyId: b.PolicyID,
	}
	if b.CopyKey != "" && !here {
		if out.FetchOnto, err = f.onto(ctx, tx, b); err != nil {
			return nil, err
		}
	}
	if b.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*b.FinishedAt)
	}
	drills := f.drills
	if drills == nil {
		if drills, err = tx.NewestDrills(ctx, []string{b.ID}); err != nil {
			return nil, err
		}
	}
	if d, ok := drills[b.ID]; ok {
		out.Drill = drillProto(&d, f)
	}
	h.mu.Lock()
	out.Progress = h.progress[b.ID] // a run's, and a send's or a fetch's after one
	h.mu.Unlock()
	return out, nil
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
