// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

const (
	seriesKindDisk      = 1
	seriesKindNet       = 2
	seriesKindContainer = 3

	chunkFormatVersion = 1
	maxChunkSlots      = 288

	// Chunk caps per (series_id, tier), bounding stored chunks regardless of
	// clock jumps (matching server_history's per-tier caps).
	chunkCapMinute     = 25 // 25 x 1-hour chunks (24 h + 1 partial hour)
	chunkCapFiveMinute = 8  // 8 x 1-day chunks (7 d + 1 partial day)
	chunkCapHour       = 32 // 32 x 1-day chunks (31 d + 1 partial day)
)

func seriesKindName(kind int) string {
	switch kind {
	case seriesKindDisk:
		return "disk"
	case seriesKindNet:
		return "net"
	case seriesKindContainer:
		return "container"
	default:
		return "unknown"
	}
}

// warnBadChunk logs an unreadable history chunk once per (series_id, tier,
// chunk_start) on this store so a corrupt or truncated blob is surfaced
// without spamming logs or blocking other series.
func (t *Tx) warnBadChunk(ck chunkKey, serverID string, kind int, name string, err error) {
	if t.db != nil {
		t.db.badChunksMu.Lock()
		if t.db.badChunks == nil {
			t.db.badChunks = make(map[chunkKey]struct{})
		}
		if _, seen := t.db.badChunks[ck]; seen {
			t.db.badChunksMu.Unlock()
			return
		}
		if len(t.db.badChunks) >= 4096 {
			clear(t.db.badChunks)
		}
		t.db.badChunks[ck] = struct{}{}
		t.db.badChunksMu.Unlock()
	}
	slog.Warn("ignoring unreadable history chunk",
		slog.Int64("series_id", ck.seriesID),
		slog.String("server_id", serverID),
		slog.String("kind", seriesKindName(kind)),
		slog.String("series", name),
		slog.Int("tier", ck.tier),
		slog.Time("chunk_start", fromMillis(ck.chunkStart)),
		slog.Any("error", err),
	)
}

type seriesKey struct {
	serverID string
	kind     int
	name     string
}

type chunkKey struct {
	seriesID   int64
	tier       int
	chunkStart int64 // epoch ms, aligned to tierChunkSpan(tier)
}

func tierStep(tier int) time.Duration {
	switch tier {
	case HistoryMinute:
		return time.Minute
	case HistoryFiveMinute:
		return 5 * time.Minute
	case HistoryHour:
		return time.Hour
	default:
		return time.Minute
	}
}

func tierChunkSpan(tier int) time.Duration {
	switch tier {
	case HistoryMinute:
		return time.Hour
	case HistoryFiveMinute, HistoryHour:
		return 24 * time.Hour
	default:
		return time.Hour
	}
}

func tierChunkCap(tier int) int {
	switch tier {
	case HistoryMinute:
		return chunkCapMinute
	case HistoryFiveMinute:
		return chunkCapFiveMinute
	case HistoryHour:
		return chunkCapHour
	default:
		return chunkCapMinute
	}
}

func chunkSlot(tier int, start time.Time) (chunkStart time.Time, slot uint16, ok bool) {
	span := tierChunkSpan(tier)
	step := tierStep(tier)
	aligned := start.UTC().Truncate(step)
	chunkStart = aligned.Truncate(span)
	idx := int(aligned.Sub(chunkStart) / step)
	maxSlots := int(span / step)
	if idx < 0 || idx >= maxSlots {
		return time.Time{}, 0, false
	}
	return chunkStart, uint16(idx), true //nolint:gosec // idx < 288
}

func clampNonNeg(v int64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(v)
}

// --- Compact varint chunk codecs ---

type diskSlot struct {
	slot                      uint16
	coveredMs                 int64
	readBytes, readPeak       int64
	writtenBytes, writtenPeak int64
	readOps, readIOPSPeak     int64
	writeOps, writeIOPSPeak   int64
}

