# Logs: threat model

Logs often hold what apps shouldn't have printed: tokens, passwords, personal data. This is what could go wrong in reading them, and what stops it. The design is in [ADR 0025](../adr/0025-logs-per-deploy.md); the code is in `internal/agent/logs`, `internal/control/logs`, and the panel's `web/src/features/logs`.

## What there is to protect

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| A running or stopped container's log | Docker's log files, on its server, as Docker keeps them | It may hold whatever the app printed |
| A removed container's log | `<data>/agent/kept-logs`, 0700, files 0600, on its server | The same, past the container's life |
| Lines on their way to a reader | In memory, on the agent and the control plane; in the browser, up to 100,000 lines | The same |
| Who read what | The audit log | It answers who saw a leaked secret |

Log lines never go in the control plane's store.

## Trust boundaries

- **Each server's agent** reads only its own Docker, as root, as it runs its containers. What it can read is what root on that server can.
- **The control plane** passes lines on, and doesn't keep them.
- **A person with a session** can read every service's logs, as they can do everything until roles arrive (phase 2).
- **An API token** reads logs only with the grant its maker asked for.
- **What a container prints is untrusted,** by the panel, the CLI and the agent alike.

## Threats and mitigations

| Threat | Mitigation |
| --- | --- |
| A read-only token made for a dashboard leaks, and is used to read secrets out of logs | Reading logs needs `TOKEN_GRANT_LOGS`, which a token has only if its maker asked for it. Tokens made before there were grants have none. A refusal says why, and is audited as `token.refuse`. |
| Someone reads logs and nobody can tell | `logs.read` goes in the audit log, with the actor and the surface, at most once an hour for each reader and service. A reading that can't be recorded isn't served. |
| The audit log itself leaks what someone was looking for | It names the service and never what was searched for. |
| A revoked token or an ended session keeps reading | A stream closes at once through the same registry as every other stream, and checks its token again every 30 s. |
| A cross-site page reads a reader's logs | REST's `GET` paths pass the guard's origin checks. A page can't read the answer to a request from another origin, and the API answers no CORS preflight. |
| A request names some other container on the server | A request names a service. The agent reads only containers with that service's Compose labels, so no request can read a container by name or ID. A source ID only narrows what those labels found. |
| One server's agent sends lines into a reader of another's | The upload names a random token, and the control plane checks that the certificate it came with is the server's that was asked. |
| A log line carries markup or script into the panel | Text only ever reaches text nodes. A test prints an `<img onerror>` and a `<script>`, and checks neither ran. |
| A log line carries terminal escapes into a reader's terminal, moving the cursor or setting the title | The CLI drops every escape but colour, and on anything but a terminal drops colour too. The C1 controls go as well, as some terminals read them as escapes. The panel draws no escape at all. |
| A line uses direction overrides to read as something else | The panel shows them as a replacement character. |
| A kept log outlives its usefulness on the disk | It stays 30 days at most, and each service keeps its last failed and last replaced container alone. It goes with the data directory. |
| A flood of logs takes down the server, the control plane or a browser | Every stage has a bound, listed in ADR 0025, and only the agent drops, and says how many. |
| A log fills the disk | Docker rotates each container's at 50 MiB, and what is kept stays within 256 MiB, and not on a disk with under 1 GiB free. |
| Someone with a token runs searches to spend a server's CPU | A query is plain text, never a pattern. A search reads at most 1,000,000 lines of each container and stops at 30 s. An agent serves 16 readers at once. |

## Known gaps

- **Nothing is redacted, and nothing here promises to.** Dockwell can't tell what in a line is a secret. An app that prints one has leaked it to everyone who can read its logs.
- **Docker's own log files** are readable by root on the server, as they always were. So is the archive, which isn't encrypted at rest.
- **Roles.** Until members arrive, whoever has a session can read every service's logs. Roles will narrow this, as they narrow everything else.
- **A session has no grant to withhold.** A person signed in can read logs, so a stolen session can too, for as long as it lasts.
