# 0065. A server's history, kept by the control plane

- Status: Proposed
- Date: 2026-11-10

## Context

A server's page draws CPU, memory, disk and network for the last 5 minutes and no further. [ADR 0040](0040-a-servers-live-use.md) kept it so: history lives in the control plane's memory, 60 samples at most, never in the store. Issue #426's audit ranks the gap thirteenth: 1Panel keeps a server's CPU, memory, load, disk I/O and network for days, over a range you pick, and a person asking why something was slow last night finds nothing here.

What bears on it:

- **The samples already arrive.** Under ADR 0040 every agent sends one on its stream every 10 s while nobody watches, as the stream's heartbeat, and every 5 s while someone does. The control plane reads each and drops it once it falls out of the 5-minute window. Keeping history costs no new sample and no new bandwidth: it's a matter of what the control plane keeps.
- **A sample's rates look back.** Each covers the time since the agent's previous sample, and the first after a reconnect primes the counters and reports 0 CPU. `ServerStats.interval` looks forward: how long until the next.
- **The agent stays within 40 MB at idle** ([ADR 0018](0018-keep-the-budgets-in-rss.md), [ADR 0060](0060-what-an-idle-agent-keeps-in-memory.md)), and keeps nothing it doesn't need. History held on each server would cost every server memory or disk, and be lost with it.
- **The control plane has one store,** SQLite on its own server or Postgres, which every query must work on, with the store's tests run on both.
- **A sample doesn't carry disk I/O.** It has CPU, memory and swap, load, each disk's use, and network in and out as counters since boot (`ServerStats`, fields 1 to 17, in the public schema).
- **Below an admin, a server is its name and state** ([ADR 0049](0049-roles-and-invites-by-project.md)): inventory, stats and history stay an admin's.

## Decision

### The agent sends what it sends now, and keeps nothing

- **No new message, no new rate.** History is built from the samples ADR 0040 already has each agent send. An agent buffers nothing while it's away.
- **A sample gains disk I/O,** as counters since boot like the network's: `ServerStats.disk_read_bytes` 18 and `disk_written_bytes` 19, summed over the server's whole disks from `/proc/diskstats`. A whole disk is a device in `/sys/block` with a `device` link; partitions, and `loop`, `ram`, `zram`, `dm-*` and `md*` devices, are left out, as they sit on top of a disk and would count its bytes two or three times. The agent keeps nothing for it between samples. An agent from before this sends neither, and its history has no disk I/O.

### The control plane keeps two tiers, by age

- **A minute for a day.** For each server, the control plane adds up the current minute in memory, keyed by its own clock's minute as each sample arrives, never the agent's, so a clock that's ahead doesn't write into the future. Each sample counts for the time since the same server's previous sample on this control plane; past 2.5 times its interval (ADR 0040's gap rule) it counts for nothing, so a gap isn't counted as covered, and so does the first sample on a new stream, which a new collector sends with no CPU. For each figure it keeps the sum weighted by those seconds, and the highest value, and the seconds covered. Bytes moved come from the counters: the last sample's minus the first's within the minute, or from zero after a reboot sets them back, so missed samples lose nothing. The sample's uptime tells a reboot from a counter that dropped while the server stayed up, as an interface made again does (tailscale0 at each tailscaled restart): that step counts nothing, rather than the counter's whole life. A peak rate is over the agent's own times between its readings, not the control plane's between arrivals, so samples a stalled stream delivers at once don't read as a burst.
- **A row for every server, every minute the control plane runs.** At each minute's end it writes one row for each server in the fleet, all in one transaction, with 0 seconds covered for one that sent nothing. So a minute with a row and nothing covered is a server that went quiet while Dockwell listened, and a minute with no row is a control plane that wasn't there, or wasn't there for all of it: the minute a control plane starts in has no row. A minute write whose commit result is lost isn't retried: a retry can't tell whether the first landed, and a minute lost is safer than a minute counted twice.
- **An hour for a month, rebuilt, never added to.** Hour H is rolled up once minute H:59 is written: its row is computed from its minute rows and replaces any row already there, so a restart right after the roll-up, a retry, or two control planes rolling the same hour give the same row. As the control plane starts, it rolls every hour of the last 24 that has minute rows and no hour row; after that, each hour rolls the hour that ended and any whose roll failed.
- **What a row holds,** as integers: CPU in thousandths of a percent, memory used and its total, swap used, load over one minute in thousandths, network in and out and disk read and written in bytes, and the fullest disk's share in thousandths, each with its highest value, plus the seconds covered. The fullest disk is kept as a share, as an hour's average of "the fullest disk's size" would mix disks. Average and peak both survive the roll-up: an hour's row still says CPU touched 100% for a minute in it.
- **Sums, so minute rows merge.** With more than one control plane on Postgres, a server's agent may move from one to another mid-minute, and both write that minute. The row's key is the server, the tier and the minute or hour it starts, and a second minute write adds to the first: sums and seconds add, the highest stays the highest. The upsert is the dialect's: SQLite's scalar `MAX(a, b)`, Postgres's `GREATEST(a, b)`.
- **A hard cap.** A server has at most 1,440 minute rows and 744 hour rows, whatever happens to the clock: each hourly roll-up deletes each server's rows of each tier past the 1,440th (or 744th) by key, as one range delete, which leaves at most an hour's more minutes between roll-ups, and an hourly prune removes minute rows older than 24 hours and hour rows older than 31 days. A server removed from the fleet keeps its rows for 31 days, for the record, and then they go. So the table holds at most 2,184 rows for each server, ever.
- **One table,** `server_history`, keyed by server, tier and time, `WITHOUT ROWID` on SQLite, and with one more index, by tier and time, for the hourly roll-up and prune, which read every server's rows between two times: without it, each is a whole-table scan, about a second of the write lock every hour at 1,000 servers. Its rows go through the store's ordinary transactions, a minute's a hundred to a statement. Samples aren't managed state, so nothing here is a change or a revision, as events and sessions aren't. The merge, the roll-up and the catch-up are tested on both stores.

