// SPDX-License-Identifier: AGPL-3.0-only

package databases

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
)

const importControlTimeout = 30 * time.Second

func importOwner(ctx context.Context) (string, error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return "", authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	if p.Token != nil {
		return "", authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, authapi.ReasonNeedsSession)
	}
	if p.Session.ID != "" {
		return "session:" + p.Session.ID, nil
	}
	return "user:" + p.User.ID, nil
}

func importRefusal(out *agentv1.DatabaseImportControlResult) error {
	if out.GetError() == "" {
		return nil
	}
	reason := out.GetReason()
	msg := errors.New(out.GetError())
	switch reason {
	case "invalid", "invalid_database", "invalid_format":
		return authapi.ReasonError(connect.CodeInvalidArgument, msg, reason)
	case "import_gone":
		return authapi.ReasonError(connect.CodeNotFound, msg, reason)
	case "too_large", "no_space", "too_many_imports":
		return authapi.ReasonError(connect.CodeResourceExhausted, msg, reason)
	case "upload_offset", "upload_busy", "incomplete":
		return authapi.ReasonError(connect.CodeFailedPrecondition, msg, reason)
	case "unavailable":
		return authapi.ReasonError(connect.CodeFailedPrecondition, msg, reason)
	default:
		return authapi.ReasonError(connect.CodeInternal, msg, cmp.Or(reason, "failed"))
	}
}

func (h *Handler) pickImportTarget(ctx context.Context, id, project, environment, serverWanted string) (*placed, store.Server, *agentapi.Session, error) {
	if id == "" {
		return nil, store.Server{}, nil, authapi.Invalid(errors.New("the request names no database. Pass its service's ID, or its name"))
	}
	list, err := h.list(ctx)
	if err != nil {
		return nil, store.Server{}, nil, h.fail(ctx, err)
	}
	one, err := h.pick(ctx, list, id, project, environment)
	if err != nil {
		return nil, store.Server{}, nil, h.fail(ctx, err)
	}
	eng := database.Of(one.db.GetEngine())
	if eng == nil || !eng.SQL() {
		name := one.ref.Name
		if eng != nil {
			return nil, store.Server{}, nil, authapi.Invalid(fmt.Errorf("%s runs %s, which doesn't support dump imports here", name, eng.Name))
		}
		return nil, store.Server{}, nil, authapi.Invalid(fmt.Errorf("%s doesn't support dump imports here", name))
	}
	if one.db.GetVolume() == "" || one.db.GetVolumeExternal() {
		return nil, store.Server{}, nil, authapi.ReasonError(
			connect.CodeFailedPrecondition,
			fmt.Errorf("%s needs a Dockwell-managed data volume so Dockwell can back it up before importing", one.ref.Name),
			"no_volume",
		)
	}
	if len(one.servers) == 0 {
		return nil, store.Server{}, nil, authapi.ReasonError(
			connect.CodeFailedPrecondition,
			fmt.Errorf("%s isn't placed on any server yet", one.ref.Name),
			"not_placed",
		)
	}
	srv := one.servers[0]
	if serverWanted != "" {
		found := false
		for _, cand := range one.servers {
			if cand.ID == serverWanted || cand.Name == serverWanted {
				srv = cand
				found = true
				break
			}
		}
		if !found {
			return nil, store.Server{}, nil, authapi.Invalid(fmt.Errorf("%s doesn't run on server %s", one.ref.Name, serverWanted))
		}
	} else if len(one.servers) > 1 {
		return nil, store.Server{}, nil, authapi.Invalid(fmt.Errorf("%s runs on more than one server. Pass server to choose where to import", one.ref.Name))
	}
	session, ok := h.agents.SessionPastMove(ctx, srv.ID, agentapi.MoveGrace)
	if !ok {
		return nil, store.Server{}, nil, authapi.ReasonError(
			connect.CodeUnavailable,
			fmt.Errorf("%s isn't connected, so Dockwell can't upload a dump there. Try again once it reconnects", srv.Name),
			"server_offline",
		)
	}
	if !session.DatabaseImports() {
		return nil, store.Server{}, nil, authapi.ReasonError(
			connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to import database dumps. Update the agent on %s, then try again", srv.Name, srv.Name),
			"agent_outdated",
		)
	}
	return one, srv, session, nil
}

