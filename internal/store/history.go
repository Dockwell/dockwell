// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A server's history comes in three tiers: 1-minute points for 24 hours,
// 5-minute points for 7 days, and 1-hour points for 31 days.
// HistoryMinute = 1 and HistoryHour = 2 keep their original values from
// migration 0029; HistoryFiveMinute = 3 adds the 5-minute tier.
const (
	HistoryMinute     = 1
	HistoryHour       = 2
	HistoryFiveMinute = 3

	// Row caps per server for each tier, whatever happens to the clock.
	HistoryMinutes     = 1440 // 24 h * 60
	HistoryFiveMinutes = 2016 // 7 d * 24 * 12
	HistoryHours       = 744  // 31 d * 24

	// Retention durations for each tier.
	MinutesKeptFor     = 24 * time.Hour
	FiveMinutesKeptFor = 7 * 24 * time.Hour
	HoursKeptFor       = 31 * 24 * time.Hour
)

// HistoryRow is a minute, 5-minute, or hour bucket of a server's history.
// Weighted sums divide by CoveredMs for averages; bytes/ops are totals moved
// across the bucket with peak per-second rates sampled in it. A row with
// CoveredMs 0 records time the control plane listened and heard nothing.
type HistoryRow struct {
	ServerID  string
	Tier      int
	Start     time.Time
	CoveredMs int64

	CPUSum, CPUMax                    int64 // thousandths of a percent
	MemorySum, MemoryMax, MemoryTotal int64 // KiB
	SwapSum, SwapMax                  int64 // KiB
	LoadSum, LoadMax                  int64 // 1-minute load in thousandths
	DiskSum, DiskMax                  int64 // fullest disk share in thousandths

	RxBytes, RxPeak           int64
	TxBytes, TxPeak           int64
	ReadBytes, ReadPeak       int64
	WrittenBytes, WrittenPeak int64
	ReadOps, ReadIOPSPeak     int64 // ops, and peak ops/s in thousandths
	WriteOps, WriteIOPSPeak   int64 // ops, and peak ops/s in thousandths
}

// DiskHistoryRow is one bucket of I/O history for a single whole block device.
type DiskHistoryRow struct {
	ServerID  string
	Device    string
	Tier      int
	Start     time.Time
	CoveredMs int64

	ReadBytes, ReadPeak       int64
	WrittenBytes, WrittenPeak int64
	ReadOps, ReadIOPSPeak     int64 // ops, and peak ops/s in thousandths
	WriteOps, WriteIOPSPeak   int64 // ops, and peak ops/s in thousandths
}

// NetHistoryRow is one bucket of traffic history for a single physical interface.
type NetHistoryRow struct {
	ServerID  string
	Interface string
	Tier      int
	Start     time.Time
	CoveredMs int64

	RxBytes, RxPeak int64
	TxBytes, TxPeak int64
}

// ContainerHistoryRow is one bucket of CPU and memory history for a single container.
type ContainerHistoryRow struct {
	ServerID    string
	ContainerID string
	Tier        int
	Start       time.Time
	CoveredMs   int64

	CPUSum, CPUMax                 int64 // thousandths of a percent
	MemorySum, MemoryMax, MemLimit int64 // KiB
}

const historyColumns = `server_id, tier, start_at, covered_ms, cpu_sum, cpu_max, memory_sum, memory_max, memory_total,
	swap_sum, swap_max, load_sum, load_max, disk_sum, disk_max, rx_bytes, rx_peak, tx_bytes, tx_peak,
	read_bytes, read_peak, written_bytes, written_peak, read_ops, read_iops_peak, write_ops, write_iops_peak`

var (
	historySums  = []string{"covered_ms", "cpu_sum", "memory_sum", "swap_sum", "load_sum", "disk_sum", "rx_bytes", "tx_bytes", "read_bytes", "written_bytes", "read_ops", "write_ops"}
	historyHighs = []string{"cpu_max", "memory_max", "memory_total", "swap_max", "load_max", "disk_max", "rx_peak", "tx_peak", "read_peak", "written_peak", "read_iops_peak", "write_iops_peak"}
)

