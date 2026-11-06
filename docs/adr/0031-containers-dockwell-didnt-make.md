# 0031. The containers Dockwell didn't make

- Status: Accepted
- Date: 2026-11-06

## Context

Issue #86 asks Dockwell to show the containers already on a server, then to take them on. Someone moving off another panel brings containers with them, and Dockwell leaves them alone but doesn't show them. On the two servers looked at, the most common thing done after editing files was changing a running container's image tag and restarting it, over 100 times on one standalone container.

This record covers seeing them. Taking one on comes after it, and will add to this.

What was settled already:
- [ADR 0021](0021-the-docker-driver.md): Dockwell's containers carry Compose's labels, in a project named after their environment, or their server for a container on its own. A container's environment is sensitive, and a plan never shows it.
- [ADR 0025](0025-logs-per-deploy.md): logs pass through from Docker without being stored, an API token needs `TOKEN_GRANT_LOGS` to read them, and reading them is in the audit log.
- [ADR 0018](0018-keep-the-budgets-in-rss.md): an idle agent stays under 40 MB, whatever it runs.

## Decision

### What is in the list

A container is **Dockwell's own** when a service or a container on its own of that name was ever managed in that Compose project, or when it runs the server's ingress. Anything else on the server is listed, stopped or not.

- **The control plane decides, as only it knows what it runs.** The agent sends what Docker lists, and the control plane holds each project and service against what it has made.
- **Labels alone wouldn't do.** Going by a project that looks like `env_…` would hide what another install left behind, which uninstalling leaves running (PRD principle 6), and what someone runs under one of Dockwell's projects with a service it has never heard of.
- **Deleted ones count as made.** A deletion commits before the server removes the container, so a container of a deleted service would read as a stranger for as long as that takes.
- **A one-off isn't a service's.** `docker compose run` in one of Dockwell's projects makes a container with that project's labels, which no service page shows, so it is listed, as `one_off`.
- **The ingress is the agent's to say.** Its proxy and keeper carry `dev.dockwell.ingress` and belong to no project, and the agent knows them without asking. It leaves them out and counts them, so the list says how many of Dockwell's own it left out.
- **A container shows once.** What is listed is exactly what no service page shows: a service's page reads containers by the same two labels this holds against what was made.

### Nothing is touched by being looked at, and no secret travels

- **Docker's list is all it reads.** One `GET /containers/json` answers a list, however many containers there are. It has no environment in it, so no value of one reaches the agent's memory on the way, let alone the control plane.
- **Of the list, it keeps what a plan would show.** Name, image, state, Docker's own status words, ports, mounts, when it was made, and of its labels only the Compose project and service. A command is left out, and so are other labels, as either can hold what a person meant to keep to themselves. `ListContainers` needs no grant for that reason, and reading one's logs does.
- **Logs read as a service's do.** `ListLogSources`, `ReadLogs` and `WatchLogs` take a `server`, which makes `id` a container, by name or full ID. So the panel's log view, the CLI's `dockwell logs read web --server hz-1`, the REST paths, `TOKEN_GRANT_LOGS` and the audit log are the ones services have. A new set of methods would have repeated every filter of three requests for no gain, at the cost of `/v1/services/{id}/logs` reading oddly for a container. The audit log names the container and goes by the server, as a container has no ID of Dockwell's.
- **A container of a service can't be read this way.** Its logs are its service's, with what a deletion put out of reach, so this says which service to read. Each read asks the agent for that one container and holds it to the same rule, so there's one rule. A server that isn't connected can't answer, and reads as `unavailable`.
- **The server keeps nothing of such a container.** Logs are kept when Dockwell removes a container, and it removes none of these. What Docker has is all there is.
- **A reader goes by the name, so a follow does too.** `docker compose up -d`, or `docker rm` then `docker run`, leaves a new container under the same name, and the page reads that one. So a follow reads the container it began with and whatever holds its name now, as a new source. The old one reads as gone, with the marker a follow that resumes gets at the last line it read. A container that's only been renamed goes on being read beside what took its name.

### Hundreds of containers

- **A list is read a container at a time.** `EachContainer` decodes Docker's answer as it comes, so what an agent holds is the one container it is reading, plus what it has kept of those before.
- **A list is one message on the agent's stream, of at most 1 MiB,** which holds a few thousand. Stats wait behind it, and a message past the stream's 8 MiB would end the stream. What doesn't fit is counted in `unlisted`, and the panel says so and how to see them. One container lists 32 ports and 32 mounts at most and counts the rest, as a port range publishes a port for each number.
- **`make containers-load`** holds an agent to its budget with 500 containers on a fake engine, with the list read as fast as the CLI can ask, as `budgets.sh` runs nothing but an idle agent.

### The panel

There is no mock, so it is built from the system's components, on a server's own page: machine-level things live there (PRD, scaling from one server to a fleet, rule 1). A panel titled "Containers Dockwell didn't make" says what they are, that looking changes nothing, and that apps run with Dockwell are under Apps, with how many containers of Dockwell's own it left out.

The region keeps its place, and what is in it changes with how many there are (rule 3):

| Containers | The panel shows |
| --- | --- |
| 0 | A line saying Docker runs nothing here that Dockwell didn't make |
| 1 to 12 | Rows of name, state, image, ports and volumes |
| 13 and up | A filter on name, image and project too |
| Past 100 | The first 100 of what the filter finds, and more on asking |
| Past what fits in a message | How many didn't fit, and `docker ps -a` |
| Any, in more than one Compose project | A project column |
| Any, on a phone | Stacked rows of a name and a line of what it is |

A row opens the container's page: what Docker says of it, and the log view an app has. It reads a container's log with no limit as something Dockwell leaves as it is, as the words an app gets promise a deploy that isn't coming. The page has nothing on it that changes the container, which is the next slice's to add, through a plan.

## Consequences

- **The list is read, not streamed.** The panel asks every 15 s while it shows, which is PRD principle 7's exception until something needs better: Docker's events could keep it live, for an agent to hold state it doesn't hold now.
- **A stopped container shows no ports,** as Docker lists a port only once it is bound, and reading each stopped container's configuration would mean inspecting it, environment and all. It reads "It publishes none now", which is true.
- **An image can read as an ID.** Once a tag means another image, Docker lists the ID of what runs. That's the honest answer for a list: inspecting the container gives the name it was made from, for what needs it.
- **Health shows in Docker's own words,** on the container's page. Reading it into the state column is a nice-to-have.
- **`dockwell containers list` isn't what `dockwell containers create` makes.** The noun has both: what it makes is Dockwell's own, so it lists under `dockwell resources list --kind container`. Its help says which is which.
- **What is taken on has to leave this list.** Adopting a container gives Dockwell a service that runs a container without its labels, and the rule above goes by labels, so taking one on must record which container is the service's, for this list as for everything else.
- **Memory.** The agent build links nothing new: `internal/docker`, which it had, and `internal/agent`.
