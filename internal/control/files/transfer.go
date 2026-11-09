// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/api/httpbody"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

// Uploads and downloads (ADR 0045). Each part goes on as the next side takes it, so
// the control plane holds a part of a file, never the file. An upload is on record
// once it's in place, and a download before anything of it goes out.

var opUpload = op{"upload", auth.StepUpMakeWayBackIn}

// eventDownload is a file or a folder downloaded, as eventRead is a file read.
const eventDownload = "file.download"

// SizeHeader says how large a file being downloaded is, before any of it, so a
// browser can say how far it got. A folder's archive is made as it goes, so it has
// none.
const SizeHeader = "Dockwell-File-Size"

// uploader names who an upload is for, to the agent: the user and the session. An
// upload is its session's, as a step-up is, so a second session, even the same
// person's, can't fill or finish one it found the ID of.
func uploader(p auth.Principal) string { return p.User.ID + "/" + p.Session.ID }

// transfers checks the agent can upload and download.
func (w *writing) transfers(ctx context.Context, at string) error {
	if w.session.TransfersFiles() {
		return nil
	}
	return w.failedAt(ctx, at, authapi.ReasonError(connect.CodeFailedPrecondition,
		fmt.Errorf("%s's agent is too old to upload or download files, so it can't here. Update the agent on %s, then try again", w.srv.GetName(), w.srv.GetName()), "agent_outdated"))
}

// BeginUpload starts an upload, or says how far one got.
func (h *Handler) BeginUpload(ctx context.Context, req *connect.Request[dockwellv1.BeginUploadRequest]) (*connect.Response[dockwellv1.BeginUploadResponse], error) {
	path, size, unpack, id, proof := req.Msg.GetPath(), req.Msg.GetSizeBytes(), req.Msg.GetUnpack(), req.Msg.GetUploadId(), req.Msg.GetProof()
	at := cmp.Or(path, id)
	w, err := h.start(ctx, req.Msg.GetServerId(), opUpload, at, func() error {
		if id != "" {
			return checkUploadID(id)
		}
		if _, ok := dockwellv1.Unpack_name[int32(unpack)]; !ok {
			return fmt.Errorf("unpack can't be %v. Leave it out, or say UNPACK_ZIP or UNPACK_TAR_GZ", unpack)
		}
		if size > files.MaxUploadBytes {
			return errors.New("that file is larger than the 16 GB an upload can be. Copy it over SSH")
		}
		return checkWrite(path)
	})
	if err != nil {
		return nil, err
	}
	if err := w.transfers(ctx, at); err != nil {
		return nil, err
	}
	if id != "" {
		out, err := w.session.BeginUpload(ctx, "", 0, 0, id, "", uploader(w.p))
		if err != nil {
			return nil, h.rpcError(ctx, err)
		}
		if r := out.GetRefusal(); r != nil {
			return nil, refusal(r)
		}
		return connect.NewResponse(&dockwellv1.BeginUploadResponse{Upload: out.GetUpload()}), nil
	}
	if err := w.hold(ctx, path, proof, path); err != nil {
		return nil, err
	}
	confirmed := ""
	if proof != "" {
		confirmed = path
	}
	out, err := w.session.BeginUpload(ctx, path, size, unpack, "", confirmed, uploader(w.p))
	if err != nil {
		return nil, w.called(ctx, path, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, w.refused(ctx, path, cmp.Or(r.GetPath(), path), r)
	}
	return connect.NewResponse(&dockwellv1.BeginUploadResponse{Upload: out.GetUpload()}), nil
}

// UploadFile writes an upload's bytes as they come, from where it got.
func (h *Handler) UploadFile(ctx context.Context, stream *connect.ClientStream[dockwellv1.UploadFileRequest]) (*connect.Response[dockwellv1.UploadFileResponse], error) {
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, authapi.Invalid(errors.New("say which upload this is, and from where"))
	}
	first := stream.Msg()
	id := first.GetUploadId()
	w, err := h.start(ctx, first.GetServerId(), opUpload, id, func() error { return checkUploadID(id) })
	if err != nil {
		return nil, err
	}
	if err := w.transfers(ctx, id); err != nil {
		return nil, err
	}
	// Each part waits until the agent takes it, and none waits once the agent is done.
	// The handler returns once the receiving does, so nothing reads the request after.
	ctx, stop := context.WithCancel(ctx)
	parts := make(chan []byte)
	received := make(chan struct{})
	defer func() {
		stop()
		<-received
	}()
	go func() {
		defer close(received)
		defer close(parts)
		give := func(data []byte) bool {
			if len(data) == 0 {
				return true
			}
			select {
			case parts <- data:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !give(first.GetContent().GetData()) {
			return
		}
		for stream.Receive() {
			if !give(stream.Msg().GetContent().GetData()) {
				return
			}
		}
	}()
	out, err := w.session.WriteUpload(ctx, id, uploader(w.p), first.GetOffset(), parts)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, refusal(r)
	}
	return connect.NewResponse(&dockwellv1.UploadFileResponse{Upload: out.GetUpload()}), nil
}

// FinishUpload puts an upload in place, and unpacks it if it was to be.
func (h *Handler) FinishUpload(ctx context.Context, req *connect.Request[dockwellv1.FinishUploadRequest]) (*connect.Response[dockwellv1.FinishUploadResponse], error) {
	id := req.Msg.GetUploadId()
	w, err := h.start(ctx, req.Msg.GetServerId(), opUpload, id, func() error { return checkUploadID(id) })
	if err != nil {
		return nil, err
	}
	if err := w.transfers(ctx, id); err != nil {
		return nil, err
	}
	// Where it goes names it on record.
	at := id
	if state, err := w.session.BeginUpload(ctx, "", 0, 0, id, "", uploader(w.p)); err == nil && state.GetUpload() != nil {
		at = state.GetUpload().GetPath()
	}
	ctx = context.WithoutCancel(ctx)
	out, err := w.session.FinishUpload(ctx, id, uploader(w.p))
	if err != nil {
		return nil, w.called(ctx, at, err)
	}
	finished := out.GetFinished()
	if finished != nil {
		w.done(ctx, finished.GetPath(), Told{WayBackIn: finished.GetWayBackIn(), Size: finished.GetFile().GetSizeBytes(), To: finished.GetUnpackedTo()})
	}
	if r := out.GetRefusal(); r != nil {
		if finished != nil {
			// It's in place, and wasn't unpacked: a person hears both.
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New(r.GetMessage()), "unpack_refused")
		}
		return nil, w.refused(ctx, at, cmp.Or(r.GetPath(), at), r)
	}
	return connect.NewResponse(finished), nil
}