func (r *HistoryRow) values() []any {
	return []any{
		r.ServerID, r.Tier, millis(r.Start), r.CoveredMs, r.CPUSum, r.CPUMax, r.MemorySum, r.MemoryMax, r.MemoryTotal,
		r.SwapSum, r.SwapMax, r.LoadSum, r.LoadMax, r.DiskSum, r.DiskMax, r.RxBytes, r.RxPeak, r.TxBytes, r.TxPeak,
		r.ReadBytes, r.ReadPeak, r.WrittenBytes, r.WrittenPeak, r.ReadOps, r.ReadIOPSPeak, r.WriteOps, r.WriteIOPSPeak,
	}
}

// minutesAStatement is how many rows one statement writes.
const minutesAStatement = 100

func (t *Tx) mergeSet(table string, sums, highs []string) string {
	set := make([]string, 0, len(sums)+len(highs))
	for _, c := range sums {
		set = append(set, fmt.Sprintf("%s = %s.%s + excluded.%s", c, table, c, c))
	}
	for _, c := range highs {
		set = append(set, fmt.Sprintf("%s = %s(%s.%s, excluded.%s)", c, t.dialect.greatest, table, c, c))
	}
	return strings.Join(set, ", ")
}

func placeholders(cols int) string {
	return "(" + strings.TrimSuffix(strings.Repeat("?, ", cols), ", ") + ")"
}

// AddMinutes writes minute rows for server_history, merging with any existing
// row for the same server and minute.
func (t *Tx) AddMinutes(ctx context.Context, rows []HistoryRow) error {
	if len(rows) == 0 {
		return nil
	}
	set := t.mergeSet("server_history", historySums, historyHighs)
	one := placeholders(27)
	for batch := range slices.Chunk(rows, minutesAStatement) {
		values := make([]any, 0, len(batch)*27)
		for i := range batch {
			r := batch[i]
			r.Tier = HistoryMinute
			values = append(values, r.values()...)
		}
		query := `INSERT INTO server_history (` + historyColumns + `) VALUES ` + strings.TrimSuffix(strings.Repeat(one+", ", len(batch)), ", ") + `
			ON CONFLICT (server_id, tier, start_at) DO UPDATE SET ` + set
		if _, err := t.exec(ctx, query, values...); err != nil {
			return fmt.Errorf("can't write the servers' history for %s: %w", batch[0].Start.Format(time.RFC3339), err)
		}
	}
	return nil
}

// AddHistoryRows writes rows of any tier directly into server_history (used by
// tests and seeding tools).
func (t *Tx) AddHistoryRows(ctx context.Context, rows []HistoryRow) error {
	if len(rows) == 0 {
		return nil
	}
	set := t.mergeSet("server_history", historySums, historyHighs)
	one := placeholders(27)
	for batch := range slices.Chunk(rows, minutesAStatement) {
		values := make([]any, 0, len(batch)*27)
		for i := range batch {
			r := batch[i]
			values = append(values, r.values()...)
		}
		query := `INSERT INTO server_history (` + historyColumns + `) VALUES ` + strings.TrimSuffix(strings.Repeat(one+", ", len(batch)), ", ") + `
			ON CONFLICT (server_id, tier, start_at) DO UPDATE SET ` + set
		if _, err := t.exec(ctx, query, values...); err != nil {
			return fmt.Errorf("can't write server history rows: %w", err)
		}
	}
	return nil
}

// AddDiskMinutes writes 1-minute per-disk I/O rows into packed chunks, merging on conflict.
func (t *Tx) AddDiskMinutes(ctx context.Context, rows []DiskHistoryRow) error {
	for i := range rows {
		rows[i].Tier = HistoryMinute
	}
	return t.AddDiskHistoryRows(ctx, rows)
}

