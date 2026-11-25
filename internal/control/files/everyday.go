// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"strings"

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

var (
	opAccess = op{"change permissions or ownership on", auth.StepUpChangeFileAccess}
	opFetch  = op{"download to", auth.StepUpDownloadWayBackIn}
)

func everydayAvailable(s *agentapi.Session) error {
	if s.FilesRest() {
		return nil
	}
	return authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("update the agent to use the everyday Files tools"), "agent_outdated")
}

func (h *Handler) everyday(ctx context.Context, server string) (*agentapi.Session, error) {
	if _, err := person(ctx); err != nil {
		return nil, err
	}
	srv, err := h.server(ctx, server)
	if err != nil {
		return nil, err
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	return session, everydayAvailable(session)
}

// ListFileAccounts lists local users and groups on the server.
func (h *Handler) ListFileAccounts(ctx context.Context, req *connect.Request[dockwellv1.ListFileAccountsRequest]) (*connect.Response[dockwellv1.ListFileAccountsResponse], error) {
	s, err := h.everyday(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	out, err := s.ListFileAccounts(ctx)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, refusal(r)
	}
	return connect.NewResponse(out.GetResponse()), nil
}

// CalculateFolderSize counts logical file bytes below a folder, up to the scan bounds.
func (h *Handler) CalculateFolderSize(ctx context.Context, req *connect.Request[dockwellv1.CalculateFolderSizeRequest]) (*connect.Response[dockwellv1.CalculateFolderSizeResponse], error) {
	p := req.Msg.GetPath()
	if err := files.CheckPath(p); err != nil {
		return nil, authapi.Invalid(err)
	}
	s, err := h.everyday(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	out, err := s.CalculateFolderSize(ctx, p)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, refusal(r)
	}
	return connect.NewResponse(out.GetResponse()), nil
}

// SearchFiles finds matching paths and lines below a folder.
func (h *Handler) SearchFiles(ctx context.Context, req *connect.Request[dockwellv1.SearchFilesRequest]) (*connect.Response[dockwellv1.SearchFilesResponse], error) {
	p, q := req.Msg.GetPath(), req.Msg.GetQuery()
	if err := files.CheckPath(p); err != nil {
		return nil, authapi.Invalid(err)
	}
	if err := files.CheckSearchQuery(q); err != nil {
		return nil, authapi.Invalid(err)
	}
	s, err := h.everyday(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	out, err := s.SearchFiles(ctx, p, q, req.Msg.GetText())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, refusal(r)
	}
	return connect.NewResponse(out.GetResponse()), nil
}

// confirmTarget accepts the agent's previous challenge alongside a proof. The
// agent compares it against the path, identity and values it sees on the retry.
func (w *writing) confirmTarget(ctx context.Context, path, proof, target string) (string, error) {
	if proof == "" {
		return "", nil
	}
	p, ok := strings.CutPrefix(target, w.srv.GetId()+":")
	if !ok || len(p) > 8192 {
		return "", w.failedAt(ctx, path, authapi.Invalid(errors.New("send the confirmation target returned with the passkey request")))
	}
	return p, w.hold(ctx, path, proof, p)
}

func (w *writing) everydayRefusal(ctx context.Context, path, target string, r *agentv1.FileRefusal, action dockwellv1.StepUpAction) error {
	err := w.refused(ctx, path, target, r)
	var e *connect.Error
	if r.GetKind() == agentv1.FileRefusal_KIND_NEEDS_STEP_UP && errors.As(err, &e) {
		if detail, detailErr := connect.NewErrorDetail(&dockwellv1.StepUpNeeded{Action: action, Target: w.target(target)}); detailErr == nil {
			e.AddDetail(detail)
		}
	}
	return err
}

// ChangeFileAccess changes permissions, owner or group on a file or folder.
func (h *Handler) ChangeFileAccess(ctx context.Context, req *connect.Request[dockwellv1.ChangeFileAccessRequest]) (*connect.Response[dockwellv1.ChangeFileAccessResponse], error) {
	m := req.Msg
	p, change := m.GetPath(), m.GetChange()
	w, err := h.start(ctx, m.GetServerId(), opAccess, p, func() error { return files.CheckAccessChange(p, change) })
	if err != nil {
		return nil, err
	}
	if err := everydayAvailable(w.session); err != nil {
		return nil, w.failedAt(ctx, p, err)
	}
	confirmed, err := w.confirmTarget(ctx, p, m.GetProof(), m.GetConfirmationTarget())
	if err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	call := &agentv1.ChangeFileAccessCall{Path: p, Change: change, Confirmed: confirmed}
	out, err := w.session.ChangeFileAccess(ctx, call)
	if err == nil && w.covered(ctx, m.GetProof(), out.GetConfirmation(), out.GetRefusal()) {
		call.Confirmed = out.GetConfirmation()
		out, err = w.session.ChangeFileAccess(ctx, call)
	}
	if err != nil {
		return nil, w.called(ctx, p, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, w.everydayRefusal(ctx, p, out.GetConfirmation(), r, dockwellv1.StepUpAction_STEP_UP_ACTION_CHANGE_FILE_ACCESS)
	}
	result := out.GetResponse()
	recorded := change
	if len(change.GetRestore()) != 0 {
		recorded = nil
	}
	w.doneWithUndo(ctx, result.GetPath(), Told{
		WayBackIn: result.GetWayBackIn(), Access: recorded,
		Changed: result.GetChanged(), Skipped: result.GetSkipped(), Stopped: result.GetStopped(),
	}, result.GetUndo())
	return connect.NewResponse(result), nil
}

// Download jobs belong to the initiating session, like uploads. Their URLs
// stay out of the job record and Activity.
func downloadOwner(p auth.Principal) string { return uploader(p) }

// DownloadFileFromURL starts a background download from a URL to a new file.
func (h *Handler) DownloadFileFromURL(ctx context.Context, req *connect.Request[dockwellv1.DownloadFileFromURLRequest]) (*connect.Response[dockwellv1.DownloadFileFromURLResponse], error) {
	m := req.Msg
	dest, invalid := files.DownloadDestination(m.GetFolder(), m.GetUrl(), m.GetName())
	at := cmp.Or(dest, m.GetFolder())
	w, err := h.start(ctx, m.GetServerId(), opFetch, at, func() error { return invalid })
	if err != nil {
		return nil, err
	}
	if err := everydayAvailable(w.session); err != nil {
		return nil, w.failedAt(ctx, at, err)
	}
	confirmed, err := w.confirmTarget(ctx, at, m.GetProof(), m.GetConfirmationTarget())
	if err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	w.sourceOrigin = files.DownloadOrigin(m.GetUrl())
	server, jobID, recordID, owner := w.srv.GetId(), rand.Text(), ids.New(ids.Event), downloadOwner(w.p)
	call := &agentv1.DownloadFileFromURLCall{Path: dest, Url: m.GetUrl(), Confirmed: confirmed, JobId: jobID, Record: recordID, Owner: owner}
	h.beginArchiveTracking(server, jobID, owner, recordID, dest, w)
	out, err := w.session.DownloadFileFromURL(ctx, call)
	if err == nil && w.covered(ctx, m.GetProof(), out.GetConfirmation(), out.GetRefusal()) {
		call.Confirmed = out.GetConfirmation()
		out, err = w.session.DownloadFileFromURL(ctx, call)
	}
	if err != nil {
		if errors.Is(err, agentapi.ErrLost) || errors.Is(err, context.DeadlineExceeded) {
			h.markArchiveUncertain(server, jobID)
		} else {
			h.cancelArchiveTracking(server, jobID)
		}
		return nil, w.called(ctx, dest, err)
	}
	if r := out.GetRefusal(); r != nil {
		h.cancelArchiveTracking(server, jobID)
		return nil, w.everydayRefusal(ctx, dest, out.GetConfirmation(), r, dockwellv1.StepUpAction_STEP_UP_ACTION_DOWNLOAD_WAY_BACK_IN)
	}
	job := out.GetJob()
	h.finishArchiveStart(ctx, server, jobID, recordID, job, w)
	return connect.NewResponse(&dockwellv1.DownloadFileFromURLResponse{Job: job}), nil
}

// CancelFileDownload stops a background download the session started.
func (h *Handler) CancelFileDownload(ctx context.Context, req *connect.Request[dockwellv1.CancelFileDownloadRequest]) (*connect.Response[dockwellv1.CancelFileDownloadResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetJobId()
	if err := checkDeletedID(id); err != nil {
		return nil, authapi.Invalid(err)
	}
	s, err := h.everyday(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	out, err := s.CancelFileDownload(ctx, id, downloadOwner(p))
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, refusal(r)
	}
	return connect.NewResponse(&dockwellv1.CancelFileDownloadResponse{Job: out.GetJob()}), nil
}

// ListPinnedFolders lists personal pins for a server.
func (h *Handler) ListPinnedFolders(ctx context.Context, req *connect.Request[dockwellv1.ListPinnedFoldersRequest]) (*connect.Response[dockwellv1.ListPinnedFoldersResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	server := req.Msg.GetServerId()
	if _, err := h.server(ctx, server); err != nil {
		return nil, err
	}
	out := &dockwellv1.ListPinnedFoldersResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		out.Paths, err = tx.PinnedFolders(ctx, p.User.ID, server)
		return err
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(out), nil
}

// PinFolder keeps a folder in the signed-in user's places.
func (h *Handler) PinFolder(ctx context.Context, req *connect.Request[dockwellv1.PinFolderRequest]) (*connect.Response[dockwellv1.PinFolderResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	server := req.Msg.GetServerId()
	// ListFiles resolves the folder and applies the ordinary Files read guard.
	list, err := h.ListFiles(ctx, connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: server, Path: req.Msg.GetPath(), Match: "￿"}))
	if err != nil {
		return nil, err
	}
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.PinFolder(ctx, p.User.ID, server, list.Msg.GetPath())
	})
	if errors.Is(err, store.ErrPinnedFoldersFull) {
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&dockwellv1.PinFolderResponse{}), nil
}

// UnpinFolder removes a personal pin, including one whose folder has gone.
func (h *Handler) UnpinFolder(ctx context.Context, req *connect.Request[dockwellv1.UnpinFolderRequest]) (*connect.Response[dockwellv1.UnpinFolderResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	server, path := req.Msg.GetServerId(), req.Msg.GetPath()
	if _, err := h.server(ctx, server); err != nil {
		return nil, err
	}
	if err := files.CheckPath(path); err != nil {
		return nil, authapi.Invalid(err)
	}
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.UnpinFolder(ctx, p.User.ID, server, path)
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&dockwellv1.UnpinFolderResponse{}), nil
}
