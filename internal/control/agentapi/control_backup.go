// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

type controlBackupTransfer struct {
	session *Session
	file    *io.SectionReader
	used    bool
	onRead  func()
}

func (t *controlBackupTransfer) send(ctx context.Context, done <-chan struct{}, stream *connect.ServerStream[agentv1.ReceiveFileResponse]) error {
	buf := make([]byte, frameBytes)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return context.Canceled
		default:
		}
		n, err := t.file.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&agentv1.ReceiveFileResponse{Data: buf[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			if t.onRead != nil {
				t.onRead()
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// KeepsControlBackups reports whether this agent supports receiving a recovery copy.
func (s *Session) KeepsControlBackups() bool { return s.transfersFiles && s.keepsControlBackups }

// ControlBackupRoom checks how much the agent can keep.
func (s *Session) ControlBackupRoom(ctx context.Context, install string, generation int64) (int64, error) {
	got, err := s.keepControlBackup(ctx, &agentv1.KeepControlBackupCall{Install: install, Generation: generation})
	return got.GetRoom(), err
}

// KeepControlBackup gives the agent a session-bound transfer for one sealed bundle.
func (s *Session) KeepControlBackup(ctx context.Context, ask *agentv1.KeepControlBackupCall, file io.ReaderAt) (*agentv1.KeepControlBackupResult, error) {
	s.agents.mu.Lock()
	t := s.agents.transferLocked(s.server)
	t.control = &controlBackupTransfer{session: s, file: io.NewSectionReader(file, 0, ask.GetSize())}
	s.agents.mu.Unlock()
	defer s.agents.closeTransfer(t)
	ask.Stream = t.token
	return s.keepControlBackup(ctx, ask)
}

func (s *Session) keepControlBackup(ctx context.Context, ask *agentv1.KeepControlBackupCall) (*agentv1.KeepControlBackupResult, error) {
	if !s.KeepsControlBackups() {
		return nil, ErrOlderAgent
	}
	result, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_KeepControlBackup{KeepControlBackup: ask}}, TransferTimeout, nil)
	if err != nil {
		return nil, err
	}
	got := result.GetKeepControlBackup()
	if got == nil || ask.GetStream() != 0 && !got.GetKept() {
		return nil, errors.New("the server did not keep the control plane backup")
	}
	return got, nil
}