// AddDiskHistoryRows writes per-disk I/O history rows of any tier into packed chunks, merging on conflict.
func (t *Tx) AddDiskHistoryRows(ctx context.Context, rows []DiskHistoryRow) error {
	if len(rows) == 0 {
		return nil
	}
	skeys := make([]seriesKey, len(rows))
	for i := range rows {
		skeys[i] = seriesKey{serverID: rows[i].ServerID, kind: seriesKindDisk, name: rows[i].Device}
	}
	ids, err := t.ensureSeriesIDs(ctx, skeys)
	if err != nil {
		return err
	}
	grouped := map[chunkKey][]diskSlot{}
	meta := map[chunkKey]seriesKey{}
	var ckeys []chunkKey
	for i := range rows {
		r := &rows[i]
		cs, slot, ok := chunkSlot(r.Tier, r.Start)
		if !ok {
			continue
		}
		ck := chunkKey{seriesID: ids[skeys[i]], tier: r.Tier, chunkStart: millis(cs)}
		if _, exists := grouped[ck]; !exists {
			ckeys = append(ckeys, ck)
			meta[ck] = skeys[i]
		}
		grouped[ck] = append(grouped[ck], diskSlot{
			slot:          slot,
			coveredMs:     r.CoveredMs,
			readBytes:     r.ReadBytes,
			readPeak:      r.ReadPeak,
			writtenBytes:  r.WrittenBytes,
			writtenPeak:   r.WrittenPeak,
			readOps:       r.ReadOps,
			readIOPSPeak:  r.ReadIOPSPeak,
			writeOps:      r.WriteOps,
			writeIOPSPeak: r.WriteIOPSPeak,
		})
	}
	existing, err := t.loadChunks(ctx, ckeys)
	if err != nil {
		return err
	}
	writes := make([]chunkWrite, 0, len(ckeys))
	for _, ck := range ckeys {
		pts, err := decodeDiskChunk(existing[ck])
		if err != nil {
			sk := meta[ck]
			t.warnBadChunk(ck, sk.serverID, sk.kind, sk.name, err)
			pts = nil
		}
		pts = mergeDiskSlots(pts, grouped[ck])
		writes = append(writes, chunkWrite{key: ck, data: encodeDiskChunk(pts)})
	}
	return t.upsertChunks(ctx, writes)
}

// AddNetMinutes writes 1-minute per-interface network rows into packed chunks, merging on conflict.
func (t *Tx) AddNetMinutes(ctx context.Context, rows []NetHistoryRow) error {
	for i := range rows {
		rows[i].Tier = HistoryMinute
	}
	return t.AddNetHistoryRows(ctx, rows)
}

// AddNetHistoryRows writes per-interface network history rows of any tier into packed chunks, merging on conflict.
func (t *Tx) AddNetHistoryRows(ctx context.Context, rows []NetHistoryRow) error {
	if len(rows) == 0 {
		return nil
	}
	skeys := make([]seriesKey, len(rows))
	for i := range rows {
		skeys[i] = seriesKey{serverID: rows[i].ServerID, kind: seriesKindNet, name: rows[i].Interface}
	}
	ids, err := t.ensureSeriesIDs(ctx, skeys)
	if err != nil {
		return err
	}
	grouped := map[chunkKey][]netSlot{}
	meta := map[chunkKey]seriesKey{}
	var ckeys []chunkKey
	for i := range rows {
		r := &rows[i]
		cs, slot, ok := chunkSlot(r.Tier, r.Start)
		if !ok {
			continue
		}
		ck := chunkKey{seriesID: ids[skeys[i]], tier: r.Tier, chunkStart: millis(cs)}
		if _, exists := grouped[ck]; !exists {
			ckeys = append(ckeys, ck)
			meta[ck] = skeys[i]
		}
		grouped[ck] = append(grouped[ck], netSlot{
			slot:      slot,
			coveredMs: r.CoveredMs,
			rxBytes:   r.RxBytes,
			rxPeak:    r.RxPeak,
			txBytes:   r.TxBytes,
			txPeak:    r.TxPeak,
		})
	}
	existing, err := t.loadChunks(ctx, ckeys)
	if err != nil {
		return err
	}
	writes := make([]chunkWrite, 0, len(ckeys))
	for _, ck := range ckeys {
		pts, err := decodeNetChunk(existing[ck])
		if err != nil {
			sk := meta[ck]
			t.warnBadChunk(ck, sk.serverID, sk.kind, sk.name, err)
			pts = nil
		}
		pts = mergeNetSlots(pts, grouped[ck])
		writes = append(writes, chunkWrite{key: ck, data: encodeNetChunk(pts)})
	}
	return t.upsertChunks(ctx, writes)
}

// AddContainerMinutes writes 1-minute per-container rows into packed chunks, merging on conflict.
func (t *Tx) AddContainerMinutes(ctx context.Context, rows []ContainerHistoryRow) error {
	for i := range rows {
		rows[i].Tier = HistoryMinute
	}
	return t.AddContainerHistoryRows(ctx, rows)
}

