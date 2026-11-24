// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestSocketsAndProcessDetailRPCs(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		server := f.lastDocker.id.ServerID
		pc := dockwellv1connect.NewProcessServiceClient(b, b.origin, connect.WithProtoJSON())

		// Open a listening TCP socket in this process so ListSockets and GetProcess see it.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		listenPort := uint32(ln.Addr().(*net.TCPAddr).Port)

		socketsRes, err := pc.ListSockets(t.Context(), connect.NewRequest(&dockwellv1.ListSocketsRequest{ServerId: server}))
		if err != nil {
			t.Fatal(err)
		}
		foundSocket := false
		for _, c := range socketsRes.Msg.GetConnections() {
			if c.GetLocalPort() == listenPort && c.GetState() == "LISTEN" && c.GetPid() == uint32(os.Getpid()) {
				foundSocket = true
				break
			}
		}
		if !foundSocket {
			t.Errorf("ListSockets did not find our listening socket on port %d (PID %d); total=%d", listenPort, os.Getpid(), socketsRes.Msg.GetTotal())
		}

		// Spawn a child with secret flags and KEY=value arguments and an env secret.
		cmd := exec.Command("sh", "-c", "sleep 60 # --api-key super_secret_key DB_PASSWORD=hunter2")
		cmd.Env = append(os.Environ(), "MY_ENV_SECRET=never_show_this_env")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		pid := uint32(cmd.Process.Pid)

		detailRes, err := pc.GetProcess(t.Context(), connect.NewRequest(&dockwellv1.GetProcessRequest{
			ServerId: server,
			Pid:      pid,
		}))
		if err != nil {
			t.Fatal(err)
		}
		d := detailRes.Msg.GetProcess()
		full := d.GetFullCommand()
		if strings.Contains(full, "super_secret_key") || strings.Contains(full, "hunter2") || strings.Contains(full, "never_show_this_env") {
			t.Errorf("GetProcess leaked a secret: %q", full)
		}
		if !strings.Contains(full, "--api-key …") || !strings.Contains(full, "DB_PASSWORD=…") {
			t.Errorf("GetProcess full_command = %q, want redacted flags", full)
		}
		if d.GetThreads() == 0 || d.GetProcess().GetParentPid() != uint32(os.Getpid()) {
			t.Errorf("GetProcess metadata: threads=%d ppid=%d", d.GetThreads(), d.GetProcess().GetParentPid())
		}

		// GetProcess on our own PID sees the listening socket we opened.
		selfDetail, err := pc.GetProcess(t.Context(), connect.NewRequest(&dockwellv1.GetProcessRequest{
			ServerId: server,
			Pid:      uint32(os.Getpid()),
		}))
		if err != nil {
			t.Fatal(err)
		}
		hasOwnSocket := false
		for _, s := range selfDetail.Msg.GetProcess().GetSockets() {
			if s.GetLocalPort() == listenPort && s.GetState() == "LISTEN" {
				hasOwnSocket = true
				break
			}
		}
		if !hasOwnSocket {
			t.Errorf("GetProcess(self) did not list our open listening socket on port %d", listenPort)
		}

		// API tokens must be refused with needs_session.
		tok := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		tc := dockwellv1connect.NewProcessServiceClient(bearer{token: tok, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
		if _, err := tc.ListSockets(t.Context(), connect.NewRequest(&dockwellv1.ListSocketsRequest{ServerId: server})); reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("ListSockets with token = %v, want needs_session", err)
		}
		if _, err := tc.GetProcess(t.Context(), connect.NewRequest(&dockwellv1.GetProcessRequest{ServerId: server, Pid: pid})); reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("GetProcess with token = %v, want needs_session", err)
		}
	})
}
