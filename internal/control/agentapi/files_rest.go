// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// FilesRest is what the agent says about the everyday Files tools and host
// terminal starting directories.
func (s *Session) FilesRest() bool { return s.filesRest }

// ListFileAccounts lists local users and groups on the server.
func (s *Session) ListFileAccounts(ctx context.Context) (*agentv1.ListFileAccountsResult, error) {
	call := &agentv1.Call_ListFileAccounts{ListFileAccounts: &agentv1.ListFileAccountsCall{}}
	r, err := s.call(ctx, &agentv1.Call{Call: call}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetListFileAccounts(); out.GetResponse() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent returned no everyday Files result", s.name)
}

// ChangeFileAccess changes permissions, owner or group on a file or folder.
func (s *Session) ChangeFileAccess(ctx context.Context, in *agentv1.ChangeFileAccessCall) (*agentv1.ChangeFileAccessResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ChangeFileAccess{ChangeFileAccess: in}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetChangeFileAccess(); out.GetResponse() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent returned no everyday Files result", s.name)
}

// CalculateFolderSize counts logical file bytes below a folder, up to the scan bounds.
func (s *Session) CalculateFolderSize(ctx context.Context, p string) (*agentv1.CalculateFolderSizeResult, error) {
	call := &agentv1.Call_CalculateFolderSize{CalculateFolderSize: &agentv1.CalculateFolderSizeCall{Path: p}}
	r, err := s.call(ctx, &agentv1.Call{Call: call}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetCalculateFolderSize(); out.GetResponse() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent returned no everyday Files result", s.name)
}

// SearchFiles finds matching paths and lines below a folder.
func (s *Session) SearchFiles(ctx context.Context, p, query string, text bool) (*agentv1.SearchFilesResult, error) {
	call := &agentv1.Call_SearchFiles{SearchFiles: &agentv1.SearchFilesCall{Path: p, Query: query, Text: text}}
	r, err := s.call(ctx, &agentv1.Call{Call: call}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetSearchFiles(); out.GetResponse() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent returned no everyday Files result", s.name)
}

// DownloadFileFromURL starts a background download from a URL to a new file.
func (s *Session) DownloadFileFromURL(ctx context.Context, in *agentv1.DownloadFileFromURLCall) (*agentv1.DownloadFileFromURLResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_DownloadFileFromUrl{DownloadFileFromUrl: in}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetDownloadFileFromUrl(); out.GetJob() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent returned no everyday Files result", s.name)
}

// CancelFileDownload stops a background download the session started.
func (s *Session) CancelFileDownload(ctx context.Context, id, owner string) (*agentv1.CancelFileDownloadResult, error) {
	call := &agentv1.Call_CancelFileDownload{CancelFileDownload: &agentv1.CancelFileDownloadCall{JobId: id, Owner: owner}}
	r, err := s.call(ctx, &agentv1.Call{Call: call}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetCancelFileDownload(); out.GetJob() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent returned no everyday Files result", s.name)
}
