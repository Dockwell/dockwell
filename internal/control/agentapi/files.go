// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
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
