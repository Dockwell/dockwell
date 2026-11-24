// SPDX-License-Identifier: AGPL-3.0-only

// Package history keeps server and container resource history from the samples
// each agent already sends (ADR 0065): it adds up each minute in memory by the
// control plane's own clock, writes a minute row for every server plus per-disk
// I/O, per-interface network and per-container CPU/memory rows at each minute's
// end, rolls 5-minute buckets every 5 minutes (kept 7 days), and rolls 1-hour
// buckets every hour (kept 31 days) while pruning expired rows.
package history

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
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
	// keepEnded is how many ended minutes wait to be written at most, while the
	// store doesn't answer: an hour's, past which the oldest go.
	keepEnded = 60
	// maxHeldRows bounds total in-memory rows queued in ended minutes while the
	// store doesn't answer, so a multi-server fleet with 128 containers/server
	// stays well within the control plane's 250 MB memory budget. When exceeded,
	// breakdown rows (containers, disks, interfaces) are dropped oldest-first
	// before server rows.
	maxHeldRows = 4096
	// writeWait bounds writing a minute, so a store that hangs holds up no other.
	writeWait = 45 * time.Second
	// maxDisksPerSample and maxIfacesPerSample bound what one server's sample
	// can write per minute.
	maxDisksPerSample  = 16
	maxIfacesPerSample = 16
	// MaxContainersPerServer caps how many containers per server are recorded
	// in history; it matches collect.MaxTrackedContainers.
	MaxContainersPerServer = 128
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
	// started is when the recorder began to listen: a minute that began before
	// it wasn't heard whole, so it isn't written (no row, not a quiet one).
	started time.Time
	// minute is the start of the minute being added up, and the maps hold its sums.
	minute     time.Time
	sums       map[string]*store.HistoryRow
	diskSums   map[diskKey]*store.DiskHistoryRow
	netSums    map[netKey]*store.NetHistoryRow
	contSums   map[contKey]*store.ContainerHistoryRow
	ended      []ended
	last       map[string]last
	fresh      map[string]bool
	contTrack  map[string]containerTrack
	toRollFive []time.Time
	toRoll     []time.Time
}

type containerTrack struct {
	omitted uint32
	tracked map[string]struct{}
}

type (
	diskKey struct{ serverID, device string }
	netKey  struct{ serverID, iface string }
	contKey struct{ serverID, containerID string }
)

type ended struct {
	minute   time.Time
	sums     map[string]*store.HistoryRow
	diskSums map[diskKey]*store.DiskHistoryRow
	netSums  map[netKey]*store.NetHistoryRow
	contSums map[contKey]*store.ContainerHistoryRow
}

type diskLast struct {
	read, written, readOps, writeOps uint64
}

type netLast struct {
	rx, tx uint64
}

// last is what the recorder kept of a server's previous sample.
type last struct {
	at, taken                time.Time
	uptime                   time.Duration
	rx, tx, read, written    uint64
	readOps, writeOps        uint64
	haveBytes, haveDiskBytes bool
	haveDiskOps              bool
	disks                    map[string]diskLast
	ifaces                   map[string]netLast
}

