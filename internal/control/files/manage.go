// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	posixpath "path"
	"strings"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	ids "github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// What a person makes, moves, deletes and puts back on a server, and what's in its
// Recently deleted (ADR 0045). Each is one request, as a save is: the agent decides
// what's refused and what's a way back in, and each goes on record, never what a file
// holds.

// op is one of these writes, as the audit log and a passkey know it.
type op struct {
	verb   string // as a sentence says it, such as "move"
	stepUp auth.StepUpAction
}

var (
	opMake     = op{"make", auth.StepUpMakeWayBackIn}
	opMove     = op{"move", auth.StepUpMoveWayBackIn}
	opDelete   = op{"delete", auth.StepUpDeleteWayBackIn}
	opRestore  = op{"put back", auth.StepUpRestoreWayBackIn}
	opCompress = op{"compress", auth.StepUpCompressWayBackIn}
	opExtract  = op{"extract", auth.StepUpExtractWayBackIn}
)

// actions names each outcome explicitly, so the audit inventory can follow every
// recorded action through the shared write handlers.
func (o op) actions() (done, failed, unconfirmed string) {
	switch o.verb {
	case "make":
		return "file.make", "file.make_failed", "file.make_unconfirmed"
	case "move":
		return "file.move", "file.move_failed", "file.move_unconfirmed"
	case "delete":
		return "file.delete", "file.delete_failed", "file.delete_unconfirmed"
	case "put back":
		return "file.restore", "file.restore_failed", "file.restore_unconfirmed"
	case "upload":
		return "file.upload", "file.upload_failed", "file.upload_unconfirmed"
	case "compress":
		return "file.compress", "file.compress_failed", "file.compress_unconfirmed"
	case "extract":
		return "file.extract", "file.extract_failed", "file.extract_unconfirmed"
	default:
		panic("file operation has no audit actions")
	}
}

// startedAction names the Activity action written when a background archive job
// begins, kept explicit for the audit inventory.
func (o op) startedAction() string {
	switch o.verb {
	case "compress":
		return "file.compress_started"
	case "extract":
		return "file.extract_started"
	default:
		panic("file operation has no started audit action")
	}
}

// eventMayHave is a write whose call ended as it acted, so it may yet happen.
const eventMayHave = "file.change_unknown"

// Told is what the event of a write says beside its path, for the timeline: where it
// went, and whether it took a passkey or can't be put back.
type Told struct {
	To        string `json:"to,omitempty"`
	WayBackIn bool   `json:"way_back_in,omitempty"`
	ForGood   bool   `json:"for_good,omitempty"`
	// Size is how many bytes an upload, download or archive came to.
	Size uint64 `json:"size,omitempty"`
	// Paths holds the first few resolved source or selected paths for a
	// compress or multi-path download event, and PathCount holds the full count.
	Paths     []string `json:"paths,omitempty"`
	PathCount int      `json:"path_count,omitempty"`
	// JobID ties a background archive job's start event to its outcome across
	// control-plane restarts.
	JobID string `json:"job_id,omitempty"`
}

// ReadTold reads what a write's event says, if it's one.
func ReadTold(detail string) (Told, bool) {
	var t Told
	if !strings.HasPrefix(detail, "{") {
		return t, false
	}
	return t, json.Unmarshal([]byte(detail), &t) == nil
}

// writing is one write as it goes.
type writing struct {
	h        *Handler
	p        auth.Principal
	srv      *dockwellv1.Server
	session  *agentapi.Session
	record   *recorder
	op       op
	giveBack func()
}

// start finds who's writing and where, checks what's asked, and reaches the agent.
// Whatever stops it is on record.
func (h *Handler) start(ctx context.Context, serverID string, o op, path string, check func() error) (*writing, error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	srv, err := h.server(ctx, serverID)
	if err != nil {
		return nil, err
	}
	w := &writing{h: h, p: p, srv: srv, record: h.recorder(ctx, p, srv), op: o, giveBack: func() {}}
	if err := check(); err != nil {
		return nil, w.failedAt(ctx, path, authapi.Invalid(err))
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, w.failedAt(ctx, path, err)
	}
	if !session.ManagesFiles() {
		return nil, w.failedAt(ctx, path, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to %s files, so it can't here. Update the agent on %s, then try again", srv.GetName(), o.verb, srv.GetName()), "agent_outdated"))
	}
	w.session = session
	return w, nil
}

// target is what a passkey confirms for paths on this server.
func (w *writing) target(paths string) string { return w.srv.GetId() + ":" + paths }

// hold takes a passkey's proof for paths, if there's one, to give back where what it
// confirmed doesn't happen. A proof stands for its one write.
func (w *writing) hold(ctx context.Context, at, proof, paths string) error {
	if proof == "" {
		return nil
	}
	back, err := w.h.auth.HoldStepUp(ctx, w.record.client, w.p, proof, w.op.stepUp, w.target(paths))
	if err != nil {
		return w.failedAt(ctx, at, authapi.StepUpRefused(err))
	}
	w.giveBack = back
	return nil
}

// covered reports whether, with no proof given, the session's cover stands in for
// the passkey the agent just asked for, for paths, and puts that on the audit log
// (ADR 0075). The write then goes again, once, with those paths confirmed, as a
// proof would have it, and the agent still writes only where its walk ends there.
func (w *writing) covered(ctx context.Context, proof, paths string, r *agentv1.FileRefusal) bool {
	if proof != "" || paths == "" || r.GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		return false
	}
	_, err := w.h.auth.HoldStepUp(ctx, w.record.client, w.p, "", w.op.stepUp, w.target(paths))
	return err == nil
}

