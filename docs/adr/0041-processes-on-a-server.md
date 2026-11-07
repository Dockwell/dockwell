# 0041. Processes on a server, and ending one

- Status: Accepted
- Date: 2026-11-07

## Context

#209 asks a server's page for its processes, as Server-Processes.dc.html has them: what runs, sorted and searchable, with the container each belongs to, its user, CPU and memory, and a way to stop one. 1Panel has the same, and its owner uses it.

[ADR 0040](0040-a-servers-live-use.md) decided that what a server's page shows costs nothing while nobody looks. [ADR 0032](0032-files-on-a-server.md) and #179 decided that what can let someone back onto a server takes a passkey just now. Ending a process is as much as root: it can stop sshd, a firewall, or the database every app uses.

## Decision

### Reading them, only while someone watches

`ProcessService.WatchProcesses` streams a server's processes while it's open. The control plane asks the agent for a list every 2 s, and the first a second after the one before it, as CPU needs two to measure. The agent reads `/proc` then and only then: each process's `stat`, `status`, `cmdline` and `cgroup`. It keeps each process's CPU ticks to measure the next list against, and forgets them 30 s after the last, so with nobody watching it reads nothing and holds nothing.

- **What a process belongs to** comes from its cgroup: a Docker container's ID, or a systemd unit such as `docker.service`. The agent names containers from Docker's list, read at most every 10 s while someone watches, with their Compose labels, which tell the panel which app a container runs, as Dockwell's project is its environment's ID.
- **Its user** is the name its container knows it by, from the container's own `/etc/passwd`, or the server's, or its ID. The container's file is opened with `openat2` and `RESOLVE_IN_ROOT` inside the process's root, so a link the container holds can't lead to a file of the server's. Without `openat2`, before Linux 5.6, the ID is all it says.
- **The kernel's own threads aren't listed,** and a list holds at most 1,000 processes, busiest first, counting the rest.
- **It needs a signed-in person.** A command line can hold a password typed with it, so no API token can call either method, as with files.

### Ending one

`ProcessService.EndProcess` sends SIGTERM, and SIGKILL 10 s later if the process is still there.

- **It takes a person and a passkey just now** (`STEP_UP_ACTION_END_PROCESS`), for that process on that server: the proof's target is `<server ID>:<PID>`. A proof ends one process. A refusal that ended nothing, as for a process that has gone, gives the proof back.
- **It names the process by its PID and when it started** (`start_ticks`, from `/proc/<pid>/stat`), so a process that ended and gave its PID to another isn't ended in its place. The agent opens a pidfd before it checks the start, and signals through it, so the process it checked is the one it signals.
- **What Dockwell needs is refused:** the process the agent runs in, which on the control server is the control plane's too, and PID 1. The panel marks them Protected and says what to do instead.
- **It goes on Activity,** as `process.end`, with what the process was: its name, command, user and container, and whether it took SIGKILL. A refusal is `process.end_failed`, and a passkey asked for that didn't come within its minute is `process.end_unconfirmed`, as #179 has it for saves and applies.
- **It isn't a revision.** A process isn't managed state: nothing Dockwell keeps says it should run, and stopping it changes no spec, so there's nothing to plan or undo. A process of an app starts again, as its container's restart policy has it; stopping the app is a change, on the app's page.

## Consequences

- With nobody on the Processes tab, the agent does nothing for it, and holds nothing.
- Each watcher costs the agent a read of `/proc` every 2 s. Two people watching one server read it twice as often, and each measures CPU since whichever read came last, which is still since a read.
- A process a container runs is named by the container's own users, which `docker top` doesn't do.
- Ending a process takes a passkey, so a script can't, by design; a script that needs to stop something runs as a revision, or over SSH.
- The Services view and panel list what systemd units run processes, from their cgroups. Restarting a unit, and units with no process, need systemd's API, which the agent doesn't speak yet.
