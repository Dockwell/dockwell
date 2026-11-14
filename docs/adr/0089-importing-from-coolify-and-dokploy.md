# 0089. Importing from Coolify and Dokploy

- Status: Accepted
- Date: 2026-11-13
- Amended: 2026-11-13, after the security review of #608: a label that holds a credential is refused for every take-on; auto deploy, root access and Coolify's backups said in each step and note; folder words and Traefik's file checked as they're read; stopping the panel comes next

## Context

Issue #604 asks for the PRD's Migration row "Importers for Coolify and Dokploy" (P1): people leaving either need what [ADR 0056](0056-importing-a-1panel-server.md) gives people leaving 1Panel. This record follows that one's shape: read what the panel runs, say what Dockwell would do with each thing and why, and plan taking on what can come, which nothing changes until a person applies.

It was written from each panel's own source, never a live server:
- Coolify v4.7.5, commit `378ce37`;
- Dokploy v0.31.2, commit `a16cf89`.

## Decision

### Where each keeps what it runs

**Coolify** runs every resource it deploys through Docker Compose, with the resource's UUID as the Compose project's name, from a folder of its own under `/data/coolify`:

| What | Its folder | Its containers' labels |
| --- | --- | --- |
| An application, from Git or an image | `applications/<uuid>/` | `coolify.type=application` |
| A service, from Coolify's templates | `services/<uuid>/`, with its `.env` | `coolify.type=service`, and each container's `coolify.service.subName` |
| A database | `databases/<uuid>/` | `coolify.type=database`, `coolify.database.subType`, such as `standalone-postgresql` |

Each container also has `coolify.managed=true`, `coolify.resourceName`, `coolify.projectName` and `coolify.environmentName`, the names a person gave them, slugged, and `coolify.pullRequestId`, which isn't 0 on a pull request's preview. Its domains are in the proxy's labels: Traefik's `Host(...)` rules, or Caddy's `caddy_N` addresses. Coolify's own containers are `coolify`, `coolify-db`, `coolify-redis`, `coolify-realtime`, `coolify-proxy` and `coolify-sentinel`; its version is its own image's tag.

**Dokploy** keeps its files under `/etc/dokploy`, and runs:

| What | How | Where |
| --- | --- | --- |
| A Compose app, of the type `docker-compose` | `docker compose -p <appName>` | `compose/<appName>/code/`, with its `.env` |
| A Compose app, of the type `stack` | `docker stack deploy`, a Swarm stack | The same |
| An application, from Git or an image | A Swarm service, `<appName>` | `applications/<appName>/` for its code |
| A database: PostgreSQL, MySQL, MariaDB, MongoDB, Redis or libSQL | A Swarm service, `<appName>` | Its volume, by name |

What it runs carries `dokploy.project`, `dokploy.environment` and `dokploy.application`, the names a person gave them. An application's domains are Traefik's rules in `traefik/dynamic/<appName>.yml`; a Compose app's are in its file's labels. Dokploy's own are the Swarm services `dokploy`, `dokploy-postgres` and `dokploy-redis`, and the container `dokploy-traefik`; its version is its own image's tag.

### How it reads: on the server, by its agent, from Docker and the panel's folders

One agent call, `ReadPanel`, reads what a panel runs, for Coolify or Dokploy, and returns what was found with nothing secret in it. The control plane decides what Dockwell would do with each.

- **It reads Docker and the panel's folders, never its database or its API.** Everything the import needs is in what Docker runs, the labels the panel gave it, and the files in its folders: names, kinds, Compose projects, domains, and which containers are the panel's own. So no password to the panel's database is read, nothing is asked of its API, and a person gives no token. What only the panel's database knows, such as which of its templates a service came from, a Git source's branch, or its scheduled tasks and backups, is said to stay with the panel, and reading it waits for a step of its own (below).
- **Read-only.** Docker is listed and inspected, and the panel's files are read from fixed paths under its folder, refused where their last part is a link, as ADR 0056 reads 1Panel's. A folder is named only from plain words of a label, `^[a-z0-9-]+$`, and only under the panel's folder, as anyone who runs a container can write its labels. Dokploy's Traefik file opens with `O_NOFOLLOW|O_NONBLOCK` and is read only where the open file is a regular one, so a link is never followed and a pipe never waited on.
- **An agent with no Docker reads no panel,** and the import says to check Docker there rather than to update the agent.
- **What it sends is what a plan shows.** Names, kinds, Compose projects, container names, images, domains and paths. Never an app's environment, a label's value beyond a domain, or a file's contents. The values taking something on needs travel later, sealed, with the plan that takes it on, as ADR 0031 says.
- **Versions.** It's written from Coolify 4.7 and Dokploy 0.31. Another version is read the same way, by labels and folders, and the answer says it wasn't one read for this record.