// failedAt puts on record a write that came to nothing, and why.
func (w *writing) failedAt(ctx context.Context, at string, err error) error {
	_, failed, _ := w.op.actions()
	w.h.note(ctx, w.record, failed, w.op.verb, at, w.srv, err)
	return err
}

// called is what came of asking the agent, short of what it said.
func (w *writing) called(ctx context.Context, at string, err error) error {
	if errors.Is(err, agentapi.ErrLost) || errors.Is(err, context.DeadlineExceeded) {
		err = w.h.rpcError(ctx, err)
		w.h.note(ctx, w.record, eventMayHave, w.op.verb, at, w.srv, err)
		return err
	}
	w.giveBack()
	return w.failedAt(ctx, at, w.h.rpcError(ctx, err))
}

// refused puts on record what the agent said in place of writing, and says it as the
// API does. paths is what a passkey would confirm.
func (w *writing) refused(ctx context.Context, at, paths string, r *agentv1.FileRefusal) error {
	at = cmp.Or(r.GetPath(), at)
	if r.GetKind() == agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		// Not a failure, as every such write starts here. What matters is whether a
		// passkey follows.
		w.giveBack()
		ctx := context.WithoutCancel(ctx)
		w.h.auth.AskedForStepUp(w.p, w.op.stepUp, w.target(paths), func() {
			detail := fmt.Sprintf("asked for a passkey to %s %s on %s, and none came", w.op.verb, at, w.srv.GetName())
			_, _, unconfirmed := w.op.actions()
			if _, err := w.record.record(unconfirmed, at, detail); err != nil {
				w.h.logger.WarnContext(ctx, "can't record a write that wasn't confirmed", slog.String("server", w.srv.GetId()), slog.String("error", err.Error()))
			}
		})
		return refusal(r)
	}
	if r.GetKind() == agentv1.FileRefusal_KIND_MAY_LAND {
		err := authapi.ReasonError(connect.CodeDeadlineExceeded, errors.New(r.GetMessage()), "file_may_land")
		w.h.note(ctx, w.record, eventMayHave, w.op.verb, at, w.srv, err)
		return err
	}
	w.giveBack()
	return w.failedAt(ctx, at, refusal(r))
}

// done puts on record a write that happened. It happened whether or not this can.
func (w *writing) done(ctx context.Context, at string, told Told) {
	detail, err := json.Marshal(told)
	if err == nil {
		done, _, _ := w.op.actions()
		_, err = w.record.record(done, at, string(detail))
	}
	if err != nil {
		w.h.logger.ErrorContext(ctx, "a file changed, and can't go on record", slog.String("server", w.srv.GetId()), slog.String("error", err.Error()))
	}
}

// checkWrite checks a path to write, as the agent will.
func checkWrite(path string) error {
	if err := files.CheckPath(path); err != nil {
		return err
	}
	return files.CheckName(path)
}

