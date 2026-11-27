// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/agent/enroll"
)

func TestStalledFileUploadLeavesArtifactReceiveRunning(t *testing.T) {
	const (
		fileStream     = 1
		artifactStream = 2
		artifactParts  = 8
		owner          = "project"
	)
	frame := bytes.Repeat([]byte("a"), artifact.Frame)
	peers := make(chan string, 2)
	fileDone := make(chan struct{})
	handler := connect.NewServerStreamHandler(agentv1connect.AgentServiceReceiveFileProcedure,
		func(_ context.Context, req *connect.Request[agentv1.ReceiveFileRequest], out *connect.ServerStream[agentv1.ReceiveFileResponse]) error {
			peers <- req.Peer().Addr
			count := artifactParts
			if req.Msg.GetStream() == fileStream {
				defer close(fileDone)
				count = 64
			}
			for range count {
				if err := out.Send(&agentv1.ReceiveFileResponse{Data: frame}); err != nil {
					return err
				}
			}
			return nil
		})
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	spool, err := artifact.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := calls{cfg: Config{Identity: &enroll.Identity{ControlURL: server.URL, CA: server.Certificate()}, artifacts: spool}}
	defer c.dropTransfers()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	file, err := c.transferClient(false)
	if err != nil {
		t.Fatal(err)
	}
	upload, err := file.ReceiveParts(ctx, &agentv1.ReceiveFileRequest{Stream: fileStream})
	if err != nil {
		t.Fatal(err)
	}
	// Leave the body unread, as when an upload waits on a disk write.
	defer upload.Close()
	filePeer := <-peers

	const size = artifactParts * artifact.Frame
	lease, err := spool.Begin(owner, artifact.Import, size, "", false, "image.tar")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.receiveDockerArtifact(ctx, lease.GetArtifactId(), owner, &agentv1.DockerArtifactStream{Stream: artifactStream, Length: size})
	if err != nil {
		t.Fatal(err)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256(bytes.Repeat(frame, artifactParts)))
	if got.GetReceivedBytes() != size || got.GetCommittedPrefixSha256() != wantHash {
		t.Fatalf("artifact received %d bytes with hash %s, want %d bytes with hash %s", got.GetReceivedBytes(), got.GetCommittedPrefixSha256(), size, wantHash)
	}
	if artifactPeer := <-peers; artifactPeer == filePeer {
		t.Fatal("artifact receive shared the stalled upload's HTTP/2 connection")
	}
	select {
	case <-fileDone:
		t.Fatal("file upload ended before the artifact check")
	default:
	}
}
