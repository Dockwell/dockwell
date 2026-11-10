# 0067. A server's system services and its system log, read

- Status: Proposed
- Date: 2026-11-10

## Context

The PRD's P1 row "Processes, systemd services, package updates, system logs" has processes ([ADR 0041](0041-processes-on-a-server.md)) and package updates ([ADR 0043](0043-package-updates-on-a-server.md)). #266 lists the rest as a gap: nothing in the panel says whether `nginx.service` a person installed by hand failed at boot, or what the server logged when the disk filled. Today that's SSH, `systemctl --failed` and `journalctl`. 1Panel and every server panel show both.

What bears on it:

- **The agent stays within 40 MB at idle,** links only what it runs ([ADR 0023](0023-a-full-build-and-an-agent-build.md)), and keeps nothing while nobody looks ([ADR 0040](0040-a-servers-live-use.md)).
- **Container logs already stream, bounded,** through `LogService` ([ADR 0025](0025-logs-per-deploy.md)): one path from the agent to the panel and the CLI, with a budget for each line, each stream and each server, and the agent alone dropping, which `make logs-load` holds it to.
- **The agent runs a server's own tools** as ADR 0043 does for the package manager: by absolute path, without a shell, in a clean environment, with a deadline.
- **Below an admin, a server is its name and state** ([ADR 0049](0049-roles-and-invites-by-project.md)). A system log holds what every program on the server writes: `sudo`'s command lines, SSH logins, other tenants' services.

## Decision

Reading only, in this record. Starting, stopping, restarting and enabling a unit change the server, and come later as flagged edits of [ADR 0064](https://github.com/Dockwell/dockwell/pull/462)'s `server.host` resource, with a plan and a passkey.

### The services

- **`ServerService.ListSystemServices`** lists a server's services: each unit's name, description, state (running, exited, failed, inactive, activating), whether it starts at boot, since when it's been in its state, and for a running one its main PID and memory. The panel calls them system services, as a service is an app's in Dockwell.
- **Read on asking, kept nowhere.** The agent runs `systemctl list-units --type=service --all --output=json` and `systemctl list-unit-files --type=service --output=json` once for each list, and `systemctl show` for the facts of the units listed, through ADR 0043's runner with a 10 s deadline, reading at most 4 MiB of output. The panel asks again every 10 s while the tab is open. A systemd older than 246, which has no JSON output (RHEL 8's 239), is read from `--plain --no-legend`.
- **OpenRC** (Alpine) is read from `rc-status --all --nocolor`: a service's name, runlevel and state, with no description, memory or time. A server with neither says so.
- **What Dockwell needs is marked:** the agent's own unit, `docker`, `containerd`, the SSH server, and the firewall's boot unit (ADR 0052). They're listed like the rest, with a word that Dockwell runs on them.
- **A unit is named only from what was listed.** A name the agent is asked about must match `^[A-Za-z0-9:_.@-]+\.service$` and be in its last list, and goes to `systemctl` as one argument after `--`.

### The system log

- **The journal is one more source of `LogService`,** beside a service's containers: `LogSource` gains a journal of a server, narrowed to a unit and a priority. So `WatchLogs` follows it, `ReadLogs` searches it, and the panel and `dockwell logs` read it as they read an app's, with ADR 0025's budgets: a line cut at 16 KiB, 2 MiB/s for a stream, the server's 4 MiB/s shared with its containers' followers, the agent alone dropping, and a marker saying so.
- **The agent reads it with `journalctl -o json`,** with `--output-fields` for the message, priority, unit or syslog identifier, PID and time, and nothing else (never `_CMDLINE`, which can hold a password typed with it). Following is one `journalctl -f` a server, while anyone follows it, shared between its followers as ADR 0025 shares a container's; the last follower leaving stops it. Reading back is `-n` and `--since`, the newest 1,000,000 lines and 30 s at most, as a search of a container's logs is. A message the journal keeps as bytes, not text, reads "a binary message of 312 bytes".
- **Levels are the journal's own,** from its priority, not inferred: emergency to error read as errors, warning as warnings, the rest as info and debug.
- **Without a journal,** as on Alpine with busybox's `syslogd`, the system log is `/var/log/messages`, followed as a file, with levels inferred as ADR 0025 does for a container. A server with neither says so.

### Who may read them

An admin, signed in: `ListSystemServices` and the journal source are `EFFECT_READ`, `NEEDS_ADMIN`, `REACH_INSTALL` and `needs_session`, so no API token reads a system log, as no token lists processes (ADR 0041). A project role never sees the tab. Reading goes on no record, as reading an app's logs doesn't.

### The panel

- **A server's page gains System services,** beside Processes: failed units first, then running ones, with **Running · Failed · All** and a search, so the hundreds of units a server has stay one step away. A failed unit says when and with what status. A row opens a sheet with the unit's facts and its last 100 lines of the journal, following, and **Open in the system log**. Until restarting is a plan, the sheet shows the command that does it, to copy: `sudo systemctl restart nginx.service`.
- **And System log,** the journal in the log viewer the apps use: a unit picker, **Errors · Warnings and up · Everything**, and search.
- **A failed unit can alert** later: a rule condition for it is a decision of its own, once the list exists.

### What it costs

- **Nothing while nobody looks:** no process, no buffer, no state.
- **A list** is two or three short `systemctl` runs, tens of milliseconds and a few hundred kilobytes of JSON for a few hundred units, every 10 s while the tab is open.
- **A follow** is one `journalctl` process for the server, a few megabytes of its own, and ADR 0025's buffers on the agent. `make logs-load` gains a journal that floods, through a fake `journalctl` on its fake engine, and holds the agent to the same 40 MB.
- **The agent build** links no D-Bus or `sd-journal` library: both are read through the tools every systemd server has. `internal/agent/system`, the reader, is listed in `build_test.go` with what `make budgets` says it costs.

### Numbers

The agent protocol takes this area's block: `Call.system_services` and its result 235, and the journal source in `LogSource` and the agent's follow and read 236.

## Consequences

- "Did something fail at boot" and "what did the server log at 03:12" are answered in the panel, for systemd and OpenRC servers alike.
- One log viewer, one set of budgets and one load test cover containers and the journal.
- An admin reads every program's log on the server; a project role reads none of it.
- Acting on a unit waits for ADR 0064's host resource, so this slice can't stop the SSH server by mistake.
- Slices: (1) system services, listed and read; (2) the system log, followed and searched; (3) acting on a unit, as a plan, after ADR 0064.
