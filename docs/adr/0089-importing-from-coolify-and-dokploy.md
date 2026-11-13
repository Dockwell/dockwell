# 0089. Importing from Coolify and Dokploy

- Status: Accepted
- Date: 2026-11-13

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
- **Read-only.** Docker is listed and inspected, and the panel's files are read from fixed paths under its folder, refused where their last part is a link, as ADR 0056 reads 1Panel's.
- **What it sends is what a plan shows.** Names, kinds, Compose projects, container names, images, domains and paths. Never an app's environment, a label's value beyond a domain, or a file's contents. The values taking something on needs travel later, sealed, with the plan that takes it on, as ADR 0031 says.
- **Versions.** It's written from Coolify 4.7 and Dokploy 0.31. Another version is read the same way, by labels and folders, and the answer says it wasn't one read for this record.

### What it does with each thing

As in ADR 0056: every import is a plan a person reviews and applies; one thing that can't come doesn't stop the rest; each app comes into a project of its own, named for it, or `<name>-coolify` or `<name>-dokploy` where Dockwell has one of that name; what was taken on before says so; and a new import of a server stands in for the person's last.

**Taken on in place, with no restart** (ADR 0031), reading each Compose project's containers as `AdoptCompose` does:

- **Each of Coolify's applications, services and databases.** A service's containers come as one project, its services named as Coolify named them. An application built from Git keeps the image Coolify built on the server; building it again from Git in Dockwell is set up from the app's page, as for any app.
- **Each of Dokploy's Compose apps of the type `docker-compose`.**
- **Their volumes stay where they are,** named volumes by name and folders by path, as taking on never moves data. A plan's note says what the panel's delete would do to them: Coolify's removes the resource's volumes unless that box is cleared, and its folder; Dokploy's runs `docker compose down`, with `--volumes` when that box is ticked, and removes the app's folder. So neither is deleted in the panel.

**Left as it is, with the reason:**

- **A pull request's preview,** which Coolify makes and removes with its pull request.
- **What Dokploy runs as a Swarm service,** its applications, databases and stacks. Swarm keeps running a service whatever happens to its container, so Dockwell can't take one on in place. Each is made again in Dockwell, on the same volumes, by a later step that stops the service as Dockwell's copy starts, with a moment down, which its plan says.
- **Domains and certificates.** Each app's domains are listed with it, and come as routes to its service once the panel's proxy hands ports 80 and 443 to Dockwell's ingress, which gets a certificate for each. That's a step of its own, after the apps, as ADR 0056 hands over OpenResty's: the plan stops the panel's proxy, and its undo starts it again.
- **What only the panel's database knows:** a service's template, an application's Git source, scheduled tasks and backups. Each import says so. Where an app came from one of the panel's templates, a move to the catalog's entry converted from that template (ADR 0085) waits for the same reason as ADR 0056's store apps: an entry runs on named volumes and loopback ports, and an upgrade from it would plan new, empty volumes.
- **The panel itself.** Its own containers are listed by name, and left running.

### Stopping the panel after

Once its apps are taken on, the panel goes on running beside Dockwell until a person stops it. A later step gives that one step: a change that stops the panel's own containers, Coolify's six or Dokploy's three services and its proxy, planned as an action on the server's host is ([ADR 0082](0082-acting-on-a-servers-system-services.md)), with no revision, whose undo starts them again. Nothing of the panel is deleted: its database and folders stay, so starting it again brings it back as it was.

### A token, where one is needed

Nothing here needs one. If a later step reads what only the panel's API knows, the token a person gives it is a secret: sent to the agent sealed for that read, used once against the panel on the same server, never kept or logged, and never shown back. A panel's database is never written, and not read at all.

### The screen

A server's Move in tab offers each panel it finds running, beside 1Panel, and an import's page is a dry-run report first, as ADR 0056's is: what comes over, what's left behind and why, and Review the plan. The CLI says the same with `dockwell import coolify --server-id <server> --plan-only` and `dockwell import dokploy`.

### Numbers

The agent protocol takes 330 to 339: `Hello.reads_panels` (330), and `read_panel` in `Call` and `CallResult` (331).

## Consequences

- **No credential changes hands.** The import reads what the server shows Docker and the panel's own folders, so nothing of the panel's database or API is touched.
- **Coolify moves whole,** as it runs everything through Compose. Dokploy moves in two parts: its Compose apps now, its Swarm-run apps and databases in a step that stops each.
- **Domains move with a minute down,** when a person hands over ports 80 and 443, which they choose when.
- **What only the panel's database knows stays with it** until a step reads it, and each import says what that is.
