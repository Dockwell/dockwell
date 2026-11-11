// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func minuteOf(server string, at time.Time, coveredMs, cpu int64) store.HistoryRow {
	return store.HistoryRow{
		ServerID: server, Start: at, CoveredMs: coveredMs,
		CPUSum: cpu * coveredMs, CPUMax: cpu, MemoryTotal: 8 << 20, RxBytes: 1000, RxPeak: 50,
	}
}

func historyOf(t *testing.T, db *store.DB, server string, tier int, since, before time.Time) []store.HistoryRow {
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

// A minute two control planes both heard adds up: what each covered, what each
// counted, and the higher of each peak.
func TestAMinuteHeardTwiceAddsUp(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		t0 := time.Date(2026, 11, 10, 14, 2, 0, 0, time.UTC)
		for _, row := range []store.HistoryRow{minuteOf("srv_a", t0, 20_000, 10_000), minuteOf("srv_a", t0, 40_000, 40_000)} {
			if err := db.Update(ctx, func(tx *store.Tx) error { return tx.AddMinutes(ctx, []store.HistoryRow{row}) }); err != nil {
				t.Fatal(err)
			}
		}
		rows := historyOf(t, db, "srv_a", store.HistoryMinute, t0, t0.Add(time.Minute))
		if len(rows) != 1 {
			t.Fatalf("%d rows for one minute, want 1", len(rows))
		}
		r := rows[0]
		if r.CoveredMs != 60_000 || r.CPUSum/r.CoveredMs != 30_000 || r.CPUMax != 40_000 || r.RxBytes != 2000 || r.RxPeak != 50 {
			t.Errorf("the minute reads %+v", r)
		}
		if !r.Start.Equal(t0) || r.Tier != store.HistoryMinute {
			t.Errorf("the minute is keyed %v tier %d", r.Start, r.Tier)
		}
	})
}

// An hour is made from its minutes, and making it again gives the same row, not
// twice the row: a restart right after the roll-up, or a second control plane,
// counts nothing twice. The hours a control plane missed are the ones it finds.
func TestAnHourIsItsMinutesHoweverOftenItsRolled(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		h := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
		var minutes []store.HistoryRow
		for m := range 60 {
			minutes = append(minutes, minuteOf("srv_a", h.Add(time.Duration(m)*time.Minute), 60_000, int64(m*1000)))
		}
		// A minute of the next hour, and a quiet server's minutes of this one.
		minutes = append(minutes, minuteOf("srv_a", h.Add(time.Hour), 60_000, 99_000), minuteOf("srv_b", h, 0, 0))
		if err := db.Update(ctx, func(tx *store.Tx) error { return tx.AddMinutes(ctx, minutes) }); err != nil {
			t.Fatal(err)
		}
		var missed []time.Time
		if err := db.View(ctx, func(tx *store.Tx) error {
			var err error
			missed, err = tx.HoursToRoll(ctx, h.Add(-time.Hour), h.Add(time.Hour))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(missed) != 1 || !missed[0].Equal(h) {
			t.Fatalf("the hours to add up are %v, want [%v]", missed, h)
		}
		for range 2 {
			if err := db.Update(ctx, func(tx *store.Tx) error { return tx.RollHour(ctx, h.Add(17*time.Minute)) }); err != nil {
				t.Fatal(err)
			}
		}
		hours := historyOf(t, db, "srv_a", store.HistoryHour, h, h.Add(time.Hour))
		if len(hours) != 1 {
			t.Fatalf("%d hour rows, want 1", len(hours))
		}
		r := hours[0]
		if r.CoveredMs != 3_600_000 || r.CPUSum/r.CoveredMs != 29_500 || r.CPUMax != 59_000 || r.RxBytes != 60_000 || r.MemoryTotal != 8<<20 {
			t.Errorf("the hour reads %+v", r)
		}
		if quiet := historyOf(t, db, "srv_b", store.HistoryHour, h, h.Add(time.Hour)); len(quiet) != 1 || quiet[0].CoveredMs != 0 {
			t.Errorf("a quiet server's hour reads %+v, want one row with nothing covered", quiet)
		}
		if err := db.View(ctx, func(tx *store.Tx) error {
			var err error
			missed, err = tx.HoursToRoll(ctx, h.Add(-time.Hour), h.Add(time.Hour))
			return err
		}); err != nil || len(missed) != 0 {
			t.Errorf("after the roll-up the hours to add up are %v, %v", missed, err)
		}
	})
}

// However the clock goes, a server keeps at most a day of minutes, by key; and
// past their age, rows go.
func TestAServersHistoryIsCappedAndAged(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		t0 := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
		write := func(rows []store.HistoryRow) {
			t.Helper()
			if err := db.Update(ctx, func(tx *store.Tx) error { return tx.AddMinutes(ctx, rows) }); err != nil {
				t.Fatal(err)
			}
		}
		var rows []store.HistoryRow
		for m := range store.HistoryMinutes + 10 {
			rows = append(rows, minuteOf("srv_a", t0.Add(time.Duration(m)*time.Minute), 1000, 1))
		}
		write(rows)
		// A clock that jumped a year ahead writes there too, and the cap, kept as its
		// hour is rolled, keeps the newest; a minute's write alone keeps them all.
		write([]store.HistoryRow{minuteOf("srv_a", t0.AddDate(1, 0, 0), 1000, 1)})
		if n := len(historyOf(t, db, "srv_a", store.HistoryMinute, t0.AddDate(-1, 0, 0), t0.AddDate(2, 0, 0))); n != store.HistoryMinutes+11 {
			t.Fatalf("%d minute rows before the roll-up, want %d", n, store.HistoryMinutes+11)
		}
		if err := db.Update(ctx, func(tx *store.Tx) error { return tx.RollHour(ctx, t0.AddDate(1, 0, 0)) }); err != nil {
			t.Fatal(err)
		}
		all := historyOf(t, db, "srv_a", store.HistoryMinute, t0.AddDate(-1, 0, 0), t0.AddDate(2, 0, 0))
		if len(all) != store.HistoryMinutes {
			t.Fatalf("%d minute rows, want %d", len(all), store.HistoryMinutes)
		}
		if !all[0].Start.Equal(t0.Add(11 * time.Minute)) {
			t.Errorf("the oldest minute kept is %v, want %v", all[0].Start, t0.Add(11*time.Minute))
		}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.PruneHistory(ctx, t0.Add(store.MinutesKeptFor+30*time.Minute))
		}); err != nil {
			t.Fatal(err)
		}
		left := historyOf(t, db, "srv_a", store.HistoryMinute, t0.AddDate(-1, 0, 0), t0.AddDate(2, 0, 0))
		for _, r := range left {
			if r.Start.Before(t0.Add(30 * time.Minute)) {
				t.Fatalf("a minute from %v outlived the day", r.Start)
			}
		}
		if len(left) == 0 {
			t.Fatal("the prune took every minute")
		}
	})
}
