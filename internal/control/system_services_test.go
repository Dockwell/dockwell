// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/system"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/servers"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/store"
)

// addSystemServer adds a systemd server whose systemctl is a stand-in that prints
// the agent's fixtures, and says how many lists it was asked for.
func (f *fullServer) addSystemServer(t *testing.T, name string) (*testServer, func() int) {
	t.Helper()
	root := t.TempDir()
	fixtures, err := filepath.Abs("../agent/system/testdata/systemd")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "run", "systemd", "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(root, "calls")
	script := "#!/bin/sh\necho \"$1\" >> '" + calls + "'\ncase \"$1\" in\nlist-units) cat '" + fixtures + "/list-units.txt' ;;\nlist-unit-files) cat '" + fixtures + "/list-unit-files.txt' ;;\nshow) cat '" + fixtures + "/show.txt' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(root, "usr", "bin", "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, name)
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		system:  &system.Reader{Tools: &hosttool.Runner{Root: root}, Root: root},
		dataDir: t.TempDir(),
	}
	s.connect(t, f)
	return s, func() int {
		b, _ := os.ReadFile(calls)
		return strings.Count(string(b), "list-units\n")
	}
}

// A person watches a server's system services, every watch of the server sharing
// one read, 8 a session at most, and nothing is read once nobody watches (ADR 0067).
func TestSystemServicesAreWatchedOnceForEveryone(t *testing.T) {
	was := servers.SystemEvery
	servers.SystemEvery = 200 * time.Millisecond
	t.Cleanup(func() { servers.SystemEvery = was })
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	srv, lists := f.addSystemServer(t, "web-1")
	sc := dockwellv1connect.NewServerServiceClient(b, b.origin, connect.WithProtoJSON())
	open := func() *connect.ServerStreamForClient[dockwellv1.WatchSystemServicesResponse] {
		t.Helper()
		w, err := sc.WatchSystemServices(t.Context(), connect.NewRequest(&dockwellv1.WatchSystemServicesRequest{ServerId: srv.id.ServerID}))
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	first, second := open(), open()
	if !first.Receive() || !second.Receive() {
		t.Fatalf("a watch ended: %v, %v", first.Err(), second.Err())
	}
	a, c := first.Msg().GetList(), second.Msg().GetList()
	if !a.GetTime().AsTime().Equal(c.GetTime().AsTime()) {
		t.Errorf("two watches of one server read it apart, at %v and %v", a.GetTime().AsTime(), c.GetTime().AsTime())
	}
	if a.GetInit() != dockwellv1.SystemInit_SYSTEM_INIT_SYSTEMD || a.GetServices()[0].GetName() != "nginx.service" || len(a.GetServices()) != 13 {
		t.Errorf("the list reads %v", a)
	}
	// It reads again while watched, once for both.
	before := lists()
	if !first.Receive() || !second.Receive() {
		t.Fatalf("a watch ended: %v, %v", first.Err(), second.Err())
	}
	if got := lists() - before; got < 1 || got > 2 {
		t.Errorf("two watches read %d lists for one round", got)
	}

	var more []*connect.ServerStreamForClient[dockwellv1.WatchSystemServicesResponse]
	for range servers.MaxSystemWatchesPerSession - 2 {
		w := open()
		if !w.Receive() {
			t.Fatalf("a watch within the limit ended: %v", w.Err())
		}
		more = append(more, w)
	}
	over := open()
	if over.Receive() || connect.CodeOf(over.Err()) != connect.CodeResourceExhausted {
		t.Errorf("watch %d of one session: %v, want ResourceExhausted", servers.MaxSystemWatchesPerSession+1, over.Err())
	}
	_ = over.Close()

	// With nobody watching, the agent runs nothing.
	for _, w := range append(more, first, second) {
		_ = w.Close()
	}
	time.Sleep(4 * servers.SystemEvery)
	settled := lists()
	time.Sleep(4 * servers.SystemEvery)
	if got := lists(); got != settled {
		t.Errorf("with nobody watching, the agent listed %d more times", got-settled)
	}
}

// No API token reads a server's system services, whatever its scope.
func TestATokenNeedsASessionForSystemServices(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	srv, lists := f.addSystemServer(t, "web-1")
	for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
		secret := createToken(t, b, scope)
		sc := dockwellv1connect.NewServerServiceClient(bearer{token: secret, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
		watch, err := sc.WatchSystemServices(t.Context(), connect.NewRequest(&dockwellv1.WatchSystemServicesRequest{ServerId: srv.id.ServerID}))
		if err == nil {
			watch.Receive()
			err = watch.Err()
		}
		if reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("WatchSystemServices with a %v token: %v, want needs_session", scope, err)
		}
	}
	if n := lists(); n != 0 {
		t.Errorf("a token's watch listed %d times", n)
	}
}
