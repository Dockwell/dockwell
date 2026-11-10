// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Uploads and downloads go between the control plane and an agent on connections of
// their own, as logs do (ADR 0045): the agent opens ReceiveFile or SendFile for a
// call, by a token that names it, and each part goes as the other side takes it, so
// neither holds more than a part, and Connect never waits on a file.

// TransferTimeout is how long one write of an upload, or one download, may take: a
// person on a slow line with a large file.
const TransferTimeout = 12 * time.Hour

// frameBytes is the most of an upload one message to an agent carries, as the
// agent's own parts are (files.FrameBytes).
const frameBytes = 128 << 10

// transfer is one upload's write or one download, on its way between a person and
// an agent.
type transfer struct {
	server string
	token  uint64
	// in carries an upload's parts to the agent, and closes when there are no more.
	in chan []byte
	// out carries a download's parts from the agent.
	out  chan *agentv1.SendFileRequest
	done chan struct{}
}

// openTransfer notes a transfer about to be asked for, by a random token, so one
// server's agent can't guess at another's.
func (a *Agents) openTransfer(server string) *transfer {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		var b [8]byte
		_, _ = rand.Read(b[:])
		token := binary.BigEndian.Uint64(b[:])
		if _, taken := a.transfers[token]; token == 0 || taken {
			continue
		}
		t := &transfer{server: server, token: token, in: make(chan []byte), out: make(chan *agentv1.SendFileRequest), done: make(chan struct{})}
		a.transfers[token] = t
		return t
	}
}

func (a *Agents) closeTransfer(t *transfer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.transfers, t.token)
	close(t.done)
}

func (a *Agents) transferFor(server string, token uint64) *transfer {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.transfers[token]; t != nil && t.server == server {
		return t
	}
	return nil
}

