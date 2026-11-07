# 0031. The containers Dockwell didn't make

- Status: Accepted
- Date: 2026-11-06

## Context

Issue #86 asks Dockwell to show the containers already on a server, then to take them on. Someone moving off another panel brings containers with them, and Dockwell leaves them alone but doesn't show them. On the two servers looked at, the most common thing done after editing files was changing a running container's image tag and restarting it, over 100 times on one standalone container.

This record covers seeing them, and taking one on.

What was settled already:
- [ADR 0021](0021-the-docker-driver.md): Dockwell's containers carry Compose's labels, in a project named after their environment, or their server for a container on its own. A container's environment is sensitive, and a plan never shows it.
- [ADR 0025](0025-logs-per-deploy.md): logs pass through from Docker without being stored, an API token needs `TOKEN_GRANT_LOGS` to read them, and reading them is in the audit log.
- [ADR 0018](0018-keep-the-budgets-in-rss.md): an idle agent stays under 40 MB, whatever it runs.

## Decision

### What is in the list

A container is **Dockwell's own** when a service or a container on its own of that name was ever managed in that Compose project, or when it runs the server's ingress. Anything else on the server is listed, stopped or not. What was taken on reads as its service's, so it leaves the list by the same rule.

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

A row opens the container's page: what Docker says of it, and the log view an app has. It reads a container's log with no limit as something Dockwell leaves as it is, as the words an app gets promise a deploy that isn't coming. Nothing on the page changes the container but taking it on, which is a plan.

### Taking one on

**A container is taken on as it stands, with no restart, by a change like any other.** `ContainerService.AdoptContainer` reads what the container runs with into a service's spec, and proposes that. The plan has one step that takes the container on, with no downtime, and undoing it hands the container back.

- **Docker can't change a running container's labels, and they are how Dockwell finds a service's containers.** Logs, health, the ingress, rollouts and the list above all go by them. So the agent keeps a record of what it took on, `adopted-containers.json` in its data directory, by full ID, and its Docker client reads each of those as if it had the labels of what it is now: in a list, in an inspect, and through a filter on labels, which Docker can't apply to them. Nothing above the client needs to know, and there's one place to get it right. A server with none taken on asks Docker for nothing more.
- **The spec knows nothing of it.** What was taken on is a fact of one server and one container, and a spec says what should run. So an undo of a change of image puts back a spec that runs as any service's does, and the container that came with the service, which the change replaced, is long gone. The change's item keeps the container's ID, as it keeps `pull` and `run`, so a change is applied from its record.
- **A rename wasn't the way.** Giving the container the service's name would need no record, but other containers reach it by the name it has, and Docker's DNS follows a rename, so it would cut them off to save a file. It keeps its name until something replaces it, and the alias then keeps that name working on its networks.
- **The read goes first, and the plan checks it again.** What was read is what a person reviews, so if the container changed by the time the plan runs or applies, it is refused, with the field that differs, never a value. After applying, the engine reads the service back as it does any step, and nothing else runs: no verify, as nothing was touched, so a stopped container can be taken on and stays stopped.
- **Its environment comes in sealed.** The container's environment lands in `Container.env`, a sensitive field: sealed in the store, said to have changed and never what it holds, and left out of an export, as for any service. An agent's report of a spec already travels this way, so this needs no second channel: its stream is mutual TLS, and the control plane has the values only to seal them. A value in a command would show, as a plan shows a command, so a container whose command, entrypoint or health check holds a value of its environment is refused, by the variable's name. A secret written into a command with no variable behind it can't be told from any other word, and shows, as it does in `docker inspect`.
- **Taking one on takes a person, from the plan on.** A read-write token couldn't read a stranger's environment before, and through a service of its own it could, so a plan that takes one on is host access (`AccessTakesOn`), which a token can't apply. A token can't plan one either: a plan shows the stranger's command, labels, user and mounts, which the list leaves out, and it stays on record for a read-only token.
- **What a replace would drop is refused by name.** A spec holds less than Docker can run. Taking on a container with `hostname`, `sysctls`, `ulimits` or the like would be safe today and lose them on the first change of image, without a word, and the point of taking it on is that a change of image is safe. So it is refused, naming each, as Compose import refuses what it can't run. `make` it again without them, or give specs the field. Port bindings with a port Docker picks are refused the same way, by what Validate says of them.
- **What's refused is whatever isn't on an allow list.** A list of what to refuse passes anything it forgot: a GPU, a log driver, `-P`, a bind's propagation. So the agent reads `Config` and `HostConfig` key by key, against what a spec holds and what makes no difference, and any other key that isn't Docker's default is refused by its name, so a setting Docker adds later can't pass. A network's static address and options are read as well. `Config.MacAddress` says a static MAC only below API 1.52, and for a container's main network alone, so a static MAC on any other network can't be told from a generated one.
- **A value of the environment shows nowhere else.** A plan shows every field but the environment, so a value in any other field of the spec is refused, by the variable's name and where it is, never the value. That looks through every field, so a new one is looked through too, but for mount paths and the working directory: a path isn't a secret, and `TZ=Asia/Shanghai` beside a mount of its zoneinfo is ordinary. An ordinary value that happens to be in a label, such as a domain, is refused with the rest, as nothing can tell it from a password. A value of any length counts for a variable whose name holds `PASS`, `SECRET`, `TOKEN`, `KEY` or `CREDENTIAL`, in any case, which takes in `PASSWORD` and `PASSPHRASE`, and for any other variable the cutoff is 6 characters: a shorter one could be in a spec by chance, such as `MODE=app` beside a label `tier=app`, and a person couldn't do anything about that refusal. So a short secret under a name that says nothing can still show in a plan.
- **A plan says what else may run it, and what's safe.** A container from a Compose project usually has something else running that project, and Dockwell taking it on doesn't stop that. So the plan warns when it's taken on, and again on the first plan that replaces it, which is when it starts to matter. It warns and doesn't refuse, as a project someone ran by hand is fine to take on. It names the tool and the folder where the labels show them. What it advises is read from what each tool does, as a plan is what someone follows: see "In 1Panel, after taking on". No other tool's remove has been read, so a plan tells no one to use it, and says to find out what it does to the folder first.
- **Volumes it came with stay its own.** A service's volumes are its environment's, `<project>_<name>`, so one read back from a stranger would plan a new, empty volume at the next replace, which is the worst thing this could do. `Mount.external` says a volume is one Dockwell didn't make, by its name in Docker: Dockwell never makes, changes or deletes it, and it must be on the server. As with `external_networks`, what else mounts it shares it, so it is host access. Volumes Docker made for the image move on to the new container as they always did, and server paths stay what they are, under every rule in ADR 0021.
- **Deleting the service before anything replaces the container lets go of it.** Dockwell never made it, so that is no reason to remove it, and it's what makes an undo of taking it on true. The step says `releases`, with no downtime and nothing lost, and the container is back on the list. Once a change has replaced it, what runs is Dockwell's own, and deleting it does what deleting any service does.

