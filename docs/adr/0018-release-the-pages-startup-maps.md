# 0018. Release the pages startup maps, and keep the budgets in RSS

- Status: Accepted
- Date: 2026-11-04

## Context

The PRD holds an agent under 40 MB of RSS at idle, and a control plane with its own agent under 250 MB. [ADR 0007](0007-one-binary-two-roles.md) puts both roles in one binary, so every control-plane feature ships in the agent too. Issue #61 tracked an agent's idle RSS growing with each one, though the agent never runs that code: from 20.8 MB at v0.1.0 to 23.9 MB at 50acc68. Its first guess was that package initialization and globals cost the memory.

Measured on an idle agent at 50acc68, built as releases are, from its own copy of the binary:

| Agent, idle 30 s | RSS | Allocated (anonymous) | Pages of the binary |
| --- | ---: | ---: | ---: |
| v0.1.0 | 21.4 MB | 5.7 MB | 15.7 MB |
| 50acc68 | 24.1 MB | 6.2 MB | 17.8 MB |
| 50acc68, built with only the agent role linked in | 12.8 MB | 3.5 MB | 9.3 MB |
| 50acc68, after unmapping the pages startup mapped | 15.1 MB | 6.0 MB | 9.1 MB |

- **Most of RSS is pages of the binary, mapped while starting.** Right after package initialization, before `main` runs, 14.5 MB of the binary is resident, and 7.2 MB of the agent-only build. Go runs the initialization of every package linked in: 180 of them do some. Each page that touches brings its neighbors along, as the kernel maps up to 64 KB around a fault from the page cache. So 75% of the code is resident, though an idle agent runs about 3 MB of it again.
- **Those pages are clean and never touched again.** Unmapped once the agent has connected, they stay unmapped: what comes back over the following minutes is about 9 MB. That is code the agent runs, type descriptors, and the function tables the garbage collector reads.
- **Allocations matter less than they look.** Package initialization allocates 1.3 MB, and 0.7 MB of it stays live. `connectrpc.com/grpcreflect` (215 kB live) and `protodesc` (209 kB) serve the control plane's reflection API, and `internal/control/authapi` (145 kB) built its token table. Removing the last moves no RSS at idle, because the collector lets the heap reach 4 MB before its first cycle. The agent-only build's lower anonymous figure comes from the pages and heap of all that initialization together, not one package.
- **The release flags don't matter.** `-s -w` strips the symbol table and DWARF, which are outside the loaded segments, so the resident pages are the same with or without them.

## Decision

- **Once a server's own agent first connects and sends its first stats, `dockwell run` releases the pages startup mapped.** It unmaps its read-only mappings of its own binary, its code and constant data, with `madvise(MADV_DONTNEED)`, then collects garbage once, so the pages each collection reads are mapped at once rather than whenever the next cycle comes. Both roles do it: the control plane through its own agent. It's `internal/footprint`, and it does nothing outside Linux.
- **Control-plane setup in Dockwell's own packages waits for the control role.** The token table that `authapi` built at init is built with the interceptor. Third-party packages that initialize for the control plane (`grpcreflect`, `protodesc`, `go-webauthn` and its CBOR and TPM decoders, `pgx`, SQLite) stay as they are: Go runs every linked package's initialization, and the release makes their pages cost nothing after startup.
- **The budgets stay in RSS.** It's what `top` and `ps` show, and what someone comparing Dockwell with other tools reads. PSS splits pages that several processes share; an agent shares none, as it runs alone on its server, so its PSS is its RSS. The budget check reports both beside anonymous memory, which is what the process allocated and only swap could take back. The rest of RSS is pages of the binary, so a change shows which kind of memory it moved.
- **The budget check warns when an agent passes 30 MB**, with a GitHub Actions warning and a line in the job summary, well before it fails at 40 MB.
- **Each figure is the median of 5 runs, and on a pull request main's build is measured beside it, at once, on the same runner** (#57). The footprint comment compares those, rather than figures from separate runs, whose difference was as big as the changes worth reporting.

## Consequences

- An idle agent's RSS falls from 24.1 MB to about 15 MB, under v0.1.0's 21.4 MB, and a control plane with its own agent's from about 34 MB to about 24 MB. Each new control-plane package costs an agent little after startup, since only its initialization touches it.
- The pages aren't freed. They move from the process to the page cache, where the kernel reclaims them first. A cgroup's memory, as systemd's `MemoryCurrent` shows it, counts the page cache too, so it falls only when the kernel reclaims them.
- A page that is touched again maps again, from the page cache, or from disk if it was reclaimed. That costs a fault, the same as a page the kernel had reclaimed before. A reconnect or a certificate renewal maps the TLS code again, for instance.
- Releasing is safe because a read-only private mapping of a file holds no changes, so dropping its pages loses nothing. Writable mappings are left alone. An upgrade renames the new binary into place (`scripts/install.sh`), so the running process keeps its own file, and pages map again from it.
- Startup still peaks at about 24 MB on an agent before the release. The budgets measure idle memory, and the peak is under them.
- What would lower it further: an agent-only build idles near 12.8 MB, but that is a second binary, which ADR 0007 rules out. Deferring `grpcreflect`'s initialization would need a change upstream, and would save heap that idle RSS doesn't show.
