# 0066. Checks of a URL

- Status: Proposed
- Date: 2026-11-10

## Context

The PRD's P0 row "Uptime checks and alerts to email, Slack, Discord, Telegram, ntfy, webhooks" is half built. [ADR 0044](0044-alerts.md) delivers alerts on what Dockwell sees from inside: a service down by its health check, a server unreachable, a disk full, a certificate failing. Nothing asks whether an address answers from outside, which is what a person means by "is my site up" and what 1Panel's and every uptime tool's checks do. #266 lists it as a P0 gap. Checks belong with alerts: they're one more thing a rule can be about.

## Decision

### A check is a revision, like a rule

- **A check is a resource of the install,** kind `alert.check`, as rules and channels are: created, changed and deleted through a plan, undone like any change (PRD principle 2), by an admin, or a token that may change the install's alerts. Its spec: the URL, how often, how long a run waits, what counts as up, how many failed runs make it down, where it runs from, and a name made from the URL's host.
- **Made where the address already is.** A website's or an app's domain row offers **Watch this address**, with the URL filled in and the defaults in one line ("Every minute · down after 2 failures · alerts every channel") with **Change**. Alerts gets a Checks tab listing every check with its state and last answers. The CLI and API come with the kind: `dockwell alert-checks create|update|delete`.

### What a run does, and what counts as down

- **One `GET`,** with no redirect followed, no proxy, no user and password in the URL, and at most 64 KiB of the answer read, only when a keyword asks for it. Nothing of the body is kept or shown: a run records its status, how long it took, whether the keyword was there, and the certificate's end.
- **Up** is an answer within the timeout whose status is in the expected range, 200 to 399 unless the check says otherwise, so `http` redirecting to `https` is up; and, when the check has a keyword, whose first 64 KiB holds it (or doesn't, for a keyword that must be absent).
- **Down** is anything else: no connection, a TLS failure (a certificate that doesn't verify is down, on `https`), a timeout, a status outside the range, the keyword missing. The check reads down once that many runs in a row have failed, 2 by default, and up again at the first run that passes.
- **Slow** is an answer that passed but took longer than the check's limit, off unless set (1, 2 or 5 s). It's shown, and isn't down.
- **A certificate about to end** isn't down either: the existing `certificate-expiring` rule (ADR 0044) fires for a checked `https` address whose certificate ends within 14 days, so a domain served elsewhere is watched as Dockwell's own domains are.
- **Defaults:** every minute (30 s, 1, 5 or 15 min), a 10 s timeout (at most 30 s, and under the interval), down after 2 failures.

### Who runs it

- **The control plane, by default,** from the control server, through netguard as an alert channel's test reaches its far end: the internet and private networks, never the control server's loopback nor a metadata address. It runs every check on its own clock, at most 16 at a time, each at its interval with a few seconds' spread, so checks made together don't fire together.
- **A server's agent, when the check says so** ("from web-2"), for an address only that server's network reaches, or to see it from where its users are. The control plane asks the agent for each run (`CheckCall`), the agent makes the one request with the same limits (its own loopback allowed, as it's that server's own services), answers, and keeps nothing: no schedule, no state, so it stays within its budget and an agent from before this simply isn't offered as a place.
- **From more than one place,** each place's answer is shown, and the check is down only when every place sees it down; down from some is shown as "down from web-2", with no alert. That keeps one network's outage from paging for a site that's up.

### How it reaches alerts

- **A new rule condition, `CONDITION_CHECK_DOWN`,** fires when a check reads down, one alert for each check, said as "shop.example.com is down: 502 from the control server, since 14:02" and resolved when it reads up. An install gets a `check-down` rule like the others: critical, to every channel, again every 4 hours while it lasts. Its duration is the check's own: failures in a row, not minutes.
- **A check going down and coming back** is also an event (`check.down`, `check.up`) on Activity and on the timeline of the app or website whose domain it watches, so an outage sits beside the deploy that caused it.

### History

As [ADR 0065](https://github.com/Dockwell/dockwell/pull/463) keeps a server's samples, and in the same shape:

- **A minute for a day.** The control plane adds up each check's runs in memory, keyed by its own clock's minute: runs, failures, latency summed and highest, and the last status. At each minute's end it writes one row for every check, in one transaction, with 0 runs for a check that didn't run that minute. So a minute with a row is one Dockwell was there for, and a minute with no row is a control plane that wasn't, which the strip draws grey rather than down. A minute write whose commit result is lost isn't retried, as a minute lost is safer than one counted twice. Minute rows merge as ADR 0065's do: sums add, the highest stays the highest, with the dialect's `MAX` or `GREATEST`.
- **An hour for a month, rebuilt.** Hour H is computed from its minute rows once H:59 is written, and replaces any row already there, so a restart or a retry can't count it twice. As the control plane starts, it rolls every hour of the last 24 that has minute rows and no hour row.
- **A hard cap,** at most 1,440 minute rows and 744 hour rows a check, by a range delete past them on each write and an hourly prune by age; a deleted check's rows go after 31 days. So at most 2,184 rows a check, ever, in one table, `check_history`, keyed by check, tier and time. The merge, the roll-up and the catch-up are tested on SQLite and Postgres.
- **Read by an admin,** as the check is: `ListAlertChecks` and the history read are `NEEDS_ADMIN`, `REACH_INSTALL`. The Checks tab draws a day as a strip of minutes, up, down, slow, or grey where Dockwell wasn't there, and a month as hours, drawing a bucket only when Dockwell was there for at least half of it.

### What it costs

- **The control plane:** a few hundred bytes a check for its state and its last 10 answers, one request a check an interval, so 100 checks every minute are under 2 requests a second; one history row a check a minute, so 100 checks write 100 rows a minute in one transaction.
- **The store:** a row is 7 integers and its key, about 100 to 130 B on SQLite and 200 to 280 B on Postgres with its index and the dead rows rolling deletes leave, so at most about 0.3 MB and 0.6 MB a check, and for 100 checks about 30 MB and 60 MB. That's the ceiling, not a growth rate, and the control-plane backup (#356) carries it.
- **An agent:** nothing between runs; a run is one request, at most 64 KiB read, in the call that asked for it. Measured with `make budgets` in the slice that has agents run checks.
- **The network:** the requests themselves, a few kilobytes each.

### Numbers

The agent protocol takes this area's block: `Call.check` and `CallResult.check` 234, in slice 2. #459 takes 230 in `Hello` and 230 and 231 in its listing messages, and ADR 0064 takes `Hello.boot_id` 231 and `DesiredResource` 232 and 233. A check of a URL needs nothing in the public `ServerStats`.

## Consequences

- "Is my site up", from outside, with an alert when it isn't, is one click from the domain that serves it.
- Checks are revisions, so who added, changed or removed one is on record, and undo brings one back.
- From the control server, a check of a site on the same server shows the server's own view: the panel says where it runs from, and from another server is one choice away.
- Slices: (1) checks run by the control plane, made from a domain row, listed under Alerts, with `CHECK_DOWN`; (2) checks from a server's agent, and from more than one place; (3) history drawn, slow, and the keyword in the panel.
