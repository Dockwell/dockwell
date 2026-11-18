// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"log/slog"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A stream's samples ask the store only of a service its caller hasn't been
// placed for yet: one it has is kept or left from what's known, and a sample with
// no app in it asks nothing (#848's review, M5). An interceptor with no store
// would fail the test on any ask. Each sample keeps its time, empty of theirs
// where none runs (M1).
func TestAStreamsSamplesAskTheStoreOnlyOfAServiceNotPlacedYet(t *testing.T) {
	i := &interceptor{logger: slog.New(slog.DiscardHandler)}
	ctx := place.WithCaller(t.Context(), place.Caller{UserID: "usr_1", Grants: []store.Grant{{ProjectID: "prj_shop", Role: store.RoleViewer}}})
	at := timestamppb.New(time.Unix(1_800_000_000, 0))
	server := func(containers ...*dockwellv1.ContainerUsage) *dockwellv1.WatchServersResponse {
		return &dockwellv1.WatchServersResponse{Server: &dockwellv1.Server{
			Id: "srv_1", Name: "hz-1",
			Stats: &dockwellv1.ServerStats{Time: at, CpuPercent: 40, MemoryTotalBytes: 8 << 30, Containers: containers},
		}}
	}
	seen := &appsSeen{placed: map[string]bool{"svc_shop": true, "svc_blog": false}}

	m := server(
		&dockwellv1.ContainerUsage{ServiceId: "svc_shop", CpuPercent: 25, Replicas: 1},
		&dockwellv1.ContainerUsage{ServiceId: "svc_blog", CpuPercent: 5, Replicas: 1},
	)
	i.trimKeepingApps(ctx, m, seen)
	got := m.GetServer().GetStats()
	if len(got.GetContainers()) != 1 || got.GetContainers()[0].GetServiceId() != "svc_shop" || got.GetCpuPercent() != 0 || got.GetMemoryTotalBytes() != 0 {
		t.Errorf("a viewer of shop reads %v, want shop's alone and none of the server's own", got)
	}

	// Shop's app stopped: its sample still comes, with its time and nothing of theirs.
	m = server(&dockwellv1.ContainerUsage{ServiceId: "svc_blog", CpuPercent: 5, Replicas: 1})
	i.trimKeepingApps(ctx, m, seen)
	if got := m.GetServer().GetStats(); got == nil || got.GetTime() == nil || len(got.GetContainers()) != 0 {
		t.Errorf("after shop's app stopped, a viewer of shop reads %v, want the sample's time, empty", got)
	}

	// A sample with no apps at all asks nothing either.
	m = server()
	i.trimKeepingApps(ctx, m, seen)
	if got := m.GetServer().GetStats(); got == nil || len(got.GetContainers()) != 0 {
		t.Errorf("a sample with no apps reads %v", got)
	}

	// A server where none of theirs has run keeps no samples, its name and state alone.
	other := &dockwellv1.WatchServersResponse{Server: &dockwellv1.Server{
		Id: "srv_2", Name: "hz-2",
		Stats: &dockwellv1.ServerStats{
			Time: at, CpuPercent: 40, MemoryTotalBytes: 8 << 30,
			Containers: []*dockwellv1.ContainerUsage{{ServiceId: "svc_blog", Replicas: 1}},
		},
	}}
	i.trimKeepingApps(ctx, other, seen)
	if got := other.GetServer(); got.GetStats() != nil || len(got.GetHistory()) != 0 {
		t.Errorf("a server where none of theirs runs reads %v", got)
	}
}

