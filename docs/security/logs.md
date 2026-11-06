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
| Someone reads logs and nobody can tell | `logs.read` goes in the audit log, with the actor and the surface, at most once an hour for each reader and service: again each hour a follow stays open, and when one ends. A reading that can't be recorded isn't served, and a follow that can't go on record again ends. |
| The audit log itself leaks what someone was looking for | It names the service and never what was searched for. |
| A revoked token or an ended session keeps reading | A stream closes at once through the same registry as every other stream, and checks its token again every 30 s. |
| A cross-site page reads a reader's logs | REST's `GET` paths pass the guard's origin checks. A page can't read the answer to a request from another origin, and the API answers no CORS preflight. |
| A request names some other container on the server | A request names a service. The agent reads only containers with that service's Compose labels, so no request can read a container by name or ID. A source ID only narrows what those labels found. |
| One server's agent sends lines into a reader of another's | The upload names a random token, and the control plane checks that the certificate it came with is the server's that was asked. |
| A log line carries markup or script into the panel | Text only ever reaches text nodes. A test prints an `<img onerror>` and a `<script>`, and checks neither ran. |
| A log line carries terminal escapes into a reader's terminal, moving the cursor, setting the title, changing what the keyboard sends, or hiding what comes next | The CLI drops every escape but plain colour codes, of digits and semicolons alone, and of those keeps only bold, faint, italic, underline, six hues and grey: nothing that can match the ground, such as concealed text, backgrounds, reverse, black, white or the wider palettes. It closes them at each line's end and on leaving. On anything but a terminal it drops colour too. All C1 controls go as well, as some terminals read them as escapes, and bytes that aren't UTF-8. The panel draws no escape at all. |
| A line uses direction overrides to read as something else | The panel and the CLI show them as a replacement character. |
| A kept log outlives its usefulness on the disk | It stays 30 days at most, and each service keeps its last failed and last replaced container alone. It goes with the data directory. Uninstalling stops what removes it, so the script names what's left and how to delete it. |
| A deleted service's logs come back under a new service of its name, or by its ID, for someone who shouldn't see them | The control plane refuses a service that stands deleted, by any name, and works this out from revisions alone. A service made again keeps its ID, so every call carries the last revision a deletion put out of reach, and a server leaves out what it kept up to it. So it holds on every server the logs are on, at once, reachable or not, including one only a failed move touched. |
| What a deleted service left stays on a server's disk | It goes 7 days after the deletion, unless undoing the deletion brings it back, and the plan says so. Each server hears when it connects and every hour, so one that was offline or joins later hears too. Until a server is back what it holds stays on its disk, where nothing reads it, and the plan names it. A revoked server never hears, so wipe its disk when you revoke it. |
| A flood of logs takes down the server, the control plane or a browser | Every stage has a bound, listed in ADR 0025, and only the agent drops, and says how many. |
| A container nobody trusts writes so much that nobody can read other services | The budget is divided by service, then by reader, up front, so no service's lines can use up another's. |
| A container writes so fast that being looked at takes the server's CPU | A server's follows read lines they don't send at 64 MiB/s between them, whether dropped or filtered out. |
| One token holds every place for reading logs on a server, so nobody can read them during an incident | One reader holds at most 8 of a server's 16 places, searches can't take the last 4, and a reader that stops taking lines loses its place a minute after a message has to wait for it. On a busy service that took 60 to 70 s. On a quiet one nothing waits until what it writes fills the buffers on the way, and at 2 lines/s the place was still held at 110 s, which costs no more than a reader who keeps up holding one. Several tokens can still fill them, each of which is on record. |
| A reader asks for pages of logs and never reads them, to pin the control plane's memory | A page holds one of 4 places for its reader, and 8 in all, until it has been written, and a write with no progress for 30 s gives up. |
| A log fills the disk | Docker rotates each container's at 50 MiB, and what is kept stays within 256 MiB, and not on a disk with under 1 GiB free. |
| Someone with a session or a logs grant crafts a token to spend a server's CPU or memory, and leaves it working after they've gone | Nothing in a token is taken on trust. It is at most 1 MiB, names at most 64 sources on a server, with names of one shape and places that can be real, and `source_ids` and `levels` are capped too. The control plane checks this and refuses what's over, and the agent checks it again for itself. What the agent gathers is linear in what it holds and stops with its reader. |
| Someone with a token runs searches to spend a server's CPU or memory | A query is plain text of at most 1 KiB, never a pattern, and finding it takes time linear in the line. A search reads at most 1,000,000 lines of each container and stops at 30 s, a server searches 4 containers at a time, and it holds 2 MiB of matches for a container and 8 MiB for a reader, whatever their length. |

## Known gaps

- **Nothing is redacted, and nothing here promises to.** Dockwell can't tell what in a line is a secret. An app that prints one has leaked it to everyone who can read its logs, and that includes an app that prints its own command line, where a value it names has been written in ([ADR 0022](../adr/0022-secrets-encrypted-at-rest.md#a-command-an-entrypoint-and-a-health-check-name-values-too)).
- **Docker's own log files** are readable by root on the server, as they always were. So is the archive, which isn't encrypted at rest.
- **Roles.** Until members arrive, whoever has a session can read every service's logs. Roles will narrow this, as they narrow everything else.
- **A session has no grant to withhold.** A person signed in can read logs, so a stolen session can too, for as long as it lasts.
- **A search stays in the panel's address.** `?q=` holds it so that a view can be reloaded and linked, which puts what you searched for in your browser's history and in any link you copy, though the audit log leaves it out as sensitive. Taking it out would lose both, and needs a way of sharing a view of its own, so this stands for now. Each change replaces the last, so it doesn't stack up behind the back button. A reload sends it to the panel, which logs no addresses, though a proxy of your own in front of it might, and `Referrer-Policy: same-origin` keeps it from other sites. A search through REST goes in `GET /v1/services/{id}/logs?query=`, so whatever logs your client's requests has it too. On a shared browser, clear a search for a secret's value before you leave.
