# 0040. A server's live use, while someone looks

- Status: Accepted
- Date: 2026-11-07

## Context

A server's page is the first thing its owner opens, and #209 asks it to show what Server-Overview.dc.html shows: CPU, memory, disk and network over time, load and uptime, what the server is, what runs on it, and what takes its disk. The issue adds a rule: stats cost nothing while nobody is looking, and the agent stays within its 40 MB at idle (ADR 0018).

Until now every agent sent a sample every 5 s, whether or not anyone watched, and the control plane kept the last 60 in memory for sparklines (ADR 0017). That sample is also the stream's heartbeat: the control plane closes a stream that says nothing for 30 s (ADR 0025), and a server's reports read as unknown after 90 s (ADR 0027).

What takes a disk is a different kind of read. Docker answers it with `/system/df`, which adds up every volume by walking it, so it takes seconds on a busy server and longer on a big one, and Docker refuses a second count while one runs.

## Decision

### How often agents sample, and who decides

The control plane decides each agent's pace, and tells it: in its welcome, as before, and with a new `Pace` message on the stream when it changes.

- **While anyone watches the fleet,** agents sample every 5 s. Every panel page watches the fleet through one `WatchServers` stream, so that is while any panel tab is open and in sight. A page out of sight for a minute, as a tab in the background or a locked phone's browser is, closes its watch, and opens it again as soon as it's seen.
- **While nobody does,** agents sample every 10 s. That is what the stream needs to stay open, a third of the 30 s a stream may stay silent, so a slow uplink or TCP's backoff can hold up two samples before the stream closes, and no more. The sample is the heartbeat; sending an empty one instead would save reading a few files under `/proc` and nothing else.
- **The last watch closing** slows agents down after 30 s, so a page that reloads, or a phone that locks for a moment, doesn't slow them down and speed them up again.
- **The first watch opening** speeds them up at once, and each agent sends a sample as it hears, so the page that opened it has a new number within a round trip.

An agent says it follows a pace in its hello (`Hello.follows_pace`). One from before this doesn't, ignores `Pace`, and sends samples that say no pace. So it's welcomed at 5 s, whoever watches, and told nothing more, as before paces, and the control plane says that pace on its samples, or the panel would take them for another and lose their trends. A control plane from before this never sends `Pace`, and a newer agent goes on at the welcome's 5 s. Each sample says the pace it was taken at (`ServerStats.interval`).

Each watch keeps only which servers changed since it last sent, and sends each as it then stands. So a page that reads slowly, or one that opens as hundreds of agents answer the faster pace at once, gets each server's latest state and misses the samples in between, rather than being dropped and starting again from a snapshot, which would only send it more.

### History, and how it's drawn

History stays where it was: in the control plane's memory, never the store, at most 60 samples from the 5 minutes before the newest. That is the charts' window. A server whose agent went unwatched has 10 s samples in it, so a page opened then draws the last 5 minutes at once, coarsely, and finely from then on.

The panel draws a trend on an even clock: one point every 5 s, back from the newest sample, each the sample whose rates cover it. A 10 s sample covers two points, and a gap longer than two and a half of a sample's own interval ends the trend, as an outage did before. Drawn one point to a sample, the slower part would look twice as quick.

### What takes the disk, only when asked

`ServerService.GetServerDiskUse` asks a server's agent, which asks Docker for `/system/df` and its own data directory, and finds the disk that holds Docker's data. It answers with that disk's size and use, and Docker's share of it: images with the build cache, what containers wrote to their own layers, volumes, and the logs json-file and local keep, with what images and volumes no container uses. The rest of what's used is everything outside Docker.

The panel asks when a server's Overview opens, and again every 5 minutes while it stays open, but not as its tab comes back into focus, and not while the agent can't read Docker. Until the answer comes, it draws each disk's used and free space, as it does when Docker can't say. The agent asks Docker one count at a time, keeps the answer for 30 s, and a failure for as long, gives Docker 5 s to say where it keeps its data and the count 60 s, so two people opening the page, or one opening it twice, ask Docker once. It holds nothing between asks but the last answer, a few hundred bytes.

Some of what Docker counts can be on another disk, such as a volume bound to one. When its images, volumes, containers and logs add up to more than its disk has used, the panel says so, and draws that disk's used and free space alone.

## Consequences

- With nobody looking, an agent reads a few files under `/proc` every 10 s and sends one small message, which its stream needs anyway, and nothing asks Docker what its data takes. An agent from before this goes on every 5 s, as it did, until it's upgraded.
- A page opened after a quiet spell draws 5 minutes of coarse history at once, which a live chart fills in within 5 minutes. A chart of an hour, as the mock's caption has it, would need samples while nobody looks, which this rules out, so the caption says what the window covers.
- What runs in the background on a server's behalf now depends on who is watching. A feature that needs samples while nobody looks, such as an alert on sustained CPU, needs its own decision on what that costs, as alerts are free (PRD principle 3) and should stay cheap.
- `GetServerDiskUse` is a read with a real cost on the server. It's an `EFFECT_READ` like the rest of `ServerService`, so any token that reads servers can ask; the agent's 30 s answer bounds how often that reaches Docker.
- Processes (#209's second part) follow the same rule: listed only while a page shows them.

## As built later: what each volume holds

`ServerDiskUse.volume_use` (60) lists each volume Docker keeps, with what it holds, where its files are and how many containers use it, from the same `/system/df` read, so it costs nothing more. `VolumeService.GetVolume` asks the agents of a volume's servers for it when its page opens, a viewer's in the volume's project, where `GetServerDiskUse` is an admin's: one volume's size says nothing of the rest of the disk.
