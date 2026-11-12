# 0026. The app screens, and what they needed of the API

- Status: Accepted
- Date: 2026-11-04

## Context

Issue #31 asks for the screens that create, show and manage apps: the list, New app, one app's overview and deployments, on phones too. [ADR 0021](0021-the-docker-driver.md) gave them a driver, Compose import and export. Three things were missing or undecided:

- **What an app is.** The object model has services in environments, and a Compose import makes several services at once. A five-service stack shouldn't read as five unrelated things.
- **What the API says about a service.** `ResourceService` names resources and never carries a spec, as a spec may hold secrets ([ADR 0020](0020-the-event-timeline.md)). So nothing said what image a service runs, or how its deploy went.
- **What acts on one service.** An edit replaces a whole spec, and no response carries one, so the panel couldn't change an image, or add a port, without reading secrets back. `UndoChange` reverses a change whole, and only its latest, so it can't go back to an older revision, or take one service of a stack back alone.

The panel uses only the public API, so whatever the screens need, every surface gets.

## Decision

### Each Compose file gets its own project

**A Compose file goes in a project of its own, named by whoever imports it or by the file's `name:`.** On screen, a project's services are one named row that folds, whose state is its most concerning service's and whose deployed time its newest's. An image app in the default project is a plain row. Each service has its own page, revisions and rollback, and that page lists what it runs with.

Before this, every file landed in the one hidden environment of someone on one server, which went wrong three ways:

- **Names collided.** If WordPress and Gitea both have a service called `db`, importing the second planned a change to the first one's database. The plan showed it, but it read as an ordinary update, and approving it lost data.
- **Nothing was isolated.** Both joined the environment's `default` network, so they could reach each other, unlike under `docker compose`.
- **They read as one stack,** whose state was the worst of everything, with no name of its own.

