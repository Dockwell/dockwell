// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	agentfiles "github.com/dockwell/dockwell/internal/agent/files"
	agentupdates "github.com/dockwell/dockwell/internal/agent/updates"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// aptStand is an Ubuntu server's files with stand-ins for apt, which print the
// agent's fixtures and note each call. A first read of what waits sleeps for
// slow, as a busy server's would.
func aptStand(t *testing.T, slow time.Duration) string {
	t.Helper()
	root := t.TempDir()
	fix, err := filepath.Abs("../agent/updates/testdata")
	if err != nil {
		t.Fatal(err)
	}
	put := func(name, content string, mode os.FileMode) {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	put("etc/os-release", "PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\nID_LIKE=debian\n", 0o644)
	put("var/lib/apt/lists/archive.ubuntu.com_dists_noble_InRelease", "", 0o644)
	head := fmt.Sprintf("#!/bin/sh\nFIX=%q\nROOT=%q\necho \"$(basename \"$0\") $*\" >> \"$ROOT/calls\"\n", fix, root)
	put("usr/bin/apt-get", head+fmt.Sprintf(`case "$*" in
*" update") touch "$ROOT/var/lib/apt/lists/archive.ubuntu.com_dists_noble_InRelease" ;;
*" upgrade") [ -e "$ROOT/slept" ] || { touch "$ROOT/slept"; sleep %.1f; }; cat "$FIX/apt-simulate-ubuntu.txt" ;;
esac
`, slow.Seconds()), 0o755)
	put("usr/bin/dpkg-query", head+`cat "$FIX/dpkg-query.txt"; exit 1`+"\n", 0o755)
	put("usr/bin/apt-cache", head+`cat "$FIX/apt-cache-show.txt"`+"\n", 0o755)
	return root
}

// calls counts how the stand-ins under root were called with what starts so.
func standCalls(t *testing.T, root, prefix string) int {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(root, "calls"))
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" && strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// addReadingServer adds hz-1 with an agent that reads its updates from root, and
// says of itself what hello makes it say.
func (f *fullServer) addReadingServer(t *testing.T, root string, hello func(*agentv1.Hello)) *testServer {
	t.Helper()
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, "hz-1")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		dataDir: t.TempDir(), hello: hello,
		updates: agentupdates.New(agentupdates.Options{Root: root}),
	}
	s.disk = agentfiles.New(s.dataDir, nil, nil)
	s.connect(t, f)
	return s
}

func TestAServersUpdates(t *testing.T) { storetest.Each(t, testAServersUpdates) }

func testAServersUpdates(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	root := aptStand(t, 0)
	server := f.addReadingServer(t, root, nil)
	c := dockwellv1connect.NewUpdateServiceClient(b, b.origin, connect.WithProtoJSON())
	id := server.id.ServerID

	res, err := c.GetServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.GetServerUpdatesRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	got := res.Msg.GetUpdates()
	if got.GetChecking() || got.GetManager() != dockwellv1.PackageManager_PACKAGE_MANAGER_APT || len(got.GetPackages()) != 8 || got.GetServerId() != id {
		t.Fatalf("first look = %v", got)
	}
	if p := got.GetPackages()[0]; p.GetName() != "libssh-4" || !p.GetSecurity() || p.GetSummary() == "" {
		t.Errorf("first package = %v", p)
	}
	if d := res.Msg.GetDockwell(); d.GetControlPlane() != testBuild.Version || d.GetAgent() == "" || d.GetNewest() != "" {
		t.Errorf("Dockwell = %v, want this control plane's version, the agent's, and no newest in a test", d)
	}
	// Lists refreshed just now aren't refreshed again.
	if n := standCalls(t, root, "apt-get -q -o DPkg"); n != 0 {
		t.Errorf("refreshed fresh lists %d times", n)
	}

	// A second look within Fresh asks nothing of the agent.
	if _, err := c.GetServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.GetServerUpdatesRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if n := standCalls(t, root, "apt-get -q -o Debug::NoLocking"); n != 1 {
		t.Errorf("read what waits %d times for two looks, want once", n)
	}

	// Check now refreshes the lists, whatever their age, and answers as it starts.
	check, err := c.CheckServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServerUpdatesRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if !check.Msg.GetUpdates().GetChecking() || len(check.Msg.GetUpdates().GetPackages()) != 8 {
		t.Errorf("Check now answered %v, want checking, with what was known", check.Msg.GetUpdates())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := c.GetServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.GetServerUpdatesRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		if !res.Msg.GetUpdates().GetChecking() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Check now never finished")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := standCalls(t, root, "apt-get -q -o DPkg::Lock::Timeout=120 update"); n != 1 {
		t.Errorf("Check now refreshed %d times, want once", n)
	}
	// And at most once a minute, however often a person asks.
	for range 3 {
		if _, err := c.CheckServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServerUpdatesRequest{Id: id})); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := standCalls(t, root, "apt-get -q -o DPkg::Lock::Timeout=120 update"); n != 1 {
		t.Errorf("refreshed %d times within a minute, want once", n)
	}

	// With the agent gone, what it last said stands, and says so.
	server.stop()
	waitConnected(t, f, id, false)
	res, err = c.GetServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.GetServerUpdatesRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if u := res.Msg.GetUpdates(); len(u.GetPackages()) != 8 || u.GetReason() != "server_offline" || !strings.Contains(u.GetError(), "hz-1's agent isn't connected") {
		t.Errorf("with the agent gone: %v", u)
	}

	_, err = c.GetServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.GetServerUpdatesRequest{Id: "srv_nowhere"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown server: %v", err)
	}
}

