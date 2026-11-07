# 0037. A terminal on a server, and in a container

- Status: Accepted
- Date: 2026-11-07

## Context

Issue #208 asks for a terminal on a server's page, and a shell in an app's container, from the panel and on a phone. A terminal is how people do whatever a panel doesn't, and 1Panel has one. The mocks are `Server-Terminal` and `M-Terminal`.

The agent runs as root, so a server's terminal is a root shell. That makes it as good as root, which earlier records have rules for:

- [ADR 0032](0032-files-on-a-server.md) and #179 decided that what can let someone back onto a server takes a passkey just now, so a stolen session can't keep a way in after it's revoked. A shell can write `authorized_keys` in one line.
- [ADR 0022](0022-secrets-encrypted-at-rest.md) spends a step-up's proof on one action, and [ADR 0016](0016-scoped-api-tokens.md) keeps tokens off what needs a person.
- [ADR 0003](0003-one-schema-protobuf-connect.md) said the terminal would be the one bidirectional exception to the schema, over a WebSocket, and [ADR 0015](0015-rest-and-openapi-from-the-schema.md) repeated it. Since then the rule that the panel uses only the public API, through the generated client, has held for every screen.
- [ADR 0025](0025-logs-per-deploy.md) put each logs call on a connection of its own, so nothing slow shares the agent's main stream.
- [ADR 0018](0018-keep-the-budgets-in-rss.md) holds the agent under 40 MB at idle.

## Decision

### The PTY runs in the agent

- **A server's shell is root's login shell,** from `/etc/passwd`, or `/bin/bash`, then `/bin/sh`, started as a login shell in root's home. The agent opens the pseudo-terminal itself, from `/dev/ptmx` with `golang.org/x/sys/unix`, which the agent build links already, so it adds no module. The shell leads a session of its own, with the terminal as its controlling terminal.
- **Its environment is its own,** not the agent's: `TERM=xterm-256color`, `HOME`, `USER`, `LOGNAME`, `SHELL`, a default `PATH` and `LANG=C.UTF-8`, and the login shell's profile adds the rest. The agent's own environment holds `GOMEMLIMIT`, which would bind every Go program run in the shell, and could hold a proxy's password.
- **The shell is the agent's child,** so it's in the agent's service and goes with it. Restarting Dockwell ends every terminal on that server, and running `systemctl restart dockwell` in a terminal ends that terminal. Closing a terminal hangs up its shell, as SSH does, so what was started with `nohup` or in `tmux` carries on.
- **A container's shell is `docker exec` with a terminal,** through the agent's Docker client: the first of `/bin/bash`, `/bin/ash` and `/bin/sh` that the container has, found with Docker's archive stat, which reads the container's files without running anything in it. A container with none of them, such as a distroless image, says so. It runs as the container's own user, as `docker exec` does.

### How the bytes go

**Between the browser and the control plane, a server stream and calls, not a WebSocket.** `TerminalService` is in the schema, like every other service:

| Method | Effect | What it does |
| --- | --- | --- |
| `OpenTerminal` | destructive | Starts a shell, after a step-up where one is needed |
| `ListTerminals` | read | Your terminals on a server, so a page that's loaded again finds them |
| `AttachTerminal` | read, server stream | What the terminal prints, from a byte offset, then as it comes, with a heartbeat every 10 s |
| `WriteTerminal` | destructive | What's typed, at a byte offset, and the terminal's size |
| `CloseTerminal` | destructive | Hangs up the shell |

This supersedes what ADR 0003 and ADR 0015 said of the terminal:

- Connect's browser client can't open a bidirectional stream, as `fetch` has no full duplex. A WebSocket would be a second API beside the schema, with no generated client, no OpenAPI and no CLI, and outside the interceptor that refuses tokens, ties a stream to its session and ends it when the session is revoked. It would need its own checks of origin and session, kept in step with the interceptor's by hand.
- Typing feels the same. What's typed reaches the server in half a round trip, and the echo comes back on the stream in the other half, as over a socket. The panel sends one write at a time and gathers what's typed meanwhile into the next, so a fast typist costs one request per round trip, not one per key.
- Each write carries the offset of its first byte in all that's been typed, and the agent takes each byte once. So a write retried after a dropped call, or sent again after the panel reattaches, types nothing twice. A write says how much the shell has taken in all, and one that didn't fit is taken in part, so the panel sends the rest.