// Connected says a server's agent has just connected, on a new stream: its next
// sample is the first of a collector that may have just started, which reports
// no CPU, so it counts for no time, though its counters still give the bytes
// moved since the last one (ADR 0065).
func (r *Recorder) Connected(serverID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fresh == nil {
		r.fresh = map[string]bool{}
	}
	r.fresh[serverID] = true
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
	if since > 0 && float64(since) <= gapAfter*float64(interval) && !r.fresh[serverID] {
		weight = since.Milliseconds()
	}
	delete(r.fresh, serverID)
	add := func(sum, high *int64, v int64) {
		*sum += v * weight
		*high = max(*high, v)
	}
	if weight > 0 {
		row.CoveredMs += weight
		add(&row.CPUSum, &row.CPUMax, int64(math.Round(safeFloat(st.GetCpuPercent())*1000)))
		add(&row.MemorySum, &row.MemoryMax, int64(st.GetMemoryUsedBytes()>>10))
		add(&row.SwapSum, &row.SwapMax, int64(st.GetSwapUsedBytes()>>10))
		add(&row.LoadSum, &row.LoadMax, int64(math.Round(safeFloat(st.GetLoad1())*1000)))
		add(&row.DiskSum, &row.DiskMax, fullest(st))
	}
	row.MemoryTotal = max(row.MemoryTotal, int64(st.GetMemoryTotalBytes()>>10))

	cur := last{
		at:      at,
		rx:      st.GetNetworkRxBytes(),
		tx:      st.GetNetworkTxBytes(),
		read:    st.GetDiskReadBytes(),
		written: st.GetDiskWrittenBytes(),
	}
	if t := st.GetTime(); t != nil {
		cur.taken = t.AsTime()
	}
	if u := st.GetUptime(); u != nil {
		cur.uptime = u.AsDuration()
	}
	cur.haveBytes = cur.rx > 0 || cur.tx > 0
	cur.haveDiskBytes = cur.read > 0 || cur.written > 0

	// Aggregate disk IOPS from per-disk counters when present.
	if disks := st.GetDiskIo(); len(disks) > 0 {
		cur.disks = make(map[string]diskLast, min(len(disks), maxDisksPerSample))
		for i, d := range disks {
			if i >= maxDisksPerSample {
				break
			}
			name := d.GetDevice()
			if name == "" || len(name) > 64 {
				continue
			}
			cur.disks[name] = diskLast{
				read:     d.GetReadBytes(),
				written:  d.GetWrittenBytes(),
				readOps:  d.GetReadOps(),
				writeOps: d.GetWriteOps(),
			}
			cur.readOps += d.GetReadOps()
			cur.writeOps += d.GetWriteOps()
		}
		cur.haveDiskOps = cur.readOps > 0 || cur.writeOps > 0
	}
	if ifaces := st.GetNetworkIo(); len(ifaces) > 0 {
		cur.ifaces = make(map[string]netLast, min(len(ifaces), maxIfacesPerSample))
		for i, nif := range ifaces {
			if i >= maxIfacesPerSample {
				break
			}
			name := nif.GetInterface()
			if name == "" || len(name) > 64 {
				continue
			}
			cur.ifaces[name] = netLast{rx: nif.GetRxBytes(), tx: nif.GetTxBytes()}
		}
	}

	booted := cur.uptime > 0 && prev.uptime > 0 && cur.uptime < prev.uptime
	over := since
	if !cur.taken.IsZero() && !prev.taken.IsZero() {
		over = cur.taken.Sub(prev.taken)
	}
	moved := func(now, before uint64, total, peak *int64, scale float64) {
		var d uint64
		switch {
		case now >= before:
			d = now - before
		case booted:
			d = now
		}
		*total += int64(min(d, math.MaxInt64/2))
		if secs := over.Seconds(); secs > 0 {
			*peak = max(*peak, int64(float64(d)/secs*scale))
		}
	}
	if seen && prev.haveBytes && cur.haveBytes {
		moved(cur.rx, prev.rx, &row.RxBytes, &row.RxPeak, 1)
		moved(cur.tx, prev.tx, &row.TxBytes, &row.TxPeak, 1)
	}
	if seen && prev.haveDiskBytes && cur.haveDiskBytes {
		moved(cur.read, prev.read, &row.ReadBytes, &row.ReadPeak, 1)
		moved(cur.written, prev.written, &row.WrittenBytes, &row.WrittenPeak, 1)
	}
	if seen && prev.haveDiskOps && cur.haveDiskOps {
		moved(cur.readOps, prev.readOps, &row.ReadOps, &row.ReadIOPSPeak, 1000)
		moved(cur.writeOps, prev.writeOps, &row.WriteOps, &row.WriteIOPSPeak, 1000)
	}

	// Per-disk I/O breakdown.
	for name, dc := range cur.disks {
		k := diskKey{serverID: serverID, device: name}
		drow := r.diskSums[k]
		if drow == nil {
			drow = &store.DiskHistoryRow{ServerID: serverID, Device: name, Start: r.minute}
			r.diskSums[k] = drow
		}
		if weight > 0 {
			drow.CoveredMs += weight
		}
		if seen {
			if dp, ok := prev.disks[name]; ok {
				moved(dc.read, dp.read, &drow.ReadBytes, &drow.ReadPeak, 1)
				moved(dc.written, dp.written, &drow.WrittenBytes, &drow.WrittenPeak, 1)
				moved(dc.readOps, dp.readOps, &drow.ReadOps, &drow.ReadIOPSPeak, 1000)
				moved(dc.writeOps, dp.writeOps, &drow.WriteOps, &drow.WriteIOPSPeak, 1000)
			}
		}
	}

	// Per-interface network breakdown.
	for name, nc := range cur.ifaces {
		k := netKey{serverID: serverID, iface: name}
		nrow := r.netSums[k]
		if nrow == nil {
			nrow = &store.NetHistoryRow{ServerID: serverID, Interface: name, Start: r.minute}
			r.netSums[k] = nrow
		}
		if weight > 0 {
			nrow.CoveredMs += weight
		}
		if seen {
			if np, ok := prev.ifaces[name]; ok {
				moved(nc.rx, np.rx, &nrow.RxBytes, &nrow.RxPeak, 1)
				moved(nc.tx, np.tx, &nrow.TxBytes, &nrow.TxPeak, 1)
			}
		}
	}

	// Per-container CPU & memory breakdown (keyed by 64-hex container ID).
	omitted := st.GetContainersOmitted()
	allConts := st.GetContainers()
	if len(allConts) > MaxContainersPerServer {
		omitted += uint32(len(allConts) - MaxContainersPerServer) //nolint:gosec // bounded by sample size
	}
	if r.contTrack == nil {
		r.contTrack = map[string]containerTrack{}
	}
	tracked := make(map[string]struct{}, min(len(allConts), MaxContainersPerServer))
	for i, c := range allConts {
		if i >= MaxContainersPerServer {
			break
		}
		cid := c.GetContainerId()
		if !isContainerID(cid) {
			continue
		}
		tracked[cid] = struct{}{}
		if weight <= 0 {
			continue
		}
		k := contKey{serverID: serverID, containerID: cid}
		crow := r.contSums[k]
		if crow == nil {
			crow = &store.ContainerHistoryRow{ServerID: serverID, ContainerID: cid, Start: r.minute}
			r.contSums[k] = crow
		}
		crow.CoveredMs += weight
		cpu := int64(math.Round(safeFloat(c.GetCpuPercent()) * 1000))
		mem := int64(c.GetMemoryBytes() >> 10)
		crow.CPUSum += cpu * weight
		crow.CPUMax = max(crow.CPUMax, cpu)
		crow.MemorySum += mem * weight
		crow.MemoryMax = max(crow.MemoryMax, mem)
		crow.MemLimit = max(crow.MemLimit, int64(c.GetMemoryLimitBytes()>>10))
	}
	r.contTrack[serverID] = containerTrack{omitted: omitted, tracked: tracked}

	r.last[serverID] = cur
}