func encodeDiskChunk(pts []diskSlot) []byte {
	slices.SortFunc(pts, func(a, b diskSlot) int { return int(a.slot) - int(b.slot) })
	buf := make([]byte, 0, 2+len(pts)*24)
	buf = append(buf, chunkFormatVersion)
	buf = binary.AppendUvarint(buf, uint64(len(pts)))
	var prev uint16
	for i, p := range pts {
		delta := p.slot
		if i > 0 {
			delta = p.slot - prev
		}
		prev = p.slot
		buf = binary.AppendUvarint(buf, uint64(delta))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.coveredMs))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.readBytes))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.readPeak))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.writtenBytes))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.writtenPeak))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.readOps))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.readIOPSPeak))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.writeOps))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.writeIOPSPeak))
	}
	return buf
}

func decodeDiskChunk(data []byte) ([]diskSlot, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[0] != chunkFormatVersion {
		return nil, fmt.Errorf("unsupported disk history chunk version %d", data[0])
	}
	pos := 1
	readU := func() (uint64, bool) {
		v, n := binary.Uvarint(data[pos:])
		if n <= 0 {
			return 0, false
		}
		pos += n
		return v, true
	}
	count, ok := readU()
	if !ok || count > 512 {
		return nil, errors.New("corrupt disk history chunk header")
	}
	out := make([]diskSlot, 0, count)
	var prev uint16
	for i := range count {
		delta, ok0 := readU()
		cov, ok1 := readU()
		rb, ok2 := readU()
		rp, ok3 := readU()
		wb, ok4 := readU()
		wp, ok5 := readU()
		ro, ok6 := readU()
		rip, ok7 := readU()
		wo, ok8 := readU()
		wip, ok9 := readU()
		if !ok0 || !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 || !ok8 || !ok9 {
			return nil, errors.New("truncated disk history chunk")
		}
		var slot uint16
		if i == 0 {
			if delta >= maxChunkSlots {
				return nil, errors.New("corrupt disk history chunk slot")
			}
			slot = uint16(delta)
		} else {
			if delta >= maxChunkSlots || uint64(prev)+delta >= maxChunkSlots {
				return nil, errors.New("corrupt disk history chunk slot")
			}
			slot = prev + uint16(delta)
		}
		prev = slot
		out = append(out, diskSlot{
			slot:          slot,
			coveredMs:     int64(cov), //nolint:gosec // non-negative metric counter
			readBytes:     int64(rb),  //nolint:gosec // non-negative metric counter
			readPeak:      int64(rp),  //nolint:gosec // non-negative metric counter
			writtenBytes:  int64(wb),  //nolint:gosec // non-negative metric counter
			writtenPeak:   int64(wp),  //nolint:gosec // non-negative metric counter
			readOps:       int64(ro),  //nolint:gosec // non-negative metric counter
			readIOPSPeak:  int64(rip), //nolint:gosec // non-negative metric counter
			writeOps:      int64(wo),  //nolint:gosec // non-negative metric counter
			writeIOPSPeak: int64(wip), //nolint:gosec // non-negative metric counter
		})
	}
	if pos != len(data) {
		return nil, errors.New("corrupt disk history chunk trailing data")
	}
	return out, nil
}

type netSlot struct {
	slot            uint16
	coveredMs       int64
	rxBytes, rxPeak int64
	txBytes, txPeak int64
}

func encodeNetChunk(pts []netSlot) []byte {
	slices.SortFunc(pts, func(a, b netSlot) int { return int(a.slot) - int(b.slot) })
	buf := make([]byte, 0, 2+len(pts)*16)
	buf = append(buf, chunkFormatVersion)
	buf = binary.AppendUvarint(buf, uint64(len(pts)))
	var prev uint16
	for i, p := range pts {
		delta := p.slot
		if i > 0 {
			delta = p.slot - prev
		}
		prev = p.slot
		buf = binary.AppendUvarint(buf, uint64(delta))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.coveredMs))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.rxBytes))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.rxPeak))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.txBytes))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.txPeak))
	}
	return buf
}

