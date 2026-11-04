# 0018. Keep the memory budgets in RSS, measured steadily

- Status: Accepted
- Date: 2026-11-04

## Context

The PRD holds an agent under 40 MB of RSS at idle, and a control plane with its own agent under 250 MB. [ADR 0007](0007-one-binary-two-roles.md) puts both roles in one binary, so every control-plane feature ships in the agent too. Issue #61 tracked an agent's idle RSS growing with each one, though the agent never runs that code: from 20.8 MB at v0.1.0 to 23.9 MB at 50acc68. Its first guess was that package initialization and globals cost the memory. Issue #57 found the footprint comment on pull requests reporting noise: +1.1 MB on a change to license headers.

Measured on an idle agent at 50acc68, built as releases are, from its own copy of the binary, 30 s after it started:

| Agent, idle 30 s | RSS | Allocated (anonymous) | Pages of the binary |
| --- | ---: | ---: | ---: |
| v0.1.0 | 21.4 MB | 5.7 MB | 15.7 MB |
| 50acc68 | 24.1 MB | 6.2 MB | 17.8 MB |
| 50acc68, built with only the agent role linked in | 12.8 MB | 3.5 MB | 9.3 MB |

- **Most of RSS is pages of the binary, mapped by package initialization.** Right after initialization, before `main` runs, 14.5 MB of the binary is resident, and 7.2 MB of the agent-only build. Go runs the initialization of every package linked in, and 180 of them do some. On each page fault the kernel maps up to 64 KB of neighboring pages that are already in the page cache, so each touch brings in the code around it. About 75% of the code ends up resident, though an idle agent runs about 3 MB of it again. Code the agent never calls is resident in full, such as `go-webauthn`, CBOR, TPM and `encoding/xml`.
- **The agent-only build is the lower bound for this code: 12.8 MB.** It's a scratch `main` that calls `agent.Run` and nothing else, so it's a second binary.
- **The release flags don't help.** `-s -w` strips the symbol table and DWARF, which sit outside the loaded segments. The three `LOAD` segments, and the pages resident, are the same with or without them. `-trimpath` and `-buildid=` change a few strings, not the layout.
- **The heap is small.** Initialization allocates 1.3 MB in all, and 0.7 MB of it stays live: `connectrpc.com/grpcreflect` 215 kB, `protodesc` 209 kB, and `internal/control/authapi`'s token table 145 kB. The agent's live heap is 1.6 MB, and the collector lets the heap reach 4 MB before it collects either way, so none of it shows in idle RSS. The agent-only build's lower anonymous memory comes from all that initialization together, not from one package.
- **Memory keeps climbing after 30 s.** Sampled every 5 to 15 s for up to 30 minutes, on 4 CPUs as on a CI runner, an idle agent at 50acc68 read 24.3 to 24.5 MB at 30 s, 26.1 to 26.2 MB at 3 minutes, and 26.5 to 27.3 MB from 6 to 30 minutes. A control plane with its own agent read 32.0 to 32.7 MB at 30 s, 33.0 to 33.3 MB at 3 minutes, and 33.9 to 34.6 MB from 10 to 30 minutes. The steepest part is a step of about 0.7 MB when the collector runs its first forced cycle, 2 minutes after its last cycle, between 120 and 150 s after starting. A sample at 30 s comes before it, and reads an agent about 2 MB low.
- **One run is noisy, and two runners more so.** Within one build, 5 runs side by side on one machine spread by up to 0.65 MB for an agent and 0.60 MB for a control plane. Figures from two CI runners differed by 1.1 MB on a change to license headers (#57). The medians of 5 runs of two identical builds, measured side by side and sampled after 180 s, differed by up to 0.26 MB for an agent and 0.39 MB for a control plane: 10 comparisons on one machine, with standard deviations of 0.13 MB and 0.20 MB.

#61 first tried releasing the pages startup maps. Once the agent had connected, it unmapped the binary's read-only mappings with `madvise(MADV_DONTNEED)` and collected once. What the agent touched again came back, about 9 MB, and an idle agent read 15.1 MB at 30 s. We don't do it:

- **It crashes position-independent builds.** With `CGO_ENABLED=0 -buildmode=pie`, which Debian, Fedora, Arch and Alpine packaging set through `GOFLAGS`, the RELRO segment is a read-only private mapping whose pages hold relocated pointers. `MADV_DONTNEED` drops them, the next touch maps the unrelocated bytes from the file, and the next garbage collection segfaults.
- **It did nothing on btrfs and overlayfs, and said so only at debug level.** It found the binary's mappings by the device and inode that `stat(/proc/self/exe)` returns, which don't match `/proc/self/maps` there. That's Fedora's and openSUSE's default root, and Docker.
- **Its benefit is mostly accounting.** The pages aren't freed: they move from the process to the page cache. A cgroup's memory, as systemd's `MemoryCurrent` shows it, counts the page cache too, so it falls only when the kernel reclaims them. Clean pages of a file are already what the kernel reclaims first under pressure, mapped or not.

It would meet the budget largely by changing what RSS counts, through code that breaks with how the binary is built and where it's stored.

## Decision

- **The budgets stay in RSS, as the PRD states them, and Dockwell doesn't change what RSS counts.** RSS is what `top` and `ps` show, and what someone comparing Dockwell with other tools reads. The budget check reports two figures beside it, so a change shows which kind of memory it moved:
  - PSS splits pages shared with other processes among them. An agent runs alone on its server, so its PSS is its RSS.
  - Anonymous memory is what the process allocated, which only swap can take back. The rest of RSS is pages of the binary.
- **The figures are measured steadily** (`scripts/budgets.sh`, #57):
  - each is the median of 5 runs, and each process runs from its own copy of the binary, as on a server of its own;
  - runs start 3 s apart, so that none contend for the CPU while starting;
  - each process is sampled after 180 s, past the collector's first forced cycle, the 5 samples 1 s apart;
  - on a pull request, main's build is measured beside it, in alternating runs on the same runner. The footprint comment compares those medians, and posts when one moves by 1.0 MB or more: five standard deviations of the noisier figure, and over twice the largest difference between identical builds. Only the pull request's runs can fail the check. When main's fail, there's no comparison, and the check says so.
- **The check warns when an agent passes 30 MB**, with a GitHub Actions warning and a line in the job summary, well before it fails at 40 MB.
- **Setup that costs only heap stays at init.** `authapi` builds its token table at init, in every process: 199 kB of allocations and up to 0.5 ms. Building it on first use instead, with `sync.OnceValue`, saved those, but moved neither idle figure. In 3 comparisons beside the same build without it, an agent differed by −0.07 to −0.02 MB and a control plane by +0.17 to +0.32 MB, each within the noise between identical builds. So the table stays a package-level variable. Third-party packages that initialize for the control plane stay as they are too: `grpcreflect`, `protodesc`, `go-webauthn` and its CBOR and TPM decoders, `pgx`, and SQLite. Go runs the initialization of every package linked in, and gives no way to defer it.

## Consequences

- **At 2209b81, an idle agent reads 26.4 MB, and a control plane with its own agent 34.3 MB:** the medians of 5 runs after 180 s, on 4 CPUs. That's 13.6 MB under the agent's budget, and 3.6 MB under its warning. Sampling once at 30 s, the check read the same build at 24.3 to 24.5 MB and 33.1 to 33.7 MB. The new figures are higher, and closer to what a server shows once it has been idle a while.
- An agent's idle RSS doesn't go back under 21 MB, which #61 asked for. Most of it is pages that package initialization maps, and Go initializes every package linked into the one binary. Each control-plane feature whose packages do work at init will cost an agent some more.
- `make budgets` takes about 7 minutes, and the Memory budgets job about as long.
- What would lower idle RSS later:
  - fewer packages that do work at init. Each one maps its code, and the kernel maps the code around it, so the saving is in pages, not heap: deferring heap alone, as the token table showed, moves nothing;
  - changes upstream, such as `grpcreflect` building its descriptor set when it's first asked for, rather than at init;
  - an agent-only binary, which idles near 12.8 MB. ADR 0007 rules that out, for one artifact to build, sign and update.
