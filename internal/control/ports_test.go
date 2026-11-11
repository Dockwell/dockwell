// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"net"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
)

// A form asks a server's agent what holds a port, only when someone asks: an
// admin's read, as what holds a server's ports is the install's (ADR 0048).
func TestServerPortsAreCheckedThroughTheAgent(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	id := f.lastDocker.id.ServerID
	sc := dockwellv1connect.NewServerServiceClient(b, b.origin, connect.WithProtoJSON())

	// Something on this machine holds a port: the test, listening.
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	held := uint32(l.Addr().(*net.TCPAddr).Port)
	res, err := sc.CheckServerPorts(t.Context(), connect.NewRequest(&dockwellv1.CheckServerPortsRequest{
		Id: id, Ports: []*dockwellv1.ServerPortAsk{{Port: held}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetPorts(); len(got) != 1 || !got[0].GetHeld() || got[0].GetPort() != held {
		t.Errorf("a port this machine listens on reads %v, want held", got)
	}

	_, err = sc.CheckServerPorts(t.Context(), connect.NewRequest(&dockwellv1.CheckServerPortsRequest{Id: id, Ports: []*dockwellv1.ServerPortAsk{{Port: 70000}}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "isn't a port") {
		t.Errorf("a port that isn't one: %v", err)
	}
	_, err = sc.CheckServerPorts(t.Context(), connect.NewRequest(&dockwellv1.CheckServerPortsRequest{Id: "srv_nope", Ports: []*dockwellv1.ServerPortAsk{{Port: 80}}}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown server: %v, want NotFound", err)
	}

	hold := aMember(t, f)
	hold(onInstall(store.RoleDeveloper))
	_, err = sc.CheckServerPorts(t.Context(), connect.NewRequest(&dockwellv1.CheckServerPortsRequest{Id: id, Ports: []*dockwellv1.ServerPortAsk{{Port: 80}}}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a developer checking a server's ports: %v, want PermissionDenied", err)
	}
	hold(onInstall(store.RoleAdmin))

	f.lastDocker.stop()
	waitConnected(t, f, id, false)
	_, err = sc.CheckServerPorts(t.Context(), connect.NewRequest(&dockwellv1.CheckServerPortsRequest{Id: id, Ports: []*dockwellv1.ServerPortAsk{{Port: 80}}}))
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "isn't connected") {
		t.Errorf("a server whose agent has gone: %v, want Unavailable, saying why", err)
	}
}