func decodeNetChunk(data []byte) ([]netSlot, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[0] != chunkFormatVersion {
		return nil, fmt.Errorf("unsupported net history chunk version %d", data[0])
	}
	pos := 1
	readU := func() (uint64, bool) {
		v, n := binary.Uvarint(data[pos:])
		if n <= 0 {
			return 0, false
		}
		pos += n
		return v, true
	}
	count, ok := readU()
	if !ok || count > 512 {
		return nil, errors.New("corrupt net history chunk header")
	}
	out := make([]netSlot, 0, count)
	var prev uint16
	for i := range count {
		delta, ok0 := readU()
		cov, ok1 := readU()
		rx, ok2 := readU()
		rxp, ok3 := readU()
		tx, ok4 := readU()
		txp, ok5 := readU()
		if !ok0 || !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
			return nil, errors.New("truncated net history chunk")
		}
		var slot uint16
		if i == 0 {
			if delta >= maxChunkSlots {
				return nil, errors.New("corrupt net history chunk slot")
			}
			slot = uint16(delta)
		} else {
			if delta >= maxChunkSlots || uint64(prev)+delta >= maxChunkSlots {
				return nil, errors.New("corrupt net history chunk slot")
			}
			slot = prev + uint16(delta)
		}
		prev = slot
		out = append(out, netSlot{
			slot:      slot,
			coveredMs: int64(cov), //nolint:gosec // non-negative metric counter
			rxBytes:   int64(rx),  //nolint:gosec // non-negative metric counter
			rxPeak:    int64(rxp), //nolint:gosec // non-negative metric counter
			txBytes:   int64(tx),  //nolint:gosec // non-negative metric counter
			txPeak:    int64(txp), //nolint:gosec // non-negative metric counter
		})
	}
	if pos != len(data) {
		return nil, errors.New("corrupt net history chunk trailing data")
	}
	return out, nil
}

type contSlot struct {
	slot                           uint16
	coveredMs                      int64
	cpuSum, cpuMax                 int64
	memorySum, memoryMax, memLimit int64
}

func encodeContChunk(pts []contSlot) []byte {
	slices.SortFunc(pts, func(a, b contSlot) int { return int(a.slot) - int(b.slot) })
	buf := make([]byte, 0, 2+len(pts)*16)
	buf = append(buf, chunkFormatVersion)
	buf = binary.AppendUvarint(buf, uint64(len(pts)))
	var prevSlot uint16
	var prevLimit int64
	for i, p := range pts {
		delta := p.slot
		if i > 0 {
			delta = p.slot - prevSlot
		}
		prevSlot = p.slot
		var cpuAvg, memAvg int64
		if p.coveredMs > 0 {
			cpuAvg = (p.cpuSum + p.coveredMs/2) / p.coveredMs
			memAvg = (p.memorySum + p.coveredMs/2) / p.coveredMs
		}
		cpuMaxDelta := max(p.cpuMax-cpuAvg, 0)
		memMaxDelta := max(p.memoryMax-memAvg, 0)
		limitDelta := p.memLimit - prevLimit
		prevLimit = p.memLimit

		buf = binary.AppendUvarint(buf, uint64(delta))
		buf = binary.AppendUvarint(buf, clampNonNeg(p.coveredMs))
		buf = binary.AppendUvarint(buf, clampNonNeg(cpuAvg))
		buf = binary.AppendUvarint(buf, clampNonNeg(cpuMaxDelta))
		buf = binary.AppendUvarint(buf, clampNonNeg(memAvg))
		buf = binary.AppendUvarint(buf, clampNonNeg(memMaxDelta))
		buf = binary.AppendVarint(buf, limitDelta)
	}
	return buf
}

