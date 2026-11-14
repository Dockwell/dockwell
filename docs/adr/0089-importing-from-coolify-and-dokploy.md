# 0089. Importing from Coolify and Dokploy

- Status: Accepted
- Date: 2026-11-13
- Amended: 2026-11-13, after the security review of #608: a label that holds a credential is refused for every take-on; auto deploy, root access and Coolify's backups said in each step and note; folder words and Traefik's file checked as they're read; stopping the panel comes next
- Amended: 2026-11-14, after three reviews of the domain handover: stopping the panel ships on its own; the handover becomes evidence and a person's choice, domain by domain (below). After the review of #629, making Swarm-run apps again was rebuilt, with a CI job that tries it on a real Swarm (below)

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
- **Domains and certificates.** Each app's domains are listed with it, and come as routes to its service once the panel's proxy hands ports 80 and 443 to Dockwell's ingress, which gets a certificate for each. That's a step of its own, after the apps, which a person chooses domain by domain (see "The domain handover: evidence and a choice").
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

A first build passed its tests on the fake Docker Engine and failed four ways on a real Swarm, which #629's review found; this one is tried on a real single-node Swarm in CI (Swarm import on Docker, `scripts/swarm-e2e.sh`), with Dokploy set up as its installer and its deploys leave it.

- **Each comes again from its running task.** The agent's read names each service's running task (`PanelApp.tasks`), in step with its services however Swarm lists them, and the plan reads that container as the take-on does, so its image, environment, volumes and server paths come as they run, with values kept out of the plan (ADR 0031). Each of Dokploy's projects comes into one of Dockwell's, named for it, each app under its appName; a stack into a project of its own, each service named as the stack named it.
- **What Swarm gave the task stays behind, and what the app had comes.** The address Swarm pinned on each network and the labels and driver a stack's volume was made with aren't refused, though a subpath still is, as the copy joins anew and mounts the volume, which is there already, by its name. Swarm's, Traefik's and Dokploy's labels, Swarm's routing mesh, a stack's own networks and the names Swarm gave the task are left out. The copy keeps each network its task shared with others, such as `dokploy-network`, where Dokploy's Traefik reaches it by its appName, and a stack's service answers there to `<stack>_<service>` too. It restarts as its service did (`PanelApp.restarts`): `any` as unless-stopped, `on-failure` as on-failure, `none` as no. What leans on the task, such as a container sharing its network, keeps the app where it is, as for any take-on.
- **The switch is in the same change, and waits.** The server's host scales each Swarm service to none first, a panel stop with `apps` set, which the agent takes only for a service Dokploy labelled as an app's. It takes the service off its published ports, which Swarm's routing mesh holds for as long as the service publishes them, and off its networks, so its name answers there for the copy alone; the plan keeps both as Docker had them (`HostPanelPart.was_ports`, `was_networks`). Then it waits, on Swarm's tasks API, until every task of the service is over, starting ones and those on other nodes too, so a copy never starts beside the task it replaces, nor in its grace period, such as two databases on one data directory; a task not over after the service's own stop grace period and a minute more fails the step, and the change rolls back. A stop that resumes after the agent restarted finds the service asking for none with a task not over, and waits again; a start waits the same way before it scales the service back up. Its undo deletes the copies first, then puts each service back as it was: its replicas, its ports and its networks.
- **Dokploy's own, by its labels.** Dokploy v0.31 gives each service of what it runs `dokploy.project` and `dokploy.application`, among others, a stack's in its `deploy.labels`; the reader and the agent both take a service for Dokploy's by those, so a stack someone deployed by hand stays behind, saying so, and the rest plans. Through an agent from before, which doesn't say `Hello.remakes_swarm_apps`, every Swarm-run app stays behind, saying to update its agent.
- **As for a take-on,** each remade app's step says when its copy is as good as root on the server, and to turn off Dokploy's Autodeploy first, which would scale its service back up beside its copy; the plan names each such app in a note.
- **What a copy can't carry stays, saying why** (`PanelApp.swarm_left`): a service with more than one replica, one with a task on every node, a swarm of more than one node, a Swarm secret or config, or an anonymous volume, and a service with no task running, as there's nothing to make it again from.
- **Its domains wait for the handover,** as every app's do, and are listed with it meanwhile.

### A token, where one is needed

Nothing here needs one. If a later step reads what only the panel's API knows, the token a person gives it is a secret: sent to the agent sealed for that read, used once against the panel on the same server, never kept or logged, and never shown back. A panel's database is never written, and not read at all.

### The screen