// ContainerUntracked reports whether serverID exceeded MaxContainersPerServer
// on its latest sample and containerID was not among the tracked containers.
func (r *Recorder) ContainerUntracked(serverID, containerID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tr, ok := r.contTrack[serverID]
	if !ok || tr.omitted == 0 {
		return false
	}
	_, kept := tr.tracked[containerID]
	return !kept
}

func safeFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

func isContainerID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
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
		if r.started.IsZero() {
			r.started = at
		}
		r.minute = m
		r.sums = map[string]*store.HistoryRow{}
		r.diskSums = map[diskKey]*store.DiskHistoryRow{}
		r.netSums = map[netKey]*store.NetHistoryRow{}
		r.contSums = map[contKey]*store.ContainerHistoryRow{}
	case m.After(r.minute):
		if !r.minute.Before(r.started) {
			r.ended = append(r.ended, ended{
				minute:   r.minute,
				sums:     r.sums,
				diskSums: r.diskSums,
				netSums:  r.netSums,
				contSums: r.contSums,
			})
		}
		if len(r.ended) > keepEnded {
			r.logger().Warn("the store hasn't taken the servers' history for an hour, so the oldest minute waiting is left out",
				slog.Time("minute", r.ended[0].minute))
			r.ended = r.ended[1:]
		}
		r.boundHeldRows()
		r.minute = m
		r.sums = map[string]*store.HistoryRow{}
		r.diskSums = map[diskKey]*store.DiskHistoryRow{}
		r.netSums = map[netKey]*store.NetHistoryRow{}
		r.contSums = map[contKey]*store.ContainerHistoryRow{}
	}
}

func (r *Recorder) boundHeldRows() {
	total := 0
	for _, e := range r.ended {
		total += len(e.sums) + len(e.diskSums) + len(e.netSums) + len(e.contSums)
	}
	if total <= maxHeldRows {
		return
	}
	droppedDetail := false
	// Drop breakdown rows from oldest minutes first while preserving server rows.
	for i := 0; i < len(r.ended) && total > maxHeldRows; i++ {
		if n := len(r.ended[i].contSums); n > 0 {
			r.ended[i].contSums = nil
			total -= n
			droppedDetail = true
		}
		if total > maxHeldRows {
			if n := len(r.ended[i].diskSums); n > 0 {
				r.ended[i].diskSums = nil
				total -= n
				droppedDetail = true
			}
		}
		if total > maxHeldRows {
			if n := len(r.ended[i].netSums); n > 0 {
				r.ended[i].netSums = nil
				total -= n
				droppedDetail = true
			}
		}
	}
	if droppedDetail {
		r.logger().Warn("held history backlog exceeded its row limit while waiting on the store, so the oldest container, disk and interface breakdowns were left out",
			slog.Int("limit", maxHeldRows))
	}
	for len(r.ended) > 1 && total > maxHeldRows {
		total -= len(r.ended[0].sums)
		r.ended = r.ended[1:]
	}
}