// MakeFile makes an empty file or a folder on a server.
func (h *Handler) MakeFile(ctx context.Context, req *connect.Request[dockwellv1.MakeFileRequest]) (*connect.Response[dockwellv1.MakeFileResponse], error) {
	path, kind, proof := req.Msg.GetPath(), req.Msg.GetType(), req.Msg.GetProof()
	w, err := h.start(ctx, req.Msg.GetServerId(), opMake, path, func() error {
		if kind != dockwellv1.FileType_FILE_TYPE_FILE && kind != dockwellv1.FileType_FILE_TYPE_DIRECTORY {
			return errors.New("say what to make: FILE_TYPE_FILE or FILE_TYPE_DIRECTORY")
		}
		return checkWrite(path)
	})
	if err != nil {
		return nil, err
	}
	if err := w.hold(ctx, path, proof, path); err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	confirmed := ""
	if proof != "" {
		confirmed = path
	}
	out, err := w.session.MakeFile(ctx, path, kind, confirmed)
	if err == nil && w.covered(ctx, proof, path, out.GetRefusal()) {
		out, err = w.session.MakeFile(ctx, path, kind, path)
	}
	if err != nil {
		return nil, w.called(ctx, path, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, w.refused(ctx, path, cmp.Or(r.GetPath(), path), r)
	}
	made := out.GetMade()
	w.done(ctx, made.GetPath(), Told{WayBackIn: made.GetWayBackIn()})
	return connect.NewResponse(made), nil
}

// MoveFile moves or renames a file or a folder on a server.
func (h *Handler) MoveFile(ctx context.Context, req *connect.Request[dockwellv1.MoveFileRequest]) (*connect.Response[dockwellv1.MoveFileResponse], error) {
	from, to, proof := req.Msg.GetFrom(), req.Msg.GetTo(), req.Msg.GetProof()
	w, err := h.start(ctx, req.Msg.GetServerId(), opMove, from, func() error {
		if err := checkWrite(from); err != nil {
			return err
		}
		return checkWrite(to)
	})
	if err != nil {
		return nil, err
	}
	paths := from + "\n" + to
	if err := w.hold(ctx, from, proof, paths); err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	confirmedFrom, confirmedTo := "", ""
	if proof != "" {
		confirmedFrom, confirmedTo = from, to
	}
	out, err := w.session.MoveFile(ctx, from, to, confirmedFrom, confirmedTo)
	if err == nil && w.covered(ctx, proof, paths, out.GetRefusal()) {
		out, err = w.session.MoveFile(ctx, from, to, from, to)
	}
	if err != nil {
		return nil, w.called(ctx, from, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, w.refused(ctx, from, paths, r)
	}
	moved := out.GetMoved()
	w.done(ctx, moved.GetFrom(), Told{To: moved.GetTo(), WayBackIn: moved.GetWayBackIn()})
	return connect.NewResponse(moved), nil
}

// DeleteFile deletes a file or a folder on a server, to Recently deleted or for good.
func (h *Handler) DeleteFile(ctx context.Context, req *connect.Request[dockwellv1.DeleteFileRequest]) (*connect.Response[dockwellv1.DeleteFileResponse], error) {
	path, forGood, proof := req.Msg.GetPath(), req.Msg.GetForGood(), req.Msg.GetProof()
	w, err := h.start(ctx, req.Msg.GetServerId(), opDelete, path, func() error { return checkWrite(path) })
	if err != nil {
		return nil, err
	}
	if err := w.hold(ctx, path, proof, path); err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	confirmed := ""
	if proof != "" {
		confirmed = path
	}
	out, err := w.session.DeleteFile(ctx, path, forGood, confirmed)
	if err == nil && w.covered(ctx, proof, path, out.GetRefusal()) {
		out, err = w.session.DeleteFile(ctx, path, forGood, path)
	}
	if err != nil {
		return nil, w.called(ctx, path, err)
	}
	if r := out.GetRefusal(); r != nil {
		if r.GetKind() == agentv1.FileRefusal_KIND_FOR_GOOD {
			// Not a failure: nothing happened, and the person decides.
			w.giveBack()
			return nil, refusal(r)
		}
		return nil, w.refused(ctx, path, cmp.Or(r.GetPath(), path), r)
	}
	deleted := out.GetDeleted()
	w.done(ctx, deleted.GetPath(), Told{WayBackIn: deleted.GetWayBackIn(), ForGood: deleted.GetDeleted() == nil})
	return connect.NewResponse(deleted), nil
}

// ListDeletedFiles says what's in a server's Recently deleted.
func (h *Handler) ListDeletedFiles(ctx context.Context, req *connect.Request[dockwellv1.ListDeletedFilesRequest]) (*connect.Response[dockwellv1.ListDeletedFilesResponse], error) {
	if _, err := person(ctx); err != nil {
		return nil, err
	}
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	if !session.ManagesFiles() {
		return connect.NewResponse(&dockwellv1.ListDeletedFilesResponse{}), nil // an agent that deletes nothing has nothing deleted
	}
	out, err := session.ListDeleted(ctx)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(out), nil
}

// RestoreFile puts back what was deleted on a server.
func (h *Handler) RestoreFile(ctx context.Context, req *connect.Request[dockwellv1.RestoreFileRequest]) (*connect.Response[dockwellv1.RestoreFileResponse], error) {
	id, proof := req.Msg.GetId(), req.Msg.GetProof()
	// Until the agent says what it is, the record goes by the ID.
	w, err := h.start(ctx, req.Msg.GetServerId(), opRestore, id, func() error { return checkDeletedID(id) })
	if err != nil {
		return nil, err
	}
	// Where it goes names it on record, and is what a proof is for, which the agent
	// checks against its own walk.
	at, err := w.deletedPath(ctx, id)
	if err != nil {
		return nil, err
	}
	confirmed := ""
	if proof != "" {
		confirmed = at
		if err := w.hold(ctx, cmp.Or(at, id), proof, at); err != nil {
			return nil, err
		}
	}
	dest := at
	at = cmp.Or(at, id)
	ctx = context.WithoutCancel(ctx)
	out, err := w.session.RestoreFile(ctx, id, confirmed)
	if err == nil && w.covered(ctx, proof, dest, out.GetRefusal()) {
		out, err = w.session.RestoreFile(ctx, id, dest)
	}
	if err != nil {
		return nil, w.called(ctx, at, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, w.refused(ctx, at, cmp.Or(r.GetPath(), at), r)
	}
	restored := out.GetRestored()
	w.done(ctx, restored.GetPath(), Told{WayBackIn: restored.GetWayBackIn()})
	return connect.NewResponse(restored), nil
}

// PurgeDeletedFile deletes for good what's in a server's Recently deleted. It needs no
// passkey, as what's there is out of reach already.
func (h *Handler) PurgeDeletedFile(ctx context.Context, req *connect.Request[dockwellv1.PurgeDeletedFileRequest]) (*connect.Response[dockwellv1.PurgeDeletedFileResponse], error) {
	id := req.Msg.GetId()
	w, err := h.start(ctx, req.Msg.GetServerId(), opDelete, id, func() error { return checkDeletedID(id) })
	if err != nil {
		return nil, err
	}
	at, err := w.deletedPath(ctx, id)
	if err != nil {
		return nil, err
	}
	at = cmp.Or(at, id)
	ctx = context.WithoutCancel(ctx)
	out, err := w.session.PurgeDeleted(ctx, id)
	if err != nil {
		return nil, w.called(ctx, at, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, w.refused(ctx, at, "", r)
	}
	purged := out.GetPurged()
	w.done(ctx, purged.GetPath(), Told{ForGood: true})
	return connect.NewResponse(&dockwellv1.PurgeDeletedFileResponse{Deleted: purged}), nil
}

// archives checks that the agent supports CompressFiles, ExtractArchive and
// multi-path downloads.
func (w *writing) archives(ctx context.Context, at string) error {
	if w.session.ArchivesFiles() {
		return nil
	}
	return w.failedAt(ctx, at, authapi.ReasonError(connect.CodeFailedPrecondition,
		fmt.Errorf("%s's agent is too old to %s archives on the server, so it can't here. Update the agent on %s, then try again", w.srv.GetName(), w.op.verb, w.srv.GetName()), "agent_outdated"))
}

// jobOwner identifies who owns a background archive job so reloads keep access
// while other users can't read or guess another user's job IDs.
func jobOwner(p auth.Principal) string { return p.User.ID }

func archiveJobKey(serverID, jobID string) string { return serverID + ":" + jobID }

// pendingArchive tracks a background compress or extract job from before the
// agent call is sent until its final Activity outcome is recorded.
type pendingArchive struct {
	w        *writing
	serverID string
	jobID    string
	owner    string
	recordID string
	path     string
	started  bool
}

const keepSettledArchives = 2 * time.Hour

func (h *Handler) pruneSettledArchivesLocked() {
	cutoff := h.now().Add(-keepSettledArchives)
	for k, at := range h.settledArchives {
		if at.Before(cutoff) {
			delete(h.settledArchives, k)
		}
	}
}

func (h *Handler) beginArchiveTracking(serverID, jobID, owner, recordID, at string, w *writing) {
	key := archiveJobKey(serverID, jobID)
	h.archiveMu.Lock()
	h.pendingArchives[key] = &pendingArchive{
		w:        w,
		serverID: serverID,
		jobID:    jobID,
		owner:    owner,
		recordID: recordID,
		path:     at,
	}
	h.archiveMu.Unlock()
}

func (h *Handler) cancelArchiveTracking(serverID, jobID string) {
	key := archiveJobKey(serverID, jobID)
	h.archiveMu.Lock()
	delete(h.pendingArchives, key)
	delete(h.endedEarly, key)
	h.archiveMu.Unlock()
}

// markArchiveUncertain leaves a job tracked after a lost stream so a later
// ArchiveJobEnded still lands on Activity if the job actually started.
func (h *Handler) markArchiveUncertain(serverID, jobID string) {
	key := archiveJobKey(serverID, jobID)
	h.archiveMu.Lock()
	if p, ok := h.pendingArchives[key]; ok {
		p.started = true
	}
	early := h.endedEarly[key]
	if early != nil {
		delete(h.endedEarly, key)
	}
	h.archiveMu.Unlock()
	if early != nil {
		h.settleArchiveJob(serverID, early.GetJob(), early.GetRecord())
	}
}

// finishArchiveStart records the job's started Activity row, marks it tracked,
// and immediately settles it if ArchiveJobEnded arrived before the call reply.
func (h *Handler) finishArchiveStart(ctx context.Context, serverID, mintedJobID, recordID string, job *dockwellv1.ArchiveJob, w *writing) {
	if job == nil || job.GetJobId() == "" {
		h.cancelArchiveTracking(serverID, mintedJobID)
		return
	}
	told := Told{
		JobID:     job.GetJobId(),
		WayBackIn: job.GetWayBackIn(),
		To:        job.GetUnpackedTo(),
		Paths:     job.GetSourcePaths(),
		PathCount: int(job.GetSourceCount()),
	}
	detail, err := json.Marshal(told)
	if err == nil {
		err = w.record.recordWithID(recordID, w.op.startedAction(), job.GetPath(), string(detail))
	}
	if err != nil {
		h.logger.ErrorContext(ctx, "an archive job started, and can't go on record", slog.String("server", serverID), slog.String("error", err.Error()))
	}

	mintedKey := archiveJobKey(serverID, mintedJobID)
	actualKey := archiveJobKey(serverID, job.GetJobId())
	h.archiveMu.Lock()
	p := h.pendingArchives[mintedKey]
	if actualKey != mintedKey {
		delete(h.pendingArchives, mintedKey)
	}
	if p == nil {
		p = &pendingArchive{
			w:        w,
			serverID: serverID,
			jobID:    job.GetJobId(),
			owner:    jobOwner(w.p),
			recordID: recordID,
		}
	}
	p.jobID = job.GetJobId()
	p.path = job.GetPath()
	p.started = true
	h.pendingArchives[actualKey] = p
	early := h.endedEarly[actualKey]
	if early != nil {
		delete(h.endedEarly, actualKey)
	}
	h.archiveMu.Unlock()

	if early != nil {
		h.settleArchiveJob(serverID, early.GetJob(), cmp.Or(early.GetRecord(), recordID))
		return
	}
	if job.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING {
		h.settleArchiveJob(serverID, job, recordID)
	}
}

func (h *Handler) archiveJobEnded(server string, ended *agentv1.ArchiveJobEnded) {
	job := ended.GetJob()
	if job == nil || job.GetJobId() == "" || job.GetState() == dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING {
		return
	}
	key := archiveJobKey(server, job.GetJobId())
	h.archiveMu.Lock()
	if p, ok := h.pendingArchives[key]; ok && !p.started {
		// The job finished before the start call returned and wrote its started row.
		// Buffer it so finishArchiveStart writes started first, then settles immediately.
		h.endedEarly[key] = ended
		h.archiveMu.Unlock()
		return
	}
	h.archiveMu.Unlock()
	h.settleArchiveJob(server, job, ended.GetRecord())
}

// settleArchiveJob records the final Activity event for a completed or failed
// background archive job once and only once, including after a control-plane
// restart by reading the persisted started event from the store.
func (h *Handler) settleArchiveJob(server string, job *dockwellv1.ArchiveJob, recordID string) {
	if job == nil || job.GetJobId() == "" || job.GetState() == dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING {
		return
	}
	key := archiveJobKey(server, job.GetJobId())
	h.archiveMu.Lock()
	h.pruneSettledArchivesLocked()
	if _, already := h.settledArchives[key]; already {
		h.archiveMu.Unlock()
		return
	}
	p, inMem := h.pendingArchives[key]
	if inMem {
		if !p.started {
			h.endedEarly[key] = &agentv1.ArchiveJobEnded{Job: job, Record: cmp.Or(recordID, p.recordID)}
			h.archiveMu.Unlock()
			return
		}
		delete(h.pendingArchives, key)
		delete(h.endedEarly, key)
		h.settledArchives[key] = h.now()
	}
	h.archiveMu.Unlock()

	ctx := context.Background()
	if inMem {
		h.recordArchiveOutcomeWithWriting(ctx, p.w, job)
		return
	}
	h.settleArchiveJobFromStore(ctx, server, key, job, recordID)
}

func (h *Handler) recordArchiveOutcomeWithWriting(ctx context.Context, w *writing, job *dockwellv1.ArchiveJob) {
	switch job.GetState() {
	case dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE:
		if job.GetKind() == dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_COMPRESS {
			w.done(ctx, job.GetPath(), Told{
				WayBackIn: job.GetWayBackIn(),
				Size:      cmp.Or(job.GetFile().GetSizeBytes(), job.GetBytes()),
				Paths:     job.GetSourcePaths(),
				PathCount: int(job.GetSourceCount()),
				JobID:     job.GetJobId(),
			})
		} else {
			w.done(ctx, job.GetPath(), Told{
				To:        job.GetUnpackedTo(),
				WayBackIn: job.GetWayBackIn(),
				JobID:     job.GetJobId(),
			})
		}
	case dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED:
		msg := cmp.Or(job.GetError(), "the archive job failed")
		_ = w.failedAt(ctx, job.GetPath(), errors.New(msg))
	case dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_UNSPECIFIED,
		dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING:
	}
}

func (h *Handler) settleArchiveJobFromStore(ctx context.Context, server, key string, job *dockwellv1.ArchiveJob, recordID string) {
	wantStarted := "file.extract_started"
	o := opExtract
	if job.GetKind() == dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_COMPRESS {
		wantStarted = "file.compress_started"
		o = opCompress
	}
	doneAction, failedAction, _ := o.actions()

	var started store.Event
	var found bool
	var alreadyRecorded bool
	err := h.db.View(ctx, func(tx *store.Tx) error {
		if recordID != "" {
			if ev, err := tx.GetEvent(ctx, recordID); err == nil {
				if ev.Target == server && ev.Action == wantStarted {
					if told, ok := ReadTold(ev.Detail); ok && told.JobID == job.GetJobId() {
						started, found = ev, true
					}
				}
			}
		}
		recent, err := tx.ListEvents(ctx, 200)
		if err != nil {
			return err
		}
		for i := range recent {
			ev := &recent[i]
			if ev.Target != server {
				continue
			}
			if ev.Action == doneAction || ev.Action == failedAction {
				if told, ok := ReadTold(ev.Detail); ok && told.JobID == job.GetJobId() {
					alreadyRecorded = true
					return nil
				}
			}
			if !found && ev.Action == wantStarted {
				if told, ok := ReadTold(ev.Detail); ok && told.JobID == job.GetJobId() {
					started, found = *ev, true
				}
			}
		}
		return nil
	})
	if err != nil || !found || alreadyRecorded {
		return
	}

	h.archiveMu.Lock()
	if _, already := h.settledArchives[key]; already {
		h.archiveMu.Unlock()
		return
	}
	h.settledArchives[key] = h.now()
	h.archiveMu.Unlock()

	srvName := server
	if srv, err := h.fleet.Get(server); err == nil {
		srvName = srv.GetName()
	}

	var action, detail string
	switch job.GetState() {
	case dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE:
		action = doneAction
		told := Told{
			WayBackIn: job.GetWayBackIn(),
			JobID:     job.GetJobId(),
		}
		if job.GetKind() == dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_COMPRESS {
			told.Size = cmp.Or(job.GetFile().GetSizeBytes(), job.GetBytes())
			told.Paths = job.GetSourcePaths()
			told.PathCount = int(job.GetSourceCount())
		} else {
			told.To = job.GetUnpackedTo()
		}
		raw, merr := json.Marshal(told)
		if merr != nil {
			return
		}
		detail = string(raw)
	case dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED:
		action = failedAction
		msg := cmp.Or(job.GetError(), "the archive job failed")
		detail = fmt.Sprintf("couldn't %s %s on %s: %s", o.verb, job.GetPath(), srvName, msg)
	default:
		return
	}
	if err := h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{
			ID:      ids.New(ids.Event),
			At:      h.now(),
			Actor:   started.Actor,
			Action:  action,
			Subject: files.Printable(job.GetPath()),
			Target:  server,
			Surface: started.Surface,
			Detail:  files.Printable(detail),
		})
	}); err != nil {
		h.logger.ErrorContext(ctx, "can't record how an archive job ended", slog.String("server", server), slog.String("error", err.Error()))
	}
}

// failLostArchiveJob records failure for a pending archive job that disappeared
// because its agent restarted.
func (h *Handler) failLostArchiveJob(server, jobID string) {
	key := archiveJobKey(server, jobID)
	h.archiveMu.Lock()
	p, ok := h.pendingArchives[key]
	if !ok || !p.started {
		h.archiveMu.Unlock()
		return
	}
	delete(h.pendingArchives, key)
	delete(h.endedEarly, key)
	h.settledArchives[key] = h.now()
	h.archiveMu.Unlock()
	_ = p.w.failedAt(context.Background(), p.path, errors.New("stopped when the agent restarted; it may be partly done"))
}

// reconcileArchiveJobs checks pending archive jobs when a server's agent
// connects: jobs the restarted agent no longer knows are settled as failed,
// while jobs that finished on the same agent before reconnecting settle cleanly.
func (h *Handler) reconcileArchiveJobs(server string) {
	h.archiveMu.Lock()
	var pending []*pendingArchive
	for _, p := range h.pendingArchives {
		if p.serverID == server && p.started {
			pending = append(pending, p)
		}
	}
	h.archiveMu.Unlock()
	if len(pending) == 0 {
		return
	}
	session, ok := h.agents.Session(server)
	if !ok || !session.ArchivesFiles() {
		return
	}
	ctx := context.Background()
	for _, p := range pending {
		out, err := session.GetArchiveJob(ctx, p.jobID, p.owner)
		if err != nil {
			continue
		}
		if out.GetRefusal().GetKind() == agentv1.FileRefusal_KIND_NOT_FOUND {
			h.failLostArchiveJob(server, p.jobID)
			continue
		}
		if job := out.GetJob(); job != nil && job.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING {
			h.settleArchiveJob(server, job, p.recordID)
		}
	}
}

// CompressFiles starts a background job that packs one or more files or folders
// into a new .zip archive on a server.
func (h *Handler) CompressFiles(ctx context.Context, req *connect.Request[dockwellv1.CompressFilesRequest]) (*connect.Response[dockwellv1.CompressFilesResponse], error) {
	paths, archivePath, proof := req.Msg.GetPaths(), req.Msg.GetArchivePath(), req.Msg.GetProof()
	w, err := h.start(ctx, req.Msg.GetServerId(), opCompress, archivePath, func() error {
		if err := files.CheckPaths(paths); err != nil {
			return err
		}
		return files.CheckZipArchivePath(archivePath)
	})
	if err != nil {
		return nil, err
	}
	if err := w.archives(ctx, archivePath); err != nil {
		return nil, err
	}
	if err := w.hold(ctx, archivePath, proof, archivePath); err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	confirmed := ""
	if proof != "" {
		confirmed = archivePath
	}
	owner := jobOwner(w.p)
	jobID := rand.Text()
	recordID := ids.New(ids.Event)
	h.beginArchiveTracking(w.srv.GetId(), jobID, owner, recordID, archivePath, w)
	out, err := w.session.CompressFiles(ctx, paths, archivePath, confirmed, owner, jobID, recordID)
	if err == nil && w.covered(ctx, proof, archivePath, out.GetRefusal()) {
		out, err = w.session.CompressFiles(ctx, paths, archivePath, archivePath, owner, jobID, recordID)
	}
	if err != nil {
		if errors.Is(err, agentapi.ErrLost) || errors.Is(err, context.DeadlineExceeded) {
			h.markArchiveUncertain(w.srv.GetId(), jobID)
		} else {
			h.cancelArchiveTracking(w.srv.GetId(), jobID)
		}
		return nil, w.called(ctx, archivePath, err)
	}
	if r := out.GetRefusal(); r != nil {
		h.cancelArchiveTracking(w.srv.GetId(), jobID)
		return nil, w.refused(ctx, archivePath, cmp.Or(r.GetPath(), archivePath), r)
	}
	job := out.GetJob()
	h.finishArchiveStart(ctx, w.srv.GetId(), jobID, recordID, job, w)
	return connect.NewResponse(&dockwellv1.CompressFilesResponse{Job: job}), nil
}

// ExtractArchive starts a background job that unpacks an existing .zip, .tar.gz
// or .tgz archive on a server into a new folder beside it.
func (h *Handler) ExtractArchive(ctx context.Context, req *connect.Request[dockwellv1.ExtractArchiveRequest]) (*connect.Response[dockwellv1.ExtractArchiveResponse], error) {
	archivePath, proof := req.Msg.GetPath(), req.Msg.GetProof()
	w, err := h.start(ctx, req.Msg.GetServerId(), opExtract, archivePath, func() error {
		if err := checkWrite(archivePath); err != nil {
			return err
		}
		if files.ArchiveKind(posixpath.Base(strings.TrimRight(archivePath, "/"))) == dockwellv1.Unpack_UNPACK_UNSPECIFIED {
			return errors.New("only .zip, .tar.gz and .tgz archives can be extracted")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := w.archives(ctx, archivePath); err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(archivePath, "/")
	dstPath := posixpath.Join(posixpath.Dir(trimmed), files.UnpackedName(posixpath.Base(trimmed)))
	stepUpTarget := archivePath + "\n" + dstPath
	if err := w.hold(ctx, archivePath, proof, stepUpTarget); err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	confirmedArchive, confirmedDst := "", ""
	if proof != "" {
		confirmedArchive, confirmedDst = archivePath, dstPath
	}
	owner := jobOwner(w.p)
	jobID := rand.Text()
	recordID := ids.New(ids.Event)
	h.beginArchiveTracking(w.srv.GetId(), jobID, owner, recordID, archivePath, w)
	out, err := w.session.ExtractArchive(ctx, archivePath, confirmedArchive, confirmedDst, owner, jobID, recordID)
	if r := out.GetRefusal(); err == nil && w.covered(ctx, proof, archivePath+"\n"+cmp.Or(r.GetPath(), dstPath), r) {
		out, err = w.session.ExtractArchive(ctx, archivePath, archivePath, cmp.Or(r.GetPath(), dstPath), owner, jobID, recordID)
	}
	if err != nil {
		if errors.Is(err, agentapi.ErrLost) || errors.Is(err, context.DeadlineExceeded) {
			h.markArchiveUncertain(w.srv.GetId(), jobID)
		} else {
			h.cancelArchiveTracking(w.srv.GetId(), jobID)
		}
		return nil, w.called(ctx, archivePath, err)
	}
	if r := out.GetRefusal(); r != nil {
		h.cancelArchiveTracking(w.srv.GetId(), jobID)
		return nil, w.refused(ctx, archivePath, archivePath+"\n"+cmp.Or(r.GetPath(), dstPath), r)
	}
	job := out.GetJob()
	h.finishArchiveStart(ctx, w.srv.GetId(), jobID, recordID, job, w)
	return connect.NewResponse(&dockwellv1.ExtractArchiveResponse{Job: job}), nil
}

// GetArchiveJob returns the current state of one background archive job.
func (h *Handler) GetArchiveJob(ctx context.Context, req *connect.Request[dockwellv1.GetArchiveJobRequest]) (*connect.Response[dockwellv1.GetArchiveJobResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	jobID := req.Msg.GetJobId()
	if err := checkDeletedID(jobID); err != nil {
		return nil, authapi.Invalid(fmt.Errorf("%q isn't an archive job ID", jobID))
	}
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	if !session.ArchivesFiles() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old for archive jobs. Update the agent on %s, then try again", srv.GetName(), srv.GetName()), "agent_outdated")
	}
	out, err := session.GetArchiveJob(ctx, jobID, jobOwner(p))
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		if r.GetKind() == agentv1.FileRefusal_KIND_NOT_FOUND {
			h.failLostArchiveJob(srv.GetId(), jobID)
		}
		return nil, refusal(r)
	}
	job := out.GetJob()
	h.settleArchiveJob(srv.GetId(), job, "")
	return connect.NewResponse(&dockwellv1.GetArchiveJobResponse{Job: job}), nil
}

// ListArchiveJobs lists active and recent archive jobs started by the caller.
func (h *Handler) ListArchiveJobs(ctx context.Context, req *connect.Request[dockwellv1.ListArchiveJobsRequest]) (*connect.Response[dockwellv1.ListArchiveJobsResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	if !session.ArchivesFiles() {
		return connect.NewResponse(&dockwellv1.ListArchiveJobsResponse{}), nil
	}
	out, err := session.ListArchiveJobs(ctx, jobOwner(p))
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	for _, job := range out.GetJobs() {
		h.settleArchiveJob(srv.GetId(), job, "")
	}
	return connect.NewResponse(&dockwellv1.ListArchiveJobsResponse{Jobs: out.GetJobs()}), nil
}

// deletedPath is where what's in Recently deleted as id was deleted from, or nothing
// for an ID that isn't there.
func (w *writing) deletedPath(ctx context.Context, id string) (string, error) {
	deleted, err := w.session.ListDeleted(ctx)
	if err != nil {
		return "", w.failedAt(ctx, id, w.h.rpcError(ctx, err))
	}
	for _, d := range deleted.GetDeleted() {
		if d.GetId() == id {
			return d.GetPath(), nil
		}
	}
	return "", nil
}

// checkDeletedID refuses what can't be an ID in Recently deleted.
func checkDeletedID(id string) error {
	if len(id) != 26 {
		return fmt.Errorf("%q isn't an ID in Recently deleted. List them with ListDeletedFiles", id)
	}
	for _, r := range id {
		if (r < 'A' || r > 'Z') && (r < '2' || r > '7') {
			return fmt.Errorf("%q isn't an ID in Recently deleted. List them with ListDeletedFiles", id)
		}
	}
	return nil
}