A server's Move in tab offers each panel it finds running, beside 1Panel, and an import's page is a dry-run report first, as ADR 0056's is: what comes over, what's left behind and why, and Review the plan. The CLI says the same with `dockwell import coolify --server-id <server> --plan-only` and `dockwell import dokploy`.

#### The screen, as built

- **The Move in tab** shows where Coolify's or Dokploy's own app runs, running or stopped. Its page reads what the panel runs, plans nothing until Review the plan, groups each step by when it moves, and offers Stop, or Start again for a stopped panel, on the panel's own row.
- **`ImportService.PlanStartPanel`** (`dockwell import start-panel`) is the way back where no undo is: it starts each of the panel's own that's stopped, a container with the restart policy its installer gives it where a stop left it at `no` (`unless-stopped` for Coolify's sentinel, `always` otherwise), a Swarm service at one replica.
- **A failure never leaves a panel down.** A change that stopped a panel's own, or scaled an app's Swarm service, and then failed rolls back with a step that starts it again, last, as the stop makes no revision to roll back; and a failed change can be undone too. A start runs the container first and puts its policy back after, so one that doesn't start stays at `no`.

### The domain handover: evidence and a choice

Three reviews of a handover that read Traefik's and Caddy's configuration to prove a domain unguarded each found a guard the reader didn't see: a router field, an entrypoint's middleware in the static configuration, a file in another form, another provider, a container the panel doesn't manage. A reader can't prove a domain unguarded by re-implementing either proxy's configuration. So the handover doesn't ship with the rest, and when it does, it's evidence and an explicit choice, domain by domain, as the owner's rule for sharing has it: the platform makes sure the person means it, then makes it work robustly.

- **A broad scan, cheap and failing closed.** Every `Host(`, `HostRegexp(` and Caddy site address is taken from the raw text of every label on every container and Swarm service on the server, and of every file in the proxy's mounts. A domain that anything other than the imported app's plain route mentions stays behind.
- **Static configuration.** Only the official `traefik` image counts. A mount over a default static-configuration path holds every domain back, but Dokploy's exact bind of its `traefik.yml`; so does a provider other than docker and swarm, or a file folder other than the one Dockwell reads. A flag with no `=` takes the next argument as its value.
- **A live probe through the old proxy, before anything stops.** For each domain, Dockwell requests `/`, and each path any rule anywhere mentions for it, over HTTPS with its name as SNI, from the server. A 401, 403 or 407, a redirect to another host or to a login path, or a TLS handshake that fails, as a client certificate would, holds the domain back, with what answered.
- **The choice.** The handover's sheet lists each domain with what was checked. One the scan found nothing for, and the probe found open, is chosen already; each other says why it stays and can't be chosen. The plan names the chosen ones under "No login or address limit found on the old proxy".
- **At apply,** the agent runs the scan and the probe again just before it stops the proxy, and refuses where a chosen domain now reads otherwise. A digest of both is in the plan's fingerprint.
- **What was built is the first layer.** The reader written for the earlier handover (routes from labels and Traefik's files, an allowlist of rules, middlewares and Caddy's directives, the proxy's static configuration and own files read in full, a domain held back whole) stays as the first check, on its branch, and the handover is a pull request of its own, with a review of its own.

### Numbers

The agent protocol takes 330 to 339: `Hello.reads_panels` (330), `read_panel` in `Call` and `CallResult` (331), `Hello.stops_panels` (332) and `DesiredResource.panel` (333). The API's plan takes 131 for `panel` in `PlannedResource` and in `PlanStep`, where it sits beside `build`'s 130 and inside the 130 to 139 the import holds in existing messages. Making Swarm-run apps again takes `Hello.remakes_swarm_apps` (335), `PanelApp.tasks` (15), `published` (16), `restarts` (17) and `swarm_left` (18); `HostPanel.apps` and `PanelAction.apps` (6); and `HostPanelPart.was_ports` (6, `HostPanelPort`) and `was_networks` (7). The handover's branch holds `Hello.reads_panels_strictly` (334), `HostPanel.proxy` and `PanelAction.proxy` (5), `Panel.domains` (7) and `Panel.proxy_left` (8), `PanelApp.routes` (14), `IMPORT_KIND_DOMAIN` (11) and `IMPORT_WAITS_FOR_PORTS` (5).

## Consequences

- **No credential changes hands.** The import reads what the server shows Docker and the panel's own folders, so nothing of the panel's database or API is touched.
- **Coolify moves whole,** as it runs everything through Compose. Dokploy moves in two parts: its Compose apps as they run, its Swarm-run apps and databases made again, each with a moment down.
- **Domains move later, each by a person's choice,** with what Dockwell checked in view, and a minute down while each gets its certificate.
- **What only the panel's database knows stays with it** until a step reads it, and each import says what that is.
