# 0025. Live logs per deploy

- Status: Accepted
- Date: 2026-11-04

## Context

Issue #30 asks to follow a service's logs live, to search them, and to filter them by level and replica. People go to logs when something is wrong, so this has to be fast and hold up when the server is under load. That raises questions the earlier records left open:

- [ADR 0021](0021-the-docker-driver.md) runs a service as a Compose-labelled container, and a replace removes the old container, with its log. A deploy that failed and was rolled back, which is when logs matter most, left nothing to read. It also set no log rotation, and Docker's default driver, `json-file`, keeps a log without a limit, a classic way to fill a server's disk.
- [ADR 0017](0017-the-change-api-and-the-agent-stream.md) gives each agent one stream, which carries stats, plans and applies. A noisy log on that stream could stall them. Past 30 s of silence, the control plane closes it.
- [ADR 0018](0018-keep-the-budgets-in-rss.md) holds an agent under 40 MB of RSS at idle, and the agent's memory must not grow without limit under load.
- [ADR 0016](0016-scoped-api-tokens.md) gives tokens two scopes by a method's effect. Logs are a read, but they often hold what apps shouldn't have printed, so a token made to read the server list shouldn't read them.

## Decision

### `LogService`

| Method | Effect | Token needs | REST | CLI |
| --- | --- | --- | --- | --- |
| `ListLogSources` | read | the logs grant | `GET /v1/services/{id}/logSources` | `dockwell logs sources` |
| `ReadLogs` | read | the logs grant | `GET /v1/services/{id}/logs` | `dockwell logs read` |
| `WatchLogs` | read, server stream | the logs grant | none | `dockwell logs follow` |

A request names a service by ID or by name, with its project and environment once there is a choice of them, as ADR 0017's edits do. A deleted service is named by its ID alone: its first deploy may have failed and rolled back, and what the server kept can still be read.

- `WatchLogs` sends the newest lines of each container, then follows, through deploys and across servers. With `once`, it stops at the end of what the servers have, which is how a time range reads.
- `ReadLogs` is the REST path: pages of lines, newest page first, oldest line first within one, at most 5,000 lines and 8 MiB. A next page reads only sources with more to read.
- A line carries its time, its stream, its level and its text. Lines come in chunks by source, and a `LogSource` says what a source is: its server, its replica, its container, the revision and change that made it, and its state. So a line is labelled by replica, container and deploy, without repeating any of it on the wire.

### Log lines pass through, and sit nowhere

The agent reads Docker's API through its own client, `internal/docker`, on demand. Nothing of a log line goes in the store, which holds no log at all. Lines cross the control plane in memory on their way to a reader, within the bounds below. An agent reads only containers with the Compose labels of the service asked for, so no request can name a container on the server in general.

### The agent's stream stays clear

Each logs call sends its lines over `AgentService.SendLogs`, a client stream on **a connection of its own**. The call itself goes down the agent's main stream as a `LogsCall`, with a random token that names the upload. So:

- **A log can't stall stats, plans or applies.** On one stream, or one HTTP/2 connection, a stalled log would share a flow-control window, a TCP send buffer and a write lock with the stream that keeps the agent alive. With its own connection, it shares only the wire.
- **Flow control is the backpressure.** The control plane reads an upload only as fast as its reader takes the lines, so a slow reader stops its own connection and nothing else, and the agent's `Send` blocks. A second connection per reader costs a TLS handshake and a little memory, only while someone reads, and the connection goes with the call.
- **A logs call doesn't hold up a move to a renewed certificate.** ADR 0017's drain waits for the calls under way, and a follow has no end. It stops at once, and the control plane carries it on over the next stream.
- The token is random, and `SendLogs` checks that the server's certificate is the one asked, so one server can't send lines into another's call.
- An agent serves at most 16 readers at once, each of at most 32 containers. One more is told so, and tries again.

`make logs-load` measures this on the agent build, with a real control plane and a container that writes as fast as it can be read, on the fake engine: see the consequences below.