// BeginDatabaseImport starts a staged dump upload or inspects an existing one for resume.
func (h *Handler) BeginDatabaseImport(ctx context.Context, req *connect.Request[dockwellv1.BeginDatabaseImportRequest]) (*connect.Response[dockwellv1.BeginDatabaseImportResponse], error) {
	owner, err := importOwner(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	one, srv, session, err := h.pickImportTarget(ctx, m.GetId(), m.GetProject(), m.GetEnvironment(), m.GetServer())
	if err != nil {
		return nil, err
	}
	var format string
	if m.GetImportId() != "" {
		if !backup.ValidImportID(m.GetImportId()) {
			return nil, authapi.Invalid(errors.New("invalid import ID"))
		}
	} else {
		if err := database.CheckSQLName("database", m.GetDatabase()); err != nil {
			return nil, invalidField("database", err)
		}
		if database.SystemDatabase(m.GetDatabase()) {
			return nil, invalidField("database", fmt.Errorf("%s is a system database and can't be imported into. Choose one of your own databases", m.GetDatabase()))
		}
		format, err = backup.DetectImportFormat(m.GetFilename())
		if err != nil {
			return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid_format")
		}
		if err := backup.CheckImportEngineFormat(one.db.GetEngine(), format); err != nil {
			return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid_format")
		}
		if m.GetSizeBytes() == 0 {
			return nil, authapi.Invalid(errors.New("that dump file is empty. Choose a non-empty dump file"))
		}
		if m.GetSizeBytes() > backup.MaxImportBytes {
			return nil, authapi.ReasonError(connect.CodeResourceExhausted, errors.New("that dump file is larger than the 16 GiB limit"), "too_large")
		}
	}

	out, err := session.DatabaseImport(ctx, &agentv1.DatabaseImportControlCall{
		ImportId: m.GetImportId(),
		Owner:    owner,
		Op: &agentv1.DatabaseImportControlCall_Begin{
			Begin: &agentv1.DatabaseImportBegin{
				Database:  m.GetDatabase(),
				Filename:  m.GetFilename(),
				Format:    format,
				SizeBytes: m.GetSizeBytes(),
				ServiceId: one.db.GetId(),
			},
		},
	}, importControlTimeout)
	if err != nil {
		return nil, h.importAgentErr(ctx, srv.Name, err)
	}
	if err := importRefusal(out); err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.BeginDatabaseImportResponse{
		ImportId:  out.GetImportId(),
		Offset:    out.GetOffset(),
		PartBytes: out.GetPartBytes(),
		MaxBytes:  out.GetMaxBytes(),
		ServerId:  srv.ID,
		Format:    out.GetFormat(),
	}), nil
}

// UploadDatabaseImport streams bounded chunks of a staged dump to the server.
func (h *Handler) UploadDatabaseImport(ctx context.Context, stream *connect.ClientStream[dockwellv1.UploadDatabaseImportRequest]) (*connect.Response[dockwellv1.UploadDatabaseImportResponse], error) {
	owner, err := importOwner(ctx)
	if err != nil {
		return nil, err
	}
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, authapi.Invalid(errors.New("say which dump upload this is, and from where"))
	}
	first := stream.Msg()
	importID := first.GetImportId()
	if !backup.ValidImportID(importID) {
		return nil, authapi.Invalid(errors.New("invalid import ID"))
	}
	_, srv, session, err := h.pickImportTarget(ctx, first.GetId(), first.GetProject(), first.GetEnvironment(), "")
	if err != nil {
		return nil, err
	}

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

	out, err := session.WriteDatabaseImport(ctx, importID, owner, first.GetOffset(), parts)
	if err != nil {
		return nil, h.importAgentErr(ctx, srv.Name, err)
	}
	if err := importRefusal(out); err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.UploadDatabaseImportResponse{
		ImportId:  out.GetImportId(),
		Offset:    out.GetOffset(),
		SizeBytes: out.GetSizeBytes(),
		Complete:  out.GetComplete(),
	}), nil
}

