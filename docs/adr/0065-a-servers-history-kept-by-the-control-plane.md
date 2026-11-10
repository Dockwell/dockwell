# 0065. A server's history, kept by the control plane

- Status: Proposed
- Date: 2026-11-10

## Context

A server's page draws CPU, memory, disk and network for the last 5 minutes and no further. [ADR 0040](0040-a-servers-live-use.md) kept it so: history lives in the control plane's memory, 60 samples at most, never in the store. Issue #426's audit ranks the gap thirteenth: 1Panel keeps a server's CPU, memory, load, disk I/O and network for days, over a range you pick, and a person asking why something was slow last night finds nothing here.

What bears on it:

- **The samples already arrive.** Under ADR 0040 every agent sends one on its stream every 10 s while nobody watches, as the stream's heartbeat, and every 5 s while someone does. The control plane reads each and drops it once it falls out of the 5-minute window. Keeping history costs no new sample and no new bandwidth: it's a matter of what the control plane keeps.
- **The agent stays within 40 MB at idle** ([ADR 0018](0018-keep-the-budgets-in-rss.md), [ADR 0060](0060-what-an-idle-agent-keeps-in-memory.md)), and keeps nothing it doesn't need. History held on each server would cost every server memory or disk, and be lost with it.
- **The control plane has one store,** SQLite on its own server or Postgres, which every query must work on, with the store's tests run on both.
- **A sample doesn't carry disk I/O.** It has CPU, memory and swap, load, each disk's use, and network in and out (`ServerStats`).

## Decision

### The agent sends what it sends now, and keeps nothing

- **No new message, no new rate.** History is built from the samples ADR 0040 already has each agent send. An agent buffers nothing while it's away: a server that isn't connected, or a control plane that's down, leaves a gap, drawn as one, as a gap in reporting is now.
- **A sample gains disk I/O:** bytes read and written per second, across the server's disks, from `/proc/diskstats` read beside the files it reads now, as the difference since its last sample. That is two counters for each disk it keeps between samples, a few hundred bytes. `ServerStats.disk_read_bytes_per_second` and `disk_write_bytes_per_second` take 230 and 231, this area's block. An agent from before this sends neither, and its history has no disk I/O.

### The control plane keeps two tiers, by age

- **A minute for a day.** For each connected server, the control plane adds up the samples of the current minute in memory: for each figure the sum weighted by the seconds each sample covers (`ServerStats.interval`), the seconds covered, and the highest value. At each minute's end it writes one row for each server that sent anything, all in one transaction. A 5 s and a 10 s sample then count for what they cover, and a minute half watched doesn't lean either way.
- **An hour for a month.** At each hour's end it adds up that hour's minute rows into one row for each server, the same way, in the same transaction as it removes minute rows older than 24 hours and hour rows older than 31 days.
- **What a row holds,** as integers: CPU in thousandths of a percent, memory used and its total, swap used, load over one minute in thousandths, network in and out and disk read and write in bytes per second, and the fullest disk's use and size, each as a weighted sum with its highest value, plus the seconds covered. Average and peak both survive the roll-up: an hour's row still says CPU touched 100% for a minute in it.
- **Sums, not averages, so rows merge.** With more than one control plane on Postgres, a server's agent may move from one to another mid-minute, and both write that minute. The row's key is the server, the tier and the minute or hour it starts, and a second write adds to the first: sums and seconds add, the highest stays the highest. An average is read as the sum over the seconds.
- **A hard cap.** A server has at most 1,440 minute rows and 744 hour rows, whatever happens to the clock: each write removes what's past its tier's age for that server, and a write that would go over removes that server's oldest rows first. A server removed from the fleet keeps its rows for 31 days, for the record, and then they go. So the table holds at most 2,184 rows for each server, ever.
- **One table,** `server_history`, keyed by server, tier and time, `WITHOUT ROWID` on SQLite and with its primary key as its only index on Postgres, written through the store's ordinary transactions. Samples aren't managed state, so nothing here is a change or a revision, as events and sessions aren't.

### What it costs

For each server, and for fifty:

- **Bandwidth:** nothing more. The samples already flow, about 150 bytes every 10 s, some 1.3 MB a day for each server; disk I/O adds about 10 bytes to each.
- **The agent's memory:** the disk counters, a few hundred bytes. Its idle budget doesn't move.
- **The control plane's memory:** one minute's sums for each connected server, about 200 bytes, so 10 kB for fifty, beside ADR 0040's 5-minute window, which stays as it is for the live charts.
- **Writes:** one row a minute for each server, in one transaction, so fifty servers write fifty rows a minute, and an hour's roll-up and pruning once an hour. SQLite takes that without notice.
- **Disk:** a row is about 130 bytes on SQLite and about 250 on Postgres, with its index. 2,184 rows are about 280 kB for each server on SQLite and 550 kB on Postgres; for fifty servers, about 14 MB and 28 MB, and that is the ceiling, not a growth rate.
- **Reads:** only when someone asks for a range. The busiest answer, a day of minutes, is grouped to at most 300 points by the query before it leaves the store.

A fleet stays cheap because every cost is a fixed amount for each server: nothing grows with time past the tiers' ages, nothing runs for history on the servers, and nothing is read until someone looks.

### Reading it

- `ServerService.GetServerHistory` takes a server and a range, 1 hour, 6 hours, 24 hours, 7 days or 30 days, and answers points on an even clock with each figure's average and peak, from the minute tier up to a day and the hour tier past it, grouped by the query so a range is never more than 300 points. A gap is a missing point, never a zero. It's `EFFECT_READ`, `NEEDS_VIEWER` as reading the server is, `idempotency_level = NO_SIDE_EFFECTS`, a GET, with a line in the panel's `methods.ts`.
- **The server's page** keeps its live 5 minutes as they are, and gains a range picker over its charts: Live, the default, then 1 h, 6 h, 24 h, 7 d and 30 d. A peak is drawn as a light band above the average, so an hour that averaged 20% and touched 100% doesn't read as a calm one.

### What this changes in ADR 0040

Its "history stays in the control plane's memory, never the store" holds for the live window, which the charts draw at 5 s. Past it, history now goes to the store, a minute at a time, and ADR 0040's consequence that an hour's chart would need samples while nobody looks no longer holds: those samples were always sent, as the heartbeat.

## Consequences

- A server's page answers what happened last night, or over the month, without anything new on the server, the network or the agent's memory.
- A control plane that was down, or a server that was away, has a gap for that time, and says so as a gap. Nothing fills it later, as no agent buffers.
- The store grows by a known amount for each server, at most a few hundred kilobytes, and its backups carry it.
- Alerts keep reading live samples; a later rule that needs a sustained value, such as CPU high for an hour, can read the minute tier rather than hold its own.
- Slices: (1) disk I/O in the sample, the table, the writer and the roll-up, with tests on SQLite and Postgres; (2) `GetServerHistory` and the range picker.
