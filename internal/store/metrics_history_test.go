// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestMetricsHistoryTiersAndBreakdowns(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		cid := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

		// Write 60 minutes (1 full hour) of server, per-disk, per-net and per-container rows.
		var srvRows []store.HistoryRow
		var diskRows []store.DiskHistoryRow
		var netRows []store.NetHistoryRow
		var contRows []store.ContainerHistoryRow
		for m := range 60 {
			at := t0.Add(time.Duration(m) * time.Minute)
			srvRows = append(srvRows, store.HistoryRow{
				ServerID: "srv_1", Start: at, CoveredMs: 60_000,
				CPUSum: 60_000 * 25_000, CPUMax: 40_000,
				MemorySum: 60_000 * (2 << 20), MemoryMax: 3 << 20, MemoryTotal: 8 << 20,
				LoadSum: 60_000 * 1500, LoadMax: 2500,
				RxBytes: 6000, RxPeak: 200, TxBytes: 3000, TxPeak: 100,
				ReadBytes: 12000, ReadPeak: 500, WrittenBytes: 24000, WrittenPeak: 900,
				ReadOps: 60, ReadIOPSPeak: 3000, WriteOps: 120, WriteIOPSPeak: 6000,
			})
			diskRows = append(diskRows, store.DiskHistoryRow{
				ServerID: "srv_1", Device: "nvme0n1", Start: at, CoveredMs: 60_000,
				ReadBytes: 12000, ReadPeak: 500, WrittenBytes: 24000, WrittenPeak: 900,
				ReadOps: 60, ReadIOPSPeak: 3000, WriteOps: 120, WriteIOPSPeak: 6000,
			})
			netRows = append(netRows, store.NetHistoryRow{
				ServerID: "srv_1", Interface: "eth0", Start: at, CoveredMs: 60_000,
				RxBytes: 6000, RxPeak: 200, TxBytes: 3000, TxPeak: 100,
			})
			contRows = append(contRows, store.ContainerHistoryRow{
				ServerID: "srv_1", ContainerID: cid, Start: at, CoveredMs: 60_000,
				CPUSum: 60_000 * 12_000, CPUMax: 20_000,
				MemorySum: 60_000 * (256 << 10), MemoryMax: 300 << 10, MemLimit: 512 << 10,
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

		// Catch-up should find 12 5-minute blocks and 1 hour block.
		var fiveBlocks []time.Time
		if err := db.View(ctx, func(tx *store.Tx) error {
			var err error
			fiveBlocks, err = tx.FiveMinutesToRoll(ctx, t0, t0.Add(time.Hour))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(fiveBlocks) != 12 {
			t.Fatalf("FiveMinutesToRoll = %d, want 12", len(fiveBlocks))
		}

		// Roll all 12 5-minute blocks and the 1-hour block (twice to check idempotency).
		for range 2 {
			if err := db.Update(ctx, func(tx *store.Tx) error {
				for _, b := range fiveBlocks {
					if err := tx.RollFiveMinutes(ctx, b); err != nil {
						return err
					}
				}
				return tx.RollHour(ctx, t0)
			}); err != nil {
				t.Fatal(err)
			}
		}

		// Verify 5-minute and 1-hour rollups across all 4 tables.
		if err := db.View(ctx, func(tx *store.Tx) error {
			s5, err := tx.History(ctx, "srv_1", store.HistoryFiveMinute, t0, t0.Add(time.Hour))
			if err != nil {
				return err
			}
			if len(s5) != 12 || s5[0].ReadOps != 300 || s5[0].WriteIOPSPeak != 6000 {
				return fmt.Errorf("5m server rows = %d (first: %+v)", len(s5), s5)
			}
			d1h, err := tx.DiskHistory(ctx, "srv_1", "nvme0n1", store.HistoryHour, t0, t0.Add(time.Hour))
			if err != nil {
				return err
			}
			if len(d1h) != 1 || d1h[0].ReadBytes != 720000 || d1h[0].WriteOps != 7200 {
				return fmt.Errorf("1h disk rows = %+v", d1h)
			}
			n1h, err := tx.NetHistory(ctx, "srv_1", "eth0", store.HistoryHour, t0, t0.Add(time.Hour))
			if err != nil {
				return err
			}
			if len(n1h) != 1 || n1h[0].RxBytes != 360000 || n1h[0].TxBytes != 180000 {
				return fmt.Errorf("1h net rows = %+v", n1h)
			}
			c5, err := tx.ContainerHistory(ctx, "srv_1", cid, store.HistoryFiveMinute, t0, t0.Add(time.Hour))
			if err != nil {
				return err
			}
			if len(c5) != 12 || c5[0].CoveredMs != 300_000 || c5[0].MemLimit != 512<<10 {
				return fmt.Errorf("5m container rows = %d (first: %+v)", len(c5), c5)
			}
			devs, err := tx.DiskHistoryDevices(ctx, "srv_1", []int{store.HistoryHour}, t0, t0.Add(time.Hour))
			if err != nil || len(devs) != 1 || devs[0] != "nvme0n1" {
				return fmt.Errorf("devices = %v, err=%w", devs, err)
			}
			ifaces, err := tx.NetHistoryInterfaces(ctx, "srv_1", []int{store.HistoryHour}, t0, t0.Add(time.Hour))
			if err != nil || len(ifaces) != 1 || ifaces[0] != "eth0" {
				return fmt.Errorf("interfaces = %v, err=%w", ifaces, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		// Pruning at t0 + 8 days should drop 1m and 5m rows while keeping 1h rows.
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.PruneHistory(ctx, t0.Add(8*24*time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.View(ctx, func(tx *store.Tx) error {
			s1, _ := tx.History(ctx, "srv_1", store.HistoryMinute, t0, t0.Add(time.Hour))
			s5, _ := tx.History(ctx, "srv_1", store.HistoryFiveMinute, t0, t0.Add(time.Hour))
			s1h, _ := tx.History(ctx, "srv_1", store.HistoryHour, t0, t0.Add(time.Hour))
			c5, _ := tx.ContainerHistory(ctx, "srv_1", cid, store.HistoryFiveMinute, t0, t0.Add(time.Hour))
			c1h, _ := tx.ContainerHistory(ctx, "srv_1", cid, store.HistoryHour, t0, t0.Add(time.Hour))
			if len(s1) != 0 || len(s5) != 0 || len(c5) != 0 || len(s1h) != 1 || len(c1h) != 1 {
				return fmt.Errorf("after 8d prune: s1=%d s5=%d c5=%d s1h=%d c1h=%d", len(s1), len(s5), len(c5), len(s1h), len(c1h))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// L1: Disk, interface, and container chunk tables enforce per-series caps on
// RollHour regardless of clock jumps, just like server_history.
func TestMetricsHistoryChunkCapsOnClockJump(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		cid := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

		// Write 30 distinct 1-hour minute chunks (exceeds cap of 25) plus one
		// chunk jumped a year into the future.
		var diskRows []store.DiskHistoryRow
		var netRows []store.NetHistoryRow
		var contRows []store.ContainerHistoryRow
		for h := range 30 {
			at := t0.Add(time.Duration(h) * time.Hour)
			diskRows = append(diskRows, store.DiskHistoryRow{ServerID: "srv_cap", Device: "sda", Start: at, CoveredMs: 60_000, ReadBytes: 100})
			netRows = append(netRows, store.NetHistoryRow{ServerID: "srv_cap", Interface: "eth0", Start: at, CoveredMs: 60_000, RxBytes: 100})
			contRows = append(contRows, store.ContainerHistoryRow{ServerID: "srv_cap", ContainerID: cid, Start: at, CoveredMs: 60_000, CPUSum: 60_000 * 1000, CPUMax: 1000})
		}
		future := t0.AddDate(1, 0, 0)
		diskRows = append(diskRows, store.DiskHistoryRow{ServerID: "srv_cap", Device: "sda", Start: future, CoveredMs: 60_000, ReadBytes: 100})
		netRows = append(netRows, store.NetHistoryRow{ServerID: "srv_cap", Interface: "eth0", Start: future, CoveredMs: 60_000, RxBytes: 100})
		contRows = append(contRows, store.ContainerHistoryRow{ServerID: "srv_cap", ContainerID: cid, Start: future, CoveredMs: 60_000, CPUSum: 60_000 * 1000, CPUMax: 1000})

		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.AddDiskMinutes(ctx, diskRows); err != nil {
				return err
			}
			if err := tx.AddNetMinutes(ctx, netRows); err != nil {
				return err
			}
			if err := tx.AddContainerMinutes(ctx, contRows); err != nil {
				return err
			}
			return tx.RollHour(ctx, future)
		}); err != nil {
			t.Fatal(err)
		}

		if err := db.View(ctx, func(tx *store.Tx) error {
			d, err := tx.DiskHistory(ctx, "srv_cap", "sda", store.HistoryMinute, t0.AddDate(-1, 0, 0), t0.AddDate(2, 0, 0))
			if err != nil {
				return err
			}
			n, err := tx.NetHistory(ctx, "srv_cap", "eth0", store.HistoryMinute, t0.AddDate(-1, 0, 0), t0.AddDate(2, 0, 0))
			if err != nil {
				return err
			}
			c, err := tx.ContainerHistory(ctx, "srv_cap", cid, store.HistoryMinute, t0.AddDate(-1, 0, 0), t0.AddDate(2, 0, 0))
			if err != nil {
				return err
			}
			// 31 hourly chunks written; cap keeps at most 25 newest hourly chunks.
			if len(d) != 25 || len(n) != 25 || len(c) != 25 {
				return fmt.Errorf("after cap: disk=%d net=%d cont=%d, want 25 each", len(d), len(n), len(c))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// measureSteadyStateSQLite fills a fresh SQLite store with full steady-state
// history across all 3 tiers (24 h of 1-minute, 7 d of 5-minute, 31 d of 1-hour)
// for nServers each having 1 disk, 1 interface, and nContainers containers, and
// returns the checkpointed on-disk SQLite size in bytes.
func measureSteadyStateSQLite(t *testing.T, nServers, nContainers int) int64 {
	t.Helper()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), fmt.Sprintf("steady-%d-%d.db", nServers, nContainers))
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	statSize := func() int64 {
		var total int64
		for _, suffix := range []string{"", "-wal"} {
			if fi, err := os.Stat(dbPath + suffix); err == nil {
				total += fi.Size()
			}
		}
		return total
	}
	baseBytes := statSize()

	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	minStart := now.Add(-24 * time.Hour)
	fiveStart := now.Add(-7 * 24 * time.Hour)
	hourStart := now.Add(-31 * 24 * time.Hour)

	for s := range nServers {
		srvID := fmt.Sprintf("srv_%02d_01jd6z3r8m9qf2k7t4xw5bnc0v", s)

		// Server, disk, and interface steady state (1440 x 1m, 2016 x 5m, 744 x 1h).
		if err := db.Update(ctx, func(tx *store.Tx) error {
			srvRows := make([]store.HistoryRow, 0, 1440+2016+744)
			diskRows := make([]store.DiskHistoryRow, 0, 1440+2016+744)
			netRows := make([]store.NetHistoryRow, 0, 1440+2016+744)
			addAll := func(tier int, at time.Time, cov int64, i int) {
				cpu := int64(15_000 + (i*317)%45_000)
				mem := int64((2 << 20) + (i*4096)%524288)
				srvRows = append(srvRows, store.HistoryRow{
					ServerID: srvID, Tier: tier, Start: at, CoveredMs: cov,
					CPUSum: cov * cpu, CPUMax: cpu + 12_000,
					MemorySum: cov * mem, MemoryMax: mem + 65536, MemoryTotal: 8 << 20,
					LoadSum: cov * int64(800+(i*23)%1500), LoadMax: 2800,
					RxBytes: int64(120_000 + (i*997)%500_000), RxPeak: 45_000,
					TxBytes: int64(80_000 + (i*733)%300_000), TxPeak: 25_000,
					ReadBytes: int64(250_000 + (i*1103)%900_000), ReadPeak: 90_000,
					WrittenBytes: int64(400_000 + (i*1301)%1_200_000), WrittenPeak: 140_000,
					ReadOps: int64(120 + i%400), ReadIOPSPeak: 8500,
					WriteOps: int64(240 + i%600), WriteIOPSPeak: 14_000,
				})
				diskRows = append(diskRows, store.DiskHistoryRow{
					ServerID: srvID, Device: "nvme0n1", Tier: tier, Start: at, CoveredMs: cov,
					ReadBytes: int64(250_000 + (i*1103)%900_000), ReadPeak: 90_000,
					WrittenBytes: int64(400_000 + (i*1301)%1_200_000), WrittenPeak: 140_000,
					ReadOps: int64(120 + i%400), ReadIOPSPeak: 8500,
					WriteOps: int64(240 + i%600), WriteIOPSPeak: 14_000,
				})
				netRows = append(netRows, store.NetHistoryRow{
					ServerID: srvID, Interface: "eth0", Tier: tier, Start: at, CoveredMs: cov,
					RxBytes: int64(120_000 + (i*997)%500_000), RxPeak: 45_000,
					TxBytes: int64(80_000 + (i*733)%300_000), TxPeak: 25_000,
				})
			}
			for i := range 1440 {
				addAll(store.HistoryMinute, minStart.Add(time.Duration(i)*time.Minute), 60_000, i)
			}
			for i := range 2016 {
				addAll(store.HistoryFiveMinute, fiveStart.Add(time.Duration(i)*5*time.Minute), 300_000, i)
			}
			for i := range 744 {
				addAll(store.HistoryHour, hourStart.Add(time.Duration(i)*time.Hour), 3_600_000, i)
			}
			if err := tx.AddHistoryRows(ctx, srvRows); err != nil {
				return err
			}
			if err := tx.AddDiskHistoryRows(ctx, diskRows); err != nil {
				return err
			}
			return tx.AddNetHistoryRows(ctx, netRows)
		}); err != nil {
			t.Fatal(err)
		}

		// Container steady state: batch 16 containers per transaction to keep memory low.
		for c0 := 0; c0 < nContainers; c0 += 16 {
			cEnd := min(c0+16, nContainers)
			if err := db.Update(ctx, func(tx *store.Tx) error {
				contRows := make([]store.ContainerHistoryRow, 0, (cEnd-c0)*(1440+2016+744))
				for c := c0; c < cEnd; c++ {
					cid := fmt.Sprintf("%08x%056x", s+1, c+1)
					limit := int64((512 + (c%4)*256) << 10)
					addC := func(tier int, at time.Time, cov int64, i int) {
						cpu := int64(5_000 + ((i*131 + c*97) % 35_000))
						mem := int64((128 << 10) + ((i*257 + c*521) % (128 << 10)))
						contRows = append(contRows, store.ContainerHistoryRow{
							ServerID: srvID, ContainerID: cid, Tier: tier, Start: at, CoveredMs: cov,
							CPUSum: cov * cpu, CPUMax: cpu + 4_500,
							MemorySum: cov * mem, MemoryMax: mem + 8192, MemLimit: limit,
						})
					}
					for i := range 1440 {
						addC(store.HistoryMinute, minStart.Add(time.Duration(i)*time.Minute), 60_000, i)
					}
					for i := range 2016 {
						addC(store.HistoryFiveMinute, fiveStart.Add(time.Duration(i)*5*time.Minute), 300_000, i)
					}
					for i := range 744 {
						addC(store.HistoryHour, hourStart.Add(time.Duration(i)*time.Hour), 3_600_000, i)
					}
				}
				return tx.AddContainerHistoryRows(ctx, contRows)
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return statSize() - baseBytes
}

func TestMetricsHistoryStorageCost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping storage cost measurement in -short mode")
	}
	// Target 1: 1 server with 30 containers <= 8 MB at steady state on SQLite.
	const limitSmall = 8 << 20
	smallBytes := measureSteadyStateSQLite(t, 1, 30)
	t.Logf("Steady-state SQLite storage (1 server, 1 disk, 1 iface, 30 containers; 24h 1m + 7d 5m + 31d 1h): %d bytes (%.2f MB), budget <= 8.00 MB",
		smallBytes, float64(smallBytes)/(1024*1024))
	if smallBytes > limitSmall {
		t.Errorf("1 server + 30 containers steady-state SQLite size = %.2f MB, exceeds 8 MB target", float64(smallBytes)/(1024*1024))
	}

	// Target 2: 10 servers with 128 containers each (1,280 containers) <= 300 MB at steady state on SQLite.
	const limitLarge = 300 << 20
	largeBytes := measureSteadyStateSQLite(t, 10, 128)
	t.Logf("Steady-state SQLite storage (10 servers, 1 disk + 1 iface + 128 containers each = 1,280 containers; 24h 1m + 7d 5m + 31d 1h): %d bytes (%.2f MB), budget <= 300.00 MB",
		largeBytes, float64(largeBytes)/(1024*1024))
	if largeBytes > limitLarge {
		t.Errorf("10 servers + 128 containers each steady-state SQLite size = %.2f MB, exceeds 300 MB target", float64(largeBytes)/(1024*1024))
	}
}

type captureLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *captureLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureLogHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureLogHandler) countMsg(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for i := range h.records {
		if h.records[i].Message == msg {
			n++
		}
	}
	return n
}

func hexBlobLiteral(b store.Backend, hex string) string {
	if b == store.Postgres {
		return fmt.Sprintf("decode('%s', 'hex')", hex)
	}
	return fmt.Sprintf("X'%s'", hex)
}

// N1: A corrupt or truncated history blob on one series never fails writes,
// reads, rollups, or pruning for any other server or chunk, logs once per bad
// chunk with its series, and heals on the next write.
func TestMetricsHistoryUnreadableBlobsDoNotBlockFleet(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, loc := storetest.Open(t, b)
		ctx := t.Context()
		h0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		h1 := h0.Add(time.Hour)
		expired := h0.Add(-40 * 24 * time.Hour)
		badCID := "1111111111111111111111111111111111111111111111111111111111111111"
		goodCID := "2222222222222222222222222222222222222222222222222222222222222222"

		seedMinute := func(at time.Time) error {
			return db.Update(ctx, func(tx *store.Tx) error {
				if err := tx.AddMinutes(ctx, []store.HistoryRow{
					{ServerID: "srv_bad", Start: at, CoveredMs: 60_000, CPUSum: 60_000 * 10_000, CPUMax: 10_000},
					{ServerID: "srv_good", Start: at, CoveredMs: 60_000, CPUSum: 60_000 * 25_000, CPUMax: 30_000},
				}); err != nil {
					return err
				}
				if err := tx.AddDiskMinutes(ctx, []store.DiskHistoryRow{
					{ServerID: "srv_bad", Device: "sda", Start: at, CoveredMs: 60_000, ReadBytes: 1000},
					{ServerID: "srv_good", Device: "nvme0n1", Start: at, CoveredMs: 60_000, ReadBytes: 2000},
				}); err != nil {
					return err
				}
				if err := tx.AddNetMinutes(ctx, []store.NetHistoryRow{
					{ServerID: "srv_bad", Interface: "eth0", Start: at, CoveredMs: 60_000, RxBytes: 500},
					{ServerID: "srv_good", Interface: "ens3", Start: at, CoveredMs: 60_000, RxBytes: 1500},
				}); err != nil {
					return err
				}
				return tx.AddContainerMinutes(ctx, []store.ContainerHistoryRow{
					{ServerID: "srv_bad", ContainerID: badCID, Start: at, CoveredMs: 60_000, CPUSum: 60_000 * 5_000, CPUMax: 5_000},
					{ServerID: "srv_good", ContainerID: goodCID, Start: at, CoveredMs: 60_000, CPUSum: 60_000 * 15_000, CPUMax: 20_000},
				})
			})
		}

		// Seed expired data, plus minute :00 of h0 and h1.
		for _, at := range []time.Time{expired, h0, h1} {
			if err := seedMinute(at); err != nil {
				t.Fatal(err)
			}
		}

		corruptSQL := func(serverID string, kind int, tier int, chunkStart time.Time, hexBytes string) {
			t.Helper()
			storetest.Exec(t, loc, fmt.Sprintf(
				`UPDATE history_chunks SET data = %s WHERE tier = %d AND chunk_start = %d AND series_id IN (SELECT id FROM history_series WHERE server_id = '%s' AND kind = %d)`,
				hexBlobLiteral(b, hexBytes), tier, chunkStart.UnixMilli(), serverID, kind,
			))
		}

		// Corrupt blob (bad version 0xff) in h0 container + disk, and truncated blob
		// (version 1, count 5, missing payload) in h1 container + net.
		corruptSQL("srv_bad", 3, store.HistoryMinute, h0, "ff010203")
		corruptSQL("srv_bad", 1, store.HistoryMinute, h0, "ff010203")
		corruptSQL("srv_bad", 3, store.HistoryMinute, h1, "010500")
		corruptSQL("srv_bad", 2, store.HistoryMinute, h1, "010500")

		// Capture logs to check unreadable chunks are logged once per chunk.
		capLogs := &captureLogHandler{}
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(capLogs))
		t.Cleanup(func() { slog.SetDefault(prevLogger) })

		// Reading srv_bad's chart across [h0, h1+1h) must not fail and must log each
		// unreadable chunk only once even when read repeatedly.
		for range 3 {
			if err := db.View(ctx, func(tx *store.Tx) error {
				if _, err := tx.ContainerHistory(ctx, "srv_bad", badCID, store.HistoryMinute, h0, h1.Add(time.Hour)); err != nil {
					return err
				}
				if _, err := tx.DiskHistory(ctx, "srv_bad", "sda", store.HistoryMinute, h0, h1.Add(time.Hour)); err != nil {
					return err
				}
				if _, err := tx.NetHistory(ctx, "srv_bad", "eth0", store.HistoryMinute, h0, h1.Add(time.Hour)); err != nil {
					return err
				}
				if _, err := tx.DiskHistoryDevices(ctx, "srv_bad", []int{store.HistoryMinute}, h0, h1.Add(time.Hour)); err != nil {
					return err
				}
				if _, err := tx.NetHistoryInterfaces(ctx, "srv_bad", []int{store.HistoryMinute}, h0, h1.Add(time.Hour)); err != nil {
					return err
				}
				return nil
			}); err != nil {
				t.Fatalf("reading with unreadable chunks failed: %v", err)
			}
		}
		if got := capLogs.countMsg("ignoring unreadable history chunk"); got != 4 {
			t.Errorf("logged unreadable chunks %d times across 3 reads of 4 bad chunks, want 4 (once each)", got)
		}

		// Rollups (5-minute and 1-hour) skip srv_bad's unreadable chunks and still
		// roll srv_good cleanly.
		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.RollFiveMinutes(ctx, h0); err != nil {
				return err
			}
			if err := tx.RollFiveMinutes(ctx, h1); err != nil {
				return err
			}
			if err := tx.RollHour(ctx, h0); err != nil {
				return err
			}
			return tx.RollHour(ctx, h1)
		}); err != nil {
			t.Fatalf("rollups failed with unreadable chunks: %v", err)
		}

		// Next minute write for both servers succeeds: srv_good keeps minute :00 and
		// adds :01; srv_bad overwrites its corrupt/truncated blobs and recovers.
		if err := seedMinute(h0.Add(time.Minute)); err != nil {
			t.Fatalf("minute flush h0+1m failed: %v", err)
		}
		if err := seedMinute(h1.Add(time.Minute)); err != nil {
			t.Fatalf("minute flush h1+1m failed: %v", err)
		}

		// Prune runs cleanly and removes expired data.
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.PruneHistory(ctx, h1.Add(2*time.Hour))
		}); err != nil {
			t.Fatalf("PruneHistory failed: %v", err)
		}

		if err := db.View(ctx, func(tx *store.Tx) error {
			// srv_good kept both minutes in h0 and h1 and rolled 5m + 1h rows.
			gMin, err := tx.ContainerHistory(ctx, "srv_good", goodCID, store.HistoryMinute, h0, h1.Add(time.Hour))
			if err != nil {
				return err
			}
			if len(gMin) != 4 {
				return fmt.Errorf("srv_good container minute rows = %d, want 4", len(gMin))
			}
			g5, err := tx.ContainerHistory(ctx, "srv_good", goodCID, store.HistoryFiveMinute, h0, h1.Add(time.Hour))
			if err != nil {
				return err
			}
			if len(g5) != 2 {
				return fmt.Errorf("srv_good container 5m rows = %d, want 2", len(g5))
			}
			g1h, err := tx.ContainerHistory(ctx, "srv_good", goodCID, store.HistoryHour, h0, h1.Add(time.Hour))
			if err != nil {
				return err
			}
			if len(g1h) != 2 {
				return fmt.Errorf("srv_good container 1h rows = %d, want 2", len(g1h))
			}
			// srv_bad healed on overwrite (has the :01 minutes in both h0 and h1).
			bMin, err := tx.ContainerHistory(ctx, "srv_bad", badCID, store.HistoryMinute, h0, h1.Add(time.Hour))
			if err != nil {
				return err
			}
			if len(bMin) != 2 {
				return fmt.Errorf("srv_bad recovered container minute rows = %d, want 2", len(bMin))
			}
			// Expired data was pruned.
			oldRows, err := tx.History(ctx, "srv_good", store.HistoryMinute, expired, expired.Add(time.Hour))
			if err != nil {
				return err
			}
			if len(oldRows) != 0 {
				return fmt.Errorf("expired server rows = %d, want 0 after prune", len(oldRows))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
