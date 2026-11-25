# 0108. Terminal saved commands, display settings, working directory and multi-tab input

- Status: Accepted
- Date: 2026-11-24

## Context

[ADR 0037](0037-a-terminal-on-a-server.md) added interactive host and container shells over Connect and deferred saved commands to a change of their own because they carry their own security and operational trade-offs. Operators also asked for a `+` session picker across host and running containers, per-user display settings (font size, family, line height, cursor style/blink, scrollback), fullscreen, opening Files at the shell's working directory, and typing into several tabs at once.

## Decision

### Per-user rows in `settings`, cleaned up on member removal

Per-user terminal display preferences (`user.terminal_preferences.<userID>`) and personal saved commands (`user.terminal_commands.<userID>`) live as JSON blobs in the `settings` table, keyed by the signed-in user's ID. Install-wide shared saved commands live under `install.terminal_commands`.

- Neither preferences nor saved commands are managed server state, so they do not create revisions or plans; reads and updates run directly under the store's transaction lock.
- All seven new `TerminalService` RPCs require a signed-in admin session (`needs_session`, admin role, install reach), matching `OpenTerminal` and `ListTerminals`; API tokens are refused.
- Removing a user (`RemoveUser`) deletes `user.terminal_preferences.<userID>` and `user.terminal_commands.<userID>` in the same transaction so personal settings and commands (including any acknowledged as containing plain-text secrets) are not left behind.

### Shared saved commands: accountability, audit and control-character safety

Any admin can read, create, edit and delete shared commands, because admins already share root shell access across the fleet. Because running a shared command sends keystrokes to a root shell:

- **Author and editor tracking.** Every shared command records `created_by` and `updated_by` (the acting principal's actor string, resolved to their display name when listed) and shows who last changed it in the panel.
- **Activity log.** Creating, editing, importing or deleting a shared command writes `terminal_command.save` or `terminal_command.delete` to the audit log and Activity (`EVENT_KIND_SETTINGS`), recording the actor and command name (and previous name on edit), never the command body. Moving a shared command to personal is recorded as a delete of the shared command.
- **Safe import defaults.** Importing a JSON commands file defaults to personal scope. Landing any command in shared requires both choosing a shared import scope and confirming explicitly. The preview lists every command (scrolling rather than truncating) and tags shared ones.
- **Plain-text secret notice.** Both server and panel scan saved commands for common credential patterns (private keys, API token prefixes, JWTs, URL credentials, Authorization headers, password/secret assignments and CLI flags) and refuse to store them until the person explicitly acknowledges they are kept in plain text in Dockwell's database.
- **Control characters and multi-line commands.** Server and panel fold `\r\n` to `\n`, trim trailing newlines, and refuse all ASCII control characters below `0x20` except `\n` and `\t`, as well as DEL (`0x7f`), on save and import. Multi-line commands are displayed with their explicit line breaks. **Insert** pastes through xterm's `paste()` (wrapping in bracketed paste when the shell enabled mode 2004) and refuses multi-line insert when any target shell does not have bracketed paste enabled, so Insert never executes a line before the person presses Enter. **Run** on a multi-line command asks for confirmation first, naming the line count.

### Working directory via `TIOCGPGRP`, `/proc` and OSC 7

For a host shell, **Open Files here** asks the agent (`GetTerminalWorkingDirectory`) for the shell's working directory:

- On Linux, the agent reads the PTY's foreground process group (`TIOCGPGRP`). Before trusting that `pgrp` as a PID, it reads `/proc/<pgrp>/stat` and checks that the process still belongs to the same process group and the shell's own session (`getsid(shellPid)`), avoiding PID reuse after a pipeline leader exits. If valid, it reads `/proc/<pgrp>/cwd`; otherwise it falls back to `/proc/<shellPid>/cwd`.
- If the agent cannot read `/proc` (older agent or non-Linux), the panel falls back to the last `OSC 7` (`file://...`) working directory emitted by the host shell, then to the shell account's home directory reported by the agent, and finally to Files' own default (`/`), never hard-coding `/root`.
- For a container shell, the agent returns an empty working directory and empty home, because Files browses the server's host filesystem rather than a container's mount namespace. The panel disables **Open Files here** on container tabs and explains why.

### Typing into several tabs (broadcast) and fullscreen

- **Opt-in, single-server multi-tab input.** Typing into several tabs is off by default. Turning it on opts in only the active tab; every additional tab must be ticked explicitly, and only live tabs on the current server can be chosen. Any change to the set of live tabs (open, close, end, moved, reload merge) or switching to an un-opted tab turns it off automatically. Mirrored pastes and cursor keys are re-encoded per target using each target terminal's own bracketed-paste and application-cursor-keys modes. Saved-command Run and Insert use the exact list of ticked tabs shown in the panel's target label.
- **Fullscreen never steals Escape from the shell.** While the terminal has focus in fullscreen, bare `Escape` always reaches the shell (for `vim`, `less`, `htop`, vi mode). Leaving fullscreen uses the Exit fullscreen button, `Shift+Escape`, or bare `Escape` only when focus is outside the terminal.

## Consequences

- Shared commands have clear provenance and an audit trail on Activity without leaking command bodies or secrets into the audit log.
- Saved commands cannot hide control sequences or silently execute intermediate lines on Insert, even across multiple broadcast tabs.
- Removing a member leaves no personal terminal preferences or commands behind in `settings`.
- Reading a host shell's working directory adds one lightweight read-only RPC (`Hello` 460, `Call` 460, `CallResult` 460 in `agent.proto`) with zero new agent dependencies.
