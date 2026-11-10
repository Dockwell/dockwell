# 0044. Alerts

- Status: Accepted
- Date: 2026-11-07

## Context

The PRD makes alerts a launch requirement: "Uptime checks and alerts to email, Slack, Discord, Telegram, ntfy, webhooks" (P0), on "one event timeline correlating deploys, config changes and alerts". 1Panel keeps email and IM alerts in its paid edition. #242 asks for rules from what Dockwell already knows (an app down, a server it can't reach, disk or memory above a level, a certificate that failed or expires soon, a backup that failed or is late, a failed change), channels for each of those services, grouping, repeats, a resolved message, mutes, and defaults that work from the first start.

It also asks three questions this record answers: whether rules and channels are revisions with plans or settings audited as [ADR 0042](0042-the-installs-settings.md) has the install's name, how evaluation survives the control plane restarting, and what it costs at 1,000 servers.

ADR 0042's line is that a setting changes what Dockwell says and suggests, not what runs, holds no secret, and has nothing to undo but setting it back. A backup policy and a backup destination are the nearest kinds that exist: the control plane runs the one and sends to the other, with what signs in to it sealed ([ADR 0033](0033-backups-of-volumes-and-databases.md)).

## Decision

### Rules and channels are revisions

A rule is a resource of kind `alert.rule`, and a channel one of kind `alert.channel`, both in the organization, both run by the control plane and on no server, as a backup policy is.

- **A channel holds what signs in to somewhere else,** such as a webhook's URL with its token. Dockwell keeps such things as `(sensitive)` fields of a sealed spec ([ADR 0022](0022-secrets-encrypted-at-rest.md)), as a backup destination's keys are: never in a plan, on Activity or in a log, and a plan says only that one changed. Settings have no sealing, and building a second way to keep a secret would be a second way to leak one.
- **A rule decides who hears of an outage.** Losing one by mistake is what undo is for, and a plan says what an edit does ("Disk above 90%" to "Disk above 85%") before it does it.
- **Rules name channels,** and the engine already refuses a change that would leave a spec naming what isn't there. Deleting a channel a rule names has to stop the rule naming it first, in the same change or before.
- **The CLI and the API come with it:** `dockwell alert-rules create|update|delete` and `dockwell alert-channels …` plan changes as any kind's do, and a token that can propose and apply changes can make them. The panel plans them the same way, through `ChangeService`, as it does backup destinations; `AlertService` only reads what's firing, lists rules and channels, tests a channel, and mutes.

### What isn't a revision

