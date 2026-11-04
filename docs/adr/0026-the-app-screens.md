# 0026. The app screens, and what they needed of the API

- Status: Accepted
- Date: 2026-11-04

## Context

Issue #31 asks for the screens that create, show and manage apps: the list, New app, one app's overview and deployments, on phones too. [ADR 0021](0021-the-docker-driver.md) gave them a driver, Compose import and export. Three things were missing or undecided:

- **What an app is.** The object model has services in environments, and a Compose import makes several services in one environment. A five-service stack shouldn't read as five unrelated things.
- **What the API says about a service.** `ResourceService` names resources and never carries a spec, as a spec may hold secrets ([ADR 0020](0020-the-event-timeline.md)). So nothing said what image a service runs, or how its deploy went.
- **What acts on one service.** An edit replaces a whole spec, and no response carries one, so the panel couldn't change an image without reading secrets back. `UndoChange` reverses a change whole, and only its latest, so it can't go back to an older revision, or take one service of a stack back alone.

The panel uses only the public API, so whatever the screens need, every surface gets.

## Decision

### On screen, an app is a service, and a stack is the services connected to each other

**A stack is the services of an environment that share a network or a volume.** The list shows it as one row with its services under it, fold or unfold. Its state is its most concerning service's, and its deployed time its newest's. Each service has its own page, revisions and rollback, and that page lists what it runs with.

Why what connects them, and not where they came from:

- **It is what a stack is.** A Compose file's services share its default network unless it says otherwise, which is why they can reach each other by name. That is the fact worth showing: these services depend on each other.
- **It stays true.** A group read from the change that first made them would split when a re-import adds a service, or when one service is made by hand and joined to the network. What is connected now needs no history.
- **It needs nothing stored.** A stack table, or a label set at import, would be a second idea of what a Compose project is, beside ADR 0021's: an environment is one Compose project. It would also read differently through `docker compose`, which has no such thing.

The other ways we considered:

- **One environment per stack.** It would make an import's orphans and exports exact, but environments are stages such as production and staging, and they stay out of sight until there is a second (PRD principle 4). A person's first Compose file shouldn't make them appear.
- **A flat list.** It is simple, and it is what the issue asks us not to do.
- **A stack of its own page.** The routes under `/apps` are one service's, and every action is on one service. A page for the group would have nothing of its own to do yet.

What it leaves open:

- A stack has no name of its own. Its title is its services' names, "api, db, web and 2 more". The `name` of a Compose file isn't kept, as an environment's ID is its project's name. If people want names, a label on the network could carry one without a new kind.
- Two files imported into one environment share its `default` network, so they read as one stack. That is true: they can reach each other. To keep them apart, use two environments.

### `ServiceService`, and what it blanks

| Method | Effect | REST |
| --- | --- | --- |
| `ServiceService.ListServices` | read | `GET /v1/services` |
| `ServiceService.ListServiceRevisions` | read | `GET /v1/services/{id}/revisions` |
| `ServiceService.DeployService` | plan | `POST /v1/services/{id}:deploy` |
| `ServiceService.RollBackService` | plan | `POST /v1/services/{id}:rollback` |
| `ComposeService.CheckCompose` | plan | `POST /v1/compose:check` |

- **A service comes with its spec, with sensitive values blanked.** `Blanked` walks the message by its `(sensitive)` option, as plans do, so a field marked later is blanked without anyone changing the handler. A sensitive map keeps its keys, so an environment variable shows by name. That is what a plan shows already. `ResourceService` stays as ADR 0020 has it.
- **Deployments come with the latest revision,** each with its server's name, as a server that has left the fleet still has one. One query reads them for a whole page.
- **A revision comes with its change,** in brief, with who proposed and applied it and from where, and with what it changed, from that change's plan. A revision that deleted the service is listed, and says so.
- **A deploy changes the image and keeps the rest.** The control plane reads the latest spec, sets the image, and proposes it as any edit. So anyone may deploy without being able to read a secret. The same image is refused: it would plan nothing.
- **A rollback puts one service back as a revision had it,** on the servers that revision ran on that are still in the fleet. It takes any earlier revision and touches this service alone. Where the revision it replaces was made by a change on its own, the rollback reverts that change, so the timeline marks it undone, as undoing it would. A rollback of one service of an import doesn't: the rest of that change stands. It brings back the spec, secrets included, and not the data in a volume.
- **`CheckCompose` reads a file as `ImportCompose` would, and records nothing.** A form can check a file on every keystroke without filling the timeline with discarded plans. It runs `Parse` and each spec's `Validate`, so the refusals are the same ones. A refusal comes back as a field of the response, with the line it is about, as a refused file isn't a failed call. It stops at the first one, as `Parse` does. The file's variables come back too, by name, each saying whether it needs a value; their values never do. It is a POST, as a file doesn't fit in a URL. Its effect is `plan`, as `ImportCompose`'s is, though it records nothing: reading a file takes real work, and a read-only token has no use for it.
- **Services are a noun of the CLI and a kind.** `dockwell services` now has `list`, `revisions`, `deploy` and `rollback` beside `create`, `update` and `delete`, under one command.

