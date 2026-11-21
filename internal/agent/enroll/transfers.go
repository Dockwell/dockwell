// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"context"
	"fmt"
	"net/http"
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

// noTransferGzip has a transfer's client ask for its parts as they are. What's
// uploaded and downloaded is mostly packed already, as archives, images and media
// are, and packing it again cost the control plane and the agent each a CPU's
// worth of work on every part for little or nothing saved.
var noTransferGzip = connect.WithAcceptCompression("gzip", nil, nil)

// transfers is the client uploads and downloads go over. Its ReceiveFile hands
// each part's bytes over in one buffer the stream keeps, so what a part holds is
// good until the stream's next Receive, which is how an upload writes it.
type transfers struct {
	agentv1connect.AgentServiceClient
	hc  *http.Client
	url string
}

func newTransfers(hc *http.Client, controlURL string) *transfers {
	return &transfers{
		AgentServiceClient: agentv1connect.NewAgentServiceClient(hc, controlURL, connect.WithGRPC(), noTransferGzip),
		hc:                 hc,
		// As the generated client does: an address that ends in a slash would
		// otherwise name //dockwell.agent.v1..., which a mux redirects, and a 301
		// turns the call into a GET.
		url: strings.TrimRight(controlURL, "/"),
	}
}

// ReceiveFile brings an upload's parts, each in the buffer the one before it was
// in: a part's bytes are only good until the next Receive.
func (t *transfers) ReceiveFile(ctx context.Context, req *connect.Request[agentv1.ReceiveFileRequest]) (*connect.ServerStreamForClient[agentv1.ReceiveFileResponse], error) {
	client := connect.NewClient[agentv1.ReceiveFileRequest, agentv1.ReceiveFileResponse](
		t.hc, t.url+agentv1connect.AgentServiceReceiveFileProcedure,
		connect.WithGRPC(), noTransferGzip, connect.WithCodec(&partCodec{}),
	)
	return client.CallServerStream(ctx, req)
}

// partCodec is the protocol buffers codec of one ReceiveFile stream, which reads
// each part's bytes into the one buffer it keeps rather than a new one a part.
// Anything else it reads as proto does.
type partCodec struct {
	buf []byte
}

func (*partCodec) Name() string { return "proto" }

func (*partCodec) Marshal(message any) ([]byte, error) {
	m, ok := message.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("%T isn't a protocol buffers message", message)
	}
	return proto.Marshal(m)
}

func (*partCodec) MarshalAppend(dst []byte, message any) ([]byte, error) {
	m, ok := message.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("%T isn't a protocol buffers message", message)
	}
	return proto.MarshalOptions{}.MarshalAppend(dst, m)
}

func (t *transfers) SendFile(ctx context.Context) *connect.ClientStreamForClient[agentv1.SendFileRequest, agentv1.SendFileResponse] {
	client := connect.NewClient[agentv1.SendFileRequest, agentv1.SendFileResponse](
		t.hc, t.url+agentv1connect.AgentServiceSendFileProcedure,
		connect.WithGRPC(), noTransferGzip, connect.WithCodec(&partCodec{}),
	)
	return client.CallClientStream(ctx)
}

func (c *partCodec) Unmarshal(data []byte, message any) error {
	part, ok := message.(*agentv1.ReceiveFileResponse)
	if !ok {
		m, ok := message.(proto.Message)
		if !ok {
			return fmt.Errorf("%T isn't a protocol buffers message", message)
		}
		return proto.Unmarshal(data, m)
	}
	got, ok := partData(data)
	if !ok {
		// Another field, or something malformed: proto says what it is.
		return proto.Unmarshal(data, part)
	}
	c.buf = append(c.buf[:0], got...)
	// Nothing an earlier read left in the message stays, as with proto.
	proto.Reset(part)
	part.Data = c.buf
	return nil
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