// Flush writes every minute that has ended by now: a row for each server of the
// fleet plus per-disk, per-interface and per-container rows, all in one
// transaction per minute. It reports the minutes it wrote, oldest first.
func (r *Recorder) Flush(ctx context.Context) []time.Time {
	r.mu.Lock()
	r.turn(r.now())
	todo := r.ended
	r.ended = nil
	r.mu.Unlock()

	var servers []string
	if r.Servers != nil && len(todo) > 0 {
		servers = r.Servers()
		r.mu.Lock()
		for id := range r.last {
			if !slices.Contains(servers, id) {
				delete(r.last, id)
				delete(r.fresh, id)
				delete(r.contTrack, id)
			}
		}
		r.mu.Unlock()
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
		diskRows := make([]store.DiskHistoryRow, 0, len(e.diskSums))
		for _, drow := range e.diskSums {
			drow.Start = e.minute
			diskRows = append(diskRows, *drow)
		}
		netRows := make([]store.NetHistoryRow, 0, len(e.netSums))
		for _, nrow := range e.netSums {
			nrow.Start = e.minute
			netRows = append(netRows, *nrow)
		}
		contRows := make([]store.ContainerHistoryRow, 0, len(e.contSums))
		for _, crow := range e.contSums {
			crow.Start = e.minute
			contRows = append(contRows, *crow)
		}
		if len(rows) == 0 && len(diskRows) == 0 && len(netRows) == 0 && len(contRows) == 0 {
			continue
		}
		write, cancel := context.WithTimeout(ctx, writeWait)
		err := r.DB.Update(write, func(tx *store.Tx) error {
			if err := tx.AddMinutes(write, rows); err != nil {
				return err
			}
			if err := tx.AddDiskMinutes(write, diskRows); err != nil {
				return err
			}
			if err := tx.AddNetMinutes(write, netRows); err != nil {
				return err
			}
			return tx.AddContainerMinutes(write, contRows)
		})
		cancel()
		if err != nil {
			r.logger().WarnContext(ctx, "can't write a minute of the servers' history, so it's left out", slog.Time("minute", e.minute), slog.Any("error", err))
			continue
		}
		wrote = append(wrote, e.minute)
	}
	return wrote
}