### What it does with each thing

As in ADR 0056: every import is a plan a person reviews and applies; one thing that can't come doesn't stop the rest; each app comes into a project of its own, named for it, or `<name>-coolify` or `<name>-dokploy` where Dockwell has one of that name; what was taken on before says so; and a new import of a server stands in for the person's last.

**Taken on in place, with no restart** (ADR 0031), reading each Compose project's containers as `AdoptCompose` does:

- **Each of Coolify's applications, services and databases.** A service's containers come as one project, its services named as Coolify named them. An application built from Git keeps the image Coolify built on the server; building it again from Git in Dockwell is set up from the app's page, as for any app.
- **Each of Dokploy's Compose apps of the type `docker-compose`.**
- **A label that holds a credential is refused, for every take-on.** A plan shows a container's labels as they are, and a panel's basic auth puts each user's password hash on its proxy's labels: Traefik's `basicauth.users` and `digestauth.users`, Caddy's `basic_auth` and `basicauth`. So a label counts as shown, as a value of the environment elsewhere in a spec does (ADR 0031), by its key (those, an `Authorization` header, or a key named for a token, secret, password or key) or by its value (a crypt, bcrypt, Argon2 or htpasswd SHA hash, a `Bearer` or `Basic` header value, or a token in its issuer's form). The agent says so, and the control plane looks again, for an agent from before. The refusal names the label and never its value, and an import's step says how: turn off Coolify's HTTP Basic Authentication, or Dokploy's basic auth, then import again. A sealed form for such a label, as an environment's values have, would let one come as it is, and is a step of its own.
- **What deploys again on a push says so.** Coolify's Auto deploy and Dokploy's Autodeploy are on by default, and a push then replaces what was taken on or starts a second copy on its volumes. The agent reads which app comes from Git by what the panel leaves: an image Coolify tagged with a commit, or built with docker compose under the app's project, or an app of more than one container; a Git checkout in Dokploy's code folder. Each such step says to turn it off before applying, and so does the plan's note. Stopping the panel (below) ends it for every app at once.
- **What is as good as root says so first.** A step whose app reaches Docker's socket, the server's own network, a system path or privileged mode says it, such as "With Docker's socket, it's as good as root on web-1", before the plan asks for a passkey.
- **Coolify's backups of a database stop** at Dockwell's first image change, as they run in its container by name. The plan's note says so, and to back it up with Dockwell's backups.
- **Their volumes stay where they are,** named volumes by name and folders by path, as taking on never moves data. A plan's note says what the panel's delete would do to them: Coolify's removes the resource's volumes unless that box is cleared, and its folder; Dokploy's runs `docker compose down`, with `--volumes` when that box is ticked, and removes the app's folder. So neither is deleted in the panel.

**Left as it is, with the reason:**

- **A pull request's preview,** which Coolify makes and removes with its pull request.
- **What Dokploy runs as a Swarm service,** its applications, databases and stacks. Swarm keeps running a service whatever happens to its container, so Dockwell can't take one on in place. Each is made again in Dockwell, on the same volumes, by a later step that stops the service as Dockwell's copy starts, with a moment down, which its plan says.
- **Domains and certificates.** Each app's domains are listed with it, and come as routes to its service once the panel's proxy hands ports 80 and 443 to Dockwell's ingress, which gets a certificate for each. That's a step of its own, after the apps, as ADR 0056 hands over OpenResty's: the plan stops the panel's proxy, and its undo starts it again.
- **What only the panel's database knows:** a service's template, an application's Git source, scheduled tasks and backups. Each import says so. Where an app came from one of the panel's templates, a move to the catalog's entry converted from that template (ADR 0085) waits for the same reason as ADR 0056's store apps: an entry runs on named volumes and loopback ports, and an upgrade from it would plan new, empty volumes.
- **The panel itself.** Its own containers are listed by name, and left running.

### Stopping the panel after

Once its apps are taken on, the panel goes on running beside Dockwell until a person stops it. The next step, brought ahead of the screen and the domains after the security review, gives that one step, as stopping the panel also ends its auto deploys and its scheduled tasks: a change that stops the panel's own containers and services, planned as an action on the server's host is ([ADR 0082](0082-acting-on-a-servers-system-services.md)), with no revision, whose undo starts them again. Nothing of the panel is deleted: its database and folders stay, so starting it again brings it back as it was.

#### Stopping the panel, as built

- **`ImportService.PlanStopPanel`** names a server and a panel, and plans one edit of the server's host, flagged as stopping that panel. It's a plan method for an admin, with the install reach, and a person's: a token can't plan one, as with every action on the server's host. Where nothing of the panel runs, nothing is planned and it says so; an agent from before says to update it (`Hello.stops_panels`).
- **What stops.** Coolify's `coolify`, `coolify-realtime`, `coolify-sentinel`, `coolify-redis` and `coolify-db`, in that order: the panel first, so it doesn't see its database go. Dokploy's Swarm services `dokploy`, `dokploy-redis` and `dokploy-postgres`, scaled to none. One already stopped, and staying so, isn't planned.
- **What stays.** The panel's proxy, `coolify-proxy` or `dokploy-traefik`: it serves the domains of what was taken on until those come as routes, a step of its own, which the plan says. What the panel ran isn't touched.
- **It stays stopped.** A container's restart policy becomes `no` before it stops, as Coolify starts its own with `always`, which Docker and a reboot would start again. A Swarm service's update carries its whole spec, as Swarm takes it: the agent reads it as Docker has it, keeps every field and number as it was, changes the replicas alone, and names the version it read, so a change made meanwhile is refused, not written over.
- **The plan keeps how each stood:** running or not, its restart policy, or its replicas. Its fingerprint covers them, so a part that moved since makes the plan stale. A stop that resumes keeps what its plan read, not what it has stopped since.
- **Undoing it starts each again as it stood,** its state first, the panel last, as a change with its own plan. That start has no undo of its own; stopping again does it.
- **The agent acts on the panel's own alone.** What a start puts back comes from a plan, so the agent takes only the panel's own names, never its proxy, a restart policy Docker has, and at most 16 replicas.
- **The panel offers it** on a server's Overview where Coolify's or Dokploy's own app runs, as a callout a person can put away, until the import's screen gives it its row. The CLI's is `dockwell import stop-panel`.

#### Swarm-run apps, as built

- **Dokploy's applications and databases come again in Dockwell.** Each runs as one Swarm service, which Swarm keeps running whatever happens to its container, so it can't be taken on in place. The agent's read names each service's running task (`PanelApp.tasks`), and the plan reads that container as the take-on does, so its image, environment, volumes and server paths come as they run, with values kept out of the plan (ADR 0031). What only Swarm, Traefik and Dokploy read of it is left behind: their labels, and the networks it joined, as a service joins its environment's.
- **Each of Dokploy's projects comes into one of Dockwell's,** named for it, and each app keeps its appName as its service's name, so apps that reached a database by that name still do.
- **The switch is in the same change.** The server's host scales each Swarm service to none first, a panel stop with `apps` set, which the agent takes only for a service Dokploy labelled as an app's; then Dockwell's copy is made on the same volumes, so each is down for a moment, which the plan says. Its undo deletes Dockwell's copies first, then scales each service back to what it ran.
- **Its domains come over** with the handover, from Dokploy's Traefik file for the app: each router's hosts and path, and the port of the server URL its service names, routed to Dockwell's copy.
- **A stack comes too,** a Compose app of the type `stack`: each of its services from its running task, into a project of the stack's own, named as the stack named them, so they still find each other; a name another stack also uses can't clash. Its routes come from its services' labels, and its services scale to none as their copies start.
- **What Swarm published comes too:** a port a service published, through its routing mesh or on its node, as Dokploy's external port for a database does, is published by the copy on the server, which a person approves.
- **What stays:** a Swarm-run app with a service that has no task running, saying why.

### A token, where one is needed

Nothing here needs one. If a later step reads what only the panel's API knows, the token a person gives it is a secret: sent to the agent sealed for that read, used once against the panel on the same server, never kept or logged, and never shown back. A panel's database is never written, and not read at all.

### The screen

A server's Move in tab offers each panel it finds running, beside 1Panel, and an import's page is a dry-run report first, as ADR 0056's is: what comes over, what's left behind and why, and Review the plan. The CLI says the same with `dockwell import coolify --server-id <server> --plan-only` and `dockwell import dokploy`.

#### Domains and the screen, as built

- **The read says where each domain goes** (`PanelApp.routes`): a Traefik router's hosts and `PathPrefix`, with the port of the service it names, or the one service the labels have; a Caddy address and the port its `reverse_proxy` names. A domain whose labels give no port reads 0. Nothing else of a label leaves the server.
- **Each domain is a step of the report** (`IMPORT_KIND_DOMAIN`), which waits for the proxy (`IMPORT_WAITS_FOR_PORTS`): a route to the app's service on its port, once its app is taken on. A domain of what stays in the panel, a preview or a Swarm-run app, says it stops answering; one with no port says to add it from its app's page. Once a route serves it, it reads as in Dockwell.
- **`ImportService.PlanPanelDomains`** (`dockwell import domains`) plans the handover as one change: the server's host stops the panel's proxy first, as a panel stop with `proxy` set acts on the proxy alone, then a route is made in each taken-on app's environment, to the service the container runs as there, named for its domain. The route check that runs as a plan is made counts the proxy as gone, as the change stops it before any route applies. The plan's note says which domains come, that each is down until its certificate comes, which stop answering, and that undoing it puts the panel back. A person plans it; with nothing taken on there's nothing to hand over, and with everything routed it says so.
- **Its undo deletes the routes first, then starts the proxy,** each as it stood: a step that starts a panel or its proxy runs after every delete, so Dockwell's ingress lets the ports go before the proxy takes them back. That start has no undo of its own.
- **What a domain can't bring stays with it: an allowlist, not a denylist.** A domain comes over only where everything about it is a form Dockwell reads in full, as a plain route drops whatever else guarded it:
  - **a rule** of `Host(...)`, optionally `&& PathPrefix(...)`, once each, either way round; any other matcher, such as `ClientIP(`, `Method(` or one Traefik adds later, keeps it behind;
  - **middlewares** whose kind is read and harmless: a redirect to HTTPS, compression, a www redirect, and headers that only set the response's. A kind is read from the labels for a reference with no provider, `@docker` or `@swarm`, and from the proxy's own files for `@file`, as Dokploy's `redirect-to-https@file` is a `redirectScheme` in its `middlewares.yml`; a label never stands in for a file's middleware;
  - **Caddy's directives**, read at every depth without their `N_` order: `header`, `try_files`, `encode`, `redir` and `log`, the `handle` blocks, and `reverse_proxy` with how it proxies; a matcher, `basicauth`, `forward_auth` or anything else keeps it behind, as does a stripped path;
  - **an entrypoint** of the web's, and **`traefik.enable`** that parses as true, as both panels run Traefik with nothing exposed by default.
- **A domain comes whole or not at all.** Where one route of it stays behind, every route of it does, as a route for its root would serve the path a sibling router guarded. The agent marks them, and the control plane holds them back again for an agent that doesn't. A rule folded onto the next line, as YAML's `>-` writes one, reads as one.
- **Every domain that won't come is named.** The report has a step for each, and the plan's note lists them as stopping answering: a domain of what isn't taken on, one with no port, one the labels give no route for, as a Swarm stack's, and the panel's own, which its proxy routes from its own files (Coolify's `proxy/dynamic`, Dokploy's `dokploy.yml`), such as its dashboard's. With nothing that can come, nothing is planned, and the refusal says whether the apps wait to be taken on.
- **A failure never leaves the proxy down.** Before it stops the proxy, the agent pulls the image of Dockwell's ingress, so the time the domains are down holds no pull. A change that stopped a panel or its proxy and then failed rolls back with a step that starts it again, last, as the stop makes no revision to roll back; and a failed change can be undone too. A start runs the container first and puts its policy back after, so one that doesn't start stays at `no`; one a stop left at `no` gets the policy its panel's installer gives it, `unless-stopped` for Coolify's proxy and sentinel, `always` otherwise.
- **`ImportService.PlanStartPanel`** (`dockwell import start-panel`) is the way back where no undo is: it starts the panel, or with `proxy` its proxy alone, each that's stopped, with the policy it had, or `always` where a stop left it at `no`. A proxy's start, there or as an undo, is refused while Dockwell's ingress serves routes on the server, naming them, as they hold the ports. An older Dokploy that runs its Traefik as a Swarm service has it scaled to none and back.
- **The Move in tab** shows where Coolify's or Dokploy's own app runs. Its page reads what the panel runs, plans nothing until Review the plan, groups each step by when it moves, offers Hand over ports 80 and 443 on the domains' group once an app is taken on, and Stop, or Start again for a stopped panel, on the panel's own row; where the proxy is stopped and no domain is a route, Start its proxy again takes the handover's place.

### Numbers

The agent protocol takes 330 to 339: `Hello.reads_panels` (330), `read_panel` in `Call` and `CallResult` (331), `Hello.stops_panels` (332) and `DesiredResource.panel` (333). The API's plan takes 131 for `panel` in `PlannedResource` and in `PlanStep`, where it sits beside `build`'s 130 and inside the 130 to 139 the import holds in existing messages. The import's enums gain `IMPORT_KIND_DOMAIN` (11) and `IMPORT_WAITS_FOR_PORTS` (5). `PanelApp.tasks` is 15, and `HostPanel.apps` and `PanelAction.apps` are 6.

## Consequences

- **No credential changes hands.** The import reads what the server shows Docker and the panel's own folders, so nothing of the panel's database or API is touched.
- **Coolify moves whole,** as it runs everything through Compose. Dokploy moves in two parts: its Compose apps now, its Swarm-run apps and databases in a step that stops each.
- **Domains move with a minute down,** when a person hands over ports 80 and 443, which they choose when.
- **What only the panel's database knows stays with it** until a step reads it, and each import says what that is.