// AddContainerHistoryRows writes per-container history rows of any tier into packed chunks, merging on conflict.
func (t *Tx) AddContainerHistoryRows(ctx context.Context, rows []ContainerHistoryRow) error {
	if len(rows) == 0 {
		return nil
	}
	skeys := make([]seriesKey, len(rows))
	for i := range rows {
		skeys[i] = seriesKey{serverID: rows[i].ServerID, kind: seriesKindContainer, name: rows[i].ContainerID}
	}
	ids, err := t.ensureSeriesIDs(ctx, skeys)
	if err != nil {
		return err
	}
	grouped := map[chunkKey][]contSlot{}
	meta := map[chunkKey]seriesKey{}
	var ckeys []chunkKey
	for i := range rows {
		r := &rows[i]
		cs, slot, ok := chunkSlot(r.Tier, r.Start)
		if !ok {
			continue
		}
		ck := chunkKey{seriesID: ids[skeys[i]], tier: r.Tier, chunkStart: millis(cs)}
		if _, exists := grouped[ck]; !exists {
			ckeys = append(ckeys, ck)
			meta[ck] = skeys[i]
		}
		grouped[ck] = append(grouped[ck], contSlot{
			slot:      slot,
			coveredMs: r.CoveredMs,
			cpuSum:    r.CPUSum,
			cpuMax:    r.CPUMax,
			memorySum: r.MemorySum,
			memoryMax: r.MemoryMax,
			memLimit:  r.MemLimit,
		})
	}
	existing, err := t.loadChunks(ctx, ckeys)
	if err != nil {
		return err
	}
	writes := make([]chunkWrite, 0, len(ckeys))
	for _, ck := range ckeys {
		pts, err := decodeContChunk(existing[ck])
		if err != nil {
			sk := meta[ck]
			t.warnBadChunk(ck, sk.serverID, sk.kind, sk.name, err)
			pts = nil
		}
		pts = mergeContSlots(pts, grouped[ck])
		writes = append(writes, chunkWrite{key: ck, data: encodeContChunk(pts)})
	}
	return t.upsertChunks(ctx, writes)
}

// capHistory deletes a server's rows of a tier past the newest keep, by key, as
// one range: what an age can't bound when the clock jumps.
func (t *Tx) capHistory(ctx context.Context, serverID string, tier, keep int) error {
	if _, err := t.exec(ctx, `DELETE FROM server_history WHERE server_id = ? AND tier = ? AND start_at < (
		SELECT start_at FROM server_history WHERE server_id = ? AND tier = ? ORDER BY start_at DESC LIMIT 1 OFFSET ?)`,
		serverID, tier, serverID, tier, keep-1); err != nil {
		return fmt.Errorf("can't keep %s's history to %d rows: %w", serverID, keep, err)
	}
	return nil
}

func (t *Tx) rollupTable(ctx context.Context, table string, keyCols, sums, highs []string, toTier int, start, end time.Time) error {
	pick := make([]string, 0, len(sums)+len(highs))
	set := make([]string, 0, len(sums)+len(highs))
	for _, c := range sums {
		pick = append(pick, "SUM("+c+")")
		set = append(set, c+" = excluded."+c)
	}
	for _, c := range highs {
		pick = append(pick, "MAX("+c+")")
		set = append(set, c+" = excluded."+c)
	}
	keys := strings.Join(keyCols, ", ")
	valCols := strings.Join(append(append([]string{}, sums...), highs...), ", ")
	query := `INSERT INTO ` + table + ` (` + keys + `, tier, start_at, ` + valCols + `)
		SELECT ` + keys + `, ?, ?, ` + strings.Join(pick, ", ") + `
		FROM ` + table + ` WHERE tier = ? AND start_at >= ? AND start_at < ? GROUP BY ` + keys + `
		ON CONFLICT (` + keys + `, tier, start_at) DO UPDATE SET ` + strings.Join(set, ", ")
	if _, err := t.exec(ctx, query, toTier, millis(start), HistoryMinute, millis(start), millis(end)); err != nil {
		return fmt.Errorf("can't roll up %s for %s: %w", table, start.Format(time.RFC3339), err)
	}
	return nil
}

