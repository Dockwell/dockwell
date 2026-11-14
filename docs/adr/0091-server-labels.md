# 0091. Server labels

- Status: Proposed
- Date: 2026-11-13

## Context

The PRD's node inventory row (P0) lists tags and groups, and two P1 rows build on them: fleet actions across a tag selector, and pools, label-based groups of nodes. The fleet's mocks group and filter by them: `Servers-List.dc.html` has a selector of `pool:web region:eu` chips over the list, "Group by region" beside it, and a server's labels among its facts; `Overview-Fleet.dc.html` groups the fleet by region. #319 asks for them. A server has no labels yet, so the fleet overview shows one group.

What was settled already:
- PRD principle 2: every change to managed state is a revision with a plan. A label decides where a fleet action or a pool reaches, so labels are managed state.
- [ADR 0064](0064-a-change-on-a-server-installing-updates-and-rebooting.md): a server's host, kind `server.host`, is each server's own record, edited by changes such as a reboot.
- [ADR 0017](0017-the-change-api-and-the-agent-stream.md): a server joins with a single-use ticket from the control plane.

## Decision

### Labels are a field of the server's host

- **`ServerHost.labels` (20), a map of key to value,** one revisioned record per server, with history and undo as any edit has. A new kind would add a record to keep in step with the server for nothing more.
- **A change of labels alone takes no step on the server.** The host kind keeps them to the control plane (`resource.ControlOnly`): its agent is never sent them, a drift check never reads them as missing there, and the host's plan has nothing to do. Such a change applies at once, with the server online or not, and its plan says so in a note.
- **Server (20) gives them,** as `labels`, on every read and on the fleet's stream, which a change of them updates at once.

### Keys and values are Kubernetes labels

- **A key** is a name of at most 63 characters, letters, digits, `-`, `_` and `.`, starting and ending with a letter or digit, with an optional prefix, a DNS subdomain of at most 253 characters, and `/`: `region`, `example.com/tier`. A value is empty, or a name as a key's is.
- **The panel and the CLI write a label as `key:value`,** as the mocks do, and a selector as such terms with spaces between: `pool:web region:eu`. A key alone matches a server that has it; `!` in front of a term matches a server that doesn't. Neither a key nor a value can hold `:` or a space, so a term reads one way.
- **One parser,** `internal/labels`, reads labels and selectors, for listing servers now and for fleet actions and pools later. The panel's checks as you type follow the same rules, and say the same words.

### Setting them

- **`ChangeService.PlanServerLabels`** plans setting and removing labels on one server or several in one change, each as `key:value`, as `dockwell servers label --server-ids srv_… --set region:eu --remove arch`. It's an admin's, as the server's host is. A token may apply it: it takes no step on any server. Setting what a server has already is refused, as there's nothing to change.
- **At join,** the ticket carries them: `CreateJoinTicketRequest.labels` (20) and `JoinTicket.labels` (20), kept with the join token and never in the ticket's value. `dockwell token create --label region:eu` and the Add server sheet both take them. As the server's agent first connects, the change that adds its host gives it those labels, on the timeline as any change.
- **On a server's page,** its labels are beside its name, and on a phone in its status card, with Add labels or Edit beside them: a sheet whose picker offers the labels the fleet uses, and each key with its colon for a value of its own. What it can't take says why as it's typed, in the words the server uses.

### Reading them

- **`ListServersRequest.selector` (20)** lists the servers a selector matches, as `dockwell servers list --selector "pool:web region:eu"`.
- **Servers** has the mock's selector over the list: chips of `key:value`, an input that takes another with the fleet's keys and values offered, and how many match. Group by groups by any key servers have, region first where they have it, with what has none last.
- **The fleet overview** groups by region once servers have one, with the Region control, and is one group until then.

### Numbers

Fields this adds to messages that exist take 20 to 29: `ServerHost.labels`, `Server.labels`, `JoinTicket.labels`, `CreateJoinTicketRequest.labels` and `ListServersRequest.selector`, each 20. New messages number from 1. No agent protocol: the ticket's labels stay on the control plane.

### As built

- **Steps on the host keep it as it is.** A reboot, an install or a unit's action takes the host's stored spec, so no such edit can lose its labels, and an edit that says a spec with one of those steps is refused.
- **The fleet's stream reads labels again as each change moves on,** so a change of them shows at once, and a host made at join shows its labels as its change applies.
- **The plan names a host by its server,** writes its labels as `key:value`, and is titled for the servers it labels; its `labels_only` note is said quietly, as what it means is that nothing runs.
- **A host not made yet,** as a server's whose agent hasn't connected since it joined, starts from the labels it joined with, so labelling it doesn't lose them.

## Alternatives considered

- **A kind of its own, `server.labels`:** the same revision and plan, and a second record per server to keep in step with its host.
- **Labels in the ticket the agent carries:** the agent would send them back as it joins, which needs the agent protocol, and a ticket would say them to whoever holds it.
- **`key=value`, as Kubernetes writes a selector:** the mocks write `pool:web`, and the generated CLI's map flags already take `KEY=VALUE` for environments, which a label isn't.

## Consequences

- Fleet actions and pools select servers with the same selectors, read by the same parser.
- A label is on Activity as a change, with its undo, and the server's history.
