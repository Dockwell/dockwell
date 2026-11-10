// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A server's history comes in two tiers (ADR 0065): a minute for a day, and an
// hour for a month.
const (
	HistoryMinute = 1
	HistoryHour   = 2
	// HistoryMinutes and HistoryHours are how many rows of each a server keeps
	// at most, whatever happens to the clock.
	HistoryMinutes = 1440
	HistoryHours   = 744
	// MinutesKeptFor and HoursKeptFor are how long rows of each tier are kept.
	MinutesKeptFor = 24 * time.Hour
	HoursKeptFor   = 31 * 24 * time.Hour
)

// HistoryRow is a minute or an hour of a server's history. Each figure is a sum
// weighted by the milliseconds the samples covered, so its average is the sum
// over CoveredMs, and its highest value; bytes are what moved in it, with the
// highest rate a sample saw. A row with CoveredMs 0 is time the control plane
// listened and heard nothing.
type HistoryRow struct {
	ServerID  string
	Tier      int
	Start     time.Time
	CoveredMs int64

	CPUSum, CPUMax                    int64 // thousandths of a percent
	MemorySum, MemoryMax, MemoryTotal int64 // KiB
	SwapSum, SwapMax                  int64 // KiB
	LoadSum, LoadMax                  int64 // the one-minute load, in thousandths
	DiskSum, DiskMax                  int64 // the fullest disk's share, in thousandths

	RxBytes, RxPeak           int64
	TxBytes, TxPeak           int64
	ReadBytes, ReadPeak       int64
	WrittenBytes, WrittenPeak int64
}

const historyColumns = `server_id, tier, start_at, covered_ms, cpu_sum, cpu_max, memory_sum, memory_max, memory_total,
	swap_sum, swap_max, load_sum, load_max, disk_sum, disk_max, rx_bytes, rx_peak, tx_bytes, tx_peak,
	read_bytes, read_peak, written_bytes, written_peak`

// historySums are the columns that add up, and historyHighs those that take the
// larger, as rows of one minute from two control planes merge.
var (
	historySums  = []string{"covered_ms", "cpu_sum", "memory_sum", "swap_sum", "load_sum", "disk_sum", "rx_bytes", "tx_bytes", "read_bytes", "written_bytes"}
	historyHighs = []string{"cpu_max", "memory_max", "memory_total", "swap_max", "load_max", "disk_max", "rx_peak", "tx_peak", "read_peak", "written_peak"}
)

func (r *HistoryRow) values() []any {
	return []any{
		r.ServerID, r.Tier, millis(r.Start), r.CoveredMs, r.CPUSum, r.CPUMax, r.MemorySum, r.MemoryMax, r.MemoryTotal,
		r.SwapSum, r.SwapMax, r.LoadSum, r.LoadMax, r.DiskSum, r.DiskMax, r.RxBytes, r.RxPeak, r.TxBytes, r.TxPeak,
		r.ReadBytes, r.ReadPeak, r.WrittenBytes, r.WrittenPeak,
	}
}

// minutesAStatement is how many rows one statement writes: a minute of a
// hundred servers in one round trip.
const minutesAStatement = 100

// AddMinutes writes minute rows, adding to a row of the same server and minute
// that's there already, as a second control plane that heard the server in that
// minute writes one too. Each server is kept to HistoryMinutes rows as its hours
// are rolled, which leaves at most an hour's more between rolls.
func (t *Tx) AddMinutes(ctx context.Context, rows []HistoryRow) error {
	set := make([]string, 0, len(historySums)+len(historyHighs))
	for _, c := range historySums {
		set = append(set, fmt.Sprintf("%s = server_history.%s + excluded.%s", c, c, c))
	}
	for _, c := range historyHighs {
		set = append(set, fmt.Sprintf("%s = %s(server_history.%s, excluded.%s)", c, t.dialect.greatest, c, c))
	}
	one := "(" + strings.TrimSuffix(strings.Repeat("?, ", 23), ", ") + ")"
	for batch := range slices.Chunk(rows, minutesAStatement) {
		values := make([]any, 0, len(batch)*23)
		for i := range batch {
			r := batch[i]
			r.Tier = HistoryMinute
			values = append(values, r.values()...)
		}
		query := `INSERT INTO server_history (` + historyColumns + `) VALUES ` + strings.TrimSuffix(strings.Repeat(one+", ", len(batch)), ", ") + `
			ON CONFLICT (server_id, tier, start_at) DO UPDATE SET ` + strings.Join(set, ", ")
		if _, err := t.exec(ctx, query, values...); err != nil {
			return fmt.Errorf("can't write the servers' history for %s: %w", batch[0].Start.Format(time.RFC3339), err)
		}
	}
	return nil
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

// RollHour makes the hour that starts at hour from its minute rows, for every
// server that has any, replacing an hour row that's there: rolling an hour
// twice, after a restart or by two control planes, gives the same row.
func (t *Tx) RollHour(ctx context.Context, hour time.Time) error {
	hour = hour.Truncate(time.Hour)
	pick := make([]string, 0, len(historySums)+len(historyHighs))
	set := make([]string, 0, cap(pick))
	for _, c := range historySums {
		pick = append(pick, "SUM("+c+")")
	}
	for _, c := range historyHighs {
		pick = append(pick, "MAX("+c+")")
	}
	for _, c := range append(append([]string{}, historySums...), historyHighs...) {
		set = append(set, c+" = excluded."+c)
	}
	query := `INSERT INTO server_history (server_id, tier, start_at, ` + strings.Join(historySums, ", ") + `, ` + strings.Join(historyHighs, ", ") + `)
		SELECT server_id, ?, ?, ` + strings.Join(pick, ", ") + `
		FROM server_history WHERE tier = ? AND start_at >= ? AND start_at < ? GROUP BY server_id
		ON CONFLICT (server_id, tier, start_at) DO UPDATE SET ` + strings.Join(set, ", ")
	if _, err := t.exec(ctx, query, HistoryHour, millis(hour), HistoryMinute, millis(hour), millis(hour.Add(time.Hour))); err != nil {
		return fmt.Errorf("can't add up the hour from %s: %w", hour.Format(time.RFC3339), err)
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
	// Each server's minutes are kept to their cap here too, once an hour, rather
	// than at every minute's write.
	for _, id := range servers {
		if err := t.capHistory(ctx, id, HistoryHour, HistoryHours); err != nil {
			return err
		}
		if err := t.capHistory(ctx, id, HistoryMinute, HistoryMinutes); err != nil {
			return err
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

// PruneHistory deletes minute rows older than a day and hour rows older than a
// month, a removed server's too.
func (t *Tx) PruneHistory(ctx context.Context, now time.Time) error {
	if _, err := t.exec(ctx, `DELETE FROM server_history WHERE (tier = ? AND start_at < ?) OR (tier = ? AND start_at < ?)`,
		HistoryMinute, millis(now.Add(-MinutesKeptFor)), HistoryHour, millis(now.Add(-HoursKeptFor))); err != nil {
		return fmt.Errorf("can't delete history past its age: %w", err)
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
			&r.ReadBytes, &r.ReadPeak, &r.WrittenBytes, &r.WrittenPeak); err != nil {
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
