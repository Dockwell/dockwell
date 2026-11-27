# 0109. The agent's memory headroom

- Status: Accepted. Amends [ADR 0060](0060-what-an-idle-agent-keeps-in-memory.md), where an agent on its own collects at `GOGC=50` and the cap on its CPUs is left for a decision of its own.
- Date: 2026-11-25

## Context

The PRD holds an agent under 40 MB of RSS, and `scripts/budgets.sh` warns from 30 MB ([ADR 0018](0018-keep-the-budgets-in-rss.md)). `make transfers-load` holds an agent to the same 40 MB while files and Docker archives upload and download. By 2d9779ec3 the agent had run out of room: on the machine with 16 CPUs that measured this, `make transfers-load` failed in 3 of 5 runs, peaking at 39.8 to 41.7 MB, and `make budgets` read an idle agent at 35.8 MB. A pull request that added one more feature failed the gate, and more are on the way.

Measured on main at 2d9779ec3, as the two scripts measure: an idle agent sampled after 180 s, and the most a transfer run's agent ever held (VmHWM).

| An idle agent, 33.9 MB with no tasks | MB | What it is |
| --- | ---: | --- |
| Pages of the binary | 21.3 | Of 24.0 MB of code and tables. The kernel maps 64 KB around each page a process touches, and over 3 minutes the collector's scans of each goroutine's stack touch nearly every 64 KB of the tables it reads: 2.57 of 2.70 MB of pctab and 3.46 of 3.79 MB of `_func`. What's resident follows the binary's size, not how much of it runs. |
| The Go heap | 10.3 | 4.0 MB live, 2.1 MB of it protobuf's descriptors and coders, built the first time a message goes over the stream. |
| The runtime's own, for each CPU | 3.1 | 0.8 MB with 4 CPUs. |
| Stacks | 1.5 | 0.9 MB with 4 CPUs. |

- **The agent's own code grew, spread across its packages.** Since ADR 0093, `internal/agent` grew by 429 KB of code, `agent/panels` by 139 KB, `resource/dockerdaemon` by 114 KB and `resource/container` by 107 KB, and x/crypto/ssh came in at 56 KB. No package the agent links goes unused, and nothing large that it links never runs.
- **What a transfer added was garbage, not what's live.** During four uploads, the live heap stayed near idle, and the collector ran about ten times a second. In 13 s, connect's reader made 45 MB of garbage: it reads each message into a `bytes.Buffer` from a pool, which starts at 512 bytes and doubles until a 128 KiB part fits, from a pool each call makes anew and each collection empties. #883's part buffer then copied each part out of it.
- **A download held a MiB before it sent a byte.** It reads a file's first MiB to tell what it holds, as a read does, so four downloads at once held 4 MiB, though a binary file shows what it is in its first bytes.

Tried, and not done, as they moved nothing:

- **`madvise(MADV_RANDOM)` on the binary's mappings,** to stop the kernel mapping 64 KB around each touch. It doesn't stop it: 134 touches of a cached file mapped 8,576 KB with it and without it, on Linux 6.8.
- **`debug.FreeOSMemory` every 10 or 30 s at idle.** An idle agent read 32.9 and 33.2 MB against 32.7 MB without it, as ADR 0060 found of one call.
- **Connect's reader sized to each message,** in a copy of connect. It lowered the peak 0.7 MB, but connect has no option for it, and reading the stream directly, as below, does more.

## Decision

