// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"unsafe"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
)

// parts sends three upload parts, and would pack each with gzip if the agent
// asked for it.
type parts struct {
	agentv1connect.UnimplementedAgentServiceHandler
	sent [][]byte
}

func (p *parts) ReceiveFile(_ context.Context, _ *connect.Request[agentv1.ReceiveFileRequest], out *connect.ServerStream[agentv1.ReceiveFileResponse]) error {
	for _, b := range p.sent {
		if err := out.Send(&agentv1.ReceiveFileResponse{Data: b}); err != nil {
			return err
		}
	}
	return nil
}

// An upload's parts come as they are, not packed with gzip, and each lands in the
// one buffer its stream keeps, so four uploads at once don't make the agent's
// collector run a thousand times in a few seconds (#883). An address that ends in
// a slash reaches the same path, as the generated client's does.
func TestAnUploadsPartsComeUnpackedIntoOneBuffer(t *testing.T) {
	p := &parts{}
	for _, c := range "abc" {
		p.sent = append(p.sent, bytes.Repeat([]byte{byte(c)}, 128<<10))
	}
	path, handler := agentv1connect.NewAgentServiceHandler(p, connect.WithCompressMinBytes(1))
	var mu sync.Mutex
	var seen []string
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Grpc-Accept-Encoding"))
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	for _, base := range []string{srv.URL, srv.URL + "/", srv.URL + "//"} {
		mu.Lock()
		seen = nil
		mu.Unlock()
		stream, err := newTransfers(srv.Client(), base).ReceiveFile(t.Context(), connect.NewRequest(&agentv1.ReceiveFileRequest{Stream: 7}))
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		var first *byte
		got := 0
		for stream.Receive() {
			data := stream.Msg().GetData()
			if got >= len(p.sent) || !bytes.Equal(data, p.sent[got]) {
				t.Fatalf("%s: part %d isn't what was sent", base, got)
			}
			if got == 0 {
				first = unsafe.SliceData(data)
			} else if unsafe.SliceData(data) != first {
				t.Errorf("%s: part %d came in a buffer of its own", base, got)
			}
			got++
		}
		if err := stream.Err(); err != nil || got != len(p.sent) {
			t.Fatalf("%s: %d of %d parts: %v", base, got, len(p.sent), err)
		}
		_ = stream.Close()
		mu.Lock()
		want := http.MethodPost + " " + agentv1connect.AgentServiceReceiveFileProcedure + " "
		if len(seen) != 1 || seen[0] != want {
			t.Errorf("from %s the server saw %q; want one %q, with no gzip asked for", base, seen, want)
		}
		mu.Unlock()
	}
}

// What isn't only a part's bytes reads as proto reads it, and what's malformed
// fails as it does there.
func TestAPartReadsAsProtoDoes(t *testing.T) {
	c := &partCodec{}
	// A field this agent doesn't know is kept, and the bytes are still read.
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte("abc"))
	b = protowire.AppendTag(b, 9, protowire.VarintType)
	b = protowire.AppendVarint(b, 5)
	var m agentv1.ReceiveFileResponse
	if err := c.Unmarshal(b, &m); err != nil || string(m.GetData()) != "abc" || len(m.ProtoReflect().GetUnknown()) == 0 {
		t.Errorf("with a field it doesn't know: %q, %v", m.GetData(), err)
	}
	// The last of a repeated field counts.
	b = protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte("old"))
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte("new"))
	if err := c.Unmarshal(b, &m); err != nil || string(m.GetData()) != "new" {
		t.Errorf("twice: %q, %v", m.GetData(), err)
	}
	// Into the same message, what the read before left is gone.
	if unknown := m.ProtoReflect().GetUnknown(); len(unknown) != 0 {
		t.Errorf("the field the read before didn't know stayed: %x", unknown)
	}
	// Cut short.
	if err := c.Unmarshal(b[:len(b)-1], &m); err == nil {
		t.Error("a part cut short read")
	}
	// Another message reads as proto reads it.
	want := &agentv1.ReceiveFileRequest{Stream: 42}
	enc, err := c.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got agentv1.ReceiveFileRequest
	if err := c.Unmarshal(enc, &got); err != nil || !proto.Equal(&got, want) {
		t.Errorf("another message = %v, %v", &got, err)
	}
}
