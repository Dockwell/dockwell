// SPDX-License-Identifier: AGPL-3.0-only

// Package history keeps a server's history from the samples its agent already
// sends (ADR 0065): it adds up each minute in memory, by the control plane's own
// clock, writes a row for every server of the fleet at each minute's end, and
// makes each hour from its minutes once the hour has ended.
package history

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	// gapAfter is how many of its intervals a sample may come after the one
	// before it and still count for that time (ADR 0040's gap rule); one that
	// comes later counts for nothing, as the first after a reconnect, which
	// reports no CPU, does.
	gapAfter = 2.5
	// defaultInterval is a sample's interval from an agent that doesn't say it.
	defaultInterval = 5 * time.Second
)

// Recorder adds up the samples of each server a minute at a time.
type Recorder struct {
	DB *store.DB
	// Servers are the fleet's servers, by ID: each gets a row every minute,
	// with nothing covered when it sent nothing.
	Servers func() []string
	// Now is the control plane's clock. Nil is time.Now.
	Now    func() time.Time
	Logger *slog.Logger

	mu sync.Mutex
	// minute is the start of the minute being added up, and sums what it has.
	minute time.Time
	sums   map[string]*store.HistoryRow
	// ended are minutes that ended with their sums, for Flush to write.
	ended []ended
	last  map[string]last
}

type ended struct {
	minute time.Time
	sums   map[string]*store.HistoryRow
}

// last is what the recorder kept of a server's previous sample.
type last struct {
	at                       time.Time
	rx, tx, read, written    uint64
	haveBytes, haveDiskBytes bool
}

func (r *Recorder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Recorder) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}

// Sample adds a server's sample to the minute it arrives in.
func (r *Recorder) Sample(serverID string, st *dockwellv1.ServerStats) {
	at := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turn(at)
	if r.last == nil {
		r.last = map[string]last{}
	}
	row := r.sums[serverID]
	if row == nil {
		row = &store.HistoryRow{ServerID: serverID, Start: r.minute}
		r.sums[serverID] = row
	}
	prev, seen := r.last[serverID]
	interval := defaultInterval
	if d := st.GetInterval(); d != nil && d.AsDuration() > 0 {
		interval = d.AsDuration()
	}
	var since time.Duration
	if seen {
		since = at.Sub(prev.at)
	}
	weight := int64(0)
	if since > 0 && float64(since) <= gapAfter*float64(interval) {
		weight = since.Milliseconds()
	}
	add := func(sum, high *int64, v int64) {
		*sum += v * weight
		*high = max(*high, v)
	}
	if weight > 0 {
		row.CoveredMs += weight
		add(&row.CPUSum, &row.CPUMax, int64(math.Round(st.GetCpuPercent()*1000)))
		add(&row.MemorySum, &row.MemoryMax, int64(st.GetMemoryUsedBytes()>>10))
		add(&row.SwapSum, &row.SwapMax, int64(st.GetSwapUsedBytes()>>10))
		add(&row.LoadSum, &row.LoadMax, int64(math.Round(st.GetLoad1()*1000)))
		add(&row.DiskSum, &row.DiskMax, fullest(st))
	}
	row.MemoryTotal = max(row.MemoryTotal, int64(st.GetMemoryTotalBytes()>>10))

	// Bytes moved come from the counters since boot, whatever the gap: a counter
	// lower than the last one means the server booted, and counts from zero.
	cur := last{at: at, rx: st.GetNetworkRxBytes(), tx: st.GetNetworkTxBytes(), read: st.GetDiskReadBytes(), written: st.GetDiskWrittenBytes()}
	cur.haveBytes = cur.rx > 0 || cur.tx > 0
	cur.haveDiskBytes = cur.read > 0 || cur.written > 0
	moved := func(now, before uint64, bytes, peak *int64) {
		d := now
		if now >= before {
			d = now - before
		}
		*bytes += int64(min(d, math.MaxInt64/2))
		if secs := since.Seconds(); secs > 0 {
			*peak = max(*peak, int64(float64(d)/secs))
		}
	}
	if seen && prev.haveBytes && cur.haveBytes {
		moved(cur.rx, prev.rx, &row.RxBytes, &row.RxPeak)
		moved(cur.tx, prev.tx, &row.TxBytes, &row.TxPeak)
	}
	if seen && prev.haveDiskBytes && cur.haveDiskBytes {
		moved(cur.read, prev.read, &row.ReadBytes, &row.ReadPeak)
		moved(cur.written, prev.written, &row.WrittenBytes, &row.WrittenPeak)
	}
	r.last[serverID] = cur
}

