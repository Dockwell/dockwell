# 0067. A server's system services and its system log, read

- Status: Proposed
- Date: 2026-11-10
- Amended: 2026-11-12, slice 3 decided in ADR 0082 (#567)

## Context

The PRD's P1 row "Processes, systemd services, package updates, system logs" has processes ([ADR 0041](0041-processes-on-a-server.md)) and package updates ([ADR 0043](0043-package-updates-on-a-server.md)). #266 lists the rest as a gap: nothing in the panel says whether `nginx.service` a person installed by hand failed at boot, or what the server logged when the disk filled. Today that's SSH, `systemctl --failed` and `journalctl`. 1Panel and every server panel show both.

What bears on it:

- **The agent stays within 40 MB at idle,** links only what it runs ([ADR 0023](0023-a-full-build-and-an-agent-build.md)), and keeps nothing while nobody looks ([ADR 0040](0040-a-servers-live-use.md)).
- **Container logs already stream, bounded** ([ADR 0025](0025-logs-per-deploy.md)): a budget for each line, each stream and each server, the agent alone dropping, which `make logs-load` holds it to; and each reading on record as `logs.read`, once an hour for a reader and a service.
- **The agent runs a server's own tools** by absolute path, without a shell, in a clean environment, with a deadline (ADR 0043). The agent's runner for them runs one tool at a time and is shared with the firewall, SSH pins and nftables, whose changes [ADR 0052](0052-a-servers-firewall-ssh-and-fail2ban.md) applies under a confirm timer.
- **Below an admin, a server is its name and state** ([ADR 0049](0049-roles-and-invites-by-project.md)). A system log holds what every program on the server writes: `sudo`'s command lines, SSH logins, other tenants' services.

## Decision

Reading only, in this record. Acting on a unit is slice 3, below.

Decided in the review of this record (2026-11-10): reading the system log goes on record, as reading an app's logs does. The owner can overrule it.

### Methods of their own

Method options belong to a method, so the system log can't be one more source of `LogService`, whose methods a project viewer and a token with the logs grant may call. It gets its own methods, which share `LogService`'s plumbing and the agent's budgets underneath:

| Method | What it does |
| --- | --- |
| `ServerService.WatchSystemServices` | Streams a server's services while it's open |
| `ServerService.WatchSystemLog` | Follows a server's system log, narrowed to a unit and a priority |
| `ServerService.ReadSystemLog` | Reads it back, or searches it, a page at a time |

Each is `EFFECT_READ`, `NEEDS_ADMIN` for both roles, `REACH_INSTALL` and `needs_session`, so a project role never sees the tab and no API token reads a system log, as no token lists processes (ADR 0041). `roles.txt` and the descriptor test hold that, as for every method.

**The CLI doesn't read them,** as it signs in with tokens: the same exception to PRD principle 1 that processes and files have. A person reads them in the panel; a script runs `journalctl` over SSH.

### On record

A reading of the system log is `logs.read`, with the actor, the surface and the server's system log as its subject, at most once an hour for one reader and one server, as ADR 0025 records an app's: a follow again each hour it stays open and once when it ends, never what was searched for, and a reading that can't be recorded isn't served. Listing services isn't recorded, as reading a server isn't.

No redaction: an admin has a root terminal on the server already ([ADR 0037](0037-a-terminal-on-a-server.md)), so the system log shows nothing they can't read there.

### The services

- **Streamed while someone watches, read once for all of them,** as ADR 0041 reads processes: the agent lists a server's services every 10 s while any watch is open, every watch of that server shares the read, and a session holds 8 watches at most. With nobody watching, the agent runs nothing and holds nothing.
- **What a list says:** each unit's name, description, state (running, exited, failed, inactive, activating), whether it starts at boot, since when it's been in its state, and for a running one its main PID and memory. Memory reads unknown when the unit has no memory accounting (`[not set]`, or 2^64−1). The panel calls them system services, as a service is an app's in Dockwell.
- **How it's read.** `systemctl list-units --type=service --all` and `systemctl list-unit-files --type=service`, with `--plain --no-legend` on every version: slice 1 found the first column, the unit's name, is all the lists are read for, and `--no-legend` drops the status circle on 239 as `--plain` does on newer ones, so JSON wasn't needed. An alias in `list-unit-files`, as Debian's `sshd.service` for `ssh.service`, isn't read, and a unit `show` names twice is listed once. Then `systemctl show` with only the properties shown: `Id`, `Description`, `LoadState`, `ActiveState`, `SubState`, `UnitFileState`, `MainPID`, `MemoryCurrent`, `StateChangeTimestamp`, `ExecMainStatus` and `Result`. Never `Environment*` or `Exec*` (other than `ExecMainStatus`, a number), which hold passwords; naming them also keeps a list of a few hundred units well under the runner's 4 MiB of output.
- **On a runner of its own,** not the agent's shared one, so three admins with the tab open never hold up a firewall change waiting on its confirm timer. Each run has a 10 s deadline.
- **systemd is detected by `/run/systemd/system`,** not by `systemctl`'s path: a container with systemd installed and another init would list every unit as inactive. **OpenRC** (Alpine) is read from `rc-status --all --nocolor`: a service's name, runlevel and state, with no description, memory or time. runit (Void), sysvinit (Devuan) and anything else say the server's init isn't one Dockwell reads.
- **What Dockwell needs is marked:** `dockwell.service`, `dockwell-access.service` (ADR 0052), `docker.service` and `docker.socket`, `containerd.service`, and `ssh.service`, `sshd.service` and `ssh.socket`. Ubuntu 22.10 and later start SSH from `ssh.socket`, so `ssh.service` can read inactive while SSH works: the socket is listed beside it.
- **A unit is named only as systemctl knows it.** A name the agent is asked about must match `^[A-Za-z0-9:_.@\\-]+\.(service|socket)$`, allowing the `\x2d` escapes of instance names, and is checked by asking systemctl about that one unit (`systemctl show --property=Id,LoadState -- NAME`), which must not read `not-found`, so the agent keeps no list between calls. It goes to `systemctl` after `--`, and to `journalctl` as `--unit=NAME`, never as a match.
- **Parsers are tested on real output** from Debian 12, Ubuntu 22.04 and 24.04, Rocky 8 and Alpine with OpenRC, kept as fixtures, as ADR 0043 does (slice 1 has a systemd set, an OpenRC sample and Debian's aliases; the rest come with a machine to take them from); tests use the runner's `Root` and `Off`, so they never run the machine's own tools.

### The system log

- **The journal, through `journalctl -o json --system`,** with `--output-fields` for the message, priority, unit or syslog identifier, PID, time and cursor, and nothing else (never `_CMDLINE`, which can hold a password typed with it). Users' journals aren't read. `--output-fields` needs systemd 236; journald is detected by `/run/systemd/journal`.
- **One follow a server, shared.** While anyone follows a server's system log, the agent runs one `journalctl -f` for it, unfiltered, and filters it for each follower by unit and priority, so followers with different filters share it. It's held to ADR 0025's budgets: a line cut at 16 KiB, 2 MiB/s for a stream, the server's 4 MiB/s shared with its containers' followers, the agent alone dropping, and a marker saying so. The child runs in a process group of its own, outside the shared runner, and ends with its last follower and with the agent.
- **Reading back and searching** run newest first with `--reverse`, and stop once ADR 0025's match budget is full, so a search never parses a million lines it can't keep. A page back resumes at the journal's `__CURSOR`, which JSON output always carries, with `--cursor`; a follow resumes after one with `--after-cursor`. One search at a time for a server, within ADR 0025's 16 readers.
- **Levels are the journal's own,** from its priority, not inferred: emergency to error read as errors, warning as warnings, the rest as info and debug. A message the journal keeps as bytes reads "a binary message of 312 bytes".
- **Without a journal,** as on Alpine with busybox's `syslogd`, the system log is `/var/log/messages`, followed as a file through a rename and a copytruncate alike, with levels inferred as ADR 0025 does for a container.

### The panel

- **A server's page gains System services,** beside Processes: failed units first, then running ones, with **Running · Failed · All** and a search, so the hundreds of units a server has stay one step away. A failed unit says when and with what status. A row opens a sheet with the unit's facts and its last 100 lines of the journal, following, and **Open in the system log**.
- **And System log,** the journal in the log viewer the apps use: a unit picker, **Errors · Warnings and up · Everything**, and search.
- **A failed unit can alert** later: a rule condition for it is a decision of its own, once the list exists.

### What it costs

- **Nothing while nobody looks:** no process, no buffer, no state.
- **A list** is two or three short `systemctl` runs, tens of milliseconds and a few hundred kilobytes of output for a few hundred units, every 10 s while anyone watches that server.
- **A follow** is one `journalctl` process for the server, and ADR 0025's buffers on the agent. journalctl maps journal files, so on a journal of several GB its RSS can reach tens of MB; that memory is file-backed, in its own process outside the agent, and the agent's unit sets no `MemoryMax`, so it doesn't count against the agent's 40 MB, which `make budgets` measures alone. `make logs-load` gains a journal that floods, through a fake `journalctl` on its fake engine, and holds the agent to the same 40 MB.
- **The agent build** links no D-Bus or `sd-journal` library: both are read through the tools every systemd server has. `internal/agent/system`, the reader, is listed in `build_test.go` with what `make budgets` says it costs.

### Numbers

The agent protocol takes this area's block: `Call.system_services` and its result 235, and `Call.system_log`, which follows and reads, and its result 236. Fields of the public schema are numbered its own way, from each message's next free number.

### Slice 3, acting on a unit

Decided in [ADR 0082](0082-acting-on-a-servers-system-services.md), which also builds reading a unit's journal back (`ReadSystemLog`) and leaves ADR 0067's 236 unused. What follows is what this record said before it.

- **A restart** is a change with a plan and no revision, as [ADR 0027](0027-health-checks-rollouts-and-rollback.md)'s restart of a service is (PRD principle 2). Whether it ships before ADR 0064 is accepted is the owner's call.
- **Start, stop, enable, disable and mask** are either desired state on [ADR 0064](https://github.com/Dockwell/dockwell/pull/462)'s `server.host` resource, put back when someone changes them over SSH as ADR 0054's drift would, or plans with no revision: the owner's call, in that slice's record.
- **Refused:** stopping, disabling or masking `dockwell.service`, `dockwell-access.service`, Docker or containerd.
- **SSH** goes under ADR 0052's confirm timer, with a passkey, and the plan names `ssh.socket` beside `ssh.service`, as stopping the service while the socket listens doesn't stop SSH.
- **Activity** records each action with the unit and the server. If ADR 0064's host resource changes in its review, that slice follows it.

## Consequences

- "Did something fail at boot" and "what did the server log at 03:12" are answered in the panel, for systemd and OpenRC servers alike.
- One log viewer, one set of budgets and one load test cover containers and the journal; the methods and the record are the system log's own.
- An admin reads every program's log on the server, signed in, and each reading is on record; a project role and a token read none of it.
- Acting on a unit waits for slice 3, so this slice can't stop the SSH server by mistake.
- Slices: (1) system services, streamed and read; (2) the system log, followed and searched; (3) acting on a unit, as a plan.
