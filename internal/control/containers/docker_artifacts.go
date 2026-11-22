// SPDX-License-Identifier: AGPL-3.0-only

package containers

import (
	"context"
	"errors"
	"io"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/api/httpbody"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

// ArtifactPrefixHeader names the already committed prefix in a resumed upload.
const ArtifactPrefixHeader = "Dockwell-Artifact-Prefix-SHA256"

// ArtifactLengthHeader declares a framed RPC upload extent.
const ArtifactLengthHeader = "Dockwell-Artifact-Length"

type artifactAccess struct {
	session *agentapi.Session
	owner   string
}

func (h *Handler) artifactAccess(ctx context.Context, server string) (artifactAccess, error) {
	p, _ := authapi.PrincipalFrom(ctx)
	if p.Token != nil || p.Session.ID == "" || p.User.ID == "" {
		return artifactAccess{}, authapi.ReasonError(connect.CodePermissionDenied, errors.New("sign in to transfer a Docker artifact"), authapi.ReasonNeedsSession)
	}
	if server == "" {
		return artifactAccess{}, authapi.Invalid(errors.New("choose one server for this artifact"))
	}
	servers, err := h.dockerServers(ctx, []string{server})
	if err != nil {
		return artifactAccess{}, err
	}
	session, _ := h.dockerAgent(servers[0])
	if session == nil || !session.ArtifactTransfers() {
		return artifactAccess{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("connect or update this server's agent to transfer an artifact"))
	}
	return artifactAccess{session: session, owner: p.User.ID + "/" + p.Session.ID}, nil
}

func artifactResult(out *agentv1.DockerArtifactControlResult, err error) (*v1.DockerArtifactInfo, error) {
	if err != nil {
		return nil, err
	}
	switch out.GetReason() { //nolint:exhaustive // other image-operation results share the generic transfer refusal
	case v1.DockerImageReason_DOCKER_IMAGE_REASON_UNSPECIFIED:
		if out.GetArtifact() != nil {
			return out.GetArtifact(), nil
		}
	case v1.DockerImageReason_DOCKER_IMAGE_REASON_EXPIRED:
		return nil, connect.NewError(connect.CodeNotFound, artifact.ErrUnavailable)
	case v1.DockerImageReason_DOCKER_IMAGE_REASON_LEASE_LIMIT:
		return nil, connect.NewError(connect.CodeResourceExhausted, artifact.ErrLeaseLimit)
	case v1.DockerImageReason_DOCKER_IMAGE_REASON_TRANSFER_BUSY:
		return nil, authapi.ReasonError(connect.CodeResourceExhausted, artifact.ErrTransferSlots, artifact.TransferSlotsReason)
	case v1.DockerImageReason_DOCKER_IMAGE_REASON_ARTIFACT_PINNED:
		return nil, connect.NewError(connect.CodeFailedPrecondition, artifact.ErrPlanned)
	case v1.DockerImageReason_DOCKER_IMAGE_REASON_QUOTA:
		if space := out.GetSpace(); space != nil {
			return nil, connect.NewError(connect.CodeResourceExhausted, &artifact.SpaceError{Needed: space.GetRequiredBytes(), Available: space.GetAvailableBytes()})
		}
		return nil, connect.NewError(connect.CodeResourceExhausted, artifact.ErrQuota)
	case v1.DockerImageReason_DOCKER_IMAGE_REASON_ARCHIVE_REFUSED:
		return nil, connect.NewError(connect.CodeInvalidArgument, artifact.ErrArchive)
	case v1.DockerImageReason_DOCKER_IMAGE_REASON_STALE:
		return nil, connect.NewError(connect.CodeFailedPrecondition, artifact.ErrOffset)
	}
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("the agent could not verify the artifact transfer"))
}

func (a artifactAccess) begin(ctx context.Context, kind v1.DockerArtifactKind, size uint64, sha string, gzip bool, name string) (*v1.DockerArtifactInfo, error) {
	return artifactResult(a.session.Artifact(ctx, &agentv1.DockerArtifactControlCall{OwnerBinding: a.owner, Operation: &agentv1.DockerArtifactControlCall_BeginUpload{
		BeginUpload: &agentv1.BeginDockerArtifactCall{Kind: kind, SizeBytes: size, Sha256: sha, Gzip: gzip, FileName: name},
	}}, time.Minute))
}

