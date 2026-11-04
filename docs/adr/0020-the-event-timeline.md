# 0020. The event timeline

- Status: Accepted
- Date: 2026-11-04

## Context

Issue #21 asks for one line of time with every revision, apply, failure, rollback and undo, and who did each from where, streamed live and filtered by service, server and actor. Each change on it links to its plan and offers undo while undo is allowed. The PRD's signature experience 3 ends with "the timeline keeps an Undo for every revision", and the Activity mock puts changes beside servers going down, alerts and access grants.

[ADR 0017](0017-the-change-api-and-the-agent-stream.md) left the timeline two sources: the changes, with their plans, progress, and who proposed and applied them; and the audit log, whose `change.*` events and new `surface` column were written for it. [ADR 0014](0014-object-model-and-store.md) says the timeline hides the change that made an install's defaults. The audit log also holds much that isn't a timeline's: wrong codes, refused tokens, every first use of a token from an address.

The panel's command bar (#24) has to reach projects and environments, and there was no way to list them.

## Decision

### `EventService` and `ResourceService`

| Method | Effect | REST |
| --- | --- | --- |
| `EventService.ListEvents` | read | `GET /v1/events` |
| `EventService.WatchEvents` | read, server stream | none |
| `ResourceService.ListResources` | read | `GET /v1/resources` |

### What is on the timeline

- **A change is one event, which moves on with the change.** Its action says where it is: `change.planned`, `change.applying`, `change.applied` or `change.failed`. A change's audit events stay in the audit log; the timeline shows the change itself, as it stands, with its plan in brief, its steps' progress, its undo deadline, and what undid it or rolled it back. The Activity mock draws a deploy as one row that goes from deploying to deployed, and a client that showed audit events would have to pair them up again across pages.
- **A change is timed from when it started applying, and from when it was planned until then.** A plan proposed yesterday and applied now is news now, so applying moves it to the top. Its ID doesn't change, and a watch sends it again with its new time.
- **Rollbacks and undos are changes of their own,** each naming the change it reverts, as ADR 0009 made them.
- **Left out:** a discarded change, which changed nothing, and Dockwell's own changes that revert nothing, which today is only the change that made the install's defaults. An automatic change that belongs on the timeline later needs another mark.
- **From the audit log, what servers did and what gave or took away a way in:**
  - servers: `server.join`, `server.renew`, `server.revoke`;
  - access: claiming the install, signing in, signing other sessions out, passkeys added and removed, authenticator codes turned on, off or unlocked, new recovery codes, API tokens made and revoked, and sign-in links, claim links and join tickets made.
- **Left in the audit log:** refusals (wrong codes, refused tokens, a suspected cloned passkey, a lock), a token's first use from an address, signing out, and the install's own setup. They answer "who tried what", which the audit log screen is for, and on the timeline they would bury the changes.
- **Events carry no secret, no spec and no address.** A change's plan is the one ChangeService already gives, with sensitive fields blanked. An audit event's free-text detail, which holds addresses, never leaves the store.

### Who, from where, and about what