// A send reads the stream's grants, and what's known of its caller's apps by
// them, together (#852). A recheck after that read starts a new cache for the
// next send, which what the earlier send learns by the grants before can't fill.
// So someone who loses a project at a recheck stops getting its apps' figures
// from the next send, not from the recheck after it.
func TestARecheckGivesTheNextSendAFreshCacheOfApps(t *testing.T) {
	i, ada, _ := gated(t, store.RoleViewer)
	before := place.WithCaller(t.Context(), place.Caller{UserID: ada.User.ID, Grants: []store.Grant{{ProjectID: "prj_shop", Role: store.RoleViewer}}})
	c := &scopedConn{StreamingHandlerConn: fakeStream{procedure: dockwellv1connect.ServerServiceWatchServersProcedure}, i: i, ctx: before}
	ctx, seen := c.narrowing()
	if ctx != before || seen.placed == nil || seen.shown == nil {
		t.Fatalf("the first send narrows by %v with %+v, want the stream's own context and empty maps", ctx, seen)
	}
	if err := c.recheck(t.Context(), ada); err != nil {
		t.Fatalf("a recheck that reads the grants: %v", err)
	}
	// The send that read before the recheck places shop's app by the grants it read.
	seen.placed["svc_shop"] = true
	seen.shown["srv_1"] = true
	after, next := c.narrowing()
	if after == before {
		t.Error("the next send narrows by the grants from before the recheck")
	}
	if caller, ok := place.CallerFrom(after); !ok || len(caller.Grants) != 1 || caller.Grants[0].ProjectID != store.Install {
		t.Errorf("the next send narrows by %+v, want the grants the recheck read", caller)
	}
	if len(next.placed) != 0 {
		t.Errorf("the next send knows %v of the caller's apps, which the grants before the recheck placed", next.placed)
	}
	// Which servers have shown theirs isn't a grant, and stays.
	if !next.shown["srv_1"] {
		t.Errorf("the next send forgot the servers that showed theirs: %v", next.shown)
	}
}

// sentStream is a stream of procedure that keeps what it sends.
type sentStream struct {
	fakeStream
	sent []any
}

func (s *sentStream) Send(m any) error {
	s.sent = append(s.sent, m)
	return nil
}

// Whether a stream trims what it sends is asked at each send, of the grants its
// last recheck read: an install admin made a viewer while the panel holds
// WatchServers open is sent the server's names and states alone from the first
// send after a recheck, not the server's own figures and every project's apps
// for as long as the stream stays open (#852's review, H1).
func TestADemotedAdminsOpenStreamIsTrimmedAfterARecheck(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		i, ada, db := gatedOn(t, backend, store.RoleAdmin)
		admin := place.WithCaller(t.Context(), place.Caller{UserID: ada.User.ID, Grants: []store.Grant{{UserID: ada.User.ID, ProjectID: store.Install, Role: store.RoleAdmin}}})
		stream := &sentStream{fakeStream: fakeStream{procedure: dockwellv1connect.ServerServiceWatchServersProcedure}}
		c := &scopedConn{StreamingHandlerConn: stream, i: i, ctx: admin}
		sample := func() *dockwellv1.WatchServersResponse {
			return &dockwellv1.WatchServersResponse{Server: &dockwellv1.Server{
				Id: "srv_1", Name: "hz-1",
				Stats: &dockwellv1.ServerStats{
					Time: timestamppb.New(time.Unix(1_800_000_000, 0)), CpuPercent: 40, MemoryTotalBytes: 8 << 30,
					Containers: []*dockwellv1.ContainerUsage{{ServiceId: "svc_other", CpuPercent: 25, Replicas: 1}},
				},
			}}
		}
		if err := c.Send(sample()); err != nil {
			t.Fatal(err)
		}
		if got := stream.sent[0].(*dockwellv1.WatchServersResponse).GetServer().GetStats(); got.GetCpuPercent() != 40 {
			t.Fatalf("an admin was sent %v, want the server's own figures", got)
		}

		if err := db.Update(t.Context(), func(tx *store.Tx) error {
			return tx.SetGrant(t.Context(), store.Grant{UserID: ada.User.ID, ProjectID: store.Install, Role: store.RoleViewer, CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		if err := c.recheck(t.Context(), ada); err != nil {
			t.Fatalf("a viewer may still watch servers: %v", err)
		}
		if err := c.Send(sample()); err != nil {
			t.Fatal(err)
		}
		got := stream.sent[1].(*dockwellv1.WatchServersResponse).GetServer()
		if got.GetName() != "hz-1" || got.GetStats().GetCpuPercent() != 0 || got.GetStats().GetMemoryTotalBytes() != 0 || len(got.GetStats().GetContainers()) != 0 {
			t.Errorf("after a recheck, the demoted viewer was sent %v, want its name and state alone", got)
		}
	})
}