// RollFiveMinutes makes the 5-minute bucket starting at block from its 1-minute
// rows across server_history and all packed breakdown chunks, replacing any
// existing bucket.
func (t *Tx) RollFiveMinutes(ctx context.Context, block time.Time) error {
	block = block.UTC().Truncate(5 * time.Minute)
	end := block.Add(5 * time.Minute)
	if err := t.rollupTable(ctx, "server_history", []string{"server_id"}, historySums, historyHighs, HistoryFiveMinute, block, end); err != nil {
		return err
	}
	_, err := t.rollupChunks(ctx, HistoryFiveMinute, block, end)
	return err
}

// FiveMinutesToRoll returns the 5-minute blocks from since up to before that
// have minute rows of a server with no 5-minute row yet.
func (t *Tx) FiveMinutesToRoll(ctx context.Context, since, before time.Time) ([]time.Time, error) {
	rows, err := t.query(ctx, `SELECT DISTINCT m.start_at - (m.start_at % 300000) FROM server_history m
		WHERE m.tier = ? AND m.start_at >= ? AND m.start_at < ? AND NOT EXISTS (
			SELECT 1 FROM server_history f WHERE f.server_id = m.server_id AND f.tier = ? AND f.start_at = m.start_at - (m.start_at % 300000))
		ORDER BY 1`, HistoryMinute, millis(since), millis(before), HistoryFiveMinute)
	if err != nil {
		return nil, fmt.Errorf("can't read which 5-minute blocks are to add up: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return nil, fmt.Errorf("can't read which 5-minute blocks are to add up: %w", err)
		}
		out = append(out, fromMillis(ms))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("can't read which 5-minute blocks are to add up: %w", err)
	}
	return out, nil
}

// RollHour makes the hour that starts at hour from its minute rows across
// server_history and all packed breakdown chunks, replacing any hour bucket
// already there, and enforces per-server and per-series row/chunk caps.
func (t *Tx) RollHour(ctx context.Context, hour time.Time) error {
	hour = hour.UTC().Truncate(time.Hour)
	end := hour.Add(time.Hour)
	if err := t.rollupTable(ctx, "server_history", []string{"server_id"}, historySums, historyHighs, HistoryHour, hour, end); err != nil {
		return err
	}
	rolledSeries, err := t.rollupChunks(ctx, HistoryHour, hour, end)
	if err != nil {
		return err
	}
	rows, err := t.query(ctx, `SELECT DISTINCT server_id FROM server_history WHERE tier = ? AND start_at = ?`, HistoryHour, millis(hour))
	if err != nil {
		return fmt.Errorf("can't read whose hours were added up: %w", err)
	}
	var servers []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("can't read whose hours were added up: %w", err)
		}
		servers = append(servers, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("can't read whose hours were added up: %w", err)
	}
	for _, id := range servers {
		if err := t.capHistory(ctx, id, HistoryHour, HistoryHours); err != nil {
			return err
		}
		if err := t.capHistory(ctx, id, HistoryFiveMinute, HistoryFiveMinutes); err != nil {
			return err
		}
		if err := t.capHistory(ctx, id, HistoryMinute, HistoryMinutes); err != nil {
			return err
		}
	}
	for _, sid := range rolledSeries {
		for _, tier := range []int{HistoryHour, HistoryFiveMinute, HistoryMinute} {
			if err := t.capChunks(ctx, sid, tier, tierChunkCap(tier)); err != nil {
				return err
			}
		}
	}
	return nil
}

// HoursToRoll are the hours from since up to before that have minute rows of a
// server with no hour row yet: what a control plane that was away when they
// ended didn't add up.
func (t *Tx) HoursToRoll(ctx context.Context, since, before time.Time) ([]time.Time, error) {
	rows, err := t.query(ctx, `SELECT DISTINCT m.start_at - (m.start_at % 3600000) FROM server_history m
		WHERE m.tier = ? AND m.start_at >= ? AND m.start_at < ? AND NOT EXISTS (
			SELECT 1 FROM server_history h WHERE h.server_id = m.server_id AND h.tier = ? AND h.start_at = m.start_at - (m.start_at % 3600000))
		ORDER BY 1`, HistoryMinute, millis(since), millis(before), HistoryHour)
	if err != nil {
		return nil, fmt.Errorf("can't read which hours are to add up: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return nil, fmt.Errorf("can't read which hours are to add up: %w", err)
		}
		out = append(out, fromMillis(ms))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("can't read which hours are to add up: %w", err)
	}
	return out, nil
}

