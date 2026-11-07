// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A server's page asks what takes its disk, and the server's agent asks Docker then,
// and only then: once for two asks close together (ADR 0040).
func TestDiskUseIsAskedForAndKeptAWhile(t *testing.T) {
	storetest.Each(t, testDiskUseIsAskedForAndKeptAWhile)
}

func testDiskUseIsAskedForAndKeptAWhile(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	sc := dockwellv1connect.NewServerServiceClient(b, b.origin, connect.WithProtoJSON())

	apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	e.AddContainer(docker.Container{ID: "a1", Name: "/by-hand", Config: docker.Config{Image: "alpine:latest"}})
	e.ContainerSizes = map[string]int64{"by-hand": 4 << 20}

	asked := func() int {
		n := 0
		for _, r := range e.Seen() {
			if strings.HasSuffix(r, "/system/df") {
				n++
			}
		}
		return n
	}
	if n := asked(); n != 0 {
		t.Fatalf("Docker was asked what its data takes %d times before anyone asked", n)
	}
	id := f.lastDocker.id.ServerID
	res, err := sc.GetServerDiskUse(t.Context(), connect.NewRequest(&dockwellv1.GetServerDiskUseRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	use := res.Msg.GetDiskUse()
	if use.GetDockerRoot() != "/var/lib/docker" || use.GetImages() == 0 || use.GetImagesBytes() < 30<<20 {
		t.Errorf("disk use = %v, want the shop image's 30 MiB under /var/lib/docker", use)
	}
	if use.GetContainers() < 2 || use.GetContainersBytes() != 4<<20 {
		t.Errorf("containers = %d taking %d, want web and by-hand, 4 MiB", use.GetContainers(), use.GetContainersBytes())
	}
	if use.GetTime() == nil {
		t.Error("disk use says nothing of when it was added up")
	}
	again, err := sc.GetServerDiskUse(t.Context(), connect.NewRequest(&dockwellv1.GetServerDiskUseRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Msg.GetDiskUse().GetTime().AsTime().Equal(use.GetTime().AsTime()) {
		t.Error("an ask 30 s within the last added it up again")
	}
	if n := asked(); n != 1 {
		t.Errorf("Docker was asked %d times for two asks close together, want once", n)
	}

	_, err = sc.GetServerDiskUse(t.Context(), connect.NewRequest(&dockwellv1.GetServerDiskUseRequest{Id: "srv_nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown server: %v, want NotFound", err)
	}
	f.lastDocker.stop()
	waitConnected(t, f, id, false)
	_, err = sc.GetServerDiskUse(t.Context(), connect.NewRequest(&dockwellv1.GetServerDiskUseRequest{Id: id}))
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "isn't connected") {
		t.Errorf("a server whose agent has gone: %v, want Unavailable, saying why", err)
	}
}

// When Docker can't say, the page hears why once, in the server's words, with what
// to do: not "can't say" twice over.
func TestDiskUseSaysWhyDockerCouldnt(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/system/df") {
			return errors.New("the disk is busy")
		}
		return nil
	})
	sc := dockwellv1connect.NewServerServiceClient(b, b.origin, connect.WithProtoJSON())
	_, err := sc.GetServerDiskUse(t.Context(), connect.NewRequest(&dockwellv1.GetServerDiskUseRequest{Id: f.lastDocker.id.ServerID}))
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "the disk is busy") {
		t.Fatalf("Docker failing: %v, want Unavailable, with what Docker said", err)
	}
	if n := strings.Count(err.Error(), "can't say"); n != 1 {
		t.Errorf("%q says it can't say %d times", err.Error(), n)
	}
}
