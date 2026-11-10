# 0060. What an idle agent keeps in memory

- Status: Accepted
- Date: 2026-11-10

## Context

The PRD holds an agent under 40 MB of RSS at idle, and `scripts/budgets.sh` warns from 30 MB ([ADR 0018](0018-keep-the-budgets-in-rss.md)). [ADR 0023](0023-a-full-build-and-an-agent-build.md) gave the agent a build of its own, which idled at 18.7 MB. Since then file transfers, terminals, scheduled tasks, the firewall reader, the 1Panel reader and more of the protocol each added a little, and an idle agent reached the warning line: 30.4 MB on the machine that measured this, which has 16 CPUs, and 27.9 MB on a CI runner, which has fewer. Nobody had looked at the whole.

Measured on main at ddc49cea, as `scripts/budgets.sh` measures: joined to a control plane, with twenty tasks scheduled, from its own copy of the binary, sampled after 180 s.

| An idle agent, 30.4 MB | MB | What it is |
| --- | ---: | --- |
| Pages of the binary | 17.2 | 8.0 of 8.3 MB of code, and 8.5 of 10.8 MB of the tables Go keeps beside it. Nearly all of the binary is resident, as ADR 0018 found. |
| The Go heap, with its stacks | 9.8 | 4.4 MB is live, and 1.5 MB is stacks, for 21 goroutines and the threads. The rest is garbage waiting for the collector, and gaps in pages that hold something live. |
| The runtime's own | 2.8 | The collector's tables, the scheduler, and what it keeps for each CPU. |
| Globals | 0.6 | |

- **What's live is mostly the protocol.** Of 4.4 MB, about 2.1 MB is what the generated code registers for its messages: 0.9 MB as the program starts, and 1.2 MB the first time a message goes over the stream, when the library reads every message that one can reach. No buffer, pool or goroutine of the agent's own stands out. The HTTP/2 connection, its header tables and TLS hold about 0.1 MB between them.
- **Go lets the heap grow to twice what's live before it collects.** That is `GOGC=100`, its default. When ADR 0018 measured, 1.6 MB was live and Go's 4 MB floor decided when it collected. At 4.4 MB live, the target is 8.9 MB, so half of an idle agent's heap is garbage waiting. An idle agent allocates slowly: it collected 6 times in its first 3 minutes.
- **Each CPU costs memory.** Go keeps a cache of free memory for each CPU it may run on, and an idle agent's few goroutines wake on any of them. The same binary reads 30.5 MB with 16 CPUs, 28.4 MB with 4, 27.6 MB with 2 and 27.2 MB with 1. So a small server already reads 2 to 3 MB under the machine that measured this.
- **What a package allocates as the program starts costs more than its size.** Go's packages allocated 2.4 MB between them before `main` ran. 1.0 MB of that came from ten regular expressions with a counted repetition, such as `^[A-Za-z_][A-Za-z0-9_]{0,254}$`: Go compiles `{0,254}` to 254 copies of what it repeats. Once compiled they hold only 0.3 MB, but the garbage of compiling them lands among what lives as long as the agent does, and Go's heap doesn't move what's live, so the gaps stay. Without them an idle agent reads 0.85 MB lower.
- **Handing memory back after starting does nothing.** One `debug.FreeOSMemory` 20 s after the agent starts moved nothing: 27.8 MB against 27.7 MB, over 12 runs each. The gaps are inside pages that hold something live.
- **Memory profiling is already off.** The linker turns it off in a binary that can't read a profile, which the agent can't.

The noise matters when a change is small. Three runs of two identical builds differed by 0.6 MB, so each figure here is the median of 8 or 12 runs, side by side with main's build, against one control plane.

## Decision

- **An agent on its own collects at half of Go's target, `GOGC=50`.** Its heap grows to one and a half times what's live, not twice. The agent sets this itself as it starts to run, in either build, on a server that joined a control plane, where the agent is all the process runs. A `GOGC` in the environment stays as the operator set it. The control server's own agent shares its process with the control plane, whose heap is another matter, and is left as it is.
  - It costs a collection about every 15 s at idle in place of every 30 s, which takes about 0.01% of a CPU.
  - File transfers already ran at this target while they ran ([ADR 0045](0045-a-file-manager-on-a-server.md)), so the agent's heaviest path doesn't change. `make logs-load` and `make transfers-load` hold the agent to their limits with it.
