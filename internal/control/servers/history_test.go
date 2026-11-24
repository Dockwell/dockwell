// SPDX-License-Identifier: AGPL-3.0-only

package servers

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestGetServerAndContainerHistory(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(5 * time.Minute)
		joinedAt := now.Add(-45 * time.Minute)
		srvID := "srv_hist_1"
		cid := strings.Repeat("b", 64)

		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.CreateServer(ctx, store.Server{
				ID:        srvID,
				Name:      "web-1",
				CreatedAt: joinedAt,
			})
		}); err != nil {
			t.Fatal(err)
		}

		hub, err := fleet.New(ctx, db, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		svc := New(hub, db, nil)

		// Seed 40 minutes of 1-minute history:
		// - minutes [-45, -35): offline (CoveredMs = 0)
		// - minutes [-35, -30): no rows (control plane offline)
		// - minutes [-30, 0): covered with per-disk, per-iface and container rows
		var srvRows []store.HistoryRow
		var diskRows []store.DiskHistoryRow
		var netRows []store.NetHistoryRow
		var contRows []store.ContainerHistoryRow
		for m := -45; m < 0; m++ {
			at := now.Add(time.Duration(m) * time.Minute)
			if m >= -35 && m < -30 {
				continue // simulate control plane down
			}
			if m < -35 {
				srvRows = append(srvRows, store.HistoryRow{ServerID: srvID, Start: at, CoveredMs: 0})
				continue
			}
			srvRows = append(srvRows, store.HistoryRow{
				ServerID: srvID, Start: at, CoveredMs: 60_000,
				CPUSum: 60_000 * 35_000, CPUMax: 72_000,
				MemorySum: 60_000 * (4 << 20), MemoryMax: 5 << 20, MemoryTotal: 8 << 20,
				LoadSum: 60_000 * 1200, LoadMax: 2400,
				RxBytes: 60_000, RxPeak: 2000, TxBytes: 30_000, TxPeak: 1000,
				ReadBytes: 120_000, ReadPeak: 4000, WrittenBytes: 240_000, WrittenPeak: 8000,
				ReadOps: 600, ReadIOPSPeak: 20_000, WriteOps: 1200, WriteIOPSPeak: 40_000,
			})
			diskRows = append(diskRows, store.DiskHistoryRow{
				ServerID: srvID, Device: "vda", Start: at, CoveredMs: 60_000,
				ReadBytes: 120_000, ReadPeak: 4000, WrittenBytes: 240_000, WrittenPeak: 8000,
				ReadOps: 600, ReadIOPSPeak: 20_000, WriteOps: 1200, WriteIOPSPeak: 40_000,
			})
			netRows = append(netRows, store.NetHistoryRow{
				ServerID: srvID, Interface: "ens3", Start: at, CoveredMs: 60_000,
				RxBytes: 60_000, RxPeak: 2000, TxBytes: 30_000, TxPeak: 1000,
			})
			contRows = append(contRows, store.ContainerHistoryRow{
				ServerID: srvID, ContainerID: cid, Start: at, CoveredMs: 60_000,
				CPUSum: 60_000 * 18_500, CPUMax: 41_000,
				MemorySum: 60_000 * (128 << 10), MemoryMax: 192 << 10, MemLimit: 512 << 10,
			})
		}

		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.AddMinutes(ctx, srvRows); err != nil {
				return err
			}
			if err := tx.AddDiskMinutes(ctx, diskRows); err != nil {
				return err
			}
			if err := tx.AddNetMinutes(ctx, netRows); err != nil {
				return err
			}
			return tx.AddContainerMinutes(ctx, contRows)
		}); err != nil {
			t.Fatal(err)
		}

		// Query 1 h range with per-disk and per-interface filters.
		res, err := svc.GetServerHistory(ctx, connect.NewRequest(&dockwellv1.GetServerHistoryRequest{
			Id:               srvID,
			Range:            dockwellv1.HistoryRange_HISTORY_RANGE_1H,
			Disk:             "vda",
			NetworkInterface: "ens3",
		}))
		if err != nil {
			t.Fatal(err)
		}
		got := res.Msg
		if len(got.GetPoints()) != 60 {
			t.Fatalf("1 h points = %d, want 60", len(got.GetPoints()))
		}
		if len(got.GetDisks()) != 1 || got.GetDisks()[0] != "vda" {
			t.Errorf("disks = %v, want [vda]", got.GetDisks())
		}
		if len(got.GetInterfaces()) != 1 || got.GetInterfaces()[0] != "ens3" {
			t.Errorf("interfaces = %v, want [ens3]", got.GetInterfaces())
		}
		states := map[dockwellv1.HistoryPointState]int{}
		var coveredPt *dockwellv1.ServerHistoryPoint
		for _, pt := range got.GetPoints() {
			states[pt.GetState()]++
			if pt.GetState() == dockwellv1.HistoryPointState_HISTORY_POINT_STATE_COVERED {
				coveredPt = pt
			}
		}
		if states[dockwellv1.HistoryPointState_HISTORY_POINT_STATE_BEFORE_JOIN] == 0 {
			t.Errorf("expected BEFORE_JOIN buckets, got states %v", states)
		}
		if states[dockwellv1.HistoryPointState_HISTORY_POINT_STATE_OFFLINE] == 0 {
			t.Errorf("expected OFFLINE buckets, got states %v", states)
		}
		if states[dockwellv1.HistoryPointState_HISTORY_POINT_STATE_NO_CONTROL_PLANE] == 0 {
			t.Errorf("expected NO_CONTROL_PLANE buckets, got states %v", states)
		}
		if coveredPt == nil || coveredPt.GetCpuPercent() != 35 || coveredPt.GetDiskReadIops() != 10 || coveredPt.GetNetworkRxBytesPerSecond() != 1000 {
			t.Errorf("covered point = %+v, want cpu=35 readIops=10 rx=1000", coveredPt)
		}

		// Query container history for 1 h.
		cres, err := svc.GetContainerHistory(ctx, connect.NewRequest(&dockwellv1.GetContainerHistoryRequest{
			ServerId:    srvID,
			ContainerId: cid,
			Range:       dockwellv1.HistoryRange_HISTORY_RANGE_1H,
		}))
		if err != nil {
			t.Fatal(err)
		}
		var contCovered *dockwellv1.ContainerHistoryPoint
		for _, pt := range cres.Msg.GetPoints() {
			if pt.GetState() == dockwellv1.HistoryPointState_HISTORY_POINT_STATE_COVERED {
				contCovered = pt
				break
			}
		}
		if contCovered == nil || contCovered.GetCpuPercent() != 18.5 || contCovered.GetMemoryBytes() != 128<<20 || contCovered.GetMemoryLimitBytes() != 512<<20 {
			t.Errorf("container covered point = %+v, want cpu=18.5 mem=128MiB limit=512MiB", contCovered)
		}

		// Invalid container ID is rejected.
		if _, err := svc.GetContainerHistory(ctx, connect.NewRequest(&dockwellv1.GetContainerHistoryRequest{
			ServerId:    srvID,
			ContainerId: "not-a-hex-id",
		})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("invalid container ID error = %v, want InvalidArgument", err)
		}
		_ = timestamppb.New(now)
	})
}