// RollFiveMinutes makes the 5-minute block that ended before now from its
// 1-minute rows, plus any previously failed 5-minute blocks (and on catchUp,
// any missed blocks from the last 24 h).
func (r *Recorder) RollFiveMinutes(ctx context.Context, catchUp bool) error {
	now := r.now()
	thisBlock := now.Truncate(5 * time.Minute)
	since := now.Add(-store.MinutesKeptFor).Truncate(5 * time.Minute).Add(5 * time.Minute)
	r.mu.Lock()
	blocks := slices.DeleteFunc(r.toRollFive, func(b time.Time) bool { return b.Before(since) })
	r.toRollFive = nil
	r.mu.Unlock()
	if catchUp {
		var missed []time.Time
		if err := r.DB.View(ctx, func(tx *store.Tx) error {
			var err error
			missed, err = tx.FiveMinutesToRoll(ctx, since, thisBlock)
			return err
		}); err != nil {
			return err
		}
		blocks = append(blocks, missed...)
	}
	blocks = append(blocks, thisBlock.Add(-5*time.Minute))
	slices.SortFunc(blocks, func(a, b time.Time) int { return a.Compare(b) })
	blocks = slices.CompactFunc(blocks, time.Time.Equal)
	var failed []time.Time
	var errs []error
	for i, b := range blocks {
		if ctx.Err() != nil {
			failed = append(failed, blocks[i:]...)
			errs = append(errs, ctx.Err())
			break
		}
		if err := r.DB.Update(ctx, func(tx *store.Tx) error {
			return tx.RollFiveMinutes(ctx, b)
		}); err != nil {
			failed = append(failed, b)
			errs = append(errs, err)
		}
	}
	if len(failed) > 0 {
		r.mu.Lock()
		r.toRollFive = append(r.toRollFive, failed...)
		r.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Roll makes the hour that ended before now from its minutes, with any hour
// whose roll failed before; then it deletes what's past its age. To catch up, as
// the control plane starts, it also rolls every hour of the last day that has
// minutes and no row yet, as after the control plane was away.
func (r *Recorder) Roll(ctx context.Context, catchUp bool) error {
	now := r.now()
	thisHour := now.Truncate(time.Hour)
	since := now.Add(-store.MinutesKeptFor).Truncate(time.Hour).Add(time.Hour)
	r.mu.Lock()
	hours := slices.DeleteFunc(r.toRoll, func(h time.Time) bool { return h.Before(since) })
	r.toRoll = nil
	r.mu.Unlock()
	if catchUp {
		var missed []time.Time
		if err := r.DB.View(ctx, func(tx *store.Tx) error {
			var err error
			missed, err = tx.HoursToRoll(ctx, since, thisHour)
			return err
		}); err != nil {
			return err
		}
		hours = append(hours, missed...)
	}
	hours = append(hours, thisHour.Add(-time.Hour))
	slices.SortFunc(hours, func(a, b time.Time) int { return a.Compare(b) })
	hours = slices.CompactFunc(hours, time.Time.Equal)
	var failed []time.Time
	var errs []error
	for i, h := range hours {
		if ctx.Err() != nil {
			failed = append(failed, hours[i:]...)
			errs = append(errs, ctx.Err())
			break
		}
		if err := r.DB.Update(ctx, func(tx *store.Tx) error {
			return tx.RollHour(ctx, h)
		}); err != nil {
			failed = append(failed, h)
			errs = append(errs, err)
		}
	}
	if len(failed) > 0 {
		r.mu.Lock()
		r.toRoll = append(r.toRoll, failed...)
		r.mu.Unlock()
	}
	if ctx.Err() == nil {
		if err := r.DB.Update(ctx, func(tx *store.Tx) error {
			return tx.PruneHistory(ctx, now)
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

const (
	rollFiveFailed = "can't add up the servers' 5-minute history, so recent 5-minute points may wait until the next 5-minute roll"
	rollFailed     = "can't add up the servers' history by the hour, so the last hour reads empty on the longer ranges until the next hour tries again"
)

// Run writes each minute as it ends, rolls 5-minute blocks every 5 minutes, and
// rolls hours + prunes expired rows every hour, until ctx ends. On startup it
// catches up any missed 5-minute blocks and hours.
func (r *Recorder) Run(ctx context.Context) {
	r.mu.Lock()
	if r.started.IsZero() {
		r.started = r.now()
	}
	r.mu.Unlock()
	if err := r.RollFiveMinutes(ctx, true); err != nil && ctx.Err() == nil {
		r.logger().WarnContext(ctx, rollFiveFailed, slog.Any("error", err))
	}
	if err := r.Roll(ctx, true); err != nil && ctx.Err() == nil {
		r.logger().WarnContext(ctx, rollFailed, slog.Any("error", err))
	}
	for {
		now := r.now()
		next := now.Truncate(time.Minute).Add(time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-time.After(next.Sub(now) + 50*time.Millisecond):
		}
		r.FlushAndRoll(ctx)
	}
}

// FlushAndRoll flushes all ended minutes and rolls any completed 5-minute
// blocks and hours they cover. When more than one minute was backlogged (e.g.
// after a store outage), it also catches up any missed blocks from the last 24 h.
func (r *Recorder) FlushAndRoll(ctx context.Context) {
	wrote := r.Flush(ctx)
	if len(wrote) == 0 {
		return
	}
	var fiveBlocks, hourBlocks []time.Time
	for _, m := range wrote {
		end := m.Add(time.Minute)
		if end.Truncate(5 * time.Minute).Equal(end) {
			fiveBlocks = append(fiveBlocks, m.Truncate(5*time.Minute))
		}
		if end.Truncate(time.Hour).Equal(end) {
			hourBlocks = append(hourBlocks, m.Truncate(time.Hour))
		}
	}
	catchUp := len(wrote) > 1
	if len(fiveBlocks) > 0 || catchUp {
		r.mu.Lock()
		r.toRollFive = append(r.toRollFive, fiveBlocks...)
		r.mu.Unlock()
		if err := r.RollFiveMinutes(ctx, catchUp); err != nil && ctx.Err() == nil {
			r.logger().WarnContext(ctx, rollFiveFailed, slog.Any("error", err))
		}
	}
	if len(hourBlocks) > 0 || catchUp {
		r.mu.Lock()
		r.toRoll = append(r.toRoll, hourBlocks...)
		r.mu.Unlock()
		if err := r.Roll(ctx, catchUp); err != nil && ctx.Err() == nil {
			r.logger().WarnContext(ctx, rollFailed, slog.Any("error", err))
		}
	}
}