### What it costs

| | One server | Fifty servers | A thousand |
| --- | --- | --- | --- |
| Bandwidth | about 12 B more a sample | about 6 kB a minute | about 120 kB a minute |
| The agent's memory | nothing kept | the same on each | the same on each |
| The control plane's memory | about 200 B for the current minute | about 10 kB | about 200 kB |
| Writes | 1 row a minute; an hour row and 60 deletes an hour | 50 rows a minute in one transaction | 1,000 rows a minute, 17 a second |
| Rows at the ceiling | 2,184 | 109,200 | 2.2 million |
| Disk at the ceiling (SQLite, Postgres) | about 0.45 MB, 0.8 MB | about 22 MB, 40 MB | about 0.45 GB, 0.8 GB |

A row is 22 integers and its key: about 180 to 220 B on SQLite, and about 300 to 400 B on Postgres with its indexes and the dead rows rolling deletes leave. The writes sit under SQLite's single writer and Postgres's advisory write lock without contention. Reads happen only when someone asks for a range: one indexed range read, grouped by the query before it leaves the store. Every cost is a fixed amount for each server: nothing grows with time past the tiers' ages, nothing runs for history on the servers, and nothing is read until someone looks.

### Reading it

- **`ServerService.GetServerHistory`,** an admin's: `EFFECT_READ`, `NEEDS_ADMIN`, `REACH_INSTALL`, like `GetServerDiskUse`, `idempotency_level = NO_SIDE_EFFECTS`, a GET, with a line in the panel's `methods.ts`. The response trimming passes a message it doesn't list whole, so a viewer's grant must never reach it.
- **Ranges and buckets,** so points sit on an even clock: 1 hour by the minute (60 points), 6 hours by 2 minutes (180), 24 hours by 5 minutes (288), 7 days by the hour (168), 30 days by 3 hours (240), the first three from the minute tier and the rest from the hour tier. A point is drawn only when at least half its time was covered, so 10 s of 5 minutes doesn't read as a calm 5 minutes. A gap is a missing point, never a zero, and a point says whether Dockwell was there: covered, quiet while it listened, or not listening.
- **The server's page** keeps its live 5 minutes as they are, and gains a range picker over its charts: Live, the default, then 1 h, 6 h, 24 h, 7 d and 30 d. A peak is drawn as a light band above the average, so an hour that averaged 20% and touched 100% doesn't read as a calm one.
- **#410's heartbeat bars** read the minute tier: 30 minutes of heard or not for each server, surviving a restart, with a control plane that was down drawn apart from a server that was quiet.
- **What else reads it:** the incident view's "the metric that broke" (PRD experience 7), the sustained CPU that suggests adding a server (the PRD's "Scaling the experience", rule 5), and a later sustained-CPU alert. The minute tier lags by up to a minute.

### Retention, backups and privacy

- **Retention is fixed** at 24 hours of minutes and 31 days of hours. A setting ([ADR 0042](0042-the-installs-settings.md)) can come later if people ask.
- **The control-plane backup (#356) carries the table,** tens of megabytes at most for a small fleet, and a restore brings it back with a gap for the time between.
- **Only numbers,** keyed by server ID: no names, processes, paths or addresses. Only admins read it, and it's gone after 31 days, a removed server's included.

### What this changes in ADR 0040

Its "history stays in the control plane's memory, never the store" holds for the live window, which the charts draw at 5 s. Past it, history now goes to the store, a minute at a time, and ADR 0040's consequence that an hour's chart would need samples while nobody looks no longer holds: those samples were always sent, as the heartbeat. Slice 1 also corrects `server.proto`'s "15 s while nobody does", which the code sends every 10 s.

## Consequences

- A server's page answers what happened last night, or over the month, without anything new on the server, the network or the agent's memory.
- A control plane that was down, or a server that was away, has a gap for that time, and the two read differently. Nothing fills it later, as no agent buffers.
- The store grows by a known amount for each server, at most about a megabyte, and its backups carry it.
- Service history is a separate decision, for the PRD's "Live node and service metrics" row.
- Slices: (1) disk I/O in the sample, the table, the writer, the roll-up and the catch-up, with tests on SQLite and Postgres; (2) `GetServerHistory`, the range picker and #410's bars.