- **An alert is the control plane's record,** like a backup's row: `alerts` holds each, from when it fired until it resolved, with what it said, and `alert_deliveries` each message sent and what the far end answered. Neither is managed state, so nothing plans them.
- **A mute is an audited action,** as ADR 0042's settings are. It holds what a rule or a server sends for a while, during an outage, when a plan would be in the way; it ends on its own; and undoing it is ending it. `MuteAlerts` and `UnmuteAlerts` are `EFFECT_APPLY`, so a token that can make changes can mute, and each goes on Activity as `alert.mute` or `alert.unmute`. Alerts go on firing and resolving under a mute, on Activity and on Alerts; only what's sent is held. When the mute ends, what's still firing is sent.
- **Acknowledging is an audited action too,** added on 2026-11-10 (#210). It says one thing: someone has seen this alert and is on it. Like a mute it has to work during an outage, when a plan would be in the way, and it changes no desired state, so it's no revision.
  - **What it records.** Who, and when, on the alert itself: `Alert.acknowledge_time` and `acknowledged_by`. One person has an alert at a time. Acknowledging one that someone else has takes it over, which is how a hand-off reads, and each time goes on Activity.
  - **What it doesn't do.** It holds nothing back and sends nothing. The alert's firing message went out when it fired, and its resolved message goes out as it would have: holding what's sent is the mute's job, and the two stay apart so that "I'm on it" never quietens anything by accident. Channels aren't told either. A chat channel that gets fired and resolved would get a third kind of message that says less than the panel does, and a webhook's receivers would need to learn it. When on-call rotations come, telling the next person is theirs to decide.
  - **It never covers a later or worse state.** It belongs to one alert, from when it fired until it resolves. An open alert doesn't change what it's about or how much it matters, so anything worse is another alert, of another rule or for another thing, and starts unacknowledged. So does the same rule firing again for the same thing after it resolved.
  - **It ends when the alert resolves.** Nothing is left to be on. The resolved alert keeps who had it, as part of its record, for as long as it's kept. Only an open alert can be acknowledged.
  - **Taking it back** is `UnacknowledgeAlert`, for "I'm not on it after all", and it's your own to take back: the write names who has the alert, so a screen that's out of date can't take back what someone else took over since. It's refused for an alert nobody has (`not_acknowledged`), for one someone else has (`not_yours`: taking it over is the way), and for one that resolved (`alert_resolved`).
  - **Who may.** As for a mute: `AcknowledgeAlert` and `UnacknowledgeAlert` are `EFFECT_APPLY`, an admin's, across the install, and a token that can make changes can call them, so a script that pages someone can mark what it picked up. Each goes on Activity as `alert.acknowledge` or `alert.unacknowledge`, naming the alert as its firing did, and an acknowledgement that took it over says from whom, so the event stands without the one before it. A token that has an alert shows as a token, wherever it shows.
- **Sending a test** changes nothing: it sends one message through a channel, one that exists or one a form holds, and says what the far end answered.

### An install starts with a rule for each condition

On its first start with alerts, an install makes one rule per condition, as Dockwell's own change, applied at once like the install's default project ([ADR 0014](0014-object-model-and-store.md)) and left off the timeline as that one is. Each sends to every channel, as a rule that names none does, so the install tells you of what matters as soon as you add one channel. A setting, `alerts.defaults`, records that they were made, so rules someone deleted stay deleted, and an install that upgrades gets them once too.

| Rule | Fires when | Severity | Repeats |
| --- | --- | --- | --- |
| `app-down` | a service isn't running, or fails its health check, for 2 minutes, on a server that's connected | critical | every 4 hours |
| `server-unreachable` | Dockwell hasn't heard from a server's agent for 2 minutes | critical | every 4 hours |
| `disk-full` | a disk of a server is over 90% full for 5 minutes | warning | daily |
| `memory-high` | a server uses over 90% of its memory for 10 minutes | warning | daily |
| `certificate-expiring` | a domain's certificate expires within 14 days | warning | daily |
| `certificate-failed` | a domain's certificate couldn't be issued, for 10 minutes | critical | daily |
| `backup-failed` | a volume's last backup failed, or its copy didn't reach its destination | critical | never |
| `backup-late` | a schedule's run hasn't started an hour after its time | warning | never |
| `change-failed` | a change fails or rolls back, a deploy among them | warning | never |

### Evaluation, on the control plane

Every 15 seconds, one loop reads what the control plane already holds and decides, for each rule, what it holds for. The agent does nothing new, so its budget stays where it is.

- **Apps:** where each service is placed, from `ListHealth`, and how it stands there as the health recorder folds what its agent reports ([ADR 0027](0027-health-checks-rollouts-and-rollback.md)), so Alerts and the app's page agree on what "down" means. An app is down from when it went bad until it has stayed good for 30 seconds, whatever it goes through between: a crash loop that reads starting, or running, for a moment each time round is down from its first fall, and one that keeps going between good and bad is down while it does. A one-shot job that exited 0, with a restart policy of `no` or `on-failure`, did its work, which the agent says (`InstanceHealth.finished`), and isn't down; nor is an app someone paused. A service on a server that isn't connected is unknown, not down: `server-unreachable` says that once, rather than each app on it.
- **Servers:** the fleet's connections, and the newest stats each agent sent, for disk and memory. A server that has never connected isn't unreachable yet.
- **Certificates:** what each server's ingress last said of each domain. "Within 14 days" holds from 14 days before it ends.
- **Backups:** each volume's newest backup, however old, while the volume is there, and each policy's next run as the scheduler counts it, said in the policy's time zone.
- **Changes:** the `change.failed` events since the last one told. A failed change is told once, as it's over: its alert opens and resolves together, and a rollback is said with it. Activity has its opening alone, beside the failed change, as it never resolved; the same goes for alerts a deleted or disabled rule closes.

**What's unknown isn't resolved.** A signal the control plane can't read now, such as an agent that hasn't reported since the control plane started, keeps an open alert open and a pending one pending. Only a signal that says the condition no longer holds resolves an alert.

**An alert resolves once it has been clear for its rule's duration,** so one dip doesn't close it and open another, and get around `repeat`. A disk or memory has to come 5 points under its rule's threshold to count as clear: a level that hovers about the line keeps one alert open. A rule that's removed or turned off forgets what it was timing, so one that comes back starts its clocks again.

**Activity says an alert in the reader's language.** The title a message carries is English; the event keeps its condition, what it's about, where and a value apart, and the panel puts them in its own words.

### Sending

- **Grouping.** A rule's alerts that fire within 30 seconds of each other go to each channel as one message, such as "3 disks are over 90% full", so one outage isn't a storm. The 30 seconds count from the oldest that hasn't gone, so alerts that keep coming don't hold back the first. Those that resolve are grouped the same way.
- **Repeats.** An alert still firing is sent again as its rule's `repeat` says, at most every 10 minutes and at least weekly, or never.
- **Resolved.** An alert whose firing message went out gets one more when it resolves, to the same channels, under a mute too: a mute holds what fires, never the all-clear. An alert whose rule is removed or turned off is closed, and gets a resolved message that says so. One that resolves before anything was queued gets none. One that resolves while its firing message is still on its way to a channel gets the resolved one there instead, which says how long it held.
- **Messages follow the content rules:** the fact ("web-1's disk is 93% full"), what it means ("Apps there can't write once it fills, and Docker can't pull"), the fix ("Free space with docker system prune, or grow the disk"), and a link to where the panel shows it, when the control plane has a domain (`--domain`).
- **An outbox, per alert and channel.** Each message an alert owes a channel is a row (`alert_sends`) until the far end takes it, with a 2xx, or Dockwell gives up. A rule's rows that are due together go as one message. One that fails is tried again after 10 seconds, 30 seconds, a minute, 2, 5, 10, then every 15 minutes, for 24 hours. A channel's deliveries keep the first failure, what went, and giving up, not every try between. Nothing waits on a channel that's down: each is sent to on its own, one message at a time. A far end that asks Dockwell to slow down (429) is waited for as long as it says, in `Retry-After` or Discord's and Telegram's `retry_after`, or the schedule's next wait if longer, with its other messages; that isn't a failure, so the channel doesn't read Failing.
- **A stop gives what's on its way 5 seconds** to land, so a far end that never answers can't hold the control plane past systemd's stop timeout. What doesn't land stays in the outbox.
- **Webhooks** get a JSON `POST`, with `Dockwell-Signature: t=<unix time>,v1=<HMAC-SHA256 of t, ".", and the body>` when the channel has a secret; [`docs/webhooks.md`](../webhooks.md) says how to check it. The payload has each alert's title in English and its facts apart. A URL has to use HTTPS, unless it's on this server or its private network. Nothing is sent to a link-local address, nor to the other addresses clouds keep metadata services on (`100.100.100.200`, `fd00:ec2::254`), whatever a name resolves to. No proxy from the environment is used: through one, the dial check would see the proxy alone. Should someone need one, it becomes a setting of its own. These rules are `internal/netguard`'s, the one guarded client for every address a person names (#341), which catalog sources, scheduled HTTP tasks and Git lookups take up too: it checks the address it's about to connect to, follows no redirect and caps what an answer reads. It refuses this server's own addresses and private networks unless a caller allows each; alerts do, as a webhook or a chat server on the install's own network is what many want, and a channel is made and tested only by a person or a token that can make changes. A test never reaches this server's own addresses, though, as it shows what answered, which would let a token read what listens on the control server's loopback; and every test goes on Activity, with who sent it, the channel's type and host, and what answered. Its client bounds each request at 30 seconds unless a caller says, caps an answer's headers at 64 KiB, and its errors name a URL by its scheme and host alone, as `netguard.Words` says them.

### Channels

Each channel is a field of `AlertChannel`'s oneof, and every field that signs in is `(sensitive)`: sealed at rest, never in a plan, a list, an event or a log, and kept when an update leaves it empty.

| Channel | What it sends | What's sealed |
| --- | --- | --- |
| Webhook | A JSON `POST`, with `Dockwell-Signature: t=<unix time>,v1=<HMAC-SHA256 of t, ".", and the body>` when the channel has a secret | The URL, which may hold a token, and the secret |
| Email | Plain text, quoted-printable, through the channel's SMTP server, with STARTTLS (587) unless it says TLS (465), or none for a server on this server or its private network | The password |
| ntfy | A JSON publish to the server's root, `https://ntfy.sh` unless it says, with the topic in the body, never in a URL; priority 5 for what's critical, 4 for a warning, 3 for a test or what resolved | The topic, which on a public server is what lets anyone read it, and the token |
| Slack | Slack's markup to an incoming webhook, with what a title holds escaped | The URL |
| Discord | Markdown to a webhook, mentioning no one whatever a title holds | The URL |
| Telegram | Plain text through the Bot API, to a chat ID or a channel's name | The bot's token |

Lists show where each sends with nothing that signs in: a URL's host, whom an email goes to, or a Telegram chat. A form gets a channel's spec with what's sealed left empty, and the names of the fields it keeps.

**What's kept stays where it was.** An update that leaves a field that signs in empty keeps it only while the channel signs in to the same place: the same webhook host, the same SMTP server, port, security and user, or the same ntfy server. Moving it asks for what it kept again, so a token that can make changes can't point a password at a server of its own. A test of a form is held to the same rule.

### Where Dockwell sends

Every request to an address a person gives a channel goes through the same rules, which `docs/security/alerts.md` sets out:

- **HTTPS, or TLS for email,** with the certificate verified, unless the address is on this server or its private network: a loopback or private address, a name with no dot, or one under `.internal`, `.lan`, `.local` or `.home.arpa`.
- **No link-local address,** nor the other addresses where clouds keep their metadata services (`100.100.100.200`, `fd00:ec2::254`), nor a multicast or unspecified one: refused when the channel is planned, if the address is written out, and again as each connection is made, whatever a name resolved to, so a name that changes what it resolves to between the two gets nothing.
- **No proxy from the environment, no user and password in a URL,** no redirect followed, 10 seconds a try, and no more than 64 KiB read back.
- **Only what the far end said** goes in a delivery, cut to a line: a status, and for Slack, Discord, ntfy and Telegram, their own reason. A webhook's answer is its status alone, as its body is its own and may hold what signs in. Nothing that signs in, and never a URL, is in what Dockwell says of a failure.
- **A password goes only over TLS,** and an email channel without TLS can't sign in.
- **Private addresses are allowed:** a webhook on this server or its network is what many installs want, such as a chat server of their own. A channel is something only a person, or a token that can make changes, can make or test.

### Surviving a restart

- **Open alerts are rows,** so a restart neither forgets them nor sends them again. When each was last queued is with it, so repeats pick up where they were, and the outbox sends what it still owes at the next start.
- **A pending alert's clock comes from its signal where the signal has one:** a service's health says since when it has been down, a server when it was last heard from, a backup when its run was due. Disk and memory start again, so at worst such an alert fires one duration late after a restart.
- **Servers get time to come back.** After a restart a server counts as unreachable only once the rule's duration has passed since the control plane started, as every agent has to dial in again.
- **Changes are read from where the last one told left off,** which a setting keeps, and from no more than an hour back, so a long stop doesn't end in old failures.
- **A message in flight when it stops** isn't on record as sent, so it goes again at the next start: a message can arrive twice, never not at all. The alerts' IDs and status in it tell a repeat from a new alert.
- **An open alert's clear clock** starts again after a restart, so at worst it resolves one duration late.

One control plane evaluates. A second on the same Postgres, when high availability comes, will need a lease, or both would send.

### Retention

Alerts that closed, what they sent, and messages to channels are kept 90 days, and forgotten once an hour after. Activity has no retention rule of its own to follow, so 90 days is this record's choice: a quarter's history on Alerts, beside a table that would otherwise grow with every outage. An alert that still owes a message is kept until it's sent or given up. Activity's events of alerts are the audit log's, and stay with it.

### What it costs

Evaluation keeps, for each rule and each thing that holds but hasn't fired yet, a key and since when: the worst case is every one of them pending at once. At 1,000 servers with 20 services each, that's 20,000 for apps and 1,000 each for unreachable servers, disks, memory and certificates, 24,000 in all, and `TestEvaluationAt1000Servers` measures 3.2 MB kept for them, judged in about 12 ms. What's firing is in the store, not in memory. Reading the signals costs no memory of its own, as the control plane holds them already, and a tick reads the store four times however many there are.

## Consequences

- The rail's Alerts opens a screen of its own, and Activity has an Alerts kind of event, `EVENT_KIND_ALERT`, with what fired or resolved, beside the deploys and changes around it.
- An install has nine resources it didn't before. Someone who wants no alerts deletes them or adds no channel; without a channel, alerts still open, close and go on Activity.
- A channel's URL, token or password can't be read back, in the panel or the API. Changing a channel without them keeps what it has.
- An open alert says who has it, on Alerts and on Activity, and offers to take it. Nothing about what's sent changes with that.
- Not yet: push to the panel as a PWA, PagerDuty and Opsgenie, uptime checks of a URL from outside, an incident view, quiet hours, on-call rotations, and rules scoped to some servers or projects. Each fits beside these rules and channels.