func decodeContChunk(data []byte) ([]contSlot, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[0] != chunkFormatVersion {
		return nil, fmt.Errorf("unsupported container history chunk version %d", data[0])
	}
	pos := 1
	readU := func() (uint64, bool) {
		v, n := binary.Uvarint(data[pos:])
		if n <= 0 {
			return 0, false
		}
		pos += n
		return v, true
	}
	readV := func() (int64, bool) {
		v, n := binary.Varint(data[pos:])
		if n <= 0 {
			return 0, false
		}
		pos += n
		return v, true
	}
	count, ok := readU()
	if !ok || count > 512 {
		return nil, errors.New("corrupt container history chunk header")
	}
	out := make([]contSlot, 0, count)
	var prevSlot uint16
	var prevLimit int64
	for i := range count {
		delta, ok0 := readU()
		cov, ok1 := readU()
		cpuAvg, ok2 := readU()
		cpuMaxDelta, ok3 := readU()
		memAvg, ok4 := readU()
		memMaxDelta, ok5 := readU()
		limitDelta, ok6 := readV()
		if !ok0 || !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
			return nil, errors.New("truncated container history chunk")
		}
		var slot uint16
		if i == 0 {
			if delta >= maxChunkSlots {
				return nil, errors.New("corrupt container history chunk slot")
			}
			slot = uint16(delta)
		} else {
			if delta >= maxChunkSlots || uint64(prevSlot)+delta >= maxChunkSlots {
				return nil, errors.New("corrupt container history chunk slot")
			}
			slot = prevSlot + uint16(delta)
		}
		prevSlot = slot
		limit := prevLimit + limitDelta
		prevLimit = limit
		covI := int64(cov)       //nolint:gosec // non-negative ms
		cpuAvgI := int64(cpuAvg) //nolint:gosec // non-negative thousandths
		memAvgI := int64(memAvg) //nolint:gosec // non-negative KiB
		out = append(out, contSlot{
			slot:      slot,
			coveredMs: covI,
			cpuSum:    cpuAvgI * covI,
			cpuMax:    cpuAvgI + int64(cpuMaxDelta), //nolint:gosec // non-negative thousandths
			memorySum: memAvgI * covI,
			memoryMax: memAvgI + int64(memMaxDelta), //nolint:gosec // non-negative KiB
			memLimit:  limit,
		})
	}
	if pos != len(data) {
		return nil, errors.New("corrupt container history chunk trailing data")
	}
	return out, nil
}

// --- Series & Chunk persistence helpers ---