func TestASlowReadAnswersWhileItChecks(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	server := f.addReadingServer(t, aptStand(t, 5*time.Second), nil)
	c := dockwellv1connect.NewUpdateServiceClient(b, b.origin, connect.WithProtoJSON())
	start := time.Now()
	res, err := c.GetServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.GetServerUpdatesRequest{Id: server.id.ServerID}))
	if err != nil {
		t.Fatal(err)
	}
	if u := res.Msg.GetUpdates(); !u.GetChecking() || len(u.GetPackages()) != 0 || time.Since(start) > 4500*time.Millisecond {
		t.Errorf("after %v: %v, want checking with nothing yet", time.Since(start), u)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := c.GetServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.GetServerUpdatesRequest{Id: server.id.ServerID}))
		if err != nil {
			t.Fatal(err)
		}
		if u := res.Msg.GetUpdates(); !u.GetChecking() {
			if len(u.GetPackages()) != 8 {
				t.Errorf("once read: %v", u)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the read never finished")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestNoUpdatesThroughAnOlderAgent(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	root := aptStand(t, 0)
	server := f.addReadingServer(t, root, func(h *agentv1.Hello) { h.Updates = false })
	c := dockwellv1connect.NewUpdateServiceClient(b, b.origin, connect.WithProtoJSON())
	res, err := c.GetServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.GetServerUpdatesRequest{Id: server.id.ServerID}))
	if err != nil {
		t.Fatal(err)
	}
	if u := res.Msg.GetUpdates(); u.GetReason() != "agent_outdated" || !strings.Contains(u.GetUnavailable(), "hz-1's agent is older than the control plane") {
		t.Errorf("through an older agent: %v", u)
	}
	if n := standCalls(t, root, ""); n != 0 {
		t.Errorf("ran apt %d times through an agent that doesn't say it reads updates", n)
	}
}

// A token reads a server's updates, as it reads the rest of what a server is, but
// can't have it reach its mirrors: Check now takes a person.
func TestATokenCantCheckNow(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	root := aptStand(t, 0)
	server := f.addReadingServer(t, root, nil)
	secret, _ := logToken(t, b)
	c := dockwellv1connect.NewUpdateServiceClient(bearer{token: secret, rt: h2cTransport(t, f.api)}, "http://"+f.api, connect.WithProtoJSON())
	if _, err := c.GetServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.GetServerUpdatesRequest{Id: server.id.ServerID})); err != nil {
		t.Errorf("a token reading updates: %v", err)
	}
	_, err := c.CheckServerUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServerUpdatesRequest{Id: server.id.ServerID}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("a token checking now: %v", err)
	}
	if n := standCalls(t, root, "apt-get -q -o DPkg"); n != 0 {
		t.Errorf("a token refreshed the lists %d times", n)
	}
}