**Between the control plane and the agent, a connection of its own for each attachment.** Opening and closing are calls on the agent's main stream ([ADR 0017](0017-the-change-api-and-the-agent-stream.md)). Each attachment is a call there too, whose bytes go over `AgentService.Terminal`, a bidirectional stream the agent opens on a connection of its own, named by a random token and checked against the server's certificate, as `SendLogs` is. So a terminal printing as fast as it can shares nothing with stats, plans and applies but the wire, and one terminal's reader can't slow another's.

**Flow control is the backpressure, end to end.** A browser that reads slowly holds the control plane's send, which holds the agent's connection, and the agent stops reading the shell's output once 256 KiB of it hasn't gone, so the shell waits to write, as it would over SSH. Nothing is dropped while someone is attached.

### How long a terminal lasts

- **It lives in the agent, apart from any stream.** A browser that loses its connection, an agent stream that drops, and an agent moving to a renewed certificate leave the shell running. The agent keeps the newest 256 KiB of what it printed, numbered by byte, so the panel reattaches from the last byte it had and misses nothing that fits. Past that it says how much was dropped. On each attachment the agent tells the program in front to redraw, as a resize would, so a full-screen program such as `htop` comes back whole.
- **One attachment at a time.** Attaching from a second tab moves the terminal there, and the first says where it went and offers to bring it back, rather than taking it back by itself.
- **It belongs to the person and the sign-in session that opened it.** Nobody else can list, attach, type into or close it. Signing out, revoking that session, or the session reaching its limits ends its terminals, as each attachment is a stream the interceptor ties to its session, and the control plane checks every terminal's session as streams do, every 30 s, attached or not. That check isn't use of the session, as a stream's is: a terminal nobody is attached to keeps no session past its idle limit, and leaves its last-seen time and address as they were.
- **It ends** when its shell exits; when you close it; when nobody has been attached for 10 minutes, which is long enough for a network to change or a laptop to wake; when nothing has been typed for an hour, so a root shell isn't left open on a screen nobody watches; and when its server is removed, which the control plane notices within 30 s of the server leaving the fleet, while the agent, which nobody can reach then, hangs it up once nobody has been attached for 10 minutes. A control plane that restarts loses which terminals it had, as that lives in memory, so it closes any its agents still have when they next connect. On the control server the agent goes down with it anyway.
- **Limits.** 8 terminals on a server and 32 on the control plane, so a reader of each fits the agent's budget, and one session holds half of each, 4 on a server and 16 in all, so one session, a stolen one included, can't take every place there is. A write carries 64 KiB at most, and the agent holds 256 KiB of what's typed that the shell hasn't read yet, what it's writing to the shell included. A terminal is between 2 × 1 and 500 × 200 characters.
- **Memory.** Output is kept in a buffer that grows as it fills, to 256 KiB, so 8 terminals' output costs the agent 2 MiB at most, besides a read buffer each and a connection each while someone is attached. `make budgets` measures an agent with none open, as at idle; the PR that brought terminals measured one with 8 open and 4 attached.

### Who may open one, and what it asks

