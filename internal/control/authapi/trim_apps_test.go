// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"log/slog"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/store"
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
