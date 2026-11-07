# Terminal: threat model

A terminal hands a browser a root shell on a server, through an agent that runs as root. This says what that protects, from whom, and what it doesn't protect. The design is in [ADR 0037](../adr/0037-a-terminal-on-a-server.md), beside the threat models for [files](files.md), [secrets](secrets.md) and [sign-in](sign-in.md). The code is in `internal/agent/terminal`, `internal/control/terminals` and `web/src/features/terminal`.

## What there is to protect

| Asset | Why it matters |
| --- | --- |
| The server | A root shell is everything: every file, every secret's value in a process's environment, Docker, and the way back in |
| The way back in | A key in `authorized_keys`, a cron job or a unit file outlives any session Dockwell can revoke |
| What's typed and printed | Passwords typed at a prompt, secrets printed by `env`, a database's rows |
| The agent's budget | Each terminal holds a shell, a buffer and, while attached, a connection on a server Dockwell keeps under 40 MB |

## Who we consider

| Who | What they have |
| --- | --- |
| An API token, read-only or read-write | A bearer secret from a CI log or a dotfile ([ADR 0016](../adr/0016-scoped-api-tokens.md)) |
| A stolen session | A person's cookie, with no passkey. It acts as the person until it is revoked ([sign-in.md](sign-in.md#known-gaps)) |
| Another session of the same person | Their phone, beside their laptop |
| A server's agent | Its own certificate, and what it says on its streams |
| A page the terminal shows | What a program on the server prints, escape codes and all |
| A signed-in person with their passkey | Root on the server. Nothing here defends against them |

## What each can and can't do

### An API token

| It tries to | What happens |
| --- | --- |
| Open, list, attach to, type into or close a terminal, with any scope | Refused with `needs_session`, by the interceptor and again by the handler, over Connect and REST. Tested |
| Begin the passkey step-up a terminal takes | Refused: no token begins one. Tested |

### A stolen session

| It tries to | What happens |
| --- | --- |
| Open a terminal | Refused with `needs_step_up`: it has no passkey. It's on Activity as `terminal.open_unconfirmed`, in a warning tone, once the minute a proof lasts has passed. Tested |
| Use a proof it watched go by for another action, another server, or a terminal already opened | Refused: a proof is for opening one terminal, on one server, in the session that confirmed it, once. Tested |
| Reach a terminal the person opened from this session | It can, as it is that session, until the session is revoked, which ends the terminal. The terminal opened with the person's passkey, so it's on Activity under their name |
| Keep a shell after the session is revoked or signed out | It can't. The terminal ends within the stream recheck (30 s), and at once on revoking or signing out, as its attachment is a stream the interceptor ties to the session. Tested |

### A shell in a container

| It tries to | What happens |
| --- | --- |
| Open a shell in a container that reaches no further than itself | It can, with a session and no passkey: that's root in the container, which a plan already gives a session, as it can deploy any image. Tested |
| Open a shell in a container that's privileged, holds Docker's socket, shares the server's network or mounts a system path | Refused with `needs_step_up` until a passkey confirms that container on that server: a proof for the server, or for another container, doesn't open it. The agent decides, on the container it runs the shell in. Tested, on the fake engine and on Docker |
| Open one in a container that runs with what no service's spec can hold, such as `--pid=host` | Counted as root, so it asks for the passkey, as what that does can't be told |
| Swap the container for a root one between the check and the shell | It can't: the agent checks the container by its ID, and runs the shell in that one |
| Open one through an agent from before shells in containers | Refused as `agent_outdated`: such an agent would read the call as one for the server's own shell. Tested |

### Another session of the same person

| It tries to | What happens |
| --- | --- |
| List, attach to, type into or close a terminal another session opened | Refused as not found: a terminal belongs to the person and the session that opened it. Tested |

### A server's agent

| It tries to | What happens |
| --- | --- |
| Carry another server's terminal | Refused: an attachment's stream is named by a random token, checked against the certificate of the server it was asked of, and it works once |
| Say a terminal on another server ended, to put a false end on record | Refused: the end goes on record only for a terminal of that server, or one whose opening the control plane recorded for that server |

### What a terminal prints

| It tries to | What happens |
| --- | --- |
| Inject markup or script into the panel | It can't: xterm.js draws what's printed as text in cells, never as HTML, and the panel's Content-Security-Policy refuses inline scripts and styles. The rules xterm.js makes for its own rows go in as constructed style sheets, and nothing printed reaches them |
| Read the clipboard | It can't: OSC 52 and the like aren't wired to the browser's clipboard. Copying takes a person choosing text, and pasting takes the platform's paste |

## What's on record

- `terminal.opening` in the audit log before a shell starts, and if it can't be written, nothing starts.
- `terminal.open`, `terminal.open_failed`, `terminal.open_unconfirmed` and `terminal.close` on Activity and the server's timeline, with who, which server, as whom it ran, when it opened and closed, and why it ended.
- Nothing typed or printed, anywhere: not the audit log, not the control plane's log, not the agent's log, not the disk. The newest 256 KiB of output lives in the agent's memory for the next attachment, and goes with the terminal. Tested against the database's own bytes in the end-to-end flow.

## What this doesn't do

- **It doesn't record a session.** Activity says who had a root shell, where and when, not what they did with it. A shell is root, so what it does can't be told from what the person does over SSH, and recording it would store every password typed into it.
- **It doesn't stop a person with their passkey.** A terminal is root on purpose.
- **A terminal outlives a dropped connection,** for up to 10 minutes with nobody attached, and an hour with nothing typed. A laptop left open on a terminal is a root shell for that long, as an SSH session would be.
- **Restarting Dockwell ends every terminal on that server,** as each shell is the agent's child, and what runs in front of it with it. What was started with `nohup` or in `tmux` carries on.
- **A program that prints faster than the reader reads waits,** as over SSH. With nobody attached, it reads on and keeps the newest, so a long command finishes while you're away.
