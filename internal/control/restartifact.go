// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/containers"
)

func streamArtifacts(mux *http.ServeMux, h *containers.Handler, guard func(http.Handler) http.Handler, authorize func(context.Context, string) (context.Context, func(), error)) {
	wrap := func(procedure string, next http.HandlerFunc) http.Handler {
		return guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, stop, err := authorize(r.Context(), procedure)
			if err != nil {
				artifactRESTError(w, err)
				return
			}
			defer stop()
			next(w, r.WithContext(ctx))
		}))
	}
	mux.Handle("PUT /v1/docker/artifacts/{server_id}/{artifact_id}/bytes", wrap(dockwellv1connect.DockerServiceUploadDockerArtifactProcedure, func(w http.ResponseWriter, r *http.Request) {
		offset, err := strconv.ParseUint(r.URL.Query().Get("offset"), 10, 64)
		if err != nil || r.ContentLength <= 0 || r.ContentLength > artifact.MaxChunk {
			artifactRESTError(w, connect.NewError(connect.CodeInvalidArgument, errors.New("give an offset and a length of up to 64 MiB")))
			return
		}
		control := http.NewResponseController(w)
		defer func() { _ = control.SetReadDeadline(time.Time{}) }()
		info, err := h.UploadArtifactBytes(r.Context(), r.PathValue("server_id"), r.PathValue("artifact_id"), r.Header.Get(containers.ArtifactPrefixHeader), offset, uint64(r.ContentLength), artifactBody{Reader: r.Body, control: control})
		if err != nil {
			artifactRESTError(w, err)
			return
		}
		b, err := protojson.Marshal(info)
		if err != nil {
			artifactRESTError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		_, _ = w.Write(b) //nolint:gosec // a typed protobuf encoded as application/json
	}))
	mux.Handle("GET /v1/docker/artifacts/{server_id}/{artifact_id}/bytes", wrap(dockwellv1connect.DockerServiceDownloadDockerArtifactProcedure, func(w http.ResponseWriter, r *http.Request) {
		info, send, err := h.ArtifactDownload(r.Context(), r.PathValue("server_id"), r.PathValue("artifact_id"))
		if err != nil {
			artifactRESTError(w, err)
			return
		}
		size := info.GetSizeBytes()
		etag := strconv.Quote(info.GetSha256())
		offset, length := uint64(0), size
		rangeHeader := r.Header.Get("Range")
		code := http.StatusOK
		if ifRange := r.Header.Get("If-Range"); rangeHeader != "" && (ifRange == "" || ifRange == etag) {
			var ok bool
			offset, length, ok = artifactRange(rangeHeader, size)
			if !ok {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			code = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, size))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Length", strconv.FormatUint(length, 10))
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", info.GetFileName()))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(code)
		if r.Method == http.MethodHead {
			return
		}
		out := &stallWriter{ResponseWriter: w, control: http.NewResponseController(w)}
		if err := send(offset, length, out); err != nil {
			panic(http.ErrAbortHandler)
		}
	}))
}

type artifactBody struct {
	io.Reader
	control *http.ResponseController
}

func (r artifactBody) Read(p []byte) (int, error) {
	_ = r.control.SetReadDeadline(time.Now().Add(transferStall))
	return r.Reader.Read(p)
}

func artifactRange(value string, size uint64) (offset, length uint64, ok bool) {
	ranges, yes := strings.CutPrefix(value, "bytes=")
	first, last, cut := strings.Cut(ranges, "-")
	if !yes || !cut || size == 0 {
		return 0, 0, false
	}
	if first == "" {
		n, err := strconv.ParseUint(last, 10, 64)
		if err != nil || n == 0 {
			return 0, 0, false
		}
		n = min(size, n)
		return size - n, n, true
	}
	begin, err := strconv.ParseUint(first, 10, 64)
	if err != nil || begin >= size {
		return 0, 0, false
	}
	end := size - 1
	if last != "" {
		n, err := strconv.ParseUint(last, 10, 64)
		if err != nil || n < begin {
			return 0, 0, false
		}
		end = min(end, n)
	}
	return begin, end - begin + 1, true
}

func artifactRESTError(w http.ResponseWriter, err error) {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		restRefuse(w, err)
		return
	}
	var status int
	switch cerr.Code() {
	case connect.CodeInvalidArgument:
		status = http.StatusBadRequest
	case connect.CodeNotFound:
		status = http.StatusNotFound
	case connect.CodeFailedPrecondition:
		status = http.StatusPreconditionFailed
	default:
		restRefuse(w, err)
		return
	}
	authapi.RESTError(w, status, cerr.Code(), cerr.Meta().Get(authapi.ReasonKey), cerr.Message())
}