// M5: On the 7 d range (30-minute buckets), seeding 1-hour rows only (as after
// an upgrade before 5-minute rows exist) spreads each hour across both of its
// 30-minute buckets with zero false NO_CONTROL_PLANE gaps, and partial 5-minute
// overlap never double-counts.
func TestServerHistory7dHourFallbackSpreadsWithoutGaps(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Hour)
		srvID := "srv_m5"
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.CreateServer(ctx, store.Server{ID: srvID, Name: "m5", CreatedAt: now.Add(-10 * 24 * time.Hour)})
		}); err != nil {
			t.Fatal(err)
		}

		// Seed 168 hour rows covering the full 7 days, plus:
		// - overlapHour (:30..:55): full second-half 5m rows (checks no double-counting)
		// - partialHour (:20..:55): 5m rows starting partway into the :00..:30 bucket
		//   (N3: the :00 bucket must blend 10m of 5m rows with 20m of the hour row and
		//   stay COVERED, never a false OFFLINE point).
		var hourRows []store.HistoryRow
		for h := -169; h <= 0; h++ {
			at := now.Add(time.Duration(h) * time.Hour)
			hourRows = append(hourRows, store.HistoryRow{
				ServerID: srvID, Tier: store.HistoryHour, Start: at, CoveredMs: 3_600_000,
				CPUSum: 3_600_000 * 40_000, CPUMax: 85_000,
				MemorySum: 3_600_000 * (2 << 20), MemoryMax: 3 << 20, MemoryTotal: 8 << 20,
			})
		}
		overlapHour := now.Add(-2 * time.Hour)
		partialHour := now.Add(-24 * time.Hour)
		var fiveRows []store.HistoryRow
		for m := 30; m < 60; m += 5 {
			fiveRows = append(fiveRows, store.HistoryRow{
				ServerID: srvID, Tier: store.HistoryFiveMinute, Start: overlapHour.Add(time.Duration(m) * time.Minute),
				CoveredMs: 300_000, CPUSum: 300_000 * 60_000, CPUMax: 65_000,
				MemorySum: 300_000 * (2 << 20), MemoryMax: 2 << 20, MemoryTotal: 8 << 20,
			})
		}
		for m := 20; m < 60; m += 5 {
			fiveRows = append(fiveRows, store.HistoryRow{
				ServerID: srvID, Tier: store.HistoryFiveMinute, Start: partialHour.Add(time.Duration(m) * time.Minute),
				CoveredMs: 300_000, CPUSum: 300_000 * 70_000, CPUMax: 75_000,
				MemorySum: 300_000 * (2 << 20), MemoryMax: 2 << 20, MemoryTotal: 8 << 20,
			})
		}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.AddHistoryRows(ctx, hourRows); err != nil {
				return err
			}
			return tx.AddHistoryRows(ctx, fiveRows)
		}); err != nil {
			t.Fatal(err)
		}

		hub, err := fleet.New(ctx, db, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		svc := New(hub, db, nil)
		res, err := svc.GetServerHistory(ctx, connect.NewRequest(&dockwellv1.GetServerHistoryRequest{
			Id:    srvID,
			Range: dockwellv1.HistoryRange_HISTORY_RANGE_7D,
		}))
		if err != nil {
			t.Fatal(err)
		}
		pts := res.Msg.GetPoints()
		if len(pts) != 336 {
			t.Fatalf("7 d points = %d, want 336", len(pts))
		}
		var covered, missing int
		var sawOverlapSecondHalf, sawPartialBucket bool
		for _, pt := range pts {
			if pt.GetState() == dockwellv1.HistoryPointState_HISTORY_POINT_STATE_COVERED {
				covered++
				if pt.GetTime().AsTime().Equal(overlapHour.Add(30 * time.Minute)) {
					sawOverlapSecondHalf = true
					// Must come purely from the 5m rows (60% CPU), not double-counted with the 40% hour row.
					if pt.GetCpuPercent() != 60 {
						t.Errorf("overlapped :30 bucket cpu = %v, want 60 (from 5m rows without double-counting)", pt.GetCpuPercent())
					}
				}
				if pt.GetTime().AsTime().Equal(partialHour) {
					sawPartialBucket = true
					// 10m at 70% (from 5m rows :20..:30) + 20m at 40% (uncovered share of hour row) = 50% CPU.
					if pt.GetCpuPercent() != 50 {
						t.Errorf("partial :00 bucket cpu = %v, want 50 (10m*70%% + 20m*40%%)", pt.GetCpuPercent())
					}
				}
			} else {
				missing++
			}
		}
		// Allow the single in-progress trailing bucket if now is past :00; every completed bucket must be covered.
		if missing > 1 || covered < 335 {
			t.Fatalf("7 d with hour rows only: covered=%d missing=%d, want >=335 covered and <=1 missing", covered, missing)
		}
		if !sawOverlapSecondHalf {
			t.Error("did not find overlapped :30 bucket in 7 d response")
		}
		if !sawPartialBucket {
			t.Error("N3: partial :00 bucket (5m starting at :20) was not COVERED")
		}
	})
}

