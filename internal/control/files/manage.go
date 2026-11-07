// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

// What a person makes, moves, deletes and puts back on a server, and what's in its
// Recently deleted (ADR 0045). Each is one request, as a save is: the agent decides
// what's refused and what's a way back in, and each goes on record, never what a file
// holds.

// op is one of these writes, as the audit log and a passkey know it.
type op struct {
	verb        string // as a sentence says it, such as "move"
	done        string // its event once it happened
	failed      string // its event when it didn't
	unconfirmed string // its event when it asked for a passkey and none came
	stepUp      auth.StepUpAction
}

var (
	opMake    = op{"make", "file.make", "file.make_failed", "file.make_unconfirmed", auth.StepUpMakeWayBackIn}
	opMove    = op{"move", "file.move", "file.move_failed", "file.move_unconfirmed", auth.StepUpMoveWayBackIn}
	opDelete  = op{"delete", "file.delete", "file.delete_failed", "file.delete_unconfirmed", auth.StepUpDeleteWayBackIn}
	opRestore = op{"put back", "file.restore", "file.restore_failed", "file.restore_unconfirmed", auth.StepUpRestoreWayBackIn}
)

// eventMayHave is a write whose call ended as it acted, so it may yet happen.
const eventMayHave = "file.change_unknown"

// Told is what the event of a write says beside its path, for the timeline: where it
// went, and whether it took a passkey or can't be put back.
type Told struct {
	To        string `json:"to,omitempty"`
	WayBackIn bool   `json:"way_back_in,omitempty"`
	ForGood   bool   `json:"for_good,omitempty"`
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
	record   func(action, path, detail string) (string, error)
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
	back, err := w.h.auth.HoldStepUp(w.p, proof, w.op.stepUp, w.target(paths))
	if err != nil {
		return w.failedAt(ctx, at, authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp))
	}
	w.giveBack = back
	return nil
}

// failedAt puts on record a write that came to nothing, and why.
func (w *writing) failedAt(ctx context.Context, at string, err error) error {
	w.h.note(ctx, w.record, w.op.failed, w.op.verb, at, w.srv, err)
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
			if _, err := w.record(w.op.unconfirmed, at, detail); err != nil {
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
		_, err = w.record(w.op.done, at, string(detail))
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
	at = cmp.Or(at, id)
	ctx = context.WithoutCancel(ctx)
	out, err := w.session.RestoreFile(ctx, id, confirmed)
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
