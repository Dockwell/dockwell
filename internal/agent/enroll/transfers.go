// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
)

// What an upload's parts cost the agent (#883). Four uploads at once took it
// 7 MB past its memory at rest, nearly none of it live: each part came packed
// with gzip and was unpacked into buffers of its own, then copied again into a
// message of its own, so every byte uploaded was garbage more than once and the
// collector ran nearly a thousand times in a few seconds, falling behind as it
// went. So a transfer's parts come as they are, and an upload's land in one
// buffer its stream keeps.
//
// connect read each part into a buffer of its own first, one that starts at 512
// bytes and doubles until the part fits, from a pool each call makes anew and the
// collector empties: four uploads at once still made 45 MB of garbage in 13 s
// that way. So an upload's stream is read here, and its parts land in its buffer
// straight from the connection (docs/adr/0109-the-agents-memory-headroom.md).

// noTransferGzip has a transfer's client ask for its parts as they are. What's
// uploaded and downloaded is mostly packed already, as archives, images and media
// are, and packing it again cost the control plane and the agent each a CPU's
// worth of work on every part for little or nothing saved.
var noTransferGzip = connect.WithAcceptCompression("gzip", nil, nil)

// Transfers is the client uploads and downloads go over. Its ReceiveParts hands
// each part's bytes over in one buffer the stream keeps, so what a part holds is
// good until the stream's next part, which is how an upload writes it.
type Transfers struct {
	agentv1connect.AgentServiceClient
	hc  *http.Client
	url string
}

func newTransfers(hc *http.Client, controlURL string) *Transfers {
	return &Transfers{
		AgentServiceClient: agentv1connect.NewAgentServiceClient(hc, controlURL, connect.WithGRPC(), noTransferGzip),
		hc:                 hc,
		// As the generated client does: an address that ends in a slash would
		// otherwise name //dockwell.agent.v1..., which a mux redirects, and a 301
		// turns the call into a GET.
		url: strings.TrimRight(controlURL, "/"),
	}
}

// maxPart bounds a part's memory and leaves room for older control planes'
// 128 KiB frames and their message fields.
const maxPart = 1 << 20

// ReceiveParts calls ReceiveFile, as gRPC does and without gzip, and returns the
// upload's parts as they come. Its errors are connect's, with the code and the
// message the control plane sent, as connect's own client gives them.
func (t *Transfers) ReceiveParts(ctx context.Context, req *agentv1.ReceiveFileRequest) (*Parts, error) {
	msg, err := proto.Marshal(req)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	body := make([]byte, 5, 5+len(msg))
	binary.BigEndian.PutUint32(body[1:], uint32(len(msg))) //nolint:gosec // a request of a few bytes
	body = append(body, msg...)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url+agentv1connect.AgentServiceReceiveFileProcedure, bytes.NewReader(body))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	hreq.Header.Set("Content-Type", "application/grpc+proto")
	hreq.Header.Set("Te", "trailers")
	res, err := t.hc.Do(hreq)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		return nil, connect.NewError(httpCode(res.StatusCode), fmt.Errorf("HTTP status %v", res.Status))
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/grpc") {
		_ = res.Body.Close()
		return nil, connect.NewError(connect.CodeUnknown, fmt.Errorf("invalid content-type: %q", ct))
	}
	return &Parts{ctx: ctx, res: res}, nil
}

// Parts is an upload's parts, as ReceiveParts reads them.
type Parts struct {
	ctx    context.Context //nolint:containedctx // the call's, to tell a cancel from a cut
	res    *http.Response
	prefix [5]byte
	buf    []byte
	err    error
}

// Next returns the next part's bytes, which are good until the next call, and
// io.EOF once the control plane has ended the stream as it should.
func (p *Parts) Next() ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	data, err := p.next()
	if err != nil {
		p.err = err
	}
	return data, err
}

func (p *Parts) next() ([]byte, error) {
	if _, err := io.ReadFull(p.res.Body, p.prefix[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, p.ended()
		}
		return nil, cutShort(p.ctx, "incomplete envelope", err)
	}
	if p.prefix[0] != 0 {
		// 1 is a packed message, which this client never asks for.
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("protocol error: a message with flags %#x", p.prefix[0]))
	}
	size := binary.BigEndian.Uint32(p.prefix[1:])
	if size > maxPart {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("message size %d is larger than %d", size, maxPart))
	}
	if cap(p.buf) < int(size) {
		p.buf = make([]byte, size)
	}
	b := p.buf[:size]
	if _, err := io.ReadFull(p.res.Body, b); err != nil {
		return nil, cutShort(p.ctx, fmt.Sprintf("promised %d bytes in enveloped message", size), err)
	}
	return readPart(b)
}

// ended reads how the stream ended, from its trailers, or from its headers when
// it ended before it began.
func (p *Parts) ended() error {
	status := p.res.Trailer.Get("Grpc-Status")
	message := p.res.Trailer.Get("Grpc-Message")
	if status == "" {
		status, message = p.res.Header.Get("Grpc-Status"), p.res.Header.Get("Grpc-Message")
	}
	code, err := strconv.ParseUint(status, 10, 32)
	switch {
	case err != nil:
		return connect.NewError(connect.CodeInternal, fmt.Errorf("protocol error: grpc-status %q", status))
	case code == 0:
		return io.EOF
	}
	return connect.NewError(connect.Code(code), errors.New(percentDecode(message)))
}

// Close ends the call.
func (p *Parts) Close() error {
	return p.res.Body.Close()
}

// readPart is what a ReceiveFileResponse holds in data. The usual message, data and
// nothing else, is read in place; anything else reads as proto reads it.
func readPart(b []byte) ([]byte, error) {
	if got, ok := partData(b); ok {
		return got, nil
	}
	var part agentv1.ReceiveFileResponse
	if err := proto.Unmarshal(b, &part); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unmarshal message: %w", err))
	}
	return part.GetData(), nil
}

// partData is what a ReceiveFileResponse holds in data, when that's all it holds.
// The last one counts, as it does for proto.
func partData(b []byte) ([]byte, bool) {
	var got []byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || num != 1 || typ != protowire.BytesType {
			return nil, false
		}
		v, m := protowire.ConsumeBytes(b[n:])
		if m < 0 {
			return nil, false
		}
		got, b = v, b[n+m:]
	}
	return got, true
}

// connectError is a failed call's error, as connect's client gives it.
func connectError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	case ctx.Err() != nil:
		return connect.NewError(connect.CodeCanceled, err)
	}
	return connect.NewError(connect.CodeUnavailable, err)
}

// cutShort is the error of a stream that stopped mid-message.
func cutShort(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return connectError(ctx, err)
	}
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf("protocol error: %s: %w", what, err))
}

// httpCode is gRPC's code for an HTTP status other than 200
// (https://github.com/grpc/grpc/blob/master/doc/http-grpc-status-mapping.md).
func httpCode(status int) connect.Code {
	switch status {
	case http.StatusBadRequest:
		return connect.CodeInternal
	case http.StatusUnauthorized:
		return connect.CodeUnauthenticated
	case http.StatusForbidden:
		return connect.CodePermissionDenied
	case http.StatusNotFound:
		return connect.CodeUnimplemented
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return connect.CodeUnavailable
	}
	return connect.CodeUnknown
}

// percentDecode reads grpc-message, which escapes what isn't printable ASCII,
// and %, as %XX. A broken escape stays as it came.
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				out.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}
