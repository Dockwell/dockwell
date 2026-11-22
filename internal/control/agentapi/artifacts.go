// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"io"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
)

type artifactGrant struct {
	session *Session
	receive bool
	// These are the exact operation, lease and extent sent on the control stream.
	id, owner      string
	offset, length uint64
	source         io.Reader
	used           bool
}

// ArtifactTransfers reports support for archive leases and the mTLS byte lane.
func (s *Session) ArtifactTransfers() bool {
	return s.transfersFiles && s.dockerCapabilities.GetArtifactTransfers()
}

// Artifact calls the agent for metadata or to open an approved stream.
func (s *Session) Artifact(ctx context.Context, ask *agentv1.DockerArtifactControlCall, timeout time.Duration) (*agentv1.DockerArtifactControlResult, error) {
	if !s.ArtifactTransfers() {
		return nil, ErrOlderAgent
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_DockerArtifactControl{DockerArtifactControl: ask}}, timeout, nil)
	if err != nil {
		return nil, err
	}
	out := r.GetDockerArtifactControl()
	if out == nil {
		return nil, errors.New("the agent did not answer the artifact operation")
	}
	return out, nil
}

func (s *Session) grantArtifact(grant *artifactGrant) (*transfer, error) {
	a := s.agents
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessions[s.server] != s || s.serial == "" {
		return nil, errors.New("the agent session changed")
	}
	count := 0
	for _, t := range a.transfers {
		if t.server == s.server && t.artifact != nil && t.artifact.receive == grant.receive {
			count++
		}
	}
	if count >= 4 {
		busy := connect.NewError(connect.CodeResourceExhausted, artifact.ErrTransferSlots)
		busy.Meta().Set(ReasonKey, artifact.TransferSlotsReason)
		return nil, busy
	}
	t := a.transferLocked(s.server)
	grant.session = s
	t.artifact = grant
	return t, nil
}

func (a *Agents) artifactCurrent(t *transfer) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.transfers[t.token] == t && a.sessions[t.server] == t.artifact.session
}

func (h *Handler) receiveArtifact(ctx context.Context, t *transfer, out *connect.ServerStream[agentv1.ReceiveFileResponse]) error {
	g := t.artifact
	buf := make([]byte, frameBytes)
	for remaining := g.length; remaining > 0; {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !h.agents.artifactCurrent(t) {
			return context.Canceled
		}
		chunk := buf[:min(remaining, uint64(len(buf)))]
		if _, err := io.ReadFull(g.source, chunk); err != nil {
			return err
		}
		if err := out.Send(&agentv1.ReceiveFileResponse{Data: chunk}); err != nil {
			return err
		}
		remaining -= uint64(len(chunk))
	}
	return nil
}

// UploadArtifact grants one exact upload extent and relays its bytes.
func (s *Session) UploadArtifact(ctx context.Context, id, owner string, offset, length uint64, r io.Reader) (*agentv1.DockerArtifactControlResult, error) {
	g := &artifactGrant{receive: true, id: id, owner: owner, offset: offset, length: length, source: r}
	t, err := s.grantArtifact(g)
	if err != nil {
		return nil, err
	}
	defer s.agents.closeTransfer(t)
	return s.Artifact(ctx, &agentv1.DockerArtifactControlCall{ArtifactId: g.id, OwnerBinding: g.owner, Operation: &agentv1.DockerArtifactControlCall_Receive{
		Receive: &agentv1.DockerArtifactStream{Stream: t.token, Offset: g.offset, Length: g.length},
	}}, TransferTimeout)
}

// DownloadArtifact grants one exact byte range and relays the agent response.
func (s *Session) DownloadArtifact(ctx context.Context, id, owner string, offset, length uint64, w io.Writer) error {
	g := &artifactGrant{id: id, owner: owner, offset: offset, length: length}
	t, err := s.grantArtifact(g)
	if err != nil {
		return err
	}
	defer s.agents.closeTransfer(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		out, err := s.Artifact(ctx, &agentv1.DockerArtifactControlCall{ArtifactId: g.id, OwnerBinding: g.owner, Operation: &agentv1.DockerArtifactControlCall_Send{
			Send: &agentv1.DockerArtifactStream{Stream: t.token, Offset: g.offset, Length: g.length},
		}}, TransferTimeout)
		if err == nil && out.GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_UNSPECIFIED {
			err = errors.New("the agent could not read the sealed artifact")
		}
		done <- err
	}()
	var sent uint64
	for {
		select {
		case part := <-t.out:
			n, err := w.Write(part.GetData())
			if err != nil {
				return err
			}
			if n != len(part.GetData()) {
				return io.ErrShortWrite
			}
			sent += uint64(len(part.GetData()))
		case err := <-done:
			if err == nil && sent != length {
				return io.ErrUnexpectedEOF
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