### Bounded all the way, and the agent alone drops

| Stage | Bound | When it is full |
| --- | --- | --- |
| Docker to the agent | 64 KiB read buffer, one line at a time, without allocating | Reads on, counting |
| A line | 16 KiB, the rest counted as `cut_bytes` | The line is cut, and says so |
| The agent, for one follow | 2 MiB/s for a stream, 4 MiB/s for the server | Drops, reason `TOO_FAST` |
| The agent to the network | 8 messages of 32 KiB | Drops, reason `READER_BEHIND` |
| The control plane | One message at a time per server, 256 KiB gathered for a reader | Stops reading: flow control |
| The browser | 100,000 lines and 64 MiB; a batch every 100 ms | The oldest leave, and the view says so |

- **Only the agent drops, and it counts exactly.** Dropping anywhere later would waste what was already carried, and would need a second way to say so. A marker, `LogGap`, travels in the stream where the lines were, with how many lines and bytes it stands for, and from when to when. It comes at most once a second per source, so a flood doesn't fill the view with markers. A test checks that what a writer wrote equals what arrived plus what markers say, line for line.
- **A reader of the past waits; a reader of the present drops.** Lines already in Docker's log aren't going anywhere, so reading a tail, a range or a search blocks on the network. A follow can't wait: Docker would rotate away what it hadn't read, without a word. A follow's history blocks too, until it catches up.
- **Dropping is cheap, and paced.** A line that is dropped costs a read and a count, with no allocation. Past 64 MiB/s a stream reads no faster, so a flood can't take a core just to be counted; a writer that much faster than that falls behind what Docker rotates away, and what goes that way can't be counted, so at that rate a marker's count can be short.

### Levels are inferred, and say so

Docker has no levels. The agent reads a level from each line it sends, without allocating: JSON and logfmt fields, then words such as `ERROR`, `[warn]` or `error:` near the start, and klog's `E0102` prefix, ignoring colour codes. It doesn't read one from the stream, as many apps log everything to standard error. An indented line, such as a stack frame, takes the level of the line it follows. A line that names none counts as info in a filter, and the panel says levels are read from the text. Reading it on the agent means one rule for every surface, and that a search for errors reaches lines the panel never loaded.

### How far search reaches

Docker's API has no search: it takes `since`, `until` and `tail`, and `tail` counts back from the end of the log whatever `until` says. So there are two searches, and the panel says which it is doing and how far it got.

- **Over what is loaded,** in the browser, on each keystroke: honest and cheap, at most 100,000 lines. The panel says how many of how many matched.
- **Over all the servers keep,** on request, through `ReadLogs`: each agent reads every line Docker has of each container, at most the newest 1,000,000 lines and for 30 s, and what it kept of those a change removed, and sends only what matches. It holds the newest matches, within 2 MiB for a container. Reading a rotated log back is about 50 MiB through Docker, a second or so of its CPU, and bounded by rotation. The answer says how many lines it read, how far back that reached, and whether it stopped at a limit, and the panel states it: "Searched 1,204,331 lines, back to 11:02: as far as the servers keep logs."

A query is plain text, in any case, never a pattern: a pattern from a request would be a way to spend a server's CPU. Older logs than the servers keep, or searching across a fleet at length, are a log store's job, which Dockwell doesn't have; shipping logs to one is later work.

### Logs mustn't fill the disk

Every container Dockwell makes gets a log config:

- **`json-file`, 5 files of 10 MiB, the older ones compressed,** so a container's log never takes more than 50 MiB of disk, and usually about 18 MiB. `json-file` over `local` because it is what `docker logs` and every log shipper read.
- **Only where the engine's default is `json-file`,** as `/info` says. Any other default is the operator's choice, which stands: `local` rotates by itself, and `journald` and the like keep logs elsewhere, with Docker's dual logging still reading them back. Within `json-file`, a container's options win over the daemon's, as Docker merges them.
- **It isn't part of a spec.** `Project` doesn't read it, so it can't read as drift, and a container made without it isn't replaced for it. It comes in with the next replace, which any deploy is, and the plan for that says nothing more about it, as nothing else changes. Until then, `LogSource.unbounded` says so, and the panel says what fixes it.
- **An export carries it,** as `logging: {driver: json-file, options: …}`, so `docker compose up` from an export doesn't lose the bound. Import takes that form and drops it, as Dockwell sets it anyway, as `expose` is dropped. Anything else in `logging`, such as another driver, is refused by name, as logs read from Docker wouldn't be there to read.

### Logs survive the deploy that wrote them

A container made by a deploy carries `dev.dockwell.revision` and `dev.dockwell.change`, given to the step through its context and, over the agent stream, in `ApplyCall.deploy`. They are Dockwell's own labels, so they aren't drift; one without them, as Compose made it, reads as a deploy unknown. Docker can't change a container's labels, so a later revision that didn't recreate it, such as one that only changed its restart policy, doesn't change them.

Before a step removes a container, the agent keeps its log, in `<data>/agent/kept-logs/<project>/<service>/<container>/`, in a directory only its owner can read:

- **Copied in two goes, so downtime barely moves.** Most of it while the container still runs, for up to 10 s, which isn't downtime. Then, once it has stopped, what it wrote since, for up to 1 s, which is: a replace's stated downtime goes from 12 s to 13 s, and the plan says so. Failing to keep a log never fails a step: it is logged, and the step goes on.
- **The bound.** A kept log is the container's newest 32 MiB of lines, in gzip files of 4 MiB each, so usually a few MiB, and the record says when it was cut. The archive holds at most **256 MiB on a server**, and each service keeps only its **last failed and its last replaced** container there. Past the budget, the oldest replaced go first, then the oldest failed, and the newest always stays. Nothing stays past **30 days**, as logs may hold secrets. On a disk with under 1 GiB free, nothing is kept, and the agent says so.
- **What counts as failed.** What a rollback replaces or removes, whatever its state: the engine says so in `ApplyCall.deploy.rollback`. And what wasn't well when it was replaced: restarting, unhealthy, or exited with an error.
- **Keeping the stopped container instead** was the other way. It would hold its volumes, which ADR 0021 refuses to delete while in use; `docker compose` would list it and treat it as a replica; and its log is out of Dockwell's bound.

`WatchLogs` shows what runs now, and containers that started since it began, even one that came and went between two looks, as a failed deploy can. Filters by revision or change, a time range and a search read what was kept too. So a failed deploy's logs are one link away, from its row in Activity or its plan sheet.

### Permissions

- **Logs need a grant of their own.** `TokenGrant` joins a token's scope, as ADR 0016 foresaw: `TOKEN_GRANT_LOGS`, which the maker of a token has to ask for, on a token of either scope. A method says it needs one with `(dockwell.v1.needs_grant)`. A token without it gets `token_logs`, and tokens made before there were grants have none. A signed-in session can read logs, as it can do everything: an install has one person until members arrive, and roles narrow this then.
- **Local access's tokens have it.** Whoever can read the control plane's data directory can read logs with Docker anyway. A local token from before there were grants is replaced the first time it is refused.
- **Reading logs is on record.** `logs.read` goes in the audit log with the actor, the surface and the service, at most once an hour for one reader and one service: reconnecting, paging and searching are one reading, and every event would bury the rest. The count lives in memory, within 10,000 entries, so a restart costs one more event each. It isn't on the timeline, whose audit events are what servers did and what gave or took away a way in ([ADR 0020](0020-the-event-timeline.md)). It never says what was searched for, which may itself be sensitive. A reading that can't be recorded isn't served.
- **Nothing is redacted.** Dockwell can't tell what in a line is a secret, and promising to would make people trust it. What it does: it never adds a known secret to a log itself, as its drivers' messages hold none ([ADR 0017](0017-the-change-api-and-the-agent-stream.md)); a spec's environment never passes through this path; and the panel says why reading takes permission. See the [threat model](../security/logs.md).

