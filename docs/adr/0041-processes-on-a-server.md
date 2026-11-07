# 0041. Processes on a server, and ending one

- Status: Accepted
- Date: 2026-11-07

## Context

#209 asks a server's page for its processes, as Server-Processes.dc.html has them: what runs, sorted and searchable, with the container each belongs to, its user, CPU and memory, and a way to stop one. 1Panel has the same, and its owner uses it.

[ADR 0040](0040-a-servers-live-use.md) decided that what a server's page shows costs nothing while nobody looks. [ADR 0032](0032-files-on-a-server.md) and #179 decided that what can let someone back onto a server takes a passkey just now. Ending a process is as much as root: it can stop sshd, a firewall, or the database every app uses.

## Decision

### Reading them, only while someone watches

`ProcessService.WatchProcesses` streams a server's processes while it's open. The control plane asks the agent for a list every 2 s, and the first a second after the one before it, as CPU needs two to measure. Every watch of a server shares that one read, and a session may hold 8 watches open. The agent reads `/proc` then and only then: each process's `stat`, `status`, `cgroup` and the first 1 KB of its `cmdline`, which can run to megabytes. It keeps each process's CPU ticks to measure the next list against, and forgets them 30 s after the last, so with nobody watching it reads nothing and holds nothing.

- **What a process belongs to** comes from its cgroup: a Docker container's ID, or a systemd unit such as `docker.service`. The agent names containers from Docker's list, read at most every 10 s while someone watches, with their Compose labels, which tell the panel which app a container runs, as Dockwell's project is its environment's ID.
- **Its user** is the name its container knows it by, from the container's own `/etc/passwd`, or the server's, or its ID. Whoever runs the container writes that file, so it's opened with `openat2` and `RESOLVE_IN_ROOT` inside the process's root, so a link it holds can't lead to a file of the server's; it's read only if it's a regular file, as a FIFO would hold the agent up and a device node would be opened by root on the server, and without waiting; only its first 256 KB are read; and a name is at most 32 bytes of what prints. No lock is held while it's read. Without `openat2`, before Linux 5.6, the ID is all it says.
- **A container's restart policy** comes from Docker, asked once a container, and whether a process is its main one from whether its container's shim started it. Docker starts only that process again, and only for `always` and `unless-stopped`, so the panel says it starts again only then: not for `no`, which Compose gives a service with no `restart:`, nor for `on-failure`, which a process that ends cleanly on SIGTERM leaves stopped.
- **The kernel's own threads aren't listed,** and a list holds at most 1,000 processes, busiest first, counting the rest.
- **It needs a signed-in person.** A command line can hold a password typed with it, so no API token can call either method, as with files.

### Ending one

`ProcessService.EndProcess` sends SIGTERM, and SIGKILL 10 s later if the process is still there.

- **It takes a person and a passkey just now** (`STEP_UP_ACTION_END_PROCESS`), for that process on that server: the proof's target is `<server ID>:<PID>:<start ticks>`, so a proof given back after a process went can't end another that has its PID now. A proof ends one process. A refusal that ended nothing, as for a process that has gone, gives the proof back.
- **It names the process by its PID and when it started** (`start_ticks`, from `/proc/<pid>/stat`), so a process that ended and gave its PID to another isn't ended in its place. A PID past 4194304, Linux's highest, which a signal would read as a negative PID and so every process, is refused before it reaches the agent. The agent opens a pidfd, then `/proc/<pid>` once, checks the start through that handle, reads the container's users through it, and signals through the pidfd, so the process it checked is the one it signals: a later one given the PID would have started later.
- **It never signals by PID alone.** A thread has a PID, and `/proc/<tid>` too, but no pidfd of its own (`EINVAL`): it's refused, saying the process it's of, rather than ending that whole process. A kernel before 5.3 has no pidfds (`ENOSYS`): it's refused, with how to end the process over SSH, rather than racing whatever takes the PID.
- **What Dockwell needs is refused:** PID 1; the process the agent runs in, which on the control server is the control plane's too, every thread of it, and what it runs under, such as the shell or container that started it; the kernel's own threads; and the SSH server that takes new logins, as that's a way back onto the server, though not a login it started. The panel marks them Protected and says what to do instead. Ending Docker's daemons, `dockerd` and `containerd`, stops every container, and ending a container's shim stops its container: they can be ended, and the panel says that first.
- **It goes on Activity,** as `process.end`, with what the process was: its name, user, container and PID, and whether it took SIGKILL. Never its command line: one can hold a password typed with it, and anyone who reads the activity log reads the event, a read-only API token too. A refusal is `process.end_failed`, and a passkey asked for that didn't come within its minute is `process.end_unconfirmed`, as #179 has it for saves and applies.
- **It isn't a revision.** A process isn't managed state: nothing Dockwell keeps says it should run, and stopping it changes no spec, so there's nothing to plan or undo. An app's main process starts again, as its container's restart policy has it; stopping the app is a change, on the app's page.

## Consequences

- With nobody on the Processes tab, the agent does nothing for it, and holds nothing.
- Each server someone watches costs its agent a read of `/proc` every 2 s, however many watch it.
- A process a container runs is named by the container's own users, which `docker top` doesn't do.
- Ending a process takes a passkey, so a script can't, by design; a script that needs to stop something runs as a revision, or over SSH.
- The Services view and panel list what systemd units run processes, from their cgroups. Restarting a unit, and units with no process, need systemd's API, which the agent doesn't speak yet.