- **A person only.** Every method is `needs_session`, so no token reaches them, read-only or read-write, and the handlers refuse a token's principal too.
- **A server's terminal takes a passkey each time one opens,** with ADR 0022's step-up, for that server (`terminal.open`). It's the rule of #179: what can let someone back onto a server takes a passkey just now. A proof opens one terminal. Reattaching, typing, resizing and closing ask nothing more, as they're that terminal in that session. It doesn't stand for ten minutes as a file's save does: a terminal opens once and lasts, where a file is saved again and again. With no passkey for that address, the panel points to SSH.
- **A container's shell takes a session, and a passkey where the container is as good as root.** `docker exec` is root in the container. Where the container reaches no further than itself, that's no more than a plan gives a session, as it can deploy any image. Where it's privileged, holds Docker's socket, shares the server's network or mounts a system path, with the same `AsGoodAsRoot` a plan goes by, it's a root shell on the server, and asks as one, for that container.
- **The agent decides whether a container is as good as root,** on the container it runs the shell in, by its ID, as it decides what lets someone back in for files ([ADR 0032](0032-files-on-a-server.md)): it reads the container as a service would run it, and counts it as root where what that reaches is as good as root, or where it runs with anything a service's spec can't hold, as what that does can't be told. It reads what the container mounts too, by the rules a plan goes by, two ways, and counts it as root if either says so. As Docker has it: each server path it binds counts as where it leads through symlinks (`Access.Leading`), so a link to `/etc` or Docker's socket is `/etc` or the socket; each volume counts as the server path it binds (`BindDevice`), followed the same way; and a volume counts as root on a driver other than `local`, or where it mounts what a plan refuses, such as a disk, an overlay or `proc` (the one list, `container.LocalMountType`, which a plan's `validateMount` goes by too). And as the kernel has it, as ADR 0032 judges a file by what it is: each mount in the container's own `/proc/<pid>/mountinfo` is what was mounted as it started, whatever a link says since, so its device and root, found in the agent's own list, give the server path really mounted, and its type gives a filesystem of the kernel's own, such as `proc` or `sysfs`. A volume that's the folder Docker keeps it in reaches nothing more. What can't be read, or can't be found on the server, counts as root, but for a tmpfs or a remote share. It refuses with `needs_step_up` unless the control plane says a passkey confirmed it, so the check and the shell are one container.
- **A container is named exactly,** by its whole name or full ID, never the start of either, as ADR 0021 has it, and what goes on record is the container the agent found: its name and full ID. Closing a shell signals the process Docker says is the exec's through a pidfd, taken before the agent asks Docker again that it's still the exec's, so a PID that went to another process isn't signalled.
- **The agent says in its hello that it has terminals** (`terminals`), and shells in containers (`container_terminals`). The control plane opens none on an agent that doesn't, and says to update it: an agent that knew only the first would take a call for a container as one for the server, and open a root shell with no passkey.

### On record, and nothing more

- `terminal.open` as it opens, naming the person, the server, the container if there is one, and the terminal, before the shell starts. If it can't be written, no shell starts.
- `terminal.close` as it ends, with how long it was open and why it ended: the shell exited, with its code, you closed it, nobody was attached, nothing was typed, the session ended, or the agent restarted. An agent says how a terminal ended on whichever stream is up, as it says how a save ended, so a terminal that ends while its server is away goes on record when it's back.
- `terminal.open_unconfirmed` for a terminal that asked for a passkey with none following within a proof's minute, as a warning, as #179 has it for saves and applies.
- All three are on Activity and on the server's timeline.
- **What's typed and what's printed is never recorded.** It isn't logged, stored or kept past the terminal: the 256 KiB lives in the agent's memory and goes with the terminal. Recording a root shell would put every password typed into it in the database.

### What the mock has that this leaves out

The mock's side column shares a session with a teammate, who can watch and type if allowed. Dockwell has one person per install so far, so there's nobody to share with, and the column's saved commands come in a change of their own. Each takes its own care: someone watching a root shell sees what it prints.

## Consequences

- The terminal works on every server and in every container, through the agent, with no SSH port and no new listener, over the same API, interceptor and tunnels as the rest of the panel.
- Each batch of keys is a request. On HTTP/1.1, with no TLS in front, a browser keeps six connections to a host, and the panel's streams hold some of them, so the panel attaches only the terminal on screen.
- A terminal outlives a dropped connection, which is the point, and that means a shell can run for up to 10 minutes with nobody watching. It ends with Dockwell, and with the session that opened it.
- What a shell prints isn't kept, so nothing can be replayed later from the audit log: Activity says who had a root shell, where and when, and not what they did with it. A shell is root, so what it does can't be told apart from what the person does over SSH.
- ADR 0003's and ADR 0015's lines on a WebSocket for the terminal point here.