- **A length is checked apart from a form.** Where a name has a form and a longest length, the regular expression says the form, with `*` or `+`, and the code checks the length: `len(name) <= 255 && nameRE.MatchString(name)`. Ten checks changed: a variable's name, a label (`resource.IsLabel`), an environment's name in a container's, a service's container's name, a health check's host, a host to add, an image's tag, a group, a firewalld zone and a database user's name. Each takes what its expression took, to the byte: a test beside each holds it to the expression it was, over thousands of names around each length.
- **A package of the agent's may allocate only so much as the program starts.** `cmd/dockwell/build_test.go` builds the agent build, runs it with `GODEBUG=inittrace=1`, and fails when one of Dockwell's own packages allocates more than 256 KB before `main` runs, or all of them more than 512 KB. They allocated 1,334 KB, and now about 350 KB. Generated code isn't counted: what it registers is the schema's size, not a package's choice.

- **The agent build links only the schema's files its protocol imports.** The agent's protocol imports messages from 18 of the 30 files of `proto/dockwell/v1`, and Go links the generated package whole, so an agent registered sign-in, alerts, the catalog and the rest of the API as it started. `make generate` now runs `scripts/agentschema` after buf: it follows the imports of `proto/dockwell/agent`, and gives each generated file of the same package that they don't reach the constraint `//go:build !agent || agenttests`. The full build has every file, as before.
  - **The tests of the agent build's packages run with `-tags agent,agenttests`.** Two of those packages, `internal/cli` and `internal/agent/enroll`, stand up a control plane in their tests for the agent to join, and a control plane needs the whole schema. `agenttests` puts the files back for them, and for vet and lint, in the Makefile and in CI. The agent build itself is still `-tags agent`, as [ADR 0023](0023-a-full-build-and-an-agent-build.md) has it, and nothing about how it's built or released changes.
  - Agent code that comes to need a message from a file left out says so as an undefined name when the agent build compiles. The file's header says what to do: import its `.proto` file from the agent's protocol, or name it in `scripts/agentschema`.
  - `cmd/dockwell/build_test.go` fails when the agent build links `auth.pb.go`, `alert.pb.go` or `catalog.pb.go`, or the catalog's format, which only files left out import.
  - buf's breaking-change check compares files, so a message can't move to another file to leave more out. What's left out is whole files.

### Not done

- **A cap on the CPUs the agent runs on.** With `GOMAXPROCS=4` the agent reads another 1.7 MB lower on a server of 16 CPUs, and nothing on a server of 4 or fewer. It would also hold the agent's own work to 4 CPUs on a large server: three transfers at once, a log search, a backup's sealing. That is a choice about what an agent may take of a server, not a saving, so it's left for a decision of its own.
- **A memory limit for the agent.** `GOMEMLIMIT` makes the collector work harder only near the limit, so one set near an idle agent's size does nothing at idle and makes the collector thrash under a transfer. The installed service's 200 MiB stays.
- **Unmapping the binary's pages.** ADR 0018 says why.

## Consequences

- **An idle agent reads 27.4 MB where main's reads 30.4 MB, 3.0 MB less,** as `scripts/budgets.sh` measures them side by side on a machine with 16 CPUs: the medians of 5 runs of each, which ran from 26.8 to 27.6 MB and from 29.1 to 30.5 MB. Over 12 runs of each it's 27.6 MB against 30.5 MB. Its anonymous memory is 10.1 MB against 13.1 MB, and the pages of the binary are the same. With 4 CPUs it reads 25.9 MB.
- **It holds less under load too, for the same CPU.** Over three runs of `make logs-load` with each build, with 20 readers following a container that floods its log, the agent read 32.0 to 33.0 MB against 38.6 to 39.3 MB, and with 8 searches of a million lines at once, 43.4 to 45.5 MB against 52.1 to 53.1 MB. One of main's three runs failed the check, at 41.2 MB against its limit of 40 MB. The agent's CPU with 20 followers was 31 to 46% of one CPU against 26 to 32%, and with one follower 20 to 26% against 18 to 21%: the runs overlap, and differ by more than the builds do. Four uploads and four downloads of 512 MiB took as long as before.
- A control plane with its own agent reads about 1 MB lower as well, as it compiles the same expressions.
- The saving is in what the agent allocates, so it holds on a server of any size.
- The agent is back under the warning line with room, and the line stays at 30 MB: the open pull requests add to it.
- **Leaving the schema's files out takes it to 26.9 MB, 0.6 MB lower again:** the medians of 12 runs of each, side by side, with main's at 30.6 MB. With 4 CPUs it reads 25.4 MB. The agent build's binary loads 19.5 MB where it loaded 20.1 MB, and its packages register about 230 KB less as it starts. A file added to the public API no longer costs every agent memory, unless the agent's protocol imports it.
- What remains is the binary, about 17 MB. The largest single piece that isn't the agent's work is the command line: cobra, pflag and `text/template` are about 0.35 MB of code, and ADR 0023 measured a `main` without them 2.5 MB lower.
- A new check of a name's form and length follows the rule above, or the test of what packages allocate fails, and says where to look.
