// SPDX-License-Identifier: AGPL-3.0-only

// Package files serves dockwell.v1.FileService: a person browsing, reading and
// saving the files on a server, through its agent, which decides what's refused.
// Nothing here is a revision: a file on a server isn't managed state, so a save
// writes the server and the audit log, and never reaches internal/revision. See
// docs/adr/0032-files-on-a-server.md and docs/security/files.md.
package files

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Audit events, each naming the file as its subject and the server as its target.
const (
	eventRead   = "file.read"
	eventRefuse = "file.refuse"
	// eventSaving goes on record before a save, and eventSave after it: what
	// can't go on record isn't written, and what may have failed isn't on the
	// timeline as saved. eventSaveUnknown is a save nobody can yet say the end of:
	// its call ended with the file perhaps written, and the agent says how it
	// came out if it can, which is eventSave or eventSaveFailed.
	eventSaving      = "file.saving"
	eventSave        = "file.save"
	eventSaveFailed  = "file.save_failed"
	eventSaveUnknown = "file.save_unknown"
	// eventSaveWayBackIn is eventSave of a file that lets someone back onto the
	// server, which took a passkey, and reads as a warning on Activity.
	eventSaveWayBackIn = "file.save_way_back_in"
)

// Handler implements FileService.
type Handler struct {
	fleet  *fleet.Hub
	agents *agentapi.Agents
	auth   *auth.Service
	db     *store.DB
	logger *slog.Logger
	now    func() time.Time
}

var _ dockwellv1connect.FileServiceHandler = (*Handler)(nil)

// New returns a handler that reaches servers through agents. people checks the
// passkey a way back onto a server takes.
func New(hub *fleet.Hub, agents *agentapi.Agents, people *auth.Service, db *store.DB, logger *slog.Logger) *Handler {
	h := &Handler{fleet: hub, agents: agents, auth: people, db: db, logger: logger, now: time.Now}
	agents.OnSaveEnded(h.saveEnded)
	return h
}

// person is who is asking. The interceptor refuses tokens already, as these
// methods need a session; this holds if it didn't.
func person(ctx context.Context) (auth.Principal, error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return auth.Principal{}, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	if p.Token != nil {
		return auth.Principal{}, authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, authapi.ReasonNeedsSession)
	}
	return p, nil
}

// server finds a server by its ID.
func (h *Handler) server(ctx context.Context, serverID string) (*dockwellv1.Server, error) {
	srv, err := h.fleet.Get(serverID)
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", serverID), "not_found")
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return srv, nil
}

// session finds a server's agent. One moving to a new stream is waited for, as it
// does that each time its certificate is renewed.
func (h *Handler) session(ctx context.Context, srv *dockwellv1.Server) (*agentapi.Session, error) {
	session, ok := h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't reach its files. Try again once it reconnects", srv.GetName()), "server_offline")
	}
	return session, nil
}