- **Actors** gain two kinds, beside a person, a token and Dockwell: a **server**, as when one joins or renews its certificate, and a **shell** account, someone running `dockwell` on the control server, such as `dockwell recover`, named as `mira@hz-1`. Dockwell hasn't signed that person in, so their account and host are all it knows.
- **Surfaces** gain `SURFACE_CLI`, for the `dockwell` command: `dockwell join` on a server, and the commands beside the control plane. The remote CLI (#22) uses it too once it says which it is.
- **An event names what it is about**, other than its actor: the server that joined, the token or passkey made or removed, the person a sign-in link is for. One that is gone keeps its ID only.
- **The audit log records both where its events are written.** Each event's writer sets its `surface`, as changes' already did, and its `target`, a new column of migration 0009, by ID. The timeline reads the columns, and never the free-text detail. Auth's events come from where their actor works: a session's from the panel, a token's from the API, a shell account's from the CLI. A server joins from the CLI, with `dockwell join`, and renews its certificate automatically. Migration 0009 fills in both for the events recorded before it, from the token or passkey ID that their detail held and from their actor, once.

### Filters and pages

- Filters combine, and each one set must hold:
  - **kinds:** changes, servers or access;
  - **a server:** the changes whose plans touch it, removed servers included, and its own events;
  - **a resource:** its changes, and those of what lives in it, so a project's include its environments' and their services';
  - **an actor:** by the ID their `Actor` has; a change counts for whoever proposed it and whoever applied it;
  - **a kind of actor,** such as Dockwell on its own.
- Pages go newest first, by time and ID, with an opaque token, as `ListChanges` does.
- **A change's servers are kept with it.** `change_servers` holds the servers its plan touches, written with the change and never changed, like its plan (migration 0009, with triggers on both databases). Changes from before it are found from the deployments of their revisions. Indexes cover a change's time on the timeline, its resources and their scopes, and the audit log in time order.

### Live

- `WatchEvents` sends the newest page as a snapshot, with the token ListEvents carries on from, then what happens: a new event, a change's event again whenever the change moves on, and the IDs of events that left, such as a discarded plan. A change whose undo or rollback moves on is sent again too, as its `undone_by` or `rolled_back_by` changed.
- It learns of what happens in three ways:
  - the engine tells it of every change that moves on, as it tells `WatchChange` of one;
  - the store tells it when a write that recorded an audit event commits in this process;
  - and it reads the store again every 3 seconds, for events another process recorded, such as `dockwell recover`, or another control plane on the same Postgres later.
- Each time it reads the store again, it reads from 30 seconds before the newest event it has seen, as an event may commit a moment after a newer one, and sends only what differs from what it sent. It pages forward from there, 500 places at a time, until it has read them all, so a burst of events within those 30 seconds can't stall it. It gathers what happens over 100 ms into one message, so a change's steps don't each make their own.
- **Reading again is cheap.** Each read is one transaction. It reads where entries are first, and skips those the watch has sent that can't have changed: audit events, which never do, and changes that can't move on again, as a failed change can't. It reads the rest in full, with the names of their actors and subjects, which the watch keeps for a minute.
- **What left the timeline.** `removed` holds each change discarded since the last message that the watch's filters keep, whether or not the watch sent it, since the panel reads older pages with `ListEvents`, and whichever process discarded it. The watch finds them by the audit log's `change.discarded` events, from the same place it reads from, and says so once for each.
- The panel lists with `ListEvents` only for older pages. REST clients poll `GET /v1/events`, as ADR 0015 has them do for every stream.

### Resources by name

- `ListResources` lists the resources that exist now, of a kind and in a scope if asked, oldest first by when their IDs were made, which is the ULID that ends each ID whatever its kind, a page at a time, with their latest revision and when it was made.
- It never carries a spec. ADR 0017 left reading specs to #22 and #24, since a spec may hold secrets. Names are enough for the command bar and the CLI's choices. The one part of a spec it carries is `display_name`, for the kinds that have one, which never holds a secret.

## Consequences

- The Activity screen, its phone layout and the command bar (#24) build on these with nothing UI-only, and the CLI (#22) gets the same timeline and lists.
- Every change records its servers in one more table. Writing a change costs a row for each server its plan touches.
- An idle watch reads the store every 3 seconds. A hundred open Activity screens are about 33 small reads a second, which SQLite and Postgres serve easily; a busier install can raise the interval or wait for a notification from Postgres when the highly available control plane needs one.
- The audit log's detail stays free text, for people. Every writer of an event the timeline shows has to set its target and surface; the tests pin each one.
- Everyone who can read the API sees every event, as an install has one person until members arrive (phase 2). Roles will narrow the timeline then, as they narrow everything else.
- Alerts, backups, deploys from Git and servers going down join the timeline as their features land, each as a kind of its own.