- **An agent on its own runs Go code on at most 4 CPUs** (`agentCPUs`, `runOnAgentCPUs` in `internal/cli`). Go keeps memory for each CPU it may run on, and an idle agent's goroutines wake on any of them. An agent's work waits on the network, the disk and Docker more than on the CPU: four uploads and four downloads at once took as long on 4 as on 16. A `GOMAXPROCS` in the environment stays as the operator set it. A server of 4 CPUs or fewer, or a container limited to them, keeps Go's own count, which follows the container's limit as it changes. The control server's own agent shares its process with the control plane, and is left as it is, as with `GOGC` (ADR 0060).
- **An agent on its own collects at `GOGC=25`** (`agentGC`), where ADR 0060 set 50. Its heap grows to a quarter past what's live, as transfers already ran (`transferGC`, ADR 0045). An idle agent collects every 5 s or so in place of every 15 s (9 collections from 60 to 196 s against 26), each of a heap of about 5 MB, which is a few hundredths of a percent of a CPU. ADR 0093 left this out while the agent was under the warning line, as a choice of CPU against memory; it's over it now. A `GOGC` in the environment still stays as the operator set it.
- **An upload's parts are read from the connection into the one buffer its stream keeps.** `enroll.Transfers.ReceiveParts` calls ReceiveFile as gRPC does, without gzip, and reads each message's 5-byte prefix and then its bytes straight into a buffer the size of a part. A message of only `data`, which is what the control plane sends, is read in place; anything else reads as proto reads it. A part over 1 MiB is refused unread. How the stream ended comes from its trailers, as the same `*connect.Error`, with the code and the message the control plane sent, that connect's client gives. Downloads, and every other call, stay on connect.
- **A journal's line is decoded once, into the fields a line is made of.** `parseJournal` decoded each line of journalctl's JSON into a map of every field, then each field again. At `GOGC=25`, two follows of a flooding journal took 103 and 104% of a CPU, past `make logs-load`'s limit of 100. Decoded into a struct, with a field that has nothing escaped taken as it is, a line takes 2.0 µs and 9 allocations where it took 4.7 µs and 25, and the follows take 79 and 82%. A test holds it to the map's reading over text, bytes, escapes, nulls and what isn't either.
- **A download reads its first MiB only when its first part doesn't already show a binary file.** What a read refuses in a text file's first MiB still stops a download before any byte goes. A binary file goes by `keyScan` from its first part on, as a file past its first MiB did: a key in a binary file still never goes, but the parts before it may.

## Consequences

Measured as the scripts measure, on the machine with 16 CPUs, with other work running on it throughout (a load of 8 to 22). The two builds ran side by side, alternating: `make budgets` with main's build as its baseline, and five runs of `make transfers-load` of each. The figures are medians, with the runs' range.

| | main at 2d9779ec3 | This |
| --- | ---: | ---: |
| `make budgets`, an idle agent | 35.6 MB (34.7 to 36.0) | 32.2 MB (32.1 to 32.4) |
| Of that, anonymous | 14.5 MB | 11.0 MB |
| `make transfers-load`, the most an agent held | 40.3 MB (40.0 to 40.4), 4 of 5 failed | 38.1 MB (37.6 to 38.3) |
| Settled, at 30 s | 34.2 MB | 32.4 MB |
| Four uploads of 512 MiB | 38.1 MB | 33.6 MB |
| Four downloads of 512 MiB | 38.4 MB | 35.6 MB |
| Four 1 MiB previews | 38.7 MB | 37.6 MB |
| Four artifact uploads | 39.4 MB | 37.0 MB |
| Mixed artifact transfers with a retry | 40.3 MB | 38.1 MB |

Rebased onto b6c16ca6a, which added one more feature to the agent, the same holds: an idle agent at 32.6 MB against main's 35.6 MB, and a transfer's peak at 37.4, 38.2 and 38.4 MB against main's 40.3, 40.9 and 41.0 MB, all three of which failed.

Each change, measured on its own against the build before it:

