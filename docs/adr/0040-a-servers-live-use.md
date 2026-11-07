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

- **While anyone watches the fleet,** agents sample every 5 s. Every panel page watches the fleet through one `WatchServers` stream, so that is while any panel tab is open.
- **While nobody does,** agents sample every 15 s. That is what the stream needs to stay open, half the 30 s a stream may stay silent, and no more. The sample is the heartbeat; sending an empty one instead would save reading a few files under `/proc` and nothing else.
- **The last watch closing** slows agents down after 30 s, so a page that reloads, or a phone that locks for a moment, doesn't slow them down and speed them up again.
- **The first watch opening** speeds them up at once, and each agent sends a sample as it hears, so the page that opened it has a new number within a round trip.

An agent from before this ignores `Pace` and goes on at its welcome's pace. A control plane from before this never sends one, and a newer agent goes on at the welcome's 5 s. Each sample says the pace it was taken at (`ServerStats.interval`).

### History, and how it's drawn

History stays where it was: in the control plane's memory, never the store, at most 60 samples from the 5 minutes before the newest. That is the charts' window. A server whose agent went unwatched has 15 s samples in it, so a page opened then draws the last 5 minutes at once, coarsely, and finely from then on.

The panel draws a trend on an even clock: one point every 5 s, back from the newest sample, each the sample whose rates cover it. A 15 s sample covers three points, and a gap longer than two and a half of a sample's own interval ends the trend, as an outage did before. Drawn one point to a sample, the slower part would look three times as quick.

### What takes the disk, only when asked

`ServerService.GetServerDiskUse` asks a server's agent, which asks Docker for `/system/df` and its own data directory, and finds the disk that holds Docker's data. It answers with that disk's size and use, and Docker's share of it: images with the build cache, what containers wrote to their own layers, volumes, and the logs json-file and local keep, with what images and volumes no container uses. The rest of what's used is everything outside Docker.

The panel asks when a server's Overview opens, and again every 5 minutes while it stays open. The agent asks Docker one count at a time, keeps the answer for 30 s, and gives a count 60 s, so two people opening the page, or one opening it twice, ask Docker once. It holds nothing between asks but the last answer, a few hundred bytes.

## Consequences

- With nobody looking, an agent reads a few files under `/proc` every 15 s and sends one small message, which its stream needs anyway, and nothing asks Docker what its data takes.
- A page opened after a quiet spell draws 5 minutes of coarse history at once, which a live chart fills in within 5 minutes. A chart of an hour, as the mock's caption has it, would need samples while nobody looks, which this rules out, so the caption says what the window covers.
- What runs in the background on a server's behalf now depends on who is watching. A feature that needs samples while nobody looks, such as an alert on sustained CPU, needs its own decision on what that costs, as alerts are free (PRD principle 3) and should stay cheap.
- `GetServerDiskUse` is a read with a real cost on the server. It's an `EFFECT_READ` like the rest of `ServerService`, so any token that reads servers can ask; the agent's 30 s answer bounds how often that reaches Docker.
- Processes (#209's second part) follow the same rule: listed only while a page shows them.
