// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// DatabaseImports reports whether this agent stages and imports database dumps.
func (s *Session) DatabaseImports() bool {
	return s.transfersFiles && s.databaseImports
}

// DatabaseImport invokes a staged dump import control operation on the agent.
func (s *Session) DatabaseImport(ctx context.Context, ask *agentv1.DatabaseImportControlCall, timeout time.Duration) (*agentv1.DatabaseImportControlResult, error) {
	if !s.DatabaseImports() {
		return nil, ErrOlderAgent
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_DatabaseImport{DatabaseImport: ask}}, timeout, nil)
	if err != nil {
		return nil, err
	}
	out := r.GetDatabaseImport()
	if out == nil {
		return nil, errors.New("the agent did not answer the database import operation")
	}
	return out, nil
}

// WriteDatabaseImport streams upload chunks from parts into a staged dump on
// the agent over the ReceiveFile byte lane.
func (s *Session) WriteDatabaseImport(ctx context.Context, importID, owner string, offset uint64, parts <-chan []byte) (*agentv1.DatabaseImportControlResult, error) {
	if !s.DatabaseImports() {
		return nil, ErrOlderAgent
	}
	t := s.agents.openTransfer(s.server)
	defer s.agents.closeTransfer(t)
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		defer close(t.in)
		for {
			select {
			case part, ok := <-parts:
				if !ok {
					return
				}
				select {
				case t.in <- part:
				case <-t.done:
					return
				case <-ctx.Done():
					return
				}
			case <-t.done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	ask := &agentv1.DatabaseImportControlCall{
		ImportId: importID,
		Owner:    owner,
		Op: &agentv1.DatabaseImportControlCall_Receive{
			Receive: &agentv1.DatabaseImportReceive{
				Stream: t.token,
				Offset: offset,
			},
		},
	}
	out, err := s.DatabaseImport(ctx, ask, TransferTimeout)
	return out, err
}
