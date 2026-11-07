// SPDX-License-Identifier: AGPL-3.0-only

package servers_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/servers"
	"github.com/dockwell/dockwell/internal/store"
)

// serve answers ServerService over HTTP from a hub with the named servers,
// whose IDs are srv_A, srv_B and so on.
func serve(t *testing.T, names ...string) (dockwellv1connect.ServerServiceClient, *fleet.Hub) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		for i, n := range names {
			if err := tx.CreateServer(t.Context(), store.Server{
				ID: "srv_" + string(rune('A'+i)), Name: n, CreatedAt: now, CertSerial: n, CertNotAfter: now,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hub, err := fleet.New(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(dockwellv1connect.NewServerServiceHandler(servers.New(hub, db)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return dockwellv1connect.NewServerServiceClient(srv.Client(), srv.URL), hub
}

// receive reads the next message, failing the test if the stream ends first.
func receive(t *testing.T, stream *connect.ServerStreamForClient[dockwellv1.WatchServersResponse]) *dockwellv1.WatchServersResponse {
	t.Helper()
	if !stream.Receive() {
		t.Fatalf("the watch ended: %v", stream.Err())
	}
	return stream.Msg()
}

func TestWatchMarksTheEndOfTheSnapshot(t *testing.T) {
	client, hub := serve(t, "hz-1", "hz-2")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := client.WatchServers(ctx, connect.NewRequest(&dockwellv1.WatchServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	for _, want := range []string{"hz-1", "hz-2"} {
		msg := receive(t, stream)
		if msg.GetServer().GetName() != want || msg.GetSnapshotComplete() {
			t.Fatalf("got %v, want %s in the snapshot", msg, want)
		}
	}
	if msg := receive(t, stream); !msg.GetSnapshotComplete() || msg.GetServer() != nil {
		t.Fatalf("got %v, want the end of the snapshot", msg)
	}

	if _, err := hub.Attach(ctx, store.Server{ID: "srv_B", Name: "hz-2"}, &dockwellv1.ServerInventory{OsName: "Debian GNU/Linux 13"}, nil); err != nil {
		t.Fatal(err)
	}
	hub.Record("srv_B", &dockwellv1.ServerStats{Time: timestamppb.Now(), CpuPercent: 42})
	for {
		msg := receive(t, stream)
		if msg.GetSnapshotComplete() {
			t.Fatal("a second end of snapshot")
		}
		if msg.GetServer().GetStats().GetCpuPercent() == 42 {
			if msg.GetServer().GetId() != "srv_B" {
				t.Errorf("the sample came from %s", msg.GetServer().GetId())
			}
			return
		}
	}
}

func TestWatchAnEmptyFleet(t *testing.T) {
	client, _ := serve(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := client.WatchServers(ctx, connect.NewRequest(&dockwellv1.WatchServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if msg := receive(t, stream); !msg.GetSnapshotComplete() || msg.GetServer() != nil {
		t.Fatalf("got %v, want only the end of the snapshot", msg)
	}
}

// A watch says it is alive every HeartbeatEvery, with nothing else to say, so a
// client can tell a quiet fleet from a stream that has stalled.
func TestAWatchSaysItIsAlive(t *testing.T) {
	was := servers.HeartbeatEvery
	servers.HeartbeatEvery = 20 * time.Millisecond
	t.Cleanup(func() { servers.HeartbeatEvery = was })
	client, _ := serve(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := client.WatchServers(ctx, connect.NewRequest(&dockwellv1.WatchServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if msg := receive(t, stream); !msg.GetSnapshotComplete() {
		t.Fatalf("got %v, want the end of the snapshot first", msg)
	}
	for range 3 {
		if msg := receive(t, stream); !msg.GetHeartbeat() || msg.GetServer() != nil || msg.GetSnapshotComplete() {
			t.Fatalf("with nothing changing, the watch sent %v, want only that it is alive", msg)
		}
	}
}

func TestListAndGet(t *testing.T) {
	client, _ := serve(t, "hz-1", "hz-2")
	list, err := client.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := list.Msg.GetServers(); len(got) != 2 || got[0].GetName() != "hz-1" || got[1].GetName() != "hz-2" {
		t.Errorf("ListServers: %v", got)
	}

	got, err := client.GetServer(t.Context(), connect.NewRequest(&dockwellv1.GetServerRequest{Id: "srv_B"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetServer().GetName() != "hz-2" {
		t.Errorf("GetServer: %v", got.Msg.GetServer())
	}

	_, err = client.GetServer(t.Context(), connect.NewRequest(&dockwellv1.GetServerRequest{Id: "srv_nope"}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeNotFound {
		t.Fatalf("GetServer of an unknown server: %v", err)
	}
	if want := "there's no server srv_nope. List them with ListServers"; cerr.Message() != want {
		t.Errorf("message %q, want %q", cerr.Message(), want)
	}
}