// ListFiles returns what a directory on a server holds. A listing that works isn't
// on record: it returns names, and it runs on every click. A refused one is, as
// someone looking for what isn't theirs mostly finds nothing. One that can't reach
// its server isn't, as the panel asks again by itself until it does.
func (h *Handler) ListFiles(ctx context.Context, req *connect.Request[dockwellv1.ListFilesRequest]) (*connect.Response[dockwellv1.ListFilesResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	path := req.Msg.GetPath()
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	record := h.recorder(ctx, p, srv)
	if err := files.CheckPath(path); err != nil {
		h.note(ctx, record, eventRefuse, "list", path, srv, err)
		return nil, authapi.Invalid(err)
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	out, err := session.ListFiles(ctx, path)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r := out.GetRefusal(); r != nil {
		err := refusal(r)
		h.note(ctx, record, eventRefuse, "list", path, srv, err)
		return nil, err
	}
	return connect.NewResponse(out.GetListing()), nil
}

// ReadFile returns a file on a server. The read goes on record first, and what
// can't go on record doesn't go out, as with a secret's reveal. A read that doesn't
// come to anything is on record too, however it failed.
func (h *Handler) ReadFile(ctx context.Context, req *connect.Request[dockwellv1.ReadFileRequest]) (*connect.Response[dockwellv1.ReadFileResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	path := req.Msg.GetPath()
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	record := h.recorder(ctx, p, srv)
	failed := func(err error) error {
		h.note(ctx, record, eventRefuse, "read", path, srv, err)
		return err
	}
	if err := files.CheckPath(path); err != nil {
		return nil, failed(authapi.Invalid(err))
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, failed(err)
	}
	out, err := session.ReadFile(ctx, path)
	if err != nil {
		return nil, failed(h.rpcError(ctx, err))
	}
	if r := out.GetRefusal(); r != nil {
		return nil, failed(refusal(r))
	}
	read := out.GetFile().GetPath()
	if _, err := record(eventRead, read, fmt.Sprintf("read %s on %s", read, srv.GetName())); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(out.GetFile()), nil
}

// SaveFile replaces a file's text on a server, if it is still as it was read. It
// isn't a revision: see the package's doc. It goes to its end whether or not the
// client stays, so what lands on the disk lands on record too.
func (h *Handler) SaveFile(ctx context.Context, req *connect.Request[dockwellv1.SaveFileRequest]) (*connect.Response[dockwellv1.SaveFileResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	path, content, version := req.Msg.GetPath(), req.Msg.GetContent(), req.Msg.GetVersion()
	srv, err := h.server(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	record := h.recorder(ctx, p, srv)
	failed := func(err error) error {
		h.note(ctx, record, eventSaveFailed, "save", path, srv, err)
		return err
	}
	switch {
	case len(content) > files.MaxFileBytes:
		return nil, failed(authapi.Invalid(fmt.Errorf("that text is %d bytes, and a file saves at %d at most. Connect to the server over SSH to write more", len(content), files.MaxFileBytes)))
	case version == "":
		return nil, failed(authapi.Invalid(errors.New("say which version of the file this replaces. Read it with ReadFile, and pass its version")))
	}
	if err := files.CheckPath(path); err != nil {
		return nil, failed(authapi.Invalid(err))
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, failed(err)
	}
	// A proof is for where a read said the path leads, and the agent writes only if
	// its own walk ends there. It says when a save needs one, as only it can tell.
	confirmed := req.Msg.GetConfirmedPath()
	if proof := req.Msg.GetProof(); proof != "" {
		if err := h.auth.UseStepUp(p, proof, auth.StepUpSaveWayBackIn, srv.GetId()+":"+confirmed); err != nil {
			return nil, failed(authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp))
		}
	} else {
		confirmed = ""
	}
	ctx = context.WithoutCancel(ctx)
	saving, err := record(eventSaving, path, fmt.Sprintf("saving %s on %s", path, srv.GetName()))
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	// unknown puts on record a save that may have landed, under where it got to.
	unknown := func(where string, err error) error {
		h.note(ctx, record, eventSaveUnknown, "save", where, srv, err)
		return err
	}
	out, err := session.SaveFile(ctx, path, content, version, saving, confirmed)
	if err != nil {
		if errors.Is(err, resource.ErrLost) || errors.Is(err, context.DeadlineExceeded) {
			return nil, unknown(path, h.rpcError(ctx, err))
		}
		return nil, failed(h.rpcError(ctx, err))
	}
	if r := out.GetRefusal(); r != nil {
		where := cmp.Or(r.GetPath(), path)
		if r.GetKind() == agentv1.FileRefusal_KIND_MAY_LAND {
			return nil, unknown(where, refusal(r))
		}
		if r.GetKind() == agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
			return nil, refusal(r) // not a failure: every such save starts here
		}
		err := refusal(r)
		h.note(ctx, record, eventSaveFailed, "save", where, srv, err)
		return nil, err
	}
	saved := out.GetSaved()
	if _, err := record(savedAs(saved.GetWayBackIn()), saved.GetPath(), fmt.Sprintf("saved %s on %s, %d bytes", saved.GetPath(), srv.GetName(), len(content))); err != nil {
		// It's on the disk, and eventSaving says so might be: the person hears it saved.
		h.logger.ErrorContext(ctx, "saved a file, and can't record that it saved", slog.String("server", srv.GetId()), slog.String("error", err.Error()))
	}
	return connect.NewResponse(saved), nil
}

// savedAs is what a save that landed goes on record as.
func savedAs(wayBackIn bool) string {
	if wayBackIn {
		return eventSaveWayBackIn
	}
	return eventSave
}

// saveEnded puts on record how a save came out, once the agent can say, for the
// person whose save it was. An agent speaks for its own server and nothing more, so
// it has to name a save on that server.
func (h *Handler) saveEnded(server string, ended *agentv1.SaveEnded) {
	ctx := context.Background()
	refuse := func(why string) {
		h.logger.WarnContext(ctx, "an agent said how a save ended, and it can't go on record", slog.String("why", why), slog.String("server", server), slog.String("record", ended.GetRecord()))
	}
	var saving store.Event
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		saving, err = tx.GetEvent(ctx, ended.GetRecord())
		return err
	})
	if err != nil {
		refuse("there's no such save")
		return
	}
	if saving.Action != eventSaving || saving.Target != server {
		refuse("that's not a save on this server")
		return
	}
	name := server
	if srv, err := h.fleet.Get(server); err == nil {
		name = srv.GetName()
	}
	action, detail := eventSaveFailed, fmt.Sprintf("couldn't save %s on %s: %s", ended.GetPath(), name, ended.GetReason())
	if ended.GetLanded() {
		action, detail = savedAs(ended.GetWayBackIn()), fmt.Sprintf("saved %s on %s, after it stopped answering", ended.GetPath(), name)
	}
	if err := h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: h.now(), Actor: saving.Actor, Action: action, Subject: files.Printable(ended.GetPath()), Target: server,
			Surface: saving.Surface, Detail: files.Printable(detail),
		})
	}); err != nil {
		h.logger.ErrorContext(ctx, "can't record how a save ended", slog.String("server", server), slog.String("error", err.Error()))
	}
}