type fakeTracker map[string]bool

func (f fakeTracker) ContainerUntracked(serverID, containerID string) bool {
	return f[serverID+":"+containerID]
}

// M6 + L3 + L6: Container charts distinguish BEFORE_CREATE, NOT_RUNNING,
// NOT_TRACKED, and OFFLINE using the same half-coverage rule as servers; and
// per-device server charts flag missing devices rather than zero-filling.
func TestContainerGapStatesHalfCoverageAndNoDeviceZeroFill(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(5 * time.Minute)
		srvID := "srv_m6"
		cid := strings.Repeat("c", 64)
		untrackedCID := strings.Repeat("d", 64)
		joinedAt := now.Add(-2 * time.Hour)
		createdAt := now.Add(-30 * time.Minute)

		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.CreateServer(ctx, store.Server{ID: srvID, Name: "m6", CreatedAt: joinedAt})
		}); err != nil {
			t.Fatal(err)
		}

		// Minutes [-60, 0):
		// - [-60, -50): server offline (CoveredMs = 0) -> OFFLINE
		// - [-50, -30): server covered, before container created -> BEFORE_CREATE
		// - [-30, -20): server covered, container stopped (no rows) -> NOT_RUNNING
		// - [-20, -15): server covered, container has only 10 s / 60 s (< half, L6) -> NOT_RUNNING
		// - [-15, 0): server covered, container covered (60 s) -> COVERED; disk "sdb" only in [-10, 0) (L3)
		var srvRows []store.HistoryRow
		var diskRows []store.DiskHistoryRow
		var contRows []store.ContainerHistoryRow
		for m := -60; m < 0; m++ {
			at := now.Add(time.Duration(m) * time.Minute)
			if m < -50 {
				srvRows = append(srvRows, store.HistoryRow{ServerID: srvID, Start: at, CoveredMs: 0})
				continue
			}
			srvRows = append(srvRows, store.HistoryRow{ServerID: srvID, Start: at, CoveredMs: 60_000, CPUSum: 60_000 * 10_000, CPUMax: 10_000})
			if m >= -10 {
				diskRows = append(diskRows, store.DiskHistoryRow{ServerID: srvID, Device: "sdb", Start: at, CoveredMs: 60_000, ReadBytes: 60_000})
			}
			if m >= -20 && m < -15 {
				contRows = append(contRows, store.ContainerHistoryRow{ServerID: srvID, ContainerID: cid, Start: at, CoveredMs: 10_000, CPUSum: 10_000 * 5_000, CPUMax: 5_000})
			} else if m >= -15 {
				contRows = append(contRows, store.ContainerHistoryRow{ServerID: srvID, ContainerID: cid, Start: at, CoveredMs: 60_000, CPUSum: 60_000 * 15_000, CPUMax: 20_000})
			}
		}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.AddMinutes(ctx, srvRows); err != nil {
				return err
			}
			if err := tx.AddDiskMinutes(ctx, diskRows); err != nil {
				return err
			}
			return tx.AddContainerMinutes(ctx, contRows)
		}); err != nil {
			t.Fatal(err)
		}

		hub, err := fleet.New(ctx, db, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		svc := New(hub, db, nil).History(fakeTracker{srvID + ":" + untrackedCID: true})

		// L3: Request disk="sdb" over 1 h; covered server buckets before -10m must set DiskIoMissing=true.
		sres, err := svc.GetServerHistory(ctx, connect.NewRequest(&dockwellv1.GetServerHistoryRequest{
			Id:    srvID,
			Range: dockwellv1.HistoryRange_HISTORY_RANGE_1H,
			Disk:  "sdb",
		}))
		if err != nil {
			t.Fatal(err)
		}
		var diskPresent, diskMissing int
		for _, pt := range sres.Msg.GetPoints() {
			if pt.GetState() != dockwellv1.HistoryPointState_HISTORY_POINT_STATE_COVERED {
				continue
			}
			if pt.GetDiskIoMissing() {
				diskMissing++
				if pt.GetDiskReadBytesPerSecond() != 0 {
					t.Errorf("missing disk bucket should leave rate unset, got %d", pt.GetDiskReadBytesPerSecond())
				}
			} else {
				diskPresent++
			}
		}
		if diskPresent == 0 || diskMissing == 0 {
			t.Errorf("L3 disk missing check: present=%d missing=%d, want both > 0", diskPresent, diskMissing)
		}

		// M6 + L6: Container history with CreatedAt.
		cres, err := svc.GetContainerHistory(ctx, connect.NewRequest(&dockwellv1.GetContainerHistoryRequest{
			ServerId:    srvID,
			ContainerId: cid,
			Range:       dockwellv1.HistoryRange_HISTORY_RANGE_1H,
			CreatedAt:   timestamppb.New(createdAt),
		}))
		if err != nil {
			t.Fatal(err)
		}
		cStates := map[dockwellv1.HistoryPointState]int{}
		for _, pt := range cres.Msg.GetPoints() {
			cStates[pt.GetState()]++
		}
		if cStates[dockwellv1.HistoryPointState_HISTORY_POINT_STATE_OFFLINE] == 0 {
			t.Errorf("expected OFFLINE when server was offline, got %v", cStates)
		}
		if cStates[dockwellv1.HistoryPointState_HISTORY_POINT_STATE_BEFORE_CREATE] == 0 {
			t.Errorf("expected BEFORE_CREATE before container creation, got %v", cStates)
		}
		// Both the 10 stopped minutes AND the 5 under-half-covered minutes (L6) must be NOT_RUNNING.
		if cStates[dockwellv1.HistoryPointState_HISTORY_POINT_STATE_NOT_RUNNING] < 14 {
			t.Errorf("expected >=14 NOT_RUNNING buckets (stopped + <50%% coverage), got %v", cStates)
		}
		if cStates[dockwellv1.HistoryPointState_HISTORY_POINT_STATE_COVERED] == 0 {
			t.Errorf("expected COVERED buckets, got %v", cStates)
		}

		// M6: Untracked container past the 128-container cap reports NOT_TRACKED and Untracked=true.
		ures, err := svc.GetContainerHistory(ctx, connect.NewRequest(&dockwellv1.GetContainerHistoryRequest{
			ServerId:    srvID,
			ContainerId: untrackedCID,
			Range:       dockwellv1.HistoryRange_HISTORY_RANGE_1H,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if !ures.Msg.GetUntracked() {
			t.Error("expected Untracked=true for capped container")
		}
		hasNotTracked := false
		for _, pt := range ures.Msg.GetPoints() {
			if pt.GetState() == dockwellv1.HistoryPointState_HISTORY_POINT_STATE_NOT_TRACKED {
				hasNotTracked = true
				break
			}
		}
		if !hasNotTracked {
			t.Error("expected NOT_TRACKED buckets for capped container")
		}
	})
}
