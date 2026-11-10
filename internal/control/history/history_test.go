// SPDX-License-Identifier: AGPL-3.0-only

package history

import (
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

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
	// It listens from before the minute, which it then hears whole.
	at.set(t0.Add(-30 * time.Second))
	r.Flush(t.Context())
	feed := func(offset time.Duration, cpu float64, rx uint64, up time.Duration) {
		at.set(t0.Add(offset))
		st := sample(cpu, rx)
		st.Uptime = durationpb.New(up)
		r.Sample("srv_a", st)
	}
	feed(1*time.Second, 99, 1000, 100*time.Second)  // primes: counts for nothing
	feed(6*time.Second, 50, 3000, 105*time.Second)  // 5 s at 50%
	feed(11*time.Second, 10, 4000, 110*time.Second) // 5 s at 10%
	feed(41*time.Second, 99, 500, 3*time.Second)    // after 30 s, a gap: counts for nothing, and the server booted
	feed(46*time.Second, 30, 1500, 8*time.Second)   // 5 s at 30%
	// The next minute's first sample comes before the flush, and stays there. It
	// comes 15 s after the last, at the idle pace, whose gap is 25 s.
	at.set(t0.Add(61 * time.Second))
	idle := sample(70, 2500)
	idle.Uptime = durationpb.New(23 * time.Second)
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
		if err := r.Roll(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	}
	hours := rowsOf(t, db, "srv_a", store.HistoryHour, h, h.Add(2*time.Hour))
	if len(hours) != 2 || hours[0].CoveredMs != 3_600_000 || hours[1].CoveredMs != 3_600_000 {
		t.Fatalf("the hours read %+v, want two whole hours", hours)
	}
}

// minuteOf is the one row a recorder wrote for a server's minute.
func minuteOf(t *testing.T, db *store.DB, server string, minute time.Time) store.HistoryRow {
	t.Helper()
	rows := rowsOf(t, db, server, store.HistoryMinute, minute, minute.Add(time.Minute))
	if len(rows) != 1 {
		t.Fatalf("%s has %d rows for %v, want 1", server, len(rows), minute)
	}
	return rows[0]
}

// listening is a recorder that has listened since before t0's minute.
func listening(t *testing.T, t0 time.Time, servers ...string) (*Recorder, *clock, *store.DB) {
	t.Helper()
	db, _ := storetest.Open(t, store.SQLite)
	at := &clock{now: t0.Add(-30 * time.Second)}
	r := &Recorder{DB: db, Now: at.Now, Servers: func() []string { return servers }}
	r.Flush(t.Context())
	return r, at, db
}

// A counter that drops while the server stays up, as tailscale0's does at each
// tailscaled restart, counts nothing for that step, not the server's whole life
// of bytes; one that drops as the server boots counts from zero.
func TestACounterThatDropsWithoutABootCountsNothing(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 14, 2, 0, 0, time.UTC)
	r, at, db := listening(t, t0, "srv_a")
	const lifetime = 5_000_000_000_000
	feed := func(offset time.Duration, rx uint64, up time.Duration) {
		at.set(t0.Add(offset))
		st := sample(10, rx)
		st.DiskReadBytes, st.DiskWrittenBytes = 0, 0
		st.Uptime = durationpb.New(up)
		r.Sample("srv_a", st)
	}
	feed(1*time.Second, lifetime, 1000*time.Second)
	feed(6*time.Second, lifetime+1000, 1005*time.Second)
	feed(11*time.Second, 1_000_000_000, 1010*time.Second) // tailscale0 made again: up all along
	feed(16*time.Second, 1_000_005_000, 1015*time.Second)
	feed(21*time.Second, 2000, 4*time.Second) // a boot: from zero
	at.set(t0.Add(61 * time.Second))
	r.Flush(t.Context())
	m := minuteOf(t, db, "srv_a", t0)
	if m.RxBytes != 1000+5000+2000 || m.RxPeak != 1000 {
		t.Errorf("moved %d in, at most %d B/s; want 8000 and 1000", m.RxBytes, m.RxPeak)
	}
}

// A rate is over the agent's own times between its readings: samples a stalled
// stream delivers at once still each cover their interval.
func TestARateIsOverTheAgentsOwnTimes(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 14, 2, 0, 0, time.UTC)
	r, at, db := listening(t, t0, "srv_a")
	taken := t0.Add(-20 * time.Second) // the server's clock is behind, which cancels out
	feed := func(arrived, read time.Duration, rx uint64) {
		at.set(t0.Add(arrived))
		st := sample(10, rx)
		st.DiskReadBytes, st.DiskWrittenBytes = 0, 0
		st.Time = timestamppb.New(taken.Add(read))
		st.Uptime = durationpb.New(time.Hour + read)
		r.Sample("srv_a", st)
	}
	feed(1*time.Second, 0, 1_000_000)
	// Ten seconds of counters, 10 MB, arriving 2 ms after the one before.
	feed(1*time.Second+2*time.Millisecond, 10*time.Second, 11_000_000)
	at.set(t0.Add(61 * time.Second))
	r.Flush(t.Context())
	if m := minuteOf(t, db, "srv_a", t0); m.RxBytes != 10_000_000 || m.RxPeak != 1_000_000 {
		t.Errorf("moved %d in, at most %d B/s; want 10000000 and 1000000", m.RxBytes, m.RxPeak)
	}
}