That is the normal case for anyone moving several apps off another panel, as each 1Panel app is its own Compose project (#77).

Where a file goes, in order:

1. **An environment named by ID** says it all.
2. **The project a request names,** by name or ID. One that exists is imported into, and the plan shows updates. One that doesn't comes to life with the plan, in one change, with the environment the request names or `production`. So undoing the import takes the project away with what is in it, and importing it again continues its history, as ADR 0014 has it for any project.
3. **The file's `name:`,** the same way. A name that can't be a project's, such as `my_app`, is refused, saying to change it or name a project; the panel offers `my-app`. An export's name is its environment's Compose project, `env_01k…`, so a file exported here goes back where it came from, and one exported elsewhere is refused.
4. **With neither,** the default project, only while that has no apps. Otherwise it is refused, with the reason `needs_project`, saying why and how to name one, as a service of the same name would change what is there. To change what is there on purpose, name `default`.

A project is already "a product or client", with its own environments, secrets and members, so a stack can later gain a staging environment without new concepts. The driver needs no change: ADR 0021's mapping stands, as each project's environment is its own Compose project, with networks of its own, and `docker compose -p` sees each.

The scope rule in ADR 0014 stands too. `scope.Tree.Shown` shows projects once there are two, so a second file makes them appear: the row's name, the breadcrumb, and a choice of project for New app from an image, which goes in the default project unless you say otherwise. Environments show only once a project has two, so the list's filter by environment stays away until then. `scope.Tree.Resolve` asks which project once there are two, for what leaves one out, so the CLI's `services create` takes `--project` from then on. A service goes by its ID or its name, with `project` and `environment` to say which, and more than one of a name is refused, saying where each is.

A project of one service has nothing to fold, so it is that service's row, under the project's name.

The other ways we considered:

- **Grouping by what connects them,** which this replaces: a stack as the services of an environment that share a network or a volume. It needed nothing stored and stayed true after an edit, but it left the collision, and two files in one environment read as one stack because they were one.
- **One environment per stack.** It would make an import's orphans and exports exact too, but environments are stages such as production and staging, and they stay out of sight until there is a second (PRD principle 4). A person's first Compose file shouldn't make them appear.
- **A flat list.** It is simple, and it is what the issue asks us not to do.
- **A project of its own page.** The routes under `/apps` are one service's, and every action is on one service. A page for the group would have nothing of its own to do yet.

What it leaves open:

- A project outlives its last service, with its networks and volumes. It shows nowhere on the list, and importing under its name again uses it. Deleting it is `dockwell projects delete`, once what lives in it has gone.
- In the default project, "Runs with" still lists what shares a network or a volume, as that is all that says two of its apps belong together.

### `ServiceService`, and what it blanks

| Method | Effect | REST |
| --- | --- | --- |
| `ServiceService.ListServices` | read | `GET /v1/services` |
| `ServiceService.ListServiceRevisions` | read | `GET /v1/services/{id}/revisions` |
| `ServiceService.DeployService` | plan | `POST /v1/services/{id}:deploy` |
| `ServiceService.RollBackService` | plan | `POST /v1/services/{id}:rollback` |
| `ServiceService.AddServicePort` | plan | `POST /v1/services/{id}/ports` |
| `ComposeService.CheckCompose` | plan | `POST /v1/compose:check` |

- **A service comes with its spec, with sensitive values blanked.** `Blanked` walks the message by its `(sensitive)` option, as plans do, so a field marked later is blanked without anyone changing the handler. A sensitive map keeps its keys, so an environment variable shows by name. That is what a plan shows already. `ResourceService` stays as ADR 0020 has it.
- **What isn't marked sensitive it shows.** A command, an entrypoint, a label and a health check are read back as they are, as seeing them is useful and they rarely hold a secret. A Compose file can write a variable's value into one, and importing it says so ([ADR 0021](0021-the-docker-driver.md), [the threat model](../security/compose-variables.md)).
- **Deployments come with the latest revision,** each with its server's name, as a server that has left the fleet still has one. One query reads them for a whole page. The change that made the revision comes with its state, as a deployment still pending under a change that has failed won't move again.
- **A revision comes with its change,** in brief, with who proposed and applied it and from where, and with what it changed, from that change's plan. A revision that deleted the service is listed, and says so. It says whether it matches the latest (`matches_latest`), and which revision a rollback put back (`rolled_back_to`), which its plan records, so no column holds it. A page of them takes a few queries, however many it holds.
- **A deploy changes the image and keeps the rest.** The control plane reads the latest spec, sets the image, and proposes it as any edit, from the revision it read: a change that lands in between makes the plan `plan_stale`, as proposing what was read would quietly undo that change. So anyone may deploy without being able to read a secret. Deploying the tag it already names pulls that tag again, as re-pulling `:latest` is the most common thing anyone does to a container, and replaces it only if that's a newer image ([ADR 0021](0021-the-docker-driver.md#what-pull-means)). Only a digest can't change, so deploying the one it runs is refused, and only where the servers say that changes nothing: the record can name an image no server runs, and deploying it again is how someone recovers.
- **Adding a port is the same.** `AddServicePort` merges one port into the latest spec on the server and proposes it, so nothing reads a secret back, and what the spec holds that no response carries survives. A port the service publishes already is refused, and so is one something else on the server has bound, by name, at plan time, for every path that plans a service: the agent asks Docker what is published, so a container Dockwell didn't make counts, a stopped one doesn't, and what the same plan takes off the port doesn't. Applying would otherwise stop what runs, fail to start it, and roll it back. A process on the server that isn't a container isn't seen, and a server that isn't connected isn't asked. It is a call of its own, not a general patch of a spec, as a surface that can't see a spec can't write one. The first deploy would otherwise end at "nothing reaches it", with no way on but deleting the app. Adding a domain will sit beside it, with routes (#27).
- **Any edit can say the revision it read, and an edit that keeps must** (#234). The deploy's check is every edit's: `Edit.base_revision` names the revision a client read the spec at, and a change that lands after it makes the plan `plan_stale`, checked as it's planned and again as it applies. An editor sends the whole spec it read, changed, and names the service's own values it keeps (`keep_env`), as no response carries them; the engine puts each back in from what's there. That takes a base revision, as the values come from what's there now and the rest from what was read, and without one a change in between would be undone with nothing to say so. `ListServices` says what each service reaches through what it mounts too, as a plan does (`host_access`), so an editor can say a passkey is coming before it plans.
- **A rollback puts one service back as a revision had it,** on the servers that revision ran on that are still in the fleet. It takes any earlier revision and touches this service alone. Where the revision it replaces was made by a change on its own, the rollback reverts that change, so the timeline marks it undone, as undoing it would. A rollback of one service of an import doesn't: the rest of that change stands. It brings back the spec, with which variables and secrets it uses, the image by the digest that revision ran where a server can still get it, and not the data in a volume, nor what a variable or secret held then: every change pins what it uses to the latest (ADR 0022), so a rollback runs with today's values. Its plan says each in a note: a rollback past a migration may leave a version that can't read its data, and one past a rotation still has the new secret. To go back on a value, change the value. **Going back to a revision that does nothing is refused,** which only the servers can say. A revision can match the record (`matches_latest`, which leaves pins out) while a rollback failed, or a container was stopped or removed by hand, so that still plans what puts it back, and it reverts nothing, so the change that got there keeps its undo. After an automatic rollback, r5 restored r3, so what is offered first is the newest revision that differs, and r3 says it matches the latest, with its plan a click away.
- **A plan can carry notes.** `Plan.notes` says what a person should know that no step says, as it is about what stays as it is or what can be read. A note never stops anyone applying a plan, and isn't in its fingerprint. Each has a reason, for a surface with words of its own, and a message for one without.
- **`CheckCompose` reads a file as `ImportCompose` would, and records nothing.** A form can check a file on every keystroke without filling the timeline with discarded plans. It runs `Parse` and each spec's `Validate`, so the refusals are the same ones. A refusal comes back as a field of the response, with the line it is about, as a refused file isn't a failed call: the line YAML's parser gave up on, the last it names, and words for people in place of its own. It stops at the first one, as `Parse` does. What the file says of itself comes back whether or not it can be read further: its `name:`, its variables by name, each saying whether it needs a value, and where it writes one in plain sight. Their values never do. It is a POST, as a file doesn't fit in a URL. Its effect is `plan`, as `ImportCompose`'s is, though it records nothing: reading a file takes real work, and a read-only token has no use for it.
- **A plan says where it lands, once there's a choice.** `Plan.places` names the project once there are several, what the change makes counting, and the environment once its project has several. The CLI prints it above the steps. A file exported from production and imported with `environment: staging` goes to staging, in the project it came from, and this is why that can't read as the wrong one again.
- **A plan's sheet counts what it lists.** The environment that comes with a new project stays out of sight, so its sheet says 4 to create where Activity and the CLI say 5. They count every resource a plan makes, from `PlanSummary`, as the CLI lists that environment too. A count of 5 over 4 rows would be something to go looking for, and nothing here depends on the two agreeing, so each keeps to what it shows.
- **A file that goes where services run says so.** `Plan.notes` has `updates_project`, naming the project, what it runs, and what `remove_orphans` would delete, for the API and the CLI as for the panel, as a file's `name:` can match a project by chance. Its summary says "Update", not "Run".
- **Services are a noun of the CLI and a kind.** `dockwell services` now has `list`, `revisions`, `deploy`, `rollback` and `add-port` beside `create`, `update` and `delete`, under one command, and each takes a service's name, as a kind's verbs do, or its ID.

### What "live" means, and how it stays live

- **A service's state is what Dockwell knows, and it says no more.** Its status comes from the latest revision's deployments and whether their servers are connected: live, deploying, waiting for a server, failed, a server offline, or rolled back. The word is "Live", not "Healthy": it means the revision deployed and passed the check its step made. Nothing watches an app's health after that until rollouts come (#28). The shape and colour are `healthy`'s, as the deployments mock draws "Live". "Live" also needs the fleet to have said its servers are there: until it answers the word is "Deployed", and once it has, one that isn't listed has left, and with none left the word is "Not placed". The revision that runs says what the header says about that. What failed stays failed, whatever is still marked as on its way.
- **A deploy that failed and rolled back says so.** The service runs what it ran before, so its deployment is live, but `rolled_back_change_id` names the failed change, and the panel shows "Rolled back" with the way to see why, until the next deploy.
- **The list follows the timeline.** It watches `WatchEvents` for changes alone, and reads `ListServices` again when one moves on: a new state, or a step that ends. A plan under review, or a pull's progress, which only rewrites a step's message, reads nothing. So an idle screen reads nothing, and a burst reads once. A stream of services would mean a second thing telling the same changes apart, for the same reads. A first snapshot says nothing about what moved, so a screen reads again on arriving if nothing watched since the list was read, as when a deploy finished while another screen was open, and the list was still within its 30 s. Moving straight between two screens that show services reads nothing. A stream that ends with no error, as when the control plane stops, is a drop like any other: it says "Reconnecting", and catches up when it is back.
- **Reading everything is fine at this size.** The panel reads every service, a page of 1,000 at a time, into one cached list that the list, each app's page and the command bar share, so moving between them reads nothing. Seventy apps are a small message. A fleet of thousands will want paging in the list before anything else does.

### Progressive disclosure on these screens

- With one server there is no server column, filter or placement choice, and no rollout list. From the second server each appears (PRD principle 4), in a place that was held for it.
- There's no choice of environment until a project has two, and a filter box only from six apps.
- A column shows only when the API has its data. Replica counts come with #28, domains with #27, and metrics when a service has any. New app offers image and Compose, as Git and catalog sources arrive in phase 2.
- **New app asks for an image.** The name comes from it unless you say otherwise. Ports, variables and volumes wait behind a button each.
- **New app asks a Compose file for a name,** filled in from its `name:`, and optional only while the default project has no apps. It says when that name is here already, so the plan updates it. What a re-import would remove says how many, and lists them only when asked, on a control that could delete them all.
- **Restart isn't on these screens yet.** It came with #28 (`RestartService`, ADR 0027), with a plan and no revision, and `dockwell services restart` calls it. The panel has no button for it yet.

### A name that's here already, by chance or on purpose

*Added with #519, as the install sheet learnt to start on a free name and check a typed one as it's typed.*

A file's `name:` can match a project by chance, and importing it then updates that project, which reads as an ordinary update in the plan. So New app now starts the name at the next that's free when the file's name is taken (`shop-2` for `shop`), says "`shop` is here already" with **Update shop instead** and **Open shop** beside the field, and plans a new project unless someone takes the update. A name typed or taken on purpose that a project has keeps meaning an update, as this record says, and says so with **Use shop-2** and **Open shop** beside it. "Free" or "Here already" sits in the field as it's typed. The API is as it was: a request that names a project that exists still imports into it.

### Refusals at their field

The API writes one message for a whole change. New app reads which field it is about from what it names, such as the path or the image, and shows it there, and shows it above the form when it names none. Compose refusals carry a line, and the field marks it and brings it into view. The same words reach the CLI, so they stay the API's, and the panel only leaves out the environment's ID, which says nothing on an app's page. A word of the file's own comes in backticks, which the panel sets in mono. A file over the API's limit is said at the field, without asking the API, as what comes back from the limit on a request is a protocol error.

### What the plan sheet gained

`ChangeSheet` takes an opening of its own, `plan`, whose `make` makes the plan any way it likes, and holds it as it holds its own: closing discards it. A form proposes first, so a refusal stays at the field, and opens the sheet only with a plan to review. Cancelling the plan comes back to the form as you left it.

### Testing without Docker

`scripts/fakedocker` serves `internal/docker/dockertest` on a socket, and `dockwell` finds it through `DOCKER_HOST`, as it finds a real one. `make e2e` runs the panel's flow on it, in CI's own job: an image through its plan to the list and its page, a port, another image, its revisions, a rollback, a deploy that fails and rolls back on its own, export, a Compose file refused, two files with a service of one name side by side, and deleting. `make docker-panel-e2e` runs the same flow on real Docker in CI's Panel end to end on Docker job, where publishing a port and reaching it means something: the `Server` header tells the two nginx images apart. A real pull, a real container exiting, and real health checks are only shown there. Each step that says what runs asks Docker itself, through its API on the socket, as the API's own word proves little. What the run made is removed whether or not it passes, so a dev machine keeps nothing on the port.

## Consequences

- **The panel can change the image of a running app and add a port, and nothing else yet.** Taking a port away, and volumes, are App-Settings, in phase 2, or `dockwell services update`, or a new Compose file. Deploy would be the wrong place, as it means "run this image". Variables and secrets come with #29.
- **Someone with several Compose files sees projects.** A second file makes the project level appear, by ADR 0014's rule. That is one choice in New app from an image, which starts on the default project, and one more flag on some commands.
- **A rollback of one service of an import isn't linked to the change it undoes.** It reverts nothing, as the rest of that change stands, so it reads as a rollback through `rolled_back_to` alone, on that service's own page.
- **`CheckCompose` is as heavy as parsing a file.** The worst file ADR 0021 could make takes about 2 s, and the form asks 300 ms after the last key. Like `ImportCompose`, it takes a session or a write token, and it keeps to every limit `Parse` has: the file's size, what its aliases come to, how deep it and its variables nest, and how many files are read at once. It takes what an import takes beside the file, `env` and `directory`, so it says what importing it would. It goes through the same limited child first, and what it reads of a file for the form comes after that, so one made to cost too much is stopped there, and says nothing of itself.
