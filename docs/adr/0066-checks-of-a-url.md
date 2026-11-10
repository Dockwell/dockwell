# 0066. Checks of a URL

- Status: Proposed
- Date: 2026-11-10

## Context

The PRD's P0 row "Uptime checks and alerts to email, Slack, Discord, Telegram, ntfy, webhooks" is half built. [ADR 0044](0044-alerts.md) delivers alerts on what Dockwell sees from inside: a service down by its health check, a server unreachable, a disk full, a certificate failing. Nothing asks whether an address answers from outside, which is what a person means by "is my site up" and what 1Panel's and every uptime tool's checks do. #266 lists it as a P0 gap. Checks belong with alerts: they're one more thing a rule can be about.

Two things nearby bear on it. An alert channel's test reaches the internet and private networks, never the control server's loopback, because a test shows what answered (ADR 0044, `alert.TestGuard`). [ADR 0053](0053-scheduled-tasks.md)'s URL tasks reach an agent's loopback and private networks and follow redirects, through a client of their own. A check also shows what answered, every interval, and with a keyword it says whether a page holds a string, so it's closer to the first than the second.

## Decision

Decided in the review of this record (2026-11-10): a check reaches the internet alone unless it says it's inside the network, and a keyword follows the same rule. The owner can overrule it.

### A check is a revision, like a rule

- **A check is a resource of the install,** kind `alert.check`, as rules and channels are: created, changed and deleted through a plan, undone like any change (PRD principle 2), by an admin. Its spec: the address, how often, how long a run waits, what counts as up, how many failed runs make it down, whether it's inside the network, and a keyword. Its name comes from the address's host, and a second check of the same host takes `-2`, then `-3`.
- **At most 200 checks an install.** A plan that would make the 201st is refused, saying to remove one first (`resource.Capped`, checked as the plan is made). With every run waiting out its timeout, as when the control server's uplink is down, 16 runs at once finish 0.5 to 1.6 runs a second, so past about 100 one-minute checks the intervals stretch; 200 keeps that within one late round.
- **Made where the address already is.** A website's or an app's domain row offers **Watch**, once it serves, with the address filled in and the defaults in one line built from the check-down rule ("Every minute · down after 2 failures · alerts every channel", or the channels it names) with **Change**. Alerts gets a Checks tab listing every check with its state and last answers. The CLI and API come with the kind: `dockwell alert-checks create|update|delete`, whose address flag is `--address`, as `--url` is the CLI's own.
- **The address isn't sealed.** It shows in plans, on Activity and in lists, and a health URL can carry `?token=`, so the form says to leave tokens and passwords out of it. Alerts and events name only the host.

### What a check may reach

- **The internet, by default.** A run uses netguard's internet-only options: a name that resolves to a private address reads failing, with what to set to allow it, and nothing is sent there.
- **Inside the network, when the check says so.** `inside_network` lets a run reach the private networks the control server is on as well (RFC 1918, unique local IPv6, carrier-grade NAT), as a channel's test does. That's the control server's own private address, its Docker bridge gateway (172.17.0.1) and every container on its bridge networks, not only other machines: "never the control server's loopback" means 127/8 and ::1, and its public address counts as the internet. Only an admin sets it, as every check is an admin's, and the plan, the Checks tab and Activity all say "inside the network".
- **A keyword is a content read,** whether a page's first 64 KiB holds a string, so it follows the same rule: without `inside_network`, it reads only what the internet serves.
- **Never** the control server's loopback, a metadata service or a link-local address, from the control plane, whatever the check says. An address written out as one of those, or as a private address without `inside_network`, is refused as the plan is made, with what to do instead, so a check that can never run doesn't plan, read down at every run and page someone.
- **From a server's agent** (slice 2), inside the network reaches that server's loopback too, as it's that server's own services, except on the control server, whose agent keeps the control plane's rule, as ADR 0044's reason holds there. The probe moves to a package both builds link, so both places judge a run by the same code, and netguard joins the agent build's list with what `make budgets` says it costs.