// PlanDatabaseImport plans importing a completed staged dump into its chosen
// logical database, backing up that database first as restores do.
func (h *Handler) PlanDatabaseImport(ctx context.Context, req *connect.Request[dockwellv1.PlanDatabaseImportRequest]) (*connect.Response[dockwellv1.PlanDatabaseImportResponse], error) {
	owner, err := importOwner(ctx)
	if err != nil {
		return nil, err
	}
	if h.planImport == nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("dump imports aren't wired on this control plane"), "unavailable")
	}
	m := req.Msg
	importID := m.GetImportId()
	if !backup.ValidImportID(importID) {
		return nil, authapi.Invalid(errors.New("invalid import ID"))
	}
	one, srv, session, err := h.pickImportTarget(ctx, m.GetId(), m.GetProject(), m.GetEnvironment(), "")
	if err != nil {
		return nil, err
	}

	out, err := session.DatabaseImport(ctx, &agentv1.DatabaseImportControlCall{
		ImportId: importID,
		Owner:    owner,
		Op:       &agentv1.DatabaseImportControlCall_Inspect{Inspect: &agentv1.DatabaseImportInspect{}},
	}, importControlTimeout)
	if err != nil {
		return nil, h.importAgentErr(ctx, srv.Name, err)
	}
	if err := importRefusal(out); err != nil {
		return nil, err
	}
	if out.GetServiceId() != "" && out.GetServiceId() != one.db.GetId() {
		return nil, authapi.ReasonError(connect.CodeNotFound, errors.New("that staged dump belongs to a different database"), "import_gone")
	}

	ref := resource.Ref{Kind: volume.Kind, Scope: one.ref.Scope, Name: one.db.GetVolume()}
	source := resource.RestoreSource{
		Server:         srv.ID,
		ImportID:       importID,
		ImportDatabase: out.GetDatabase(),
		ImportFilename: out.GetFilename(),
		ImportFormat:   out.GetFormat(),
	}
	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		summary = fmt.Sprintf("Import %s into %s on %s", out.GetFilename(), out.GetDatabase(), one.ref.Name)
	}
	change, err := h.planImport(ctx, ref, source, summary)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanDatabaseImportResponse{Change: change}), nil
}

// CancelDatabaseImport discards a staged dump upload on its server.
func (h *Handler) CancelDatabaseImport(ctx context.Context, req *connect.Request[dockwellv1.CancelDatabaseImportRequest]) (*connect.Response[dockwellv1.CancelDatabaseImportResponse], error) {
	owner, err := importOwner(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	importID := m.GetImportId()
	if !backup.ValidImportID(importID) {
		return nil, authapi.Invalid(errors.New("invalid import ID"))
	}
	_, srv, session, err := h.pickImportTarget(ctx, m.GetId(), m.GetProject(), m.GetEnvironment(), "")
	if err != nil {
		return nil, err
	}
	out, err := session.DatabaseImport(ctx, &agentv1.DatabaseImportControlCall{
		ImportId: importID,
		Owner:    owner,
		Op:       &agentv1.DatabaseImportControlCall_Cancel{Cancel: &agentv1.DatabaseImportCancel{}},
	}, importControlTimeout)
	if err != nil {
		return nil, h.importAgentErr(ctx, srv.Name, err)
	}
	if err := importRefusal(out); err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.CancelDatabaseImportResponse{}), nil
}

func (h *Handler) importAgentErr(ctx context.Context, serverName string, err error) error {
	if errors.Is(err, agentapi.ErrOlderAgent) {
		return authapi.ReasonError(
			connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to import database dumps. Update the agent on %s, then try again", serverName, serverName),
			"agent_outdated",
		)
	}
	return h.fail(ctx, err)
}
