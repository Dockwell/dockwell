// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// FileTimeout is how long a server has to list a directory or read a file. It is
// a person waiting on a click.
const FileTimeout = 30 * time.Second

// ListFiles asks the agent what a directory holds.
func (s *Session) ListFiles(ctx context.Context, path string) (*agentv1.ListFilesResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ListFiles{ListFiles: &agentv1.ListFilesCall{Path: path}}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetListFiles(); out.GetListing() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a listing with neither what it found nor a refusal", s.name)
}

// ReadFile asks the agent for a file's text.
func (s *Session) ReadFile(ctx context.Context, path string) (*agentv1.ReadFileResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ReadFile{ReadFile: &agentv1.ReadFileCall{Path: path}}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetReadFile(); out.GetFile() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a read with neither the file nor a refusal", s.name)
}

// GuardsWaysIn reports an agent that asks for a passkey before saving what can let
// someone back onto its server. Only it can tell where a path leads, so one that
// doesn't can't be saved through.
func (s *Session) GuardsWaysIn() bool { return s.guardsWaysIn }

// SaveFile asks the agent to replace a file's text, if it is still the version
// read. A stream that drops as it does can leave it either way, which lost says.
// record is what to call the save if it ends once nobody is waiting (OnSaveEnded),
// and confirmed the path a passkey confirmed, if any.
func (s *Session) SaveFile(ctx context.Context, path, content, version, record, confirmed string) (*agentv1.SaveFileResult, error) {
	call := &agentv1.SaveFileCall{Path: path, Content: content, Version: version, Record: record, ConfirmedPath: confirmed}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_SaveFile{SaveFile: call}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetSaveFile(); out.GetSaved() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a save with neither the file nor a refusal", s.name)
}

// ManagesFiles reports an agent that makes, moves, deletes and puts back files, each
// with the care ADR 0045 says, a passkey for a way in among it.
func (s *Session) ManagesFiles() bool { return s.managesFiles }

// MakeFile asks the agent to make an empty file or a folder. confirmed is the path a
// passkey confirmed, if any.
func (s *Session) MakeFile(ctx context.Context, path string, kind dockwellv1.FileType, confirmed string) (*agentv1.MakeFileResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_MakeFile{MakeFile: &agentv1.MakeFileCall{Path: path, Type: kind, ConfirmedPath: confirmed}}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetMakeFile(); out.GetMade() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a make with neither what it made nor a refusal", s.name)
}

// MoveFile asks the agent to move or rename what's at from. confirmedFrom and
// confirmedTo are the paths a passkey confirmed, if any.
func (s *Session) MoveFile(ctx context.Context, from, to, confirmedFrom, confirmedTo string) (*agentv1.MoveFileResult, error) {
	call := &agentv1.MoveFileCall{From: from, To: to, ConfirmedFrom: confirmedFrom, ConfirmedTo: confirmedTo}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_MoveFile{MoveFile: call}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetMoveFile(); out.GetMoved() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a move with neither where it went nor a refusal", s.name)
}

// DeleteFile asks the agent to delete what's at path, to Recently deleted or for
// good. confirmed is the path a passkey confirmed, if any.
func (s *Session) DeleteFile(ctx context.Context, path string, forGood bool, confirmed string) (*agentv1.DeleteFileResult, error) {
	call := &agentv1.DeleteFileCall{Path: path, ForGood: forGood, ConfirmedPath: confirmed}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_DeleteFile{DeleteFile: call}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetDeleteFile(); out.GetDeleted() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a delete with neither what it deleted nor a refusal", s.name)
}

// ListDeleted asks the agent what's in Recently deleted.
func (s *Session) ListDeleted(ctx context.Context) (*dockwellv1.ListDeletedFilesResponse, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ListDeleted{ListDeleted: &agentv1.ListDeletedCall{}}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetListDeleted(); out.GetListing() != nil {
		return out.GetListing(), nil
	}
	return nil, fmt.Errorf("%s's agent answered a list of what's deleted with nothing", s.name)
}

// RestoreFile asks the agent to put back what was deleted. confirmed is the path a
// passkey confirmed, if any.
func (s *Session) RestoreFile(ctx context.Context, id, confirmed string) (*agentv1.RestoreFileResult, error) {
	call := &agentv1.RestoreFileCall{Id: id, ConfirmedPath: confirmed}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_RestoreFile{RestoreFile: call}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetRestoreFile(); out.GetRestored() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a put back with neither where it went nor a refusal", s.name)
}

// PurgeDeleted asks the agent to delete for good what's in Recently deleted.
func (s *Session) PurgeDeleted(ctx context.Context, id string) (*agentv1.PurgeDeletedResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_PurgeDeleted{PurgeDeleted: &agentv1.PurgeDeletedCall{Id: id}}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetPurgeDeleted(); out.GetPurged() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered a delete from Recently deleted with neither what went nor a refusal", s.name)
}
