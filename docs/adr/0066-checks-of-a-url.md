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

As [ADR 0065](https://github.com/Dockwell/dockwell/pull/463) keeps a server's samples, and in the same shape: the control plane adds up each check's runs a minute at a time, in memory, and writes one row a minute for each check (runs, failures, latency summed and highest, the last status) and rolls them into hours, a minute's rows for a day and an hour's for a month, at most 2,184 rows a check. The Checks tab draws a day as a strip of minutes, up, down or slow, and a month as hours.

### What it costs

- **The control plane:** a few hundred bytes a check for its state and last answer, one request a check an interval, so 100 checks every minute are under 2 requests a second; one history row a check a minute.
- **An agent:** nothing between runs; a run is one request, at most 64 KiB read, in the call that asked for it. Measured with `make budgets` in the slice that has agents run checks.
- **The network:** the requests themselves, a few kilobytes each.

### Numbers

The agent protocol takes this area's block: `Call.check` and `CallResult.check` 234 (230 and 231 are #459's, and 231 to 233 are proposed for ADR 0064).

## Consequences

- "Is my site up", from outside, with an alert when it isn't, is one click from the domain that serves it.
- Checks are revisions, so who added, changed or removed one is on record, and undo brings one back.
- From the control server, a check of a site on the same server shows the server's own view: the panel says where it runs from, and from another server is one choice away.
- Slices: (1) checks run by the control plane, made from a domain row, listed under Alerts, with `CHECK_DOWN`; (2) checks from a server's agent, and from more than one place; (3) history drawn, slow, and the keyword in the panel.