// PruneHistory deletes 1-minute data older than 24 h, 5-minute data older than
// 7 days, and 1-hour data older than 31 days across server_history and
// history_chunks, then removes orphaned history_series entries.
func (t *Tx) PruneHistory(ctx context.Context, now time.Time) error {
	now = now.UTC()
	minCut := millis(now.Add(-MinutesKeptFor))
	fiveCut := millis(now.Add(-FiveMinutesKeptFor))
	hourCut := millis(now.Add(-HoursKeptFor))
	if _, err := t.exec(ctx, `DELETE FROM server_history WHERE (tier = ? AND start_at < ?) OR (tier = ? AND start_at < ?) OR (tier = ? AND start_at < ?)`,
		HistoryMinute, minCut, HistoryFiveMinute, fiveCut, HistoryHour, hourCut); err != nil {
		return fmt.Errorf("can't delete server_history past its age: %w", err)
	}
	// A chunk expires once its entire span [chunk_start, chunk_start + span) is
	// older than the tier's retention cutoff.
	minChunkCut := millis(now.Add(-MinutesKeptFor - tierChunkSpan(HistoryMinute)))
	fiveChunkCut := millis(now.Add(-FiveMinutesKeptFor - tierChunkSpan(HistoryFiveMinute)))
	hourChunkCut := millis(now.Add(-HoursKeptFor - tierChunkSpan(HistoryHour)))
	if _, err := t.exec(ctx, `DELETE FROM history_chunks WHERE (tier = ? AND chunk_start <= ?) OR (tier = ? AND chunk_start <= ?) OR (tier = ? AND chunk_start <= ?)`,
		HistoryMinute, minChunkCut, HistoryFiveMinute, fiveChunkCut, HistoryHour, hourChunkCut); err != nil {
		return fmt.Errorf("can't delete history_chunks past their age: %w", err)
	}
	if _, err := t.exec(ctx, `DELETE FROM history_series WHERE NOT EXISTS (SELECT 1 FROM history_chunks c WHERE c.series_id = history_series.id)`); err != nil {
		return fmt.Errorf("can't delete expired history series: %w", err)
	}
	return nil
}

// History reads a server's rows of a tier from since up to before, oldest first.
func (t *Tx) History(ctx context.Context, serverID string, tier int, since, before time.Time) ([]HistoryRow, error) {
	rows, err := t.query(ctx, `SELECT `+historyColumns+` FROM server_history
		WHERE server_id = ? AND tier = ? AND start_at >= ? AND start_at < ? ORDER BY start_at`,
		serverID, tier, millis(since), millis(before))
	if err != nil {
		return nil, fmt.Errorf("can't read %s's history: %w", serverID, err)
	}
	defer rows.Close()
	var out []HistoryRow
	for rows.Next() {
		var r HistoryRow
		var start int64
		if err := rows.Scan(&r.ServerID, &r.Tier, &start, &r.CoveredMs, &r.CPUSum, &r.CPUMax, &r.MemorySum, &r.MemoryMax, &r.MemoryTotal,
			&r.SwapSum, &r.SwapMax, &r.LoadSum, &r.LoadMax, &r.DiskSum, &r.DiskMax, &r.RxBytes, &r.RxPeak, &r.TxBytes, &r.TxPeak,
			&r.ReadBytes, &r.ReadPeak, &r.WrittenBytes, &r.WrittenPeak, &r.ReadOps, &r.ReadIOPSPeak, &r.WriteOps, &r.WriteIOPSPeak); err != nil {
			return nil, fmt.Errorf("can't read %s's history: %w", serverID, err)
		}
		r.Start = fromMillis(start)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("can't read %s's history: %w", serverID, err)
	}
	return out, nil
}

