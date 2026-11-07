# 0047. The audit log, for a person

- Status: Accepted
- Date: 2026-11-07

## Context

Issue #223 asks for Settings' audit log: every action, filterable and exportable, as `Audit-Log.dc.html` draws it. The PRD's Access rows list "scoped API tokens; audit log" at launch.

Dockwell already records an audit log in the store, `audit_events`: who did what, when, from where, and to what, with a line of words for people. [ADR 0020](0020-the-event-timeline.md) built the timeline on part of it and left the rest there on purpose: refusals (wrong codes, refused tokens, a suspected copied passkey, a lock), a token's first use from an address, signing out, and the install's own setup. Those answer "who tried what", "which the audit log screen is for". ADR 0020 also kept each event's words out of the API, because they name addresses, and everyone who can read the timeline, API tokens included, would read them.

Two things have no place in the store yet: an IP address of its own for each event, which the mock gives a column, and a retention to choose, which it shows as "kept for 1 year".

## Decision

### `AuditService.ListAuditEvents`

| Method | Effect | Who | REST |
| --- | --- | --- | --- |
| `AuditService.ListAuditEvents` | read | a person signed in (`needs_session`) | `GET /v1/auditEvents` |

- **It returns the events as they were recorded,** newest first by time and ID, 100 to a page and at most 500, with an opaque page token, as other lists do. Each event has:
  - its action;
  - its actor, named as the timeline names actors;
  - where it came from;
  - what it acted on, by ID and by its name where Dockwell still has one;
  - the ID it is about;
  - what it recorded, as it was recorded: words for people, or facts for a program, such as a setting's value as JSON;
  - the same event as the timeline has it (`Event`, built by `events.FromAudit`), with the facts its kind carries: a health check's state, a backup's volume and destination, a setting's value and what it replaced, a public address, a file's path. A change's events carry the change as it is now, with its plan in brief.
- **Someone who wasn't signed in has no actor.** Their address, which the store keeps in the actor's name (`anonymous:203.0.113.9`), is a field of its own. Every other address is in the event's words, as it was recorded. The screen shows those words where the mock has an IP column, rather than an address parsed out of them: nothing reads those words to find what an event is about (ADR 0020), and this doesn't start to.
- **The screen says each event in a sentence,** as Activity does from those facts, in the reader's language, and words of its own for what only the audit log keeps: refusals, step-ups, a change at each step. A change's row names its service and environment, and opens to who proposed and applied it, its state, its servers and its plan, read when it opens. What was recorded stays in the opened row, and in the JSON export.
- **A token the CLI made for local access says so by what made it.** The token records the shell account that asked for it, as `mira@hz-1`, and `Actor.local_access` carries that and its scope. The screen shows it as "CLI on hz-1". A token's name is free text, so nothing reads it to decide who acted: anyone can name a token "Local access on prod-1". Activity and an app's deployments say it the same way, from one hook.
- **No API token reads it, of any scope.** Its words name where people signed in from and were refused from. A read-only token reads the state of what an install runs (ADR 0016), and who signs in from where is not that. Exporting to a SIEM with a token can come later as a grant, as reading logs did (`TOKEN_GRANT_LOGS`, [ADR 0025](0025-logs-per-deploy.md)). Until then the CLI can't read it either, as its token is a token.

### Filters

Each filter that is set must hold:

- **Since:** a time. The screen offers the last 24 hours, 7 days, 30 days, or everything.
- **Actor:** a kind. People, API tokens, Dockwell, servers, someone in a shell on the control server, or someone who wasn't signed in. This is `AuditActor`, an enum of its own. The timeline's `ActorKind` has no stranger in it, and gains none.
- **Surface:** the panel, the API, the CLI, or Dockwell on its own.
- **Query:** words, matched whatever their case, against:
  - the action;
  - the IDs of the actor, the target and the subject;
  - the event's words;
  - the names people, tokens and servers have now.

  It's a `LIKE` scan over the time-ordered index, which an install's log serves easily. A search index can come if a log outgrows it. A query holds 200 characters at most, as each one is matched against several columns. SQLite's `lower()` folds only ASCII letters, so on SQLite "Ä" doesn't match "ä"; Postgres folds every letter.

### Export

The panel exports what the filters keep. It pages through the API and saves the result as CSV, or as JSON lines in the API's own shape. There is no export endpoint: the API is the export. A browser holds 10,000 events at most, and the screen says so when there were more.

- The CSV's detail is the sentence the screen says. The JSON lines keep what was recorded, beside the typed event.
- A CSV field a spreadsheet would run as a formula, one starting `=`, `+`, `-`, `@`, a tab or a carriage return, gets a `'` before it and is quoted, so it reads as text. Names of tokens, people and servers are free text, and land in the CSV.
- The file is dated in the browser's time zone, as the screen shows times.

### Retention

The log keeps every event for as long as the install exists, as it always has. Choosing how long, as the mock's retention row does, is a later setting with its own record.

The PRD's packaging gives Community a 30-day audit log, and Organization unlimited audit retention and export. Nothing in this build is held back by edition: the open-source build keeps every event and exports all of it. How long Community keeps events, and what it can export, follows the packaging decision, which hasn't been made, and then gets a record of its own. Until then, nothing here builds a limit.

## Consequences

- The owner sees what the timeline hides: refused sign-ins with the address they came from, a token's first use from a new address, and who signed out. They can narrow it and take it away as a file.
- A stolen API token learns nothing from the audit log, but a stolen session does. That's the same boundary as revealing a secret, short of the step-up, since reading the log changes nothing.
- An audit event's words are now part of what a person reads in the panel. Their writers follow the content rules already, and they stay free text, never parsed.
- When roles arrive, reading the audit log is an owner's and an admin's, which the members' record will say.