// fullest is the share of its fullest disk a sample says is used, in thousandths.
func fullest(st *dockwellv1.ServerStats) int64 {
	var share int64
	for _, d := range st.GetDisks() {
		if d.GetSizeBytes() > 0 {
			share = max(share, int64(float64(d.GetUsedBytes())/float64(d.GetSizeBytes())*1000))
		}
	}
	return share
}

// turn starts the minute at belongs to, once the one being added up has ended,
// setting that one aside for Flush. Called with r.mu held.
func (r *Recorder) turn(at time.Time) {
	m := at.Truncate(time.Minute)
	switch {
	case r.minute.IsZero():
		r.minute, r.sums = m, map[string]*store.HistoryRow{}
	case m.After(r.minute):
		r.ended = append(r.ended, ended{minute: r.minute, sums: r.sums})
		r.minute, r.sums = m, map[string]*store.HistoryRow{}
	}
}

// Flush writes every minute that has ended by now: a row for each server of the
// fleet, with nothing covered for one that sent nothing, in one transaction.
// A write whose result is lost isn't tried again: a second try can't tell
// whether the first landed, and a minute lost is safer than one counted twice.
// It reports the minutes it wrote, oldest first.
func (r *Recorder) Flush(ctx context.Context) []time.Time {
	r.mu.Lock()
	r.turn(r.now())
	todo := r.ended
	r.ended = nil
	r.mu.Unlock()

	var servers []string
	if r.Servers != nil && len(todo) > 0 {
		servers = r.Servers()
	}
	var wrote []time.Time
	for _, e := range todo {
		rows := make([]store.HistoryRow, 0, len(e.sums)+len(servers))
		for _, row := range e.sums {
			row.Start = e.minute
			rows = append(rows, *row)
		}
		for _, id := range servers {
			if e.sums[id] == nil {
				rows = append(rows, store.HistoryRow{ServerID: id, Start: e.minute})
			}
		}
		if len(rows) == 0 {
			continue
		}
		if err := r.DB.Update(ctx, func(tx *store.Tx) error { return tx.AddMinutes(ctx, rows) }); err != nil {
			r.logger().WarnContext(ctx, "can't write a minute of the servers' history, so it's left out", slog.Time("minute", e.minute), slog.Any("error", err))
			continue
		}
		wrote = append(wrote, e.minute)
	}
	return wrote
}

// Roll makes the hour that ended before now from its minutes, and every hour of
// the last day that has minutes and no row yet, as after the control plane was
// away; then it deletes what's past its age.
func (r *Recorder) Roll(ctx context.Context) error {
	now := r.now()
	thisHour := now.Truncate(time.Hour)
	// Only hours whose minutes are all still kept: an hour that began more than a
	// day ago has lost some, and rolling it again would make it smaller.
	since := now.Add(-store.MinutesKeptFor).Truncate(time.Hour).Add(time.Hour)
	var hours []time.Time
	if err := r.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		hours, err = tx.HoursToRoll(ctx, since, thisHour)
		return err
	}); err != nil {
		return err
	}
	last := thisHour.Add(-time.Hour)
	if len(hours) == 0 || !hours[len(hours)-1].Equal(last) {
		hours = append(hours, last)
	}
	return r.DB.Update(ctx, func(tx *store.Tx) error {
		for _, h := range hours {
			if err := tx.RollHour(ctx, h); err != nil {
				return err
			}
		}
		return tx.PruneHistory(ctx, now)
	})
}

// Run writes each minute as it ends, and each hour once its last minute is
// written, until ctx ends. As it starts it rolls the hours it missed.
func (r *Recorder) Run(ctx context.Context) {
	if err := r.Roll(ctx); err != nil && ctx.Err() == nil {
		r.logger().WarnContext(ctx, "can't add up the servers' history by the hour", slog.Any("error", err))
	}
	for {
		now := r.now()
		next := now.Truncate(time.Minute).Add(time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-time.After(next.Sub(now) + 50*time.Millisecond):
		}
		for _, m := range r.Flush(ctx) {
			if m.Add(time.Minute).Truncate(time.Hour).Equal(m.Add(time.Minute)) {
				if err := r.Roll(ctx); err != nil && ctx.Err() == nil {
					r.logger().WarnContext(ctx, "can't add up the servers' history by the hour", slog.Any("error", err))
				}
			}
		}
	}
}