// DiskHistory reads one whole disk's I/O history rows for a server and tier.
func (t *Tx) DiskHistory(ctx context.Context, serverID, device string, tier int, since, before time.Time) ([]DiskHistoryRow, error) {
	sid, ok, err := t.findSeriesID(ctx, seriesKey{serverID: serverID, kind: seriesKindDisk, name: device})
	if err != nil || !ok {
		return nil, err
	}
	span := tierChunkSpan(tier)
	step := tierStep(tier)
	chunkSince := since.UTC().Truncate(span)
	rows, err := t.query(ctx, `SELECT chunk_start, data FROM history_chunks
		WHERE series_id = ? AND tier = ? AND chunk_start >= ? AND chunk_start < ? ORDER BY chunk_start`,
		sid, tier, millis(chunkSince), millis(before))
	if err != nil {
		return nil, fmt.Errorf("can't read %s's disk %s history: %w", serverID, device, err)
	}
	defer rows.Close()
	var out []DiskHistoryRow
	for rows.Next() {
		var cs int64
		var data []byte
		if err := rows.Scan(&cs, &data); err != nil {
			return nil, fmt.Errorf("can't read %s's disk %s history: %w", serverID, device, err)
		}
		pts, err := decodeDiskChunk(data)
		if err != nil {
			t.warnBadChunk(chunkKey{seriesID: sid, tier: tier, chunkStart: cs}, serverID, seriesKindDisk, device, err)
			continue
		}
		base := fromMillis(cs)
		for _, p := range pts {
			at := base.Add(time.Duration(p.slot) * step)
			if at.Before(since) || !at.Before(before) {
				continue
			}
			out = append(out, DiskHistoryRow{
				ServerID:      serverID,
				Device:        device,
				Tier:          tier,
				Start:         at,
				CoveredMs:     p.coveredMs,
				ReadBytes:     p.readBytes,
				ReadPeak:      p.readPeak,
				WrittenBytes:  p.writtenBytes,
				WrittenPeak:   p.writtenPeak,
				ReadOps:       p.readOps,
				ReadIOPSPeak:  p.readIOPSPeak,
				WriteOps:      p.writeOps,
				WriteIOPSPeak: p.writeIOPSPeak,
			})
		}
	}
	return out, rows.Err()
}

// DiskHistoryDevices lists distinct whole block devices with history in [since, before).
func (t *Tx) DiskHistoryDevices(ctx context.Context, serverID string, tiers []int, since, before time.Time) ([]string, error) {
	return t.seriesNamesInRange(ctx, serverID, seriesKindDisk, tiers, since, before)
}

// NetHistory reads one physical interface's traffic history rows for a server and tier.
func (t *Tx) NetHistory(ctx context.Context, serverID, iface string, tier int, since, before time.Time) ([]NetHistoryRow, error) {
	sid, ok, err := t.findSeriesID(ctx, seriesKey{serverID: serverID, kind: seriesKindNet, name: iface})
	if err != nil || !ok {
		return nil, err
	}
	span := tierChunkSpan(tier)
	step := tierStep(tier)
	chunkSince := since.UTC().Truncate(span)
	rows, err := t.query(ctx, `SELECT chunk_start, data FROM history_chunks
		WHERE series_id = ? AND tier = ? AND chunk_start >= ? AND chunk_start < ? ORDER BY chunk_start`,
		sid, tier, millis(chunkSince), millis(before))
	if err != nil {
		return nil, fmt.Errorf("can't read %s's interface %s history: %w", serverID, iface, err)
	}
	defer rows.Close()
	var out []NetHistoryRow
	for rows.Next() {
		var cs int64
		var data []byte
		if err := rows.Scan(&cs, &data); err != nil {
			return nil, fmt.Errorf("can't read %s's interface %s history: %w", serverID, iface, err)
		}
		pts, err := decodeNetChunk(data)
		if err != nil {
			t.warnBadChunk(chunkKey{seriesID: sid, tier: tier, chunkStart: cs}, serverID, seriesKindNet, iface, err)
			continue
		}
		base := fromMillis(cs)
		for _, p := range pts {
			at := base.Add(time.Duration(p.slot) * step)
			if at.Before(since) || !at.Before(before) {
				continue
			}
			out = append(out, NetHistoryRow{
				ServerID:  serverID,
				Interface: iface,
				Tier:      tier,
				Start:     at,
				CoveredMs: p.coveredMs,
				RxBytes:   p.rxBytes,
				RxPeak:    p.rxPeak,
				TxBytes:   p.txBytes,
				TxPeak:    p.txPeak,
			})
		}
	}
	return out, rows.Err()
}

// NetHistoryInterfaces lists distinct physical interfaces with history in [since, before).
func (t *Tx) NetHistoryInterfaces(ctx context.Context, serverID string, tiers []int, since, before time.Time) ([]string, error) {
	return t.seriesNamesInRange(ctx, serverID, seriesKindNet, tiers, since, before)
}