### What "live" means, and how it stays live

- **A service's state is what Dockwell knows, and it says no more.** Its status comes from the latest revision's deployments and whether their servers are connected: live, deploying, waiting for a server, failed, a server offline, or rolled back. The word is "Live", not "Healthy": it means the revision deployed and passed the check its step made. Nothing watches an app's health after that until rollouts come (#28). The shape and colour are `healthy`'s, as the deployments mock draws "Live".
- **A deploy that failed and rolled back says so.** The service runs what it ran before, so its deployment is live, but `rolled_back_change_id` names the failed change, and the panel shows "Rolled back" with the way to see why, until the next deploy.
- **The list follows the timeline.** It watches `WatchEvents` for changes alone, and reads `ListServices` again when one moves on: a new state, or a step that ends. A plan under review, or a pull's progress, which only rewrites a step's message, reads nothing. So an idle screen reads nothing, and a burst reads once. A stream of services would mean a second thing telling the same changes apart, for the same reads.
- **Reading everything is fine at this size.** The panel reads every service, a page of 1,000 at a time, into one cached list that the list, each app's page and the command bar share, so moving between them reads nothing. Seventy apps are a small message. A fleet of thousands will want paging in the list before anything else does.

### Progressive disclosure on these screens

- With one server there is no server column, filter or placement choice, and no rollout list. From the second server each appears (PRD principle 4), in a place that was held for it.
- There's no environment choice until there is a second environment, and a filter box only from six apps.
- A column shows only when the API has its data. Replica counts come with #28, domains with #27, and metrics when a service has any. New app offers image and Compose, as Git and catalog sources arrive in phase 2.
- **New app asks for an image.** The name comes from it unless you say otherwise. Ports, variables and volumes wait behind a button each.
- **Restart isn't here.** Nothing plans one: a restart changes no spec, so it would be a new kind of step for the driver and the agent. It should come with rollouts, rather than as a change that writes a revision and does nothing.

### Refusals at their field

The API writes one message for a whole change. New app reads which field it is about from what it names, such as the path or the image, and shows it there, and shows it above the form when it names none. Compose refusals carry a line, and the field marks it. The same words reach the CLI, so they stay the API's, and the panel only leaves out the environment's ID, which says nothing on an app's page.

### What the plan sheet gained

`ChangeSheet` takes an opening of its own, `plan`, whose `make` makes the plan any way it likes, and holds it as it holds its own: closing discards it. A form proposes first, so a refusal stays at the field, and opens the sheet only with a plan to review. Cancelling the plan comes back to the form as you left it.

### Testing without Docker

`scripts/fakedocker` serves `internal/docker/dockertest` on a socket, and `dockwell` finds it through `DOCKER_HOST`, as it finds a real one. `make e2e` runs the panel's flow on it: an image through its plan to the list and its page, another image, its revisions, a rollback, export, a Compose file refused and then run, and deleting. `make docker-panel-e2e` runs the same flow on real Docker in CI's Docker job, where publishing a port and reaching it means something. A real pull, a real container exiting, and real health checks are only shown there.

## Consequences

- **The panel can't change anything but the image of a running app yet.** Ports and volumes are set when it is made. Changing them is App-Settings, in phase 2, or `dockwell services update`, or a new Compose file. Deploy would be the wrong place, as it means "run this image". Variables and secrets come with #29.
- **A rollback doesn't always read as one on the timeline.** One that reverts nothing, as of one service of an import, is marked only by its summary. Keeping which revision it went back to would take a column.
- **`CheckCompose` is as heavy as parsing a file.** The worst file ADR 0021 could make takes about 2 s, and the form asks 300 ms after the last key. Like `ImportCompose`, it takes a session or a write token, and it keeps to every limit `Parse` has: the file's size, what its aliases come to, how deep it and its variables nest, and how many files are read at once.
