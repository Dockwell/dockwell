// SPDX-License-Identifier: AGPL-3.0-only

package history

import (
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func sample(cpu float64, rx uint64) *dockwellv1.ServerStats {
	return &dockwellv1.ServerStats{
		CpuPercent: cpu, MemoryUsedBytes: 2 << 30, MemoryTotalBytes: 8 << 30, Load1: 0.5,
		NetworkRxBytes: rx, NetworkTxBytes: 1, DiskReadBytes: rx * 2, DiskWrittenBytes: 1,
		Disks:    []*dockwellv1.DiskUsage{{UsedBytes: 30, SizeBytes: 100}, {UsedBytes: 90, SizeBytes: 100}},
		Interval: durationpb.New(5 * time.Second),
	}
}

func rowsOf(t *testing.T, db *store.DB, server string, tier int, since, before time.Time) []store.HistoryRow {
	t.Helper()
	var rows []store.HistoryRow
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		rows, err = tx.History(t.Context(), server, tier, since, before)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// Each sample counts for the time since the one before it, by the control
// plane's clock; one after a gap, as the first after a reconnect, counts for
// nothing; bytes come from the counters, from zero after a reboot; and a server
// that sent nothing gets a row that says so.
func TestAMinuteIsWhatItsSamplesCovered(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	t0 := time.Date(2026, 11, 10, 14, 2, 0, 0, time.UTC)
	at := &clock{now: t0}
	r := &Recorder{DB: db, Now: at.Now, Servers: func() []string { return []string{"srv_a", "srv_quiet"} }}
	feed := func(offset time.Duration, cpu float64, rx uint64) {
		at.set(t0.Add(offset))
		r.Sample("srv_a", sample(cpu, rx))
	}
	feed(1*time.Second, 99, 1000)  // primes: counts for nothing
	feed(6*time.Second, 50, 3000)  // 5 s at 50%
	feed(11*time.Second, 10, 4000) // 5 s at 10%
	feed(41*time.Second, 99, 500)  // after 30 s, a gap: counts for nothing, and the server booted
	feed(46*time.Second, 30, 1500) // 5 s at 30%
	// The next minute's first sample comes before the flush, and stays there. It
	// comes 15 s after the last, at the idle pace, whose gap is 25 s.
	at.set(t0.Add(61 * time.Second))
	idle := sample(70, 2500)
	idle.Interval = durationpb.New(10 * time.Second)
	r.Sample("srv_a", idle)
	at.set(t0.Add(61*time.Second + 50*time.Millisecond))
	if wrote := r.Flush(t.Context()); len(wrote) != 1 || !wrote[0].Equal(t0) {
		t.Fatalf("the flush wrote %v, want [%v]", wrote, t0)
	}
	rows := rowsOf(t, db, "srv_a", store.HistoryMinute, t0, t0.Add(time.Minute))
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	m := rows[0]
	if m.CoveredMs != 15_000 || m.CPUSum/m.CoveredMs != 30_000 || m.CPUMax != 50_000 {
		t.Errorf("covered %d ms at %d average, highest %d; want 15000, 30000, 50000", m.CoveredMs, m.CPUSum/max(m.CoveredMs, 1), m.CPUMax)
	}
	// 2000 and 1000, then from zero after the boot 500, then 1000.
	if m.RxBytes != 4500 || m.ReadBytes != 9000 {
		t.Errorf("moved %d in and read %d, want 4500 and 9000", m.RxBytes, m.ReadBytes)
	}
	if m.RxPeak != 400 {
		t.Errorf("the highest rate in is %d B/s, want 400", m.RxPeak)
	}
	if m.MemoryTotal != 8<<20 || m.MemorySum/m.CoveredMs != 2<<20 || m.DiskMax != 900 || m.LoadMax != 500 {
		t.Errorf("memory %d of %d KiB, fullest disk %d, load %d", m.MemorySum/m.CoveredMs, m.MemoryTotal, m.DiskMax, m.LoadMax)
	}
	quiet := rowsOf(t, db, "srv_quiet", store.HistoryMinute, t0, t0.Add(time.Minute))
	if len(quiet) != 1 || quiet[0].CoveredMs != 0 {
		t.Errorf("a server that sent nothing reads %+v, want one row with nothing covered", quiet)
	}
	// The sample of the minute after is written with it.
	at.set(t0.Add(2*time.Minute + 10*time.Millisecond))
	r.Flush(t.Context())
	next := rowsOf(t, db, "srv_a", store.HistoryMinute, t0.Add(time.Minute), t0.Add(2*time.Minute))
	if len(next) != 1 || next[0].CoveredMs != 15_000 || next[0].CPUMax != 70_000 {
		t.Errorf("the next minute reads %+v", next)
	}
}

// An hour is rolled from its minutes once it has ended, and an hour the control
// plane missed is rolled as it starts again; rolling twice changes nothing.
func TestTheHoursAreRolledAndCaughtUp(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	h := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	var minutes []store.HistoryRow
	for m := range 120 {
		minutes = append(minutes, store.HistoryRow{ServerID: "srv_a", Start: h.Add(time.Duration(m) * time.Minute), CoveredMs: 60_000, CPUSum: 60_000 * 1000, CPUMax: 1000})
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error { return tx.AddMinutes(t.Context(), minutes) }); err != nil {
		t.Fatal(err)
	}
	at := &clock{now: h.Add(2*time.Hour + 30*time.Second)}
	r := &Recorder{DB: db, Now: at.Now}
	for range 2 {
		if err := r.Roll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	hours := rowsOf(t, db, "srv_a", store.HistoryHour, h, h.Add(2*time.Hour))
	if len(hours) != 2 || hours[0].CoveredMs != 3_600_000 || hours[1].CoveredMs != 3_600_000 {
		t.Fatalf("the hours read %+v, want two whole hours", hours)
	}
}