| Change | An idle agent | A transfer's peak |
| --- | ---: | ---: |
| 4 CPUs | −1.5 MB | −2 MB |
| `GOGC=25` | −0.9 MB | none |
| Parts read in place | none | −0.7 MB at the peak, −1.5 MB for four uploads (measured as connect's reader sized to each part, which does less) |
| A download's head | none | not measured on its own: four downloads peaked at 35.6 MB with it, and at 36.8 to 37.8 MB on 4 CPUs without it |

- **`make logs-load` passes, where main failed it here:** with 20 followers of a flood, the agent held 36.1 and 36.2 MB at most, where main's held 40.7 MB and failed. Two follows of a flooding journal took 79 and 82% of a CPU, under its limit of 100; at `GOGC=50` they took 95%, and 20 followers held 40.0 and 40.1 MB.
- **The gate passes with about 2 MB to spare** on a machine of 16 CPUs, where it failed 4 times in 5. On a CI runner, with 4 CPUs, the cap changes nothing, and the rest holds.
- **An idle agent is 3.3 MB lower on 16 CPUs,** and about 0.9 MB on a server of 4 or fewer, where only `GOGC=25` applies. It's still over the warning line here, at 32.2 MB. Under 30 MB needs a smaller binary: 21 MB of an idle agent is its pages, and they grow with every feature's code.
- **Four uploads and four downloads at once took as long on 4 CPUs as on 16:** 5 to 7 s and 4 s, against 5 to 6 s and 4 s. Four artifact uploads, which hash every byte, took 9 to 12 s against 6 to 12 s, in runs as noisy as the machine.
- What would make it smaller, left for decisions of their own:
  - **Fewer sorts of struct slices.** `slices.SortFunc` on 24 struct types instantiates pdqsort 24 times, about 350 KB of code. Sorting pointers, or with `sort.Slice`, shares one. About 0.4 MB of pages.
  - **Fewer of the public API's files in the agent's protocol.** The agent links 23 of its files, `change.proto` the largest. Each message keeps four methods and its type, and each file's descriptor is unmarshalled in full the first time one of its messages goes over the stream.
  - **A preview over a transfer's stream.** Four 1 MiB previews at once each hold their bytes, and the main stream marshals each again, about 1 MB at the peak.
- An agent on a server of more than 4 CPUs runs at most 4 of its goroutines at once. A backup sealed while three transfers run shares 4 CPUs with them, and may take longer on a large server than it did. `GOMAXPROCS` in the environment of the installed service lifts it.

## Follow-up on main at 43a4a2cc1

An uninstrumented run reached 39.5 MB during mixed artifact transfers and 39.6 MB by the final archive check. A heap profile during mixed transfers attributed 2.26 MiB to protobuf descriptors and their strings, and about 0.25 MiB to the two HTTP/2 frame readers. Counters in the diagnostic build recorded about 19.5 MB of allocations for 2 GiB of mixed artifact traffic, or at most 0.0091 allocated bytes per payload byte including profiler overhead. The peak had both resident binary pages and live or recently allocated transfer memory.

File and artifact traffic can share one transfer connection with 144 KiB of receive credit per stream and 432 KiB in all. New file and artifact payloads use 31 KiB frames. The receivers still accept older 128 KiB payloads. HTTP/2 read frames are capped at 16 KiB independently of gRPC message sizing. Downloads reuse the generated Connect sender and their protobuf part requests. The collector and workload use the existing settings.

In successive pilots, changing to 31 KiB payloads lowered the whole-run reading from 39.6 to 38.7 MB. Sharing the connection left the pilot whole-run peak at 38.7 MB and lowered artifact downloads from 38.6 to 38.3 MB. Capping the HTTP/2 frame reader and reusing send state lowered artifact uploads from 37.2 to 36.6 MB and the whole run to 38.6 MB. Streaming previews reduced the preview phase by 0.6 MB. Late artifact phases continued to set the whole-run peak, so previews keep their existing route.

| Alternating runs | Whole-run peaks | Median |
| --- | --- | ---: |
| Main | 39.6, 39.3, 38.9 MB | 39.3 MB |
| Transfer changes | 38.7, 38.7, 38.5 MB | 38.7 MB |

The original follow-up gate called for a 1.5 MB median reduction and a peak of 38.5 MB or less in each run. The measured reduction was 0.6 MB and two runs peaked at 38.7 MB, so the original gate was missed.

The changes are accepted for the measured improvement over main. Transfer peaks were lower in all three alternating pairs, with a 0.6 MB lower median. The idle standalone median was 34.3 MB on the branch and 34.7 MB on main, a reduction of 0.4 MB. Both builds reached 47.6 MB in the log search load and 37.8 MiB in the forwarding load.