// ReceiveFile sends an agent an upload's parts, as they come.
func (h *Handler) ReceiveFile(ctx context.Context, req *connect.Request[agentv1.ReceiveFileRequest], stream *connect.ServerStream[agentv1.ReceiveFileResponse]) error {
	peer, err := requireServer(ctx)
	if err != nil {
		return err
	}
	if _, err := h.auth.CheckConnection(ctx, peer.ServerID, peer.Serial); err != nil {
		return h.refuse(ctx, peer, err)
	}
	t := h.agents.transferFor(peer.ServerID, req.Msg.GetStream())
	if t == nil {
		return connect.NewError(connect.CodeNotFound, errors.New("nobody is sending this upload"))
	}
	for {
		select {
		case part, ok := <-t.in:
			if !ok {
				return nil
			}
			// A frame at a time, however large a part came, so the agent holds a frame.
			for len(part) > 0 {
				n := min(len(part), frameBytes)
				if err := stream.Send(&agentv1.ReceiveFileResponse{Data: part[:n]}); err != nil {
					return err
				}
				part = part[n:]
			}
		case <-t.done:
			return connect.NewError(connect.CodeCanceled, errors.New("whoever sent this upload has gone"))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// SendFile takes a download's parts from an agent, as whoever asked for it takes
// them.
func (h *Handler) SendFile(ctx context.Context, stream *connect.ClientStream[agentv1.SendFileRequest]) (*connect.Response[agentv1.SendFileResponse], error) {
	peer, err := requireServer(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := h.auth.CheckConnection(ctx, peer.ServerID, peer.Serial); err != nil {
		return nil, h.refuse(ctx, peer, err)
	}
	var t *transfer
	for stream.Receive() {
		msg := stream.Msg()
		if t == nil {
			if t = h.agents.transferFor(peer.ServerID, msg.GetStream()); t == nil {
				return nil, connect.NewError(connect.CodeNotFound, errors.New("nobody is waiting for this download"))
			}
		}
		select {
		case t.out <- msg:
		case <-t.done:
			return connect.NewResponse(&agentv1.SendFileResponse{}), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentv1.SendFileResponse{}), nil
}

// TransfersFiles reports an agent that uploads and downloads files.
func (s *Session) TransfersFiles() bool { return s.transfersFiles }

// BeginUpload asks the agent to start an upload, or say how far one got.
func (s *Session) BeginUpload(ctx context.Context, path string, size uint64, unpack dockwellv1.Unpack, id, confirmed, owner string) (*agentv1.UploadResult, error) {
	call := &agentv1.BeginUploadCall{Path: path, SizeBytes: size, Unpack: unpack, UploadId: id, ConfirmedPath: confirmed, Owner: owner}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_BeginUpload{BeginUpload: call}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetBeginUpload(); out.GetUpload() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered an upload with neither where it stands nor a refusal", s.name)
}

// WriteUpload has the agent write what parts gives to an upload, from offset, until
// parts closes or ctx ends. A part waits until the agent takes it.
func (s *Session) WriteUpload(ctx context.Context, id, owner string, offset uint64, parts <-chan []byte) (*agentv1.UploadResult, error) {
	t := s.agents.openTransfer(s.server)
	defer s.agents.closeTransfer(t)
	ctx, cancel := context.WithTimeout(ctx, TransferTimeout)
	defer cancel()
	go func() {
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
	call := &agentv1.WriteUploadCall{Stream: t.token, UploadId: id, Offset: offset, Owner: owner}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_WriteUpload{WriteUpload: call}}, TransferTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetWriteUpload(); out.GetUpload() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered an upload's write with neither where it got nor a refusal", s.name)
}

// FinishUpload asks the agent to put an upload in place, and unpack it if it was to
// be, which takes as long as the archive does.
func (s *Session) FinishUpload(ctx context.Context, id, owner string) (*agentv1.FinishUploadResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_FinishUpload{FinishUpload: &agentv1.FinishUploadCall{UploadId: id, Owner: owner}}}, time.Hour, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetFinishUpload(); out.GetFinished() != nil || out.GetRefusal() != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered putting an upload in place with neither what it did nor a refusal", s.name)
}

// CancelUpload has the agent remove what's there of an upload, and its note.
// Its answer has a refusal, or nothing.
func (s *Session) CancelUpload(ctx context.Context, id, owner string) (*agentv1.FinishUploadResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_FinishUpload{FinishUpload: &agentv1.FinishUploadCall{UploadId: id, Owner: owner, Cancel: true}}}, FileTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetFinishUpload(); out != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent didn't answer stopping an upload", s.name)
}

// Download asks the agent for what's at path, and passes each part it sends to
// each, the first saying what it is, until it has sent it all, or each fails, or
// ctx ends, which stops the agent at once.
func (s *Session) Download(ctx context.Context, path string, each func(*agentv1.SendFileRequest) error) (*agentv1.DownloadFileResult, error) {
	t := s.agents.openTransfer(s.server)
	defer s.agents.closeTransfer(t)
	ctx, cancel := context.WithTimeout(ctx, TransferTimeout)
	defer cancel()
	type answer struct {
		r   *agentv1.CallResult
		err error
	}
	answered := make(chan answer, 1)
	go func() {
		r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_DownloadFile{DownloadFile: &agentv1.DownloadFileCall{Stream: t.token, Path: path}}}, TransferTimeout, nil)
		answered <- answer{r, err}
	}()
	for {
		select {
		case m := <-t.out:
			if err := each(m); err != nil {
				cancel()
				return nil, err
			}
		case a := <-answered:
			// The parts sent before the answer may still be on their way.
			for {
				select {
				case m := <-t.out:
					if err := each(m); err != nil {
						return nil, err
					}
					continue
				default:
				}
				break
			}
			if a.err != nil {
				return nil, a.err
			}
			if out := a.r.GetDownloadFile(); out != nil {
				return out, nil
			}
			return nil, fmt.Errorf("%s's agent answered a download with nothing", s.name)
		}
	}
}