## In 1Panel, after taking on

Read from 1Panel's source at `726fd94e07`, for an app from its store. An app's Compose file and what it binds live in one folder, `/opt/1panel/apps/<app>/<name>`, which is the Compose working-directory label: its MySQL binds `./data/:/var/lib/mysql`. So what a taken-on service mounts is in that folder.

| In 1Panel | What it runs | Until Dockwell changes the image | After that |
| --- | --- | --- | --- |
| Stop | `compose stop` | Stops this container | Finds nothing to stop |
| Restart | `compose restart` | Restarts this container | Finds nothing to restart |
| Start | `compose up` | Starts this container | Starts a second copy |
| Rebuild, or a change of port | `compose down`, then `up` | Removes this container and makes another, which Dockwell doesn't know | Starts a second copy |
| Upgrade, or editing its settings | ends in `compose up` | Makes another, if what it asks for changed | Starts a second copy |
| Uninstall | `compose down`, then deletes the folder, with no way to keep it | Removes this container, then deletes its data | Deletes the folder under the running service |

So, after taking an app on:

1. Leave it alone in 1Panel, which goes on listing it, and don't uninstall it there. Nothing in 1Panel keeps the folder.
2. Change its image from Dockwell from then on, not there.
3. To clear it out of 1Panel, first give the service what it mounts somewhere outside that folder, with a copy you've checked, and only then uninstall it there.

A second copy shares the folders and answers to the same name on the network, and two databases on one folder can corrupt it, which is why this is said twice. Where it publishes a port the service holds, the second copy fails to start, and Compose still makes it.

## Consequences

- **The list is read, not streamed.** The panel asks every 15 s while it shows, which is PRD principle 7's exception until something needs better: Docker's events could keep it live, for an agent to hold state it doesn't hold now.
- **A stopped container shows no ports,** as Docker lists a port only once it is bound, and reading each stopped container's configuration would mean inspecting it, environment and all. It reads "It publishes none now", which is true.
- **An image can read as an ID.** Once a tag means another image, Docker lists the ID of what runs. That's the honest answer for a list: inspecting the container gives the name it was made from, for what needs it.
- **Health shows in Docker's own words,** on the container's page. Reading it into the state column is a nice-to-have.
- **`dockwell containers list` isn't what `dockwell containers create` makes.** The noun has both: what it makes is Dockwell's own, so it lists under `dockwell resources list --kind container`. Its help says which is which.
- **Its name changes at the first replace.** It comes back as `env_…-api-2`, so `docker logs legacy-api` on the server stops working, and the plan says so as it is taken on. Containers on its networks go on reaching it by the old name.
- **What it wrote outside its volumes goes with the old container** at the first replace, as with any `docker rm`. The plan says so.
- **Undoing the deletion of something let go of** plans a new container, which its ports then refuse, as the old one still holds them. Take it on again instead.
- **An export of a taken-on service lists its volumes as `external`, which `ImportCompose` doesn't take yet,** so that file doesn't import again. Slice 3 is the place for it.
- **Memory.** The agent build links nothing new: `internal/docker`, which it had, and `internal/agent`.