func (t *Tx) seriesNamesInRange(ctx context.Context, serverID string, kind int, tiers []int, since, before time.Time) ([]string, error) {
	if len(tiers) == 0 {
		return nil, nil
	}
	maxSpan := time.Hour
	for _, tier := range tiers {
		if sp := tierChunkSpan(tier); sp > maxSpan {
			maxSpan = sp
		}
	}
	args := make([]any, 0, len(tiers)+4)
	args = append(args, serverID, kind)
	ph := make([]string, len(tiers))
	for i, tier := range tiers {
		ph[i] = "?"
		args = append(args, tier)
	}
	args = append(args, millis(since.UTC().Truncate(maxSpan)), millis(before))
	rows, err := t.query(ctx, `SELECT c.series_id, s.name, c.tier, c.chunk_start, c.data
		FROM history_series s JOIN history_chunks c ON c.series_id = s.id
		WHERE s.server_id = ? AND s.kind = ? AND c.tier IN (`+strings.Join(ph, ", ")+`) AND c.chunk_start >= ? AND c.chunk_start < ?
		ORDER BY s.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("can't list %s's history series: %w", serverID, err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var sid int64
		var name string
		var tier int
		var cs int64
		var data []byte
		if err := rows.Scan(&sid, &name, &tier, &cs, &data); err != nil {
			return nil, err
		}
		if seen[name] {
			continue
		}
		base := fromMillis(cs)
		step := tierStep(tier)
		hit := false
		switch kind {
		case seriesKindDisk:
			pts, err := decodeDiskChunk(data)
			if err != nil {
				t.warnBadChunk(chunkKey{seriesID: sid, tier: tier, chunkStart: cs}, serverID, kind, name, err)
				continue
			}
			for _, p := range pts {
				at := base.Add(time.Duration(p.slot) * step)
				if !at.Before(since) && at.Before(before) {
					hit = true
					break
				}
			}
		case seriesKindNet:
			pts, err := decodeNetChunk(data)
			if err != nil {
				t.warnBadChunk(chunkKey{seriesID: sid, tier: tier, chunkStart: cs}, serverID, kind, name, err)
				continue
			}
			for _, p := range pts {
				at := base.Add(time.Duration(p.slot) * step)
				if !at.Before(since) && at.Before(before) {
					hit = true
					break
				}
			}
		}
		if hit {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, rows.Err()
}

// ContainerHistory reads one container's CPU and memory history rows for a tier.
func (t *Tx) ContainerHistory(ctx context.Context, serverID, containerID string, tier int, since, before time.Time) ([]ContainerHistoryRow, error) {
	sid, ok, err := t.findSeriesID(ctx, seriesKey{serverID: serverID, kind: seriesKindContainer, name: containerID})
	if err != nil || !ok {
		return nil, err
	}
	span := tierChunkSpan(tier)
	step := tierStep(tier)
	chunkSince := since.UTC().Truncate(span)
	rows, err := t.query(ctx, `SELECT chunk_start, data FROM history_chunks
		WHERE series_id = ? AND tier = ? AND chunk_start >= ? AND chunk_start < ? ORDER BY chunk_start`,
		sid, tier, millis(chunkSince), millis(before))
	if err != nil {
		return nil, fmt.Errorf("can't read container %s history on %s: %w", containerID, serverID, err)
	}
	defer rows.Close()
	var out []ContainerHistoryRow
	for rows.Next() {
		var cs int64
		var data []byte
		if err := rows.Scan(&cs, &data); err != nil {
			return nil, fmt.Errorf("can't read container %s history on %s: %w", containerID, serverID, err)
		}
		pts, err := decodeContChunk(data)
		if err != nil {
			t.warnBadChunk(chunkKey{seriesID: sid, tier: tier, chunkStart: cs}, serverID, seriesKindContainer, containerID, err)
			continue
		}
		base := fromMillis(cs)
		for _, p := range pts {
			at := base.Add(time.Duration(p.slot) * step)
			if at.Before(since) || !at.Before(before) {
				continue
			}
			out = append(out, ContainerHistoryRow{
				ServerID:    serverID,
				ContainerID: containerID,
				Tier:        tier,
				Start:       at,
				CoveredMs:   p.coveredMs,
				CPUSum:      p.cpuSum,
				CPUMax:      p.cpuMax,
				MemorySum:   p.memorySum,
				MemoryMax:   p.memoryMax,
				MemLimit:    p.memLimit,
			})
		}
	}
	return out, rows.Err()
}