### What a run does, and what counts as down

- **One `GET`,** with no redirect followed, no proxy, no user and password in the URL, and at most 64 KiB of the answer read, only when a keyword asks for it. Nothing of the body is kept or shown: a run records when it ran, its status, how long it took, and what went wrong in words.
- **Up** is an answer within the timeout whose status is in the expected range, 200 to 399 unless the check says otherwise, so `http` redirecting to `https` is up; and, when the check has a keyword, whose first 64 KiB holds it (or doesn't, for a keyword that must be absent). A keyword check that gets a redirect fails, saying where it moved to. As an `http` address that redirects reads up even while `https` is broken, the form offers the `https` address when the site serves one (slice 3).
- **Down** is anything else: no connection, a TLS failure (a certificate that doesn't verify is down, on `https`), a timeout, a status outside the range, the keyword missing. The check reads failing at the first failed run, and down once that many runs in a row have failed, 2 by default; up again at the first that passes.
- **Slow** is an answer that passed but took longer than the check's limit, off unless set (1, 2 or 5 s). It's shown, and isn't down (slice 3).
- **Defaults:** every minute (30 s, 1, 5 or 15 min), a 10 s timeout (at most 30 s, and under the interval), down after 2 failures. netguard bounds connecting and the TLS handshake at 10 s whatever the timeout, so a longer timeout lengthens only the wait for the answer's headers.
- **Certificates aren't checked here.** The `certificate-expiring` rule reads the certificates of the routes Dockwell serves. A run that records its certificate's end, and the rule reading it for a checked host Dockwell doesn't serve, is a later slice of its own.

### Who runs it, and falling behind

- **The control plane, by default,** from the control server. It runs every check on its own clock, at most 16 at a time. Checks read as the control plane starts are spread over their interval; checks a plan makes start 2 s apart, so 20 made at once don't ask together.
- **It never waits for a slot.** A run that's due while all 16 are taken is skipped, recorded as skipped with why, and counts neither as up nor as a failure; the check runs at its next interval. So a slow minute never holds up reading the checks again, nor the checks after it.
- **A server's agent, when the check says so** ("from web-2", slice 2), for an address only that server's network reaches, or to see it from where its users are. The control plane asks the agent for each run (`CheckCall`), and the agent makes the one request, answers and keeps nothing. A server runs at most 20 checks and 4 at once; a run its agent is too busy for, too old for or away from is unknown, not down. `make budgets` measures an agent with 20 checks running, as one that runs a check every 30 s never reaches ADR 0060's idle.
- **From more than one place,** each place's answer is shown, and the check is down only when every place that ran sees it down; a place that couldn't run is left out, and when none ran, the result is unknown. Down from some is shown as "down from web-2", with no alert.

### How it reaches alerts

- **A new rule condition, `CONDITION_CHECK_DOWN`,** fires when a check reads down, one alert for each check, said as "shop.example.com is down" with what the last run said, and resolved when it reads up. An install gets a `check-down` rule like the others: critical, to every channel, again every 4 hours while it lasts, held 2 minutes.
- **It holds both ways, as every rule does (ADR 0044).** The alert opens once the check has been down for the rule's duration, counted from its first failed run, and resolves only once it has been up for that duration. A failing run while the alert is open starts the clear time again. So a site that fails twice and passes once, round and round, is one alert, and doesn't get around `repeat`. A rule left without a duration holds 2 minutes.
- **What's unknown isn't resolved.** Until the runner has read the install's checks, every check is in the dark, so the evaluator's first tick after a start can't resolve an open alert. A check waiting for its first run, as after a start or a change, is unknown. A check whose spec can't be read stays listed, with why, and is unknown. In each case an open alert stays open and says nothing.
- **A restart** writes nothing on its own: an outage that spans one stays one alert, opened once and resolved once. When events come (slice 3), `check.down` is written as an alert opens and `check.up` as it resolves, so they follow the same rule and a restart writes neither twice.
- **One cause, many alerts.** When every check from the control server fails with a network error in the same minute, the alert says once that the control server can't reach the internet, not "N addresses are down" (slice 3).
- **Later defaults arrive once.** ADR 0044 wrote the default rules once, on a new install. A rule added in a later release, as `check-down` is, is made on an upgraded install once, recorded under its own setting (`alerts.defaults.check-down`), and stays deleted if someone deletes it.

### Events

`check.down` and `check.up` (slice 3) go on Activity and, as the check then stores the route it was made from, on the timeline of that app or website, placed by what they're about as ADR 0049 places an alert's, so the app's developers and viewers see them. They are the alert's opened and resolved lines for that check, not two more. A check made from the Checks tab has no route, and its events go on Activity alone.

### History

As [ADR 0065](https://github.com/Dockwell/dockwell/pull/463) keeps a server's samples, in the same table writer and transaction each minute, and in the same shape:

- **A minute for a day.** The control plane adds up each check's runs in memory, keyed by its own clock's minute: runs, failures, skipped runs, latency summed and highest, and the last status, which on a merge of two control planes' rows is the one with more runs. At each minute's end it writes one row for every check, with 0 runs for a check that didn't run that minute. A minute with no row is a control plane that wasn't there, drawn grey. A minute with a row and no run is normal for a 5- or 15-minute check, 4 minutes in 5, and is drawn as its last run's state while that run is within its interval. A minute write whose result is lost isn't retried.
- **An hour for a month, rebuilt** from its minute rows once its last minute is written, replacing any row there, and caught up as the control plane starts, as ADR 0065 does.
- **A hard cap,** at most 1,440 minute rows and 744 hour rows a check, by a range delete on each write and an hourly prune by age; a deleted check's rows go after 31 days.
- **Read by an admin,** as the check is: `ListAlertChecks` and the history read are `NEEDS_ADMIN`, `REACH_INSTALL`. The Checks tab draws a day as a strip of minutes and a month as hours, drawing a bucket only when Dockwell was there for at least half of it.

### What it costs

- **The control plane:** about 2.6 KB a check for its state and its last 10 runs, as slice 1 measured it (200 checks hold 0.53 MB); one request a check an interval, so 100 checks every minute are under 2 requests a second; one history row a check a minute.
- **The store:** a row is 8 integers and its key, about 100 to 130 B on SQLite and 200 to 280 B on Postgres, so at most about 0.3 MB and 0.6 MB a check, and for 200 checks about 60 MB and 120 MB. That's the ceiling, not a growth rate, and the control-plane backup (#356) carries it.
- **An agent:** nothing between runs; a run is one request, at most 64 KiB read, in the call that asked for it.
- **The network:** the requests themselves, a few kilobytes each.

### Numbers

The agent protocol takes this area's block: `Call.check` and `CallResult.check` 234, in slice 2. #459 takes 230 in `Hello` and 230 and 231 in its listing messages, and ADR 0064 takes `Hello.boot_id` 231 and `DesiredResource` 232 and 233. The public schema numbers its own fields: `AlertCheck.inside_network` 9, `AlertCheckInfo.problem` 8, `AlertCheckRun.skipped` 6.

### Open

- Nothing watches the control server itself: if it goes down, no check runs and no alert goes out. A heartbeat Dockwell sends each minute to a dead man's switch service outside it is a later slice.

## Consequences

- "Is my site up", from outside, with an alert when it isn't, is one click from the domain that serves it.
- Checks are revisions, so who added, changed or removed one is on record, and undo brings one back.
- A check reaches inside the network only when it says so, and everyone who reads its plan sees that.
- An outage is one alert, across a flap and across a restart of the control plane.
- From the control server, a check of a site on the same server shows the server's own view: the panel says where it runs from, and from another server is one choice away.
- Slices: (1) checks run by the control plane, made from a domain row, listed under Alerts, with `CHECK_DOWN`, inside the network, the cap and the hold; (2) checks from a server's agent, and from more than one place; (3) history drawn, slow, the keyword in the panel, events, offering `https`, and one alert for one cause.