// The first sample on a new stream, as after the agent restarted, counts for no
// time: a new collector's first reports no CPU. Its counters still give the bytes
// moved since the sample before.
func TestTheFirstSampleOfANewStreamCountsForNothing(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 14, 2, 0, 0, time.UTC)
	r, at, db := listening(t, t0, "srv_a")
	feed := func(offset time.Duration, cpu float64, rx uint64) {
		at.set(t0.Add(offset))
		st := sample(cpu, rx)
		st.Uptime = durationpb.New(time.Hour + offset)
		r.Sample("srv_a", st)
	}
	feed(1*time.Second, 80, 1000)
	feed(6*time.Second, 80, 2000)
	feed(11*time.Second, 80, 3000)
	r.Connected("srv_a") // the agent restarted, 4 s
	feed(15*time.Second, 0, 4000)
	feed(20*time.Second, 80, 5000)
	at.set(t0.Add(61 * time.Second))
	r.Flush(t.Context())
	m := minuteOf(t, db, "srv_a", t0)
	if m.CoveredMs != 15_000 || m.CPUSum/m.CoveredMs != 80_000 {
		t.Errorf("covered %d ms at %d average; want 15000 at 80000", m.CoveredMs, m.CPUSum/max(m.CoveredMs, 1))
	}
	if m.RxBytes != 4000 {
		t.Errorf("moved %d in, want 4000 across the restart", m.RxBytes)
	}
}

// The minute the recorder starts in wasn't heard whole, so it has no row, for any
// server: not one that reads quiet while Dockwell listened. The next has.
func TestTheMinuteTheRecorderStartsInHasNoRow(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	t0 := time.Date(2026, 11, 10, 14, 2, 0, 0, time.UTC)
	at := &clock{now: t0.Add(20 * time.Second)}
	r := &Recorder{DB: db, Now: at.Now, Servers: func() []string { return []string{"srv_a", "srv_quiet"} }}
	r.Sample("srv_a", sample(50, 1000))
	at.set(t0.Add(65 * time.Second))
	r.Sample("srv_a", sample(50, 2000))
	at.set(t0.Add(70 * time.Second))
	r.Sample("srv_a", sample(50, 3000))
	at.set(t0.Add(121 * time.Second))
	if wrote := r.Flush(t.Context()); len(wrote) != 1 || !wrote[0].Equal(t0.Add(time.Minute)) {
		t.Fatalf("the flushes wrote %v, want only %v", wrote, t0.Add(time.Minute))
	}
	for _, id := range []string{"srv_a", "srv_quiet"} {
		if rows := rowsOf(t, db, id, store.HistoryMinute, t0, t0.Add(time.Minute)); len(rows) != 0 {
			t.Errorf("%s has a row for the start minute: %+v", id, rows)
		}
	}
	if m := minuteOf(t, db, "srv_a", t0.Add(time.Minute)); m.CoveredMs != 5_000 {
		t.Errorf("the minute after covered %d ms, want 5000", m.CoveredMs)
	}
	if m := minuteOf(t, db, "srv_quiet", t0.Add(time.Minute)); m.CoveredMs != 0 {
		t.Errorf("a quiet server's minute after covered %d ms", m.CoveredMs)
	}
}

// While the store doesn't take them, an hour of ended minutes waits at most, and
// what was kept of a server the fleet no longer lists goes.
func TestEndedMinutesWaitAnHourAtMost(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 14, 2, 0, 0, time.UTC)
	r, at, _ := listening(t, t0, "srv_a")
	for m := range 70 {
		at.set(t0.Add(time.Duration(m)*time.Minute + time.Second))
		r.Sample("srv_a", sample(10, uint64(m)))
		r.Sample("srv_gone", sample(10, uint64(m)))
	}
	r.mu.Lock()
	waiting, oldest := len(r.ended), r.ended[0].minute
	r.mu.Unlock()
	if waiting != keepEnded || !oldest.Equal(t0.Add(9*time.Minute)) {
		t.Errorf("%d minutes wait, the oldest %v; want %d from %v", waiting, oldest, keepEnded, t0.Add(9*time.Minute))
	}
	r.Flush(t.Context())
	r.mu.Lock()
	_, kept := r.last["srv_gone"]
	r.mu.Unlock()
	if kept {
		t.Error("a server the fleet no longer lists is still kept")
	}
}

// Each hour rolls the hour that ended; catching up, as the control plane starts,
// rolls the hours it missed too.
func TestOnlyTheStartCatchesUp(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	h := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	var minutes []store.HistoryRow
	for m := range 120 {
		minutes = append(minutes, store.HistoryRow{ServerID: "srv_a", Start: h.Add(time.Duration(m) * time.Minute), CoveredMs: 60_000})
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error { return tx.AddMinutes(t.Context(), minutes) }); err != nil {
		t.Fatal(err)
	}
	at := &clock{now: h.Add(2*time.Hour + 30*time.Second)}
	r := &Recorder{DB: db, Now: at.Now}
	if err := r.Roll(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if hours := rowsOf(t, db, "srv_a", store.HistoryHour, h, h.Add(2*time.Hour)); len(hours) != 1 || !hours[0].Start.Equal(h.Add(time.Hour)) {
		t.Fatalf("an hourly roll made %+v, want the hour that ended alone", hours)
	}
	if err := r.Roll(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if hours := rowsOf(t, db, "srv_a", store.HistoryHour, h, h.Add(2*time.Hour)); len(hours) != 2 {
		t.Fatalf("catching up made %d hours, want 2", len(hours))
	}
}
