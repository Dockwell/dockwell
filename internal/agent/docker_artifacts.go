// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"io"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/agent/files"
)

func (c *calls) dockerArtifact(ctx context.Context, ask *agentv1.DockerArtifactControlCall) *agentv1.CallResult {
	out := &agentv1.DockerArtifactControlResult{}
	spool := c.cfg.artifacts
	id, owner := ask.GetArtifactId(), ask.GetOwnerBinding()
	var err error
	if ask.GetReceive() != nil || ask.GetSend() != nil {
		wait := ctx
		if ask.GetReceive() != nil {
			var cancel context.CancelFunc
			wait, cancel = context.WithTimeoutCause(ctx, time.Second, artifact.ErrTransferSlots)
			defer cancel()
		}
		release, admitErr := c.cfg.takeArtifactTransfer(wait)
		err = admitErr
		if err == nil {
			defer release()
		}
	}
	switch {
	case err != nil:
	case spool == nil || owner == "" && ask.GetDiscardExport() == nil && ask.GetReleasePin() == nil:
		err = artifact.ErrUnavailable
	case ask.GetPreparePush() != nil:
		out.Artifact, out.Descriptor_, err = c.prepareImagePush(ctx, owner, ask.GetPreparePush())
	case ask.GetBeginUpload() != nil:
		begin := ask.GetBeginUpload()
		out.Artifact, err = spool.Begin(owner, begin.GetKind(), begin.GetSizeBytes(), begin.GetSha256(), begin.GetGzip(), begin.GetFileName())
	case ask.GetList() != nil:
		out.Artifacts = spool.List(owner)
	case ask.GetSnapshotFolder() != nil:
		folder := ask.GetSnapshotFolder()
		if c.cfg.Files == nil || !artifact.ContextPath(folder.GetDockerfilePath()) {
			err = artifact.ErrArchive
			break
		}
		if override := folder.GetOverrideArtifactId(); override != "" {
			info, readErr := spool.Status(override, owner)
			if readErr != nil || info.GetKind() != artifact.Dockerfile || info.GetState() != artifact.Ready {
				err = artifact.ErrUnavailable
				break
			}
		}
		var release func()
		release, err = c.cfg.takeArtifactTransfer(ctx)
		if err != nil {
			break
		}
		defer release()
		out.Artifact, err = c.cfg.Files.SnapshotContext(ctx, spool, owner, folder.GetPath(), folder.GetDockerfilePath())
		if errors.Is(err, files.ErrContextHardLink) || errors.Is(err, files.ErrContextEntry) || errors.Is(err, context.DeadlineExceeded) {
			return callFailed(err)
		}
		if out.Artifact == nil && err == nil {
			err = artifact.ErrUnavailable
		}
	case !artifact.ValidID(id):
		err = artifact.ErrUnavailable
	case ask.GetPin() != nil && ask.GetPin().GetArtifactId() == id:
		out.Artifact, err = spool.Pin(ask.GetPin(), owner)
	case ask.GetReleasePin() != nil && ask.GetReleasePin().GetArtifactId() == id:
		err = spool.ReleasePin(ask.GetReleasePin())
	case ask.GetDiscardExport() != nil:
		err = spool.DiscardExport(id)
	case ask.GetStatus() != nil:
		out.Artifact, err = spool.Status(id, owner)
	case ask.GetSeal() != nil:
		out.Artifact, err = spool.QueueSeal(id, owner)
	case ask.GetAbort() != nil:
		out.Artifact, err = spool.AbortUpload(ctx, id, owner)
	case ask.GetReceive() != nil:
		out.Artifact, err = c.receiveDockerArtifact(ctx, id, owner, ask.GetReceive())
	case ask.GetSend() != nil:
		err = c.sendDockerArtifact(ctx, id, owner, ask.GetSend())
	default:
		err = artifact.ErrUnavailable
	}
	switch {
	case err == nil:
	case errors.Is(err, artifact.ErrLeaseLimit):
		out.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_LEASE_LIMIT
	case errors.Is(err, artifact.ErrTransferSlots), ask.GetReceive() != nil && errors.Is(err, artifact.ErrBusy):
		out.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_TRANSFER_BUSY
	case errors.Is(err, artifact.ErrPlanned):
		out.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_ARTIFACT_PINNED
	case errors.Is(err, artifact.ErrQuota):
		out.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_QUOTA
		var space *artifact.SpaceError
		if errors.As(err, &space) {
			out.Space = &v1.DockerArtifactSpace{RequiredBytes: space.Needed, AvailableBytes: space.Available}
		}
	case errors.Is(err, artifact.ErrUnavailable):
		out.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_EXPIRED
	case errors.Is(err, artifact.ErrArchive):
		out.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_ARCHIVE_REFUSED
	case errors.Is(err, artifact.ErrOffset), errors.Is(err, artifact.ErrBusy):
		out.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_STALE
	default:
		out.Reason = v1.DockerImageReason_DOCKER_IMAGE_REASON_VERIFICATION_FAILED
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_DockerArtifactControl{DockerArtifactControl: out}}
}

func (cfg Config) takeArtifactTransfer(ctx context.Context) (func(), error) {
	if cfg.Files == nil {
		return func() {}, nil
	}
	return cfg.Files.WaitTransfer(ctx)
}

// checkArtifactPins reconciles this spool with the durable change records.
func (cfg Config) checkArtifactPins(ctx context.Context) {
	pins, absentExpired := cfg.artifacts.Pins()
	if len(pins) == 0 {
		return
	}
	client, release, err := cfg.Identity.Client()
	if err != nil {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got, err := client.CheckDockerArtifactPins(ctx, connect.NewRequest(&agentv1.CheckDockerArtifactPinsRequest{Pins: pins}))
	if err != nil {
		return
	}
	for _, pin := range got.Msg.GetEnded() {
		_ = cfg.artifacts.ReleasePin(pin)
	}
	for _, pin := range got.Msg.GetAbsent() {
		if absentExpired[pin.GetArtifactId()] {
			_ = cfg.artifacts.ReleasePin(pin)
		}
	}
}

func (c *calls) receiveDockerArtifact(ctx context.Context, id, owner string, ask *agentv1.DockerArtifactStream) (*v1.DockerArtifactInfo, error) {
	client, err := c.transferClient(true)
	if err != nil {
		return nil, err
	}
	stream, err := client.ReceiveFile(ctx, connect.NewRequest(&agentv1.ReceiveFileRequest{Stream: ask.GetStream()}))
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	next := func() ([]byte, error) {
		if stream.Receive() {
			return stream.Msg().GetData(), nil
		}
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	return c.cfg.artifacts.Receive(ctx, id, owner, ask.GetOffset(), ask.GetLength(), next)
}

type artifactSender func([]byte) error

func (w artifactSender) Write(p []byte) (int, error) {
	if err := w(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *calls) sendDockerArtifact(ctx context.Context, id, owner string, ask *agentv1.DockerArtifactStream) error {
	client, err := c.transferClient(true)
	if err != nil {
		return err
	}
	ctx, closeStream := context.WithCancel(ctx)
	defer closeStream()
	stream := client.SendFile(ctx)
	w := artifactSender(func(p []byte) error {
		return stream.Send(&agentv1.SendFileRequest{Stream: ask.GetStream(), Data: p})
	})
	if err := c.cfg.artifacts.Send(ctx, id, owner, ask.GetOffset(), ask.GetLength(), w); err != nil {
		return err
	}
	_, err = stream.CloseAndReceive()
	return err
}