// CancelUpload stops an upload and removes what it had. Nothing landed, so nothing
// goes on record.
func (h *Handler) CancelUpload(ctx context.Context, req *connect.Request[dockwellv1.CancelUploadRequest]) (*connect.Response[dockwellv1.CancelUploadResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetUploadId()
	if err := checkUploadID(id); err != nil {
		return nil, authapi.Invalid(err)
	}
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	if !session.TransfersFiles() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to upload files, so it has no upload to stop. Update the agent on %s", srv.GetName(), srv.GetName()), "agent_outdated")
	}
	out, err := session.CancelUpload(ctx, id, uploader(p))
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		return nil, refusal(r)
	}
	return connect.NewResponse(&dockwellv1.CancelUploadResponse{}), nil
}

// DownloadFile sends a file, or a folder as a zip archive, as the agent reads it.
// It's on record before any of it goes out, and what can't go on record doesn't go.
func (h *Handler) DownloadFile(ctx context.Context, req *connect.Request[dockwellv1.DownloadFileRequest], stream *connect.ServerStream[dockwellv1.DownloadFileResponse]) error {
	p, err := person(ctx)
	if err != nil {
		return err
	}
	path := req.Msg.GetPath()
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return err
	}
	record := h.recorder(ctx, p, srv)
	failed := func(err error) error {
		h.note(ctx, record, eventRefuse, "download", path, srv, err)
		return err
	}
	if err := files.CheckPath(path); err != nil {
		return failed(authapi.Invalid(err))
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return failed(err)
	}
	if !session.TransfersFiles() {
		return failed(authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to download files, so it can't here. Update the agent on %s, then try again", srv.GetName(), srv.GetName()), "agent_outdated"))
	}
	contentType, started := "application/octet-stream", false
	each := func(m *agentv1.SendFileRequest) error {
		if !started {
			started = true
			path = cmp.Or(m.GetPath(), path)
			detail := fmt.Sprintf(`{"size":%d}`, m.GetSizeBytes())
			if _, err := record.record(eventDownload, path, detail); err != nil {
				return h.rpcError(ctx, err)
			}
			if m.GetArchive() {
				contentType = "application/zip"
			}
			header := stream.ResponseHeader()
			header.Set("Content-Disposition", attachment(m.GetName()))
			header.Set("X-Content-Type-Options", "nosniff")
			if !m.GetArchive() {
				header.Set(SizeHeader, strconv.FormatUint(m.GetSizeBytes(), 10))
			}
		}
		if len(m.GetData()) == 0 {
			return nil
		}
		return stream.Send(&dockwellv1.DownloadFileResponse{Body: &httpbody.HttpBody{ContentType: contentType, Data: m.GetData()}})
	}
	out, err := session.Download(ctx, path, each)
	if err != nil {
		return failed(h.rpcError(ctx, err))
	}
	if r := out.GetRefusal(); r != nil {
		// Before it started, nothing went out. After, what went is cut short, and says so.
		return failed(refusal(r))
	}
	if !started {
		h.logger.WarnContext(ctx, "a download ended with nothing sent", slog.String("server", srv.GetId()))
	}
	return nil
}

// attachment is a Content-Disposition that saves a file as name, with a plain
// fallback for what can't read the encoded one.
func attachment(name string) string {
	plain := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename=%q; filename*=UTF-8''%s`, plain, url.PathEscape(name))
}

// checkUploadID checks an upload's ID is one BeginUpload could have given.
func checkUploadID(id string) error {
	if checkDeletedID(id) != nil {
		return fmt.Errorf("%q isn't an upload's ID. BeginUpload gives one", id)
	}
	return nil
}