func (t *Tx) ensureSeriesIDs(ctx context.Context, keys []seriesKey) (map[seriesKey]int64, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	uniq := make(map[seriesKey]struct{}, len(keys))
	list := make([]seriesKey, 0, len(keys))
	for _, k := range keys {
		if _, ok := uniq[k]; !ok {
			uniq[k] = struct{}{}
			list = append(list, k)
		}
	}
	out := make(map[seriesKey]int64, len(list))

	// First look up any existing IDs in batches.
	for batch := range slices.Chunk(list, 100) {
		if err := t.lookupSeriesBatch(ctx, batch, out); err != nil {
			return nil, err
		}
	}
	// Insert missing series, then fetch their assigned IDs.
	var missing []seriesKey
	for _, k := range list {
		if _, ok := out[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	one := placeholders(3)
	for batch := range slices.Chunk(missing, 100) {
		vals := make([]any, 0, len(batch)*3)
		for _, k := range batch {
			vals = append(vals, k.serverID, k.kind, k.name)
		}
		q := `INSERT INTO history_series (server_id, kind, name) VALUES ` +
			strings.TrimSuffix(strings.Repeat(one+", ", len(batch)), ", ") +
			` ON CONFLICT (server_id, kind, name) DO NOTHING`
		if _, err := t.exec(ctx, q, vals...); err != nil {
			return nil, fmt.Errorf("can't register history series: %w", err)
		}
	}
	for batch := range slices.Chunk(missing, 100) {
		if err := t.lookupSeriesBatch(ctx, batch, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (t *Tx) lookupSeriesBatch(ctx context.Context, batch []seriesKey, out map[seriesKey]int64) error {
	if len(batch) == 0 {
		return nil
	}
	type skGroup struct {
		serverID string
		kind     int
	}
	byGroup := map[skGroup][]string{}
	for _, k := range batch {
		g := skGroup{serverID: k.serverID, kind: k.kind}
		byGroup[g] = append(byGroup[g], k.name)
	}
	for g, names := range byGroup {
		for chunk := range slices.Chunk(names, 200) {
			ph := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
			args := make([]any, 0, len(chunk)+2)
			args = append(args, g.serverID, g.kind)
			for _, name := range chunk {
				args = append(args, name)
			}
			rows, err := t.query(ctx, `SELECT id, name FROM history_series WHERE server_id = ? AND kind = ? AND name IN (`+ph+`)`, args...)
			if err != nil {
				return fmt.Errorf("can't look up history series: %w", err)
			}
			for rows.Next() {
				var id int64
				var name string
				if err := rows.Scan(&id, &name); err != nil {
					_ = rows.Close()
					return fmt.Errorf("can't scan history series: %w", err)
				}
				out[seriesKey{serverID: g.serverID, kind: g.kind, name: name}] = id
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			_ = rows.Close()
		}
	}
	return nil
}

func (t *Tx) findSeriesID(ctx context.Context, k seriesKey) (int64, bool, error) {
	rows, err := t.query(ctx, `SELECT id FROM history_series WHERE server_id = ? AND kind = ? AND name = ?`, k.serverID, k.kind, k.name)
	if err != nil {
		return 0, false, fmt.Errorf("can't look up history series: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, false, rows.Err()
	}
	var id int64
	if err := rows.Scan(&id); err != nil {
		return 0, false, err
	}
	return id, true, rows.Err()
}

func (t *Tx) loadChunks(ctx context.Context, keys []chunkKey) (map[chunkKey][]byte, error) {
	out := make(map[chunkKey][]byte, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	// Group by (tier, chunk_start) when that produces fewer index lookups (the
	// steady-state minute-flush and roll-up path), or by (series_id, tier) for
	// multi-day bulk writes. Both use direct B-tree index seeks on SQLite and Postgres.
	type tcKey struct {
		tier       int
		chunkStart int64
	}
	type stKey struct {
		seriesID int64
		tier     int
	}
	byTC := map[tcKey][]int64{}
	byST := map[stKey][]int64{}
	for _, k := range keys {
		byTC[tcKey{k.tier, k.chunkStart}] = append(byTC[tcKey{k.tier, k.chunkStart}], k.seriesID)
		byST[stKey{k.seriesID, k.tier}] = append(byST[stKey{k.seriesID, k.tier}], k.chunkStart)
	}
	if len(byTC) <= len(byST) {
		for g, sids := range byTC {
			for chunk := range slices.Chunk(sids, 200) {
				ph := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
				args := make([]any, 0, len(chunk)+2)
				args = append(args, g.tier, g.chunkStart)
				for _, sid := range chunk {
					args = append(args, sid)
				}
				rows, err := t.query(ctx, `SELECT series_id, data FROM history_chunks WHERE tier = ? AND chunk_start = ? AND series_id IN (`+ph+`)`, args...)
				if err != nil {
					return nil, fmt.Errorf("can't read history chunks: %w", err)
				}
				for rows.Next() {
					var sid int64
					var data []byte
					if err := rows.Scan(&sid, &data); err != nil {
						_ = rows.Close()
						return nil, fmt.Errorf("can't scan history chunk: %w", err)
					}
					out[chunkKey{seriesID: sid, tier: g.tier, chunkStart: g.chunkStart}] = append([]byte(nil), data...)
				}
				if err := rows.Err(); err != nil {
					_ = rows.Close()
					return nil, err
				}
				_ = rows.Close()
			}
		}
		return out, nil
	}
	for g, starts := range byST {
		for chunk := range slices.Chunk(starts, 200) {
			ph := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
			args := make([]any, 0, len(chunk)+2)
			args = append(args, g.seriesID, g.tier)
			for _, cs := range chunk {
				args = append(args, cs)
			}
			rows, err := t.query(ctx, `SELECT chunk_start, data FROM history_chunks WHERE series_id = ? AND tier = ? AND chunk_start IN (`+ph+`)`, args...)
			if err != nil {
				return nil, fmt.Errorf("can't read history chunks: %w", err)
			}
			for rows.Next() {
				var cs int64
				var data []byte
				if err := rows.Scan(&cs, &data); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("can't scan history chunk: %w", err)
				}
				out[chunkKey{seriesID: g.seriesID, tier: g.tier, chunkStart: cs}] = append([]byte(nil), data...)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return nil, err
			}
			_ = rows.Close()
		}
	}
	return out, nil
}

type chunkWrite struct {
	key  chunkKey
	data []byte
}

func (t *Tx) upsertChunks(ctx context.Context, writes []chunkWrite) error {
	if len(writes) == 0 {
		return nil
	}
	one := placeholders(4)
	for batch := range slices.Chunk(writes, minutesAStatement) {
		vals := make([]any, 0, len(batch)*4)
		for _, w := range batch {
			vals = append(vals, w.key.seriesID, w.key.tier, w.key.chunkStart, w.data)
		}
		q := `INSERT INTO history_chunks (series_id, tier, chunk_start, data) VALUES ` +
			strings.TrimSuffix(strings.Repeat(one+", ", len(batch)), ", ") +
			` ON CONFLICT (series_id, tier, chunk_start) DO UPDATE SET data = excluded.data`
		if _, err := t.exec(ctx, q, vals...); err != nil {
			return fmt.Errorf("can't write history chunks: %w", err)
		}
	}
	return nil
}

// capChunks deletes chunks of one series and tier past the newest keep, by
// chunk_start, as one range delete: what age pruning can't bound on clock jumps.
func (t *Tx) capChunks(ctx context.Context, seriesID int64, tier, keep int) error {
	if _, err := t.exec(ctx, `DELETE FROM history_chunks WHERE series_id = ? AND tier = ? AND chunk_start < (
		SELECT chunk_start FROM history_chunks WHERE series_id = ? AND tier = ? ORDER BY chunk_start DESC LIMIT 1 OFFSET ?)`,
		seriesID, tier, seriesID, tier, keep-1); err != nil {
		return fmt.Errorf("can't cap history chunks for series %d: %w", seriesID, err)
	}
	return nil
}

// rollupChunks rolls 1-minute chunks covering [start, end) into toTier
// (HistoryFiveMinute or HistoryHour), replacing the target slot in each
// series' target chunk so repeated roll-ups are idempotent.
func (t *Tx) rollupChunks(ctx context.Context, toTier int, start, end time.Time) ([]int64, error) {
	hourChunk := start.UTC().Truncate(time.Hour)
	hourChunkMs := millis(hourChunk)
	rows, err := t.query(ctx, `SELECT c.series_id, s.server_id, s.kind, s.name, c.data
		FROM history_chunks c JOIN history_series s ON s.id = c.series_id
		WHERE c.tier = ? AND c.chunk_start = ?`, HistoryMinute, hourChunkMs)
	if err != nil {
		return nil, fmt.Errorf("can't read minute chunks for roll-up %s: %w", start.Format(time.RFC3339), err)
	}
	type rawChunk struct {
		seriesID int64
		serverID string
		kind     int
		name     string
		data     []byte
	}
	var src []rawChunk
	for rows.Next() {
		var rc rawChunk
		if err := rows.Scan(&rc.seriesID, &rc.serverID, &rc.kind, &rc.name, &rc.data); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("can't scan minute chunk for roll-up: %w", err)
		}
		rc.data = append([]byte(nil), rc.data...)
		src = append(src, rc)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(src) == 0 {
		return nil, nil
	}

	minSlotLo := uint16(start.UTC().Sub(hourChunk) / time.Minute) //nolint:gosec // 0..59
	minSlotHi := uint16(end.UTC().Sub(hourChunk) / time.Minute)   //nolint:gosec // 1..60
	dstChunkStart, dstSlot, ok := chunkSlot(toTier, start)
	if !ok {
		return nil, nil
	}
	dstStartMs := millis(dstChunkStart)

	type aggResult struct {
		seriesID int64
		serverID string
		kind     int
		name     string
		disk     diskSlot
		net      netSlot
		cont     contSlot
	}
	var results []aggResult
	var targetKeys []chunkKey
	for _, rc := range src {
		srcKey := chunkKey{seriesID: rc.seriesID, tier: HistoryMinute, chunkStart: hourChunkMs}
		switch rc.kind {
		case seriesKindDisk:
			pts, err := decodeDiskChunk(rc.data)
			if err != nil {
				t.warnBadChunk(srcKey, rc.serverID, rc.kind, rc.name, err)
				continue
			}
			var agg diskSlot
			agg.slot = dstSlot
			has := false
			for _, p := range pts {
				if p.slot < minSlotLo || p.slot >= minSlotHi {
					continue
				}
				has = true
				agg.coveredMs += p.coveredMs
				agg.readBytes += p.readBytes
				agg.readPeak = max(agg.readPeak, p.readPeak)
				agg.writtenBytes += p.writtenBytes
				agg.writtenPeak = max(agg.writtenPeak, p.writtenPeak)
				agg.readOps += p.readOps
				agg.readIOPSPeak = max(agg.readIOPSPeak, p.readIOPSPeak)
				agg.writeOps += p.writeOps
				agg.writeIOPSPeak = max(agg.writeIOPSPeak, p.writeIOPSPeak)
			}
			if has {
				results = append(results, aggResult{seriesID: rc.seriesID, serverID: rc.serverID, kind: rc.kind, name: rc.name, disk: agg})
				targetKeys = append(targetKeys, chunkKey{seriesID: rc.seriesID, tier: toTier, chunkStart: dstStartMs})
			}
		case seriesKindNet:
			pts, err := decodeNetChunk(rc.data)
			if err != nil {
				t.warnBadChunk(srcKey, rc.serverID, rc.kind, rc.name, err)
				continue
			}
			var agg netSlot
			agg.slot = dstSlot
			has := false
			for _, p := range pts {
				if p.slot < minSlotLo || p.slot >= minSlotHi {
					continue
				}
				has = true
				agg.coveredMs += p.coveredMs
				agg.rxBytes += p.rxBytes
				agg.rxPeak = max(agg.rxPeak, p.rxPeak)
				agg.txBytes += p.txBytes
				agg.txPeak = max(agg.txPeak, p.txPeak)
			}
			if has {
				results = append(results, aggResult{seriesID: rc.seriesID, serverID: rc.serverID, kind: rc.kind, name: rc.name, net: agg})
				targetKeys = append(targetKeys, chunkKey{seriesID: rc.seriesID, tier: toTier, chunkStart: dstStartMs})
			}
		case seriesKindContainer:
			pts, err := decodeContChunk(rc.data)
			if err != nil {
				t.warnBadChunk(srcKey, rc.serverID, rc.kind, rc.name, err)
				continue
			}
			var agg contSlot
			agg.slot = dstSlot
			has := false
			for _, p := range pts {
				if p.slot < minSlotLo || p.slot >= minSlotHi {
					continue
				}
				has = true
				agg.coveredMs += p.coveredMs
				agg.cpuSum += p.cpuSum
				agg.cpuMax = max(agg.cpuMax, p.cpuMax)
				agg.memorySum += p.memorySum
				agg.memoryMax = max(agg.memoryMax, p.memoryMax)
				agg.memLimit = max(agg.memLimit, p.memLimit)
			}
			if has {
				results = append(results, aggResult{seriesID: rc.seriesID, serverID: rc.serverID, kind: rc.kind, name: rc.name, cont: agg})
				targetKeys = append(targetKeys, chunkKey{seriesID: rc.seriesID, tier: toTier, chunkStart: dstStartMs})
			}
		}
	}
	if len(results) == 0 {
		return nil, nil
	}

	existing, err := t.loadChunks(ctx, targetKeys)
	if err != nil {
		return nil, err
	}
	writes := make([]chunkWrite, 0, len(results))
	rolledIDs := make([]int64, 0, len(results))
	for i := range results {
		res := &results[i]
		ck := targetKeys[i]
		rolledIDs = append(rolledIDs, res.seriesID)
		switch res.kind {
		case seriesKindDisk:
			pts, err := decodeDiskChunk(existing[ck])
			if err != nil {
				t.warnBadChunk(ck, res.serverID, res.kind, res.name, err)
				pts = nil
			}
			pts = replaceDiskSlot(pts, res.disk)
			writes = append(writes, chunkWrite{key: ck, data: encodeDiskChunk(pts)})
		case seriesKindNet:
			pts, err := decodeNetChunk(existing[ck])
			if err != nil {
				t.warnBadChunk(ck, res.serverID, res.kind, res.name, err)
				pts = nil
			}
			pts = replaceNetSlot(pts, res.net)
			writes = append(writes, chunkWrite{key: ck, data: encodeNetChunk(pts)})
		case seriesKindContainer:
			pts, err := decodeContChunk(existing[ck])
			if err != nil {
				t.warnBadChunk(ck, res.serverID, res.kind, res.name, err)
				pts = nil
			}
			pts = replaceContSlot(pts, res.cont)
			writes = append(writes, chunkWrite{key: ck, data: encodeContChunk(pts)})
		}
	}
	if err := t.upsertChunks(ctx, writes); err != nil {
		return nil, err
	}
	return rolledIDs, nil
}

func replaceDiskSlot(pts []diskSlot, v diskSlot) []diskSlot {
	for i := range pts {
		if pts[i].slot == v.slot {
			pts[i] = v
			return pts
		}
	}
	return append(pts, v)
}

func mergeDiskSlots(pts, adds []diskSlot) []diskSlot {
	var idx [288]int
	for i, p := range pts {
		if int(p.slot) < len(idx) {
			idx[p.slot] = i + 1
		}
	}
	for _, v := range adds {
		if int(v.slot) < len(idx) && idx[v.slot] > 0 {
			p := &pts[idx[v.slot]-1]
			p.coveredMs += v.coveredMs
			p.readBytes += v.readBytes
			p.readPeak = max(p.readPeak, v.readPeak)
			p.writtenBytes += v.writtenBytes
			p.writtenPeak = max(p.writtenPeak, v.writtenPeak)
			p.readOps += v.readOps
			p.readIOPSPeak = max(p.readIOPSPeak, v.readIOPSPeak)
			p.writeOps += v.writeOps
			p.writeIOPSPeak = max(p.writeIOPSPeak, v.writeIOPSPeak)
		} else {
			pts = append(pts, v)
			if int(v.slot) < len(idx) {
				idx[v.slot] = len(pts)
			}
		}
	}
	return pts
}

func replaceNetSlot(pts []netSlot, v netSlot) []netSlot {
	for i := range pts {
		if pts[i].slot == v.slot {
			pts[i] = v
			return pts
		}
	}
	return append(pts, v)
}

func mergeNetSlots(pts, adds []netSlot) []netSlot {
	var idx [288]int
	for i, p := range pts {
		if int(p.slot) < len(idx) {
			idx[p.slot] = i + 1
		}
	}
	for _, v := range adds {
		if int(v.slot) < len(idx) && idx[v.slot] > 0 {
			p := &pts[idx[v.slot]-1]
			p.coveredMs += v.coveredMs
			p.rxBytes += v.rxBytes
			p.rxPeak = max(p.rxPeak, v.rxPeak)
			p.txBytes += v.txBytes
			p.txPeak = max(p.txPeak, v.txPeak)
		} else {
			pts = append(pts, v)
			if int(v.slot) < len(idx) {
				idx[v.slot] = len(pts)
			}
		}
	}
	return pts
}

func replaceContSlot(pts []contSlot, v contSlot) []contSlot {
	for i := range pts {
		if pts[i].slot == v.slot {
			pts[i] = v
			return pts
		}
	}
	return append(pts, v)
}

func mergeContSlots(pts, adds []contSlot) []contSlot {
	var idx [288]int
	for i, p := range pts {
		if int(p.slot) < len(idx) {
			idx[p.slot] = i + 1
		}
	}
	for _, v := range adds {
		if int(v.slot) < len(idx) && idx[v.slot] > 0 {
			p := &pts[idx[v.slot]-1]
			p.coveredMs += v.coveredMs
			p.cpuSum += v.cpuSum
			p.cpuMax = max(p.cpuMax, v.cpuMax)
			p.memorySum += v.memorySum
			p.memoryMax = max(p.memoryMax, v.memoryMax)
			p.memLimit = max(p.memLimit, v.memLimit)
		} else {
			pts = append(pts, v)
			if int(v.slot) < len(idx) {
				idx[v.slot] = len(pts)
			}
		}
	}
	return pts
}
