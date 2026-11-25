// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"unsafe"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
)

// parts sends three upload parts, and would pack each with gzip if the agent
// asked for it, then ends as end says.
type parts struct {
	agentv1connect.UnimplementedAgentServiceHandler
	sent [][]byte
	end  error
}

func (p *parts) ReceiveFile(_ context.Context, _ *connect.Request[agentv1.ReceiveFileRequest], out *connect.ServerStream[agentv1.ReceiveFileResponse]) error {
	for _, b := range p.sent {
		if err := out.Send(&agentv1.ReceiveFileResponse{Data: b}); err != nil {
			return err
		}
	}
	return p.end
}

// serveParts serves p over HTTP/2 with TLS, as the agent listener does, and
// records each request's method, path and Grpc-Accept-Encoding.
func serveParts(t *testing.T, p *parts) (*httptest.Server, func() []string) {
	t.Helper()
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
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		got := seen
		seen = nil
		return got
	}
}

// An upload's parts come as they are, not packed with gzip, and each lands in the
// one buffer its stream keeps, so four uploads at once don't make the agent's
// collector run a thousand times in a few seconds (#883, ADR 0109). An address
// that ends in a slash reaches the same path, as the generated client's does.
func TestAnUploadsPartsComeUnpackedIntoOneBuffer(t *testing.T) {
	p := &parts{}
	for _, c := range "abc" {
		p.sent = append(p.sent, bytes.Repeat([]byte{byte(c)}, 128<<10))
	}
	srv, seen := serveParts(t, p)
	for _, base := range []string{srv.URL, srv.URL + "/", srv.URL + "//"} {
		stream, err := newTransfers(srv.Client(), base).ReceiveParts(t.Context(), &agentv1.ReceiveFileRequest{Stream: 7})
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		var first *byte
		got := 0
		for {
			data, err := stream.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: part %d: %v", base, got, err)
			}
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
		if got != len(p.sent) {
			t.Fatalf("%s: %d of %d parts", base, got, len(p.sent))
		}
		if _, err := stream.Next(); !errors.Is(err, io.EOF) {
			t.Errorf("%s: after the end, Next says %v, want io.EOF again", base, err)
		}
		_ = stream.Close()
		want := http.MethodPost + " " + agentv1connect.AgentServiceReceiveFileProcedure + " "
		if got := seen(); len(got) != 1 || got[0] != want {
			t.Errorf("from %s the server saw %q; want one %q, with no gzip asked for", base, got, want)
		}
	}
}

// A stream the control plane ends with an error, before its first part or after
// some, says the code and the message it sent, as connect's client does.
func TestAnUploadEndsAsTheControlPlaneSays(t *testing.T) {
	for _, n := range []int{0, 2} {
		p := &parts{end: connect.NewError(connect.CodeNotFound, errors.New("nobody is sending this upload: 100% gone, é"))}
		for range n {
			p.sent = append(p.sent, []byte("part"))
		}
		srv, _ := serveParts(t, p)
		stream, err := newTransfers(srv.Client(), srv.URL).ReceiveParts(t.Context(), &agentv1.ReceiveFileRequest{Stream: 7})
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for {
			if _, err = stream.Next(); err != nil {
				break
			}
			got++
		}
		_ = stream.Close()
		if got != n || connect.CodeOf(err) != connect.CodeNotFound || err.Error() != "not_found: nobody is sending this upload: 100% gone, é" {
			t.Errorf("after %d parts: %d parts, then %v (%v)", n, got, err, connect.CodeOf(err))
		}
	}
}

// What isn't a gRPC answer, or is cut short, or holds a part far past a frame,
// fails as an error and doesn't make the agent take the memory it claims.
func TestAnUploadThatIsntOneFails(t *testing.T) {
	envelope := func(size uint32, b []byte) []byte {
		out := []byte{0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(out[1:], size)
		return append(out, b...)
	}
	for _, c := range []struct {
		name   string
		status int
		ct     string
		body   []byte
		code   connect.Code
		begin  bool
	}{
		{"a 404", http.StatusNotFound, "text/plain", nil, connect.CodeUnimplemented, true},
		{"not gRPC", http.StatusOK, "text/html", nil, connect.CodeUnknown, true},
		{"no status", http.StatusOK, "application/grpc", nil, connect.CodeInternal, false},
		{"a part cut short", http.StatusOK, "application/grpc", envelope(10, []byte("abc")), connect.CodeInternal, false},
		{"a prefix cut short", http.StatusOK, "application/grpc", []byte{0, 0}, connect.CodeInternal, false},
		{"a part too large", http.StatusOK, "application/grpc", envelope(maxPart+1, nil), connect.CodeResourceExhausted, false},
		{"a packed part", http.StatusOK, "application/grpc", append([]byte{1}, envelope(0, nil)[1:]...), connect.CodeInternal, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.ct)
				w.WriteHeader(c.status)
				_, _ = w.Write(c.body)
			}))
			srv.EnableHTTP2 = true
			srv.StartTLS()
			t.Cleanup(srv.Close)
			stream, err := newTransfers(srv.Client(), srv.URL).ReceiveParts(t.Context(), &agentv1.ReceiveFileRequest{Stream: 7})
			if c.begin {
				if err == nil || connect.CodeOf(err) != c.code {
					t.Fatalf("ReceiveParts = %v, want %v", err, c.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if _, err := stream.Next(); err == nil || connect.CodeOf(err) != c.code {
				t.Errorf("Next = %v, want %v", err, c.code)
			}
		})
	}
}

// What isn't only a part's bytes reads as proto reads it, and what's malformed
// fails as it does there.
func TestAPartReadsAsProtoDoes(t *testing.T) {
	// A field this agent doesn't know is skipped, and the bytes are still read.
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte("abc"))
	b = protowire.AppendTag(b, 9, protowire.VarintType)
	b = protowire.AppendVarint(b, 5)
	if got, err := readPart(b); err != nil || string(got) != "abc" {
		t.Errorf("with a field it doesn't know: %q, %v", got, err)
	}
	// The last of a repeated field counts.
	b = protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte("old"))
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte("new"))
	if got, err := readPart(b); err != nil || string(got) != "new" {
		t.Errorf("twice: %q, %v", got, err)
	}
	// Cut short.
	if _, err := readPart(b[:len(b)-1]); err == nil {
		t.Error("a part cut short read")
	}
	// An empty message is an empty part, as proto reads it.
	if got, err := readPart(nil); err != nil || len(got) != 0 {
		t.Errorf("empty: %q, %v", got, err)
	}
}