func (a artifactAccess) status(ctx context.Context, id string) (*v1.DockerArtifactInfo, error) {
	if !artifact.ValidID(id) {
		return nil, connect.NewError(connect.CodeNotFound, artifact.ErrUnavailable)
	}
	return artifactResult(a.session.Artifact(ctx, &agentv1.DockerArtifactControlCall{ArtifactId: id, OwnerBinding: a.owner, Operation: &agentv1.DockerArtifactControlCall_Status{Status: &agentv1.DockwellDockerArtifactEmpty{}}}, time.Minute))
}

func (a artifactAccess) abort(ctx context.Context, id string) (*v1.DockerArtifactInfo, error) {
	return artifactResult(a.session.Artifact(ctx, &agentv1.DockerArtifactControlCall{ArtifactId: id, OwnerBinding: a.owner, Operation: &agentv1.DockerArtifactControlCall_Abort{Abort: &agentv1.DockwellDockerArtifactEmpty{}}}, time.Minute))
}

// BeginDockerArtifactUpload reserves a lease on the selected agent.
func (h *Handler) BeginDockerArtifactUpload(ctx context.Context, req *connect.Request[v1.BeginDockerArtifactUploadRequest]) (*connect.Response[v1.DockerArtifactInfo], error) {
	m := req.Msg
	a, err := h.artifactAccess(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	if !artifact.UploadKind(m.GetKind()) {
		return nil, authapi.Invalid(errors.New("choose an image archive, build context or Dockerfile"))
	}
	info, err := a.begin(ctx, m.GetKind(), m.GetSizeBytes(), m.GetSha256(), m.GetGzip(), m.GetFileName())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(info), nil
}

// GetDockerArtifact reads the current committed offset or verified preview.
func (h *Handler) GetDockerArtifact(ctx context.Context, req *connect.Request[v1.GetDockerArtifactRequest]) (*connect.Response[v1.DockerArtifactInfo], error) {
	a, err := h.artifactAccess(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	info, err := a.status(ctx, req.Msg.GetArtifactId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(info), nil
}

// ListDockerArtifacts returns the current session's leases for one server.
func (h *Handler) ListDockerArtifacts(ctx context.Context, req *connect.Request[v1.ListDockerArtifactsRequest]) (*connect.Response[v1.ListDockerArtifactsResponse], error) {
	a, err := h.artifactAccess(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	out, err := a.session.Artifact(ctx, &agentv1.DockerArtifactControlCall{OwnerBinding: a.owner, Operation: &agentv1.DockerArtifactControlCall_List{List: &agentv1.DockwellDockerArtifactEmpty{}}}, time.Minute)
	if err != nil || out.GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_UNSPECIFIED {
		_, err := artifactResult(out, err)
		return nil, err
	}
	return connect.NewResponse(&v1.ListDockerArtifactsResponse{Artifacts: out.GetArtifacts()}), nil
}

// AbortDockerArtifact releases the staged bytes held by this lease.
func (h *Handler) AbortDockerArtifact(ctx context.Context, req *connect.Request[v1.AbortDockerArtifactRequest]) (*connect.Response[v1.DockerArtifactInfo], error) {
	a, err := h.artifactAccess(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	info, err := a.abort(ctx, req.Msg.GetArtifactId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(info), nil
}

// FinishDockerArtifactUpload queues validation of a complete upload.
func (h *Handler) FinishDockerArtifactUpload(ctx context.Context, req *connect.Request[v1.FinishDockerArtifactUploadRequest]) (*connect.Response[v1.DockerArtifactInfo], error) {
	a, err := h.artifactAccess(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	info, err := artifactResult(a.session.Artifact(ctx, &agentv1.DockerArtifactControlCall{
		ArtifactId: req.Msg.GetArtifactId(), OwnerBinding: a.owner, Operation: &agentv1.DockerArtifactControlCall_Seal{Seal: &agentv1.DockwellDockerArtifactEmpty{}},
	}, time.Minute))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(info), nil
}

// UploadArtifactBytes is shared by the raw HTTP and framed RPC adapters.
func (h *Handler) UploadArtifactBytes(ctx context.Context, server, id, prefix string, offset, length uint64, r io.Reader) (*v1.DockerArtifactInfo, error) {
	a, err := h.artifactAccess(ctx, server)
	if err != nil {
		return nil, err
	}
	info, err := a.status(ctx, id)
	if err != nil {
		return nil, err
	}
	if !artifact.UploadKind(info.GetKind()) || info.GetState() != artifact.Receiving || offset != info.GetReceivedBytes() ||
		length == 0 || length > artifact.MaxChunk || length > info.GetSizeBytes()-offset ||
		(offset != 0 || prefix != "") && prefix != info.GetCommittedPrefixSha256() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, artifact.ErrOffset)
	}
	return artifactResult(a.session.UploadArtifact(ctx, id, a.owner, offset, length, r))
}

type uploadArtifactReader struct {
	in   *connect.ClientStream[v1.UploadDockerArtifactRequest]
	data []byte
}

func (r *uploadArtifactReader) Read(p []byte) (int, error) {
	for len(r.data) == 0 {
		if !r.in.Receive() {
			if err := r.in.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		msg := r.in.Msg()
		r.data = msg.GetContent().GetData()
		if len(r.data) > artifact.Frame || msg.GetServerId() != "" || msg.GetArtifactId() != "" || msg.GetOffset() != 0 {
			return 0, errors.New("an artifact frame must carry only up to 128 KiB of bytes")
		}
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// UploadDockerArtifact receives one extent over framed RPC.
func (h *Handler) UploadDockerArtifact(ctx context.Context, in *connect.ClientStream[v1.UploadDockerArtifactRequest]) (*connect.Response[v1.DockerArtifactInfo], error) {
	n, err := strconv.ParseUint(in.RequestHeader().Get(ArtifactLengthHeader), 10, 64)
	if err != nil {
		return nil, authapi.Invalid(errors.New("supply the length of this upload segment"))
	}
	if !in.Receive() {
		return nil, authapi.Invalid(errors.New("name the artifact and offset"))
	}
	first := in.Msg()
	if len(first.GetContent().GetData()) != 0 {
		return nil, authapi.Invalid(errors.New("send the artifact and offset before its bytes"))
	}
	info, err := h.UploadArtifactBytes(ctx, first.GetServerId(), first.GetArtifactId(), in.RequestHeader().Get(ArtifactPrefixHeader), first.GetOffset(), n, &uploadArtifactReader{in: in})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(info), nil
}

// ArtifactDownload binds the immutable download to the calling actor and session.
func (h *Handler) ArtifactDownload(ctx context.Context, server, id string) (*v1.DockerArtifactInfo, func(uint64, uint64, io.Writer) error, error) {
	a, err := h.artifactAccess(ctx, server)
	if err != nil {
		return nil, nil, err
	}
	info, err := a.status(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if info.GetKind() != artifact.Export || info.GetState() != artifact.Ready {
		return nil, nil, connect.NewError(connect.CodeNotFound, artifact.ErrUnavailable)
	}
	return info, func(off, n uint64, w io.Writer) error {
		if off > info.GetSizeBytes() || n > info.GetSizeBytes()-off {
			return authapi.Invalid(artifact.ErrOffset)
		}
		return a.session.DownloadArtifact(ctx, id, a.owner, off, n, w)
	}, nil
}

// DownloadDockerArtifact sends a range of sealed bytes over framed RPC.
func (h *Handler) DownloadDockerArtifact(ctx context.Context, req *connect.Request[v1.DownloadDockerArtifactRequest], out *connect.ServerStream[v1.DownloadDockerArtifactResponse]) error {
	m := req.Msg
	info, send, err := h.ArtifactDownload(ctx, m.GetServerId(), m.GetArtifactId())
	if err != nil {
		return err
	}
	offset, length := m.GetOffset(), m.GetLength()
	if offset > info.GetSizeBytes() {
		return authapi.Invalid(artifact.ErrOffset)
	}
	if length == 0 {
		length = info.GetSizeBytes() - offset
	}
	out.ResponseHeader().Set("ETag", strconv.Quote(info.GetSha256()))
	return send(offset, length, artifactWriter(func(p []byte) error {
		return out.Send(&v1.DownloadDockerArtifactResponse{Body: &httpbody.HttpBody{ContentType: "application/x-tar", Data: p}})
	}))
}

type artifactWriter func([]byte) error

func (w artifactWriter) Write(p []byte) (int, error) {
	if err := w(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
