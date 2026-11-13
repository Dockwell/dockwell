# 0090. Removing a server, and marking one compromised

- Status: Proposed
- Date: 2026-11-13

## Context

A server can join the fleet, and nothing takes one out of it. The PRD's fleet section has a person add a second server as a designed moment and says removing it reverses that, and ADR 0088 needs a removal to react to: a server that goes takes back every certificate's key it had, re-keyed for the servers left and revoked.

What was settled already:
- [ADR 0014](0014-object-model-and-store.md#placement-and-deployments): once a server is revoked, nothing runs there. Moving or deleting a resource that was on it plans no step there, and its deployments are superseded at once.
- [ADR 0016](0016-scoped-api-tokens.md): no API token applies what may cut off access to a server.
- [ADR 0033](0033-backups-of-volumes-and-databases.md): a backup lives on the server that has its volume, and a copy in storage can be fetched onto another; a volume's data can't move between servers yet.
- [ADR 0054](0054-the-agent-keeps-its-desired-state.md): an agent told it was removed forgets what it kept, and puts nothing back.
- [ADR 0088](0088-certificates-by-dns-01-and-wildcard-domains.md#rotation-and-revocation): the control plane replaces and revokes the keys a removed server had.

## Decision

### Removing is a change with a plan

- **`ChangeService.PlanRemoveServer` plans it,** as `PlanReboot` plans a reboot: the server, what happens to what runs there, and whether it may be compromised. It's an admin's, and applying it takes a person's passkey, as it cuts off a server and revokes what it holds. No token applies it.
- **The plan is the confirmation.** It lists, beside each resource's own row:
  - **what moves:** each app, with its routes and volumes, from this server to the one chosen, the new copy first;
  - **what stops:** each app this server runs, its containers taken off it;
  - **what stays on the server,** as Dockwell lets go of it: apps left running, every volume's data, backups on its disk, its firewall rules and jails, and what Dockwell never made;
  - **which certificates by DNS it re-issues and revokes;**
  - **backups:** those on its disk go with it, and those with a copy in storage stay, to fetch onto a server that has the volume.
- **A removal can't be undone.** A removed server joins again with a new ticket, as a new server. The plan says so, and undoing the change is refused.
- **The control server can't be removed.** The panel and the control plane run on it; moving the control plane comes first.

### What happens to a server's apps is one choice

A server still running apps asks what happens to them, one choice for them all, and nothing else:

- **Leave them running,** the default. Nothing on the server changes: Dockwell lets go of its apps, routes and volumes there, which keep running as containers Dockwell didn't make, and the server's agent is told it was removed. It needs nothing of the server, so it works while the server is away.
- **Move them to** another server, picked from the fleet, the only one when there's one. Each app runs on the server chosen before its copy here goes. A volume can't take its data along yet (ADR 0033), so the plan says that each starts empty there and its data stays here.
- **Stop them.** Each app's containers come off the server, as deleting it from there does. Its volumes' data stays on the server.

Whatever the choice, Dockwell takes no step on the server for a volume, a backup or a host's settings: their data and rules stay as they are, and the control plane forgets them as the server goes.

### Marking it compromised is one step away

- **For a server that may be in someone else's hands,** the same plan with `compromised`: it revokes the server as the change commits, before any other step, so its agent's reconnects are refused from then on, and the control plane replaces and revokes every key it had at once, as ADR 0088 says.
- **It takes no step on the server,** whose agent can't be trusted to take it: its apps can move or be left, and not stopped.

### How it's built

- **Each resource on the server is an edit of the change,** as an edit of placement or a deletion, so a removal is revisions with a plan like any other change to managed state. An edit marked `forget` takes no step on the server leaving: the engine treats that server as removed for it, as ADR 0014 does once a server is revoked.
- **The server's revocation is the change's last act** where it isn't compromised: once every step has passed, the change is applied and the server revoked in one transaction, with `server.revoke` on its timeline, and its agent's stream is closed with the reason that it was removed. A step that fails rolls the change back with the server still in the fleet.
- **The agent, told it was removed,** forgets its desired state as ADR 0054 has it, and takes its certificates by DNS out of Caddy's config and its own copy.

### Numbers

Fields this adds to messages that exist take 140 to 149: `Plan.removes` 140. New messages number from 1. The agent protocol's block, 340 to 349, is held for an agent step of its own, which this needs none of.

## Alternatives considered

- **Refusing to remove a server that runs apps,** as other panels do: a person would delete or move each by hand first, which is a missing feature by the interaction principles.
- **Moving a volume's data with it,** by a backup taken here and fetched there: it needs restoring onto another server, which ADR 0033 leaves for later, and a destination. The plan says what stays, and a person can do that by hand until then.
- **Stopping apps by default:** it takes a person's apps down for a choice they may not have read, where leaving them changes nothing.

## Consequences

- A server's removal is on Activity as a change, with each resource's revision, and the server's own timeline ends with `server.revoke`.
- What a removed server leaves running isn't Dockwell's any more: its agent puts nothing back, and a person cleans it up, or joins it again and takes it on.
- Revoking the server hands its keys to ADR 0088's re-keying and revocation, which slice 2b built first.