### When a server goes away

Each server's relay reads until there is nothing more, and carries on whenever its agent returns.

- **A stated condition.** `ServerLogStatus` says each server is reading, done, offline or unavailable, with why, and the panel says so where it is seen: "hz-1 is offline." Containers go on running and logging there meanwhile.
- **What resumes.** A place in a log is a source's last line: its time, and how many lines share that time, as two lines can. When a server comes back, or a reader reconnects with `resume_token`, each source carries on after its place, with no line twice. A new container since is read from its start; one that was removed since reads on from what was kept.
- **What can't resume.** Docker rotates what was written while nobody read, so past 50 MiB of output there is a real gap. If the next line is over a second after the place, the agent checks what Docker's oldest line is, and if it is later than the place, sends a marker with reason `GONE` and the times. A container removed and not kept gets the same marker. Lines dropped under load were counted, and aren't read again unless the stream restarts from a place before them. Across servers, lines are ordered by each server's own clock, so only as well as those agree.
- **Closing the page stops the work.** The control plane's context ends, which cancels the call down the main stream and ends its upload. The agent's request to Docker goes with it. A test holds this to well under a second, and the browser test measured under 60 ms.

### The panel

- **Only what shows is drawn.** `LogStream` keeps each row's height in a Fenwick tree, so a row's position and which row is at a position are O(log n) at any length, and draws what fits. Lines leave from the start and arrive at the end without an O(n) step. Heights start as an estimate from how many characters fit, and rows that show measure themselves, keeping a reader's place when one above them changes.
- **Batched.** Lines reach the store at most every 100 ms, whatever the rate, and only the log redraws. A filter or search is one pass over plain data.
- **Following** stops the moment a reader goes up, by wheel, key or finger, before the next line can pull them back; and on choosing text to copy. It resumes with Follow, the End key, or a jump button that says how many lines came meanwhile.
- **Colours are safe and legible.** `parseAnsi` keeps colour and weight, and nothing else. Colours map to the design's tokens, so black on a dark screen still reads, and backgrounds are dropped. Every other code goes, control characters and direction overrides show as a replacement character, and text only ever reaches text nodes. The CLI does the same for a terminal, as what a container prints could move the cursor or set the title.
- **Screen readers** hear when the stream starts and stops, and not each line.

## Consequences

- **Memory.** An idle agent of the agent build reads 20.3 MB, 0.6 MB more than the base it was measured beside at 19.6 MB, and a control plane with its own agent 40.4 MB, against 39.8 MB: medians of 5 runs after 180 s (ADR 0018), each within the noise the footprint comment ignores. `internal/agent/logs` is new on `agentPackages`, and brings no module the agent didn't link. During a flood an agent reads about 4 MB more, and doesn't grow: `make logs-load` read 19.3 MB before any reader, and 24.0 MB at most through 60 s of a container writing as fast as it could be read, with a reader following, then with one stuck, its stats on time throughout.
- **CPU under a flood** is about 17% of a core, on the agent, for a writer it can't carry. A reader that keeps up costs far less.
- **Disk.** Each container takes up to 50 MiB, and each server up to 256 MiB more for what it kept.
- **A replace's downtime reads 13 s,** one more than ADR 0021 said, for a stopped container's last lines.
- **Budget for the wire:** a live stream can carry 2 MiB/s from each server, and a server 4 MiB/s, so a flood is visible on a small link. It is bounded, and a reader who can't take it slows it down.
- **Only services have logs for now.** A container on its own runs with the same labels, in its server's project, so `LogService` can take it when a screen needs it.
- **Search reaches what Docker and the archive hold.** Past that, it would need a log store, which the PRD leaves for later.
- **Migration 0017** adds `api_tokens.grants`.
- **`dockwell logs follow` ends with no error on an interrupt,** exit status 0. Everything a reader isn't told by a line goes to standard error, so standard output stays the lines.