// recorder writes a server's audit events for what p does to one of its files, by
// a path that's fit to store, as a name can hold anything but a slash, and says what
// each one is called. It writes whether or not the caller stays.
func (h *Handler) recorder(ctx context.Context, p auth.Principal, srv *dockwellv1.Server) func(action, path, detail string) (string, error) {
	client, _ := authapi.ClientFrom(ctx)
	ctx = context.WithoutCancel(ctx)
	return func(action, path, detail string) (string, error) {
		event := id.New(id.Event)
		return event, h.db.Update(ctx, func(tx *store.Tx) error {
			return tx.RecordEvent(ctx, store.Event{
				ID: event, At: h.now(), Actor: p.Actor(), Action: action, Subject: files.Printable(path), Target: srv.GetId(),
				Surface: client.Surface(p), Detail: files.Printable(detail),
			})
		})
	}
}

// note puts on record something done to a file that came to nothing, and why. A
// refusal stands whether or not this does. A caller that left isn't a refusal.
func (h *Handler) note(ctx context.Context, record func(action, path, detail string) (string, error), event, doing, path string, srv *dockwellv1.Server, why error) {
	if errors.Is(why, context.Canceled) || connect.CodeOf(why) == connect.CodeCanceled {
		return
	}
	detail := fmt.Sprintf("couldn't %s %s on %s: %s", doing, path, srv.GetName(), why.Error())
	if event == eventSaveUnknown {
		detail = fmt.Sprintf("may have saved %s on %s: %s", path, srv.GetName(), why.Error())
	}
	if _, err := record(event, path, detail); err != nil {
		h.logger.WarnContext(ctx, "can't record a refused file", slog.String("server", srv.GetId()), slog.String("error", err.Error()))
	}
}

// refusal is what the agent said, as the API says it.
func refusal(r *agentv1.FileRefusal) error {
	err := errors.New(r.GetMessage())
	switch r.GetKind() {
	case agentv1.FileRefusal_KIND_NOT_FOUND:
		return authapi.ReasonError(connect.CodeNotFound, err, "not_found")
	case agentv1.FileRefusal_KIND_REFUSED:
		return authapi.ReasonError(connect.CodePermissionDenied, err, "file_refused")
	case agentv1.FileRefusal_KIND_NOT_A_DIRECTORY:
		return authapi.ReasonError(connect.CodeFailedPrecondition, err, "not_a_directory")
	case agentv1.FileRefusal_KIND_NOT_A_FILE:
		return authapi.ReasonError(connect.CodeFailedPrecondition, err, "not_a_file")
	case agentv1.FileRefusal_KIND_NOT_ANSWERING:
		return authapi.ReasonError(connect.CodeDeadlineExceeded, err, "path_not_answering")
	case agentv1.FileRefusal_KIND_CHANGED:
		return authapi.ReasonError(connect.CodeFailedPrecondition, err, "file_changed")
	case agentv1.FileRefusal_KIND_MAY_LAND:
		return authapi.ReasonError(connect.CodeDeadlineExceeded, err, "save_may_land")
	case agentv1.FileRefusal_KIND_NEEDS_STEP_UP:
		return authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp)
	case agentv1.FileRefusal_KIND_DENIED, agentv1.FileRefusal_KIND_UNSPECIFIED:
	}
	return authapi.ReasonError(connect.CodePermissionDenied, err, "file_denied")
}

func (h *Handler) rpcError(ctx context.Context, err error) error {
	var cerr *connect.Error
	var failed *resource.ServerError
	switch {
	case errors.As(err, &cerr):
		return cerr
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, resource.ErrLost), errors.Is(err, resource.ErrNotConnected):
		return authapi.ReasonError(connect.CodeUnavailable, err, "server_offline")
	case errors.Is(err, context.DeadlineExceeded):
		return authapi.ReasonError(connect.CodeDeadlineExceeded, err, "agent_timeout")
	case errors.As(err, &failed):
		return authapi.ReasonError(connect.CodeFailedPrecondition, err, "agent_failed")
	}
	h.logger.ErrorContext(ctx, "file call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't reach that file; nothing went out. Try again, and check its logs if it keeps failing"))
}
