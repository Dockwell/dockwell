# 0021. The Docker driver, with Compose's names and labels

- Status: Accepted
- Date: 2026-11-04

## Context

Issue #26 asks Dockwell to run services from an image or a Compose file through the Docker API, as revisions. Rollouts (#28), logs (#30), ingress (#27) and the app screens (#31) build on it. Three principles shape it:
- **Principle 2:** every change is a revision with a plan, reversible in one action. Volumes and networks too.
- **Principle 5:** agents hold their own desired state and keep serving without the control plane. What someone changes by hand comes back as drift.
- **Principle 6:** no lock-in. Underneath is plain Docker and Compose, uninstalling leaves workloads running, and any service can be exported as Compose.

Earlier records left it these seams:
- [ADR 0009](0009-resources-revisions-and-plans.md): the driver interface (Validate, Observe, Plan, Apply, Verify), with honest downtime and data loss, and drift that never fires on noise. A deleted volume moves aside for 7 days.
- [ADR 0014](0014-object-model-and-store.md): a service lives in an environment and ran, in the prototype, as a container named `<name>.<env ID>`. #26 replaces that with Compose's labels.
- [ADR 0017](0017-the-change-api-and-the-agent-stream.md): agents report the kinds they have drivers for, and the agent in M2 had none.
- [ADR 0018](0018-keep-the-budgets-in-rss.md): an idle agent reads about 26.7 MB of its 40 MB budget, with a warning at 30 MB, and every package in the one binary ([ADR 0007](0007-one-binary-two-roles.md)) costs it resident pages.
- [ADR 0013](0013-mirrors-for-releases-and-images.md#container-images-from-m3): images will be pulled by digest, through a map of registry mirrors.

## Decision

### A small Docker client of our own

`internal/docker` speaks the Docker Engine API over the unix socket with `net/http`, and has only what the drivers use:
- the engine's version, to agree an API version;
- containers: inspect, list, create, start, stop, remove, rename, update the restart policy, and join a network;
- images: inspect, and pull with progress;
- volumes and networks: inspect, create and remove.

**Not the official SDK.** `github.com/docker/docker` and its client bring in dozens of packages, OpenTelemetry's and containerd's among them, and every one would be resident in every agent. The client is about 900 lines, its types included.

**One API version, agreed with each engine.** The client uses the newest version both speak, from 1.41 (Docker 20.10) to 1.47 (Docker 27.3). The fields it uses mean the same in all of them. Docker 29 takes nothing older than 1.44, and still takes 1.47. One difference matters: before 1.44 a container joins one network at create, so the others join before it starts. An engine older than 1.41 is refused with how to upgrade.

**The socket is the docker CLI's.** It is `DOCKER_HOST` when that names a unix socket, as with rootless Docker, and `/var/run/docker.sock` otherwise. A `DOCKER_HOST` the agent can't use, such as `tcp://`, makes the Docker kinds unavailable with that reason.

**Names are looked up exactly.** Docker takes a reference as a full ID, then an exact name, then a prefix of one ID alone. So a name made of hex digits, such as `db` or `cafe`, would otherwise find a container whose ID starts with it, and Dockwell would stop it, remove it, or read it back as drift. The client finds a container or a network by its exact name, and the drivers act on the ID it returns from then on. An image named without a tag is asked for as `name:latest`, which never matches an ID.

**Errors say how to fix them.** Docker not installed, not running, its socket not allowed, too old, or taking the connection but never answering each have their own message with the fix, such as "Docker is installed, but isn't running. Start it with systemctl start docker; the agent finds it within a minute". Docker's own refusals pass through as Docker wrote them.

### The agent offers what it can run

- **A driver may say it can't run now.** A `resource.Readier` driver, as each Docker driver is, answers `Ready`. The agent offers in its hello only the kinds whose drivers are ready, and gives the others with why, in `Hello.unavailable`. It checks again every 30 s, and sends `Kinds` on its stream when that changes, so Docker started after the agent is picked up without a restart. The check runs beside the stream, one at a time, so a Docker that hangs holds up neither stats nor the stream's end, and drivers on one Docker client share one answer: the agent asks Docker once, not once for each kind.
- **A plan for a kind the agent can't run fails as before,** with `no_driver`, now with the agent's reason: "hz-1 can't run service web now: Docker is installed, but isn't running. Start it with systemctl start docker; the agent finds it within a minute".
- **Pulls show their progress.** A driver reports how a step is going through its context (`resource.Progress`). The agent sends it to the control plane at most once a second, as `CallProgress`; a message that comes sooner waits, the latest of them going when the second ends, so a change of phase always shows, and the engine puts it in the running step's message for watchers, such as "Creating service db on hz-1: pulling postgres:17, 12 of 48 MB". It isn't recorded: the step's end is.

### Compose names everything

An environment is one Compose project, so `docker compose` sees and manages what Dockwell runs.

- **The project is the environment's ID in lowercase,** such as `env_01kat4zr6e3v0y5xbkzb5d9q6m`. Compose takes it as a project name, and unlike a name it never changes, so renaming an environment recreates nothing.
- **A service runs as its project's service of the same name,** in a container named as Compose names a service's first: `env_01kat…-web-1`. It carries Compose's labels: `com.docker.compose.project`, `.service`, `.container-number` (1) and `.oneoff` (`False`). On each of its networks other containers reach it by its service's name, as under Compose.
- **Volumes and networks** are named `<project>_<name>`, with `com.docker.compose.project` and `.volume` or `.network`. Compose checks those labels when it finds one of its names, so they must match.
- **A container on its own** keeps its name, in its server's project, such as `srv_01kat…`. A name starting `env_` is refused, so a container run by hand never manages a service's. The `.env_` names ADR 0014 reserved are free again.
- **A container carries a config hash,** `com.docker.compose.config-hash`, as Compose lists only containers that carry one as its project's: without it `docker compose ps` leaves them out, and `docker compose up` fails on their names. Dockwell's is a SHA-256 of the spec, with the environment's values left out, as they may be secrets. Volumes and networks carry none, since Compose recreates one whose hash isn't its own, which would empty a volume.
- **Compose's other labels are left out:** its image digest and version. Dockwell's hash never equals the one Compose computes from its own types, so `docker compose up` with an exported file recreates each container once, and from then on compose's own labels are on it. Neither reads as drift (below). CI's Docker job pins docker compose, at v2.38.2, so a new runner image can't change what it checks.

### The specs

- **A container** gains what a Compose service runs with: an entrypoint, mounts of a volume or a server path, networks, a user, a working directory, a health check, and a port's server address. A service's volumes and networks are its environment's, by name; a container's on its own are Docker's.
- **Ports Docker can't bind together are refused,** as the new container would fail to start only once the old one was gone. A port published on no address is on every one, IPv4 and IPv6, so it overlaps any other address; `0.0.0.0` overlaps every IPv4 address, and `::` every IPv6 one, but not each other.
- **Volumes and networks are kinds of their own,** `volume` and `network`, in an environment, with IDs `vol_…` and `net_…`. Docker can't change either once it is made, and making one again would empty a volume or cut off a network's containers, so a plan that changes one is refused with what to do instead: make a new one, move across, delete the old.
- **A service's volumes and networks are checked when a change is proposed.** The service driver says what a spec names (`resource.Referrer`), and the engine checks it against what is stored and what the change makes, deletes and moves: a mount needs a volume, and a network a network, in the service's environment and on each server it runs on, so one in use can't be deleted or moved away. It checks only the pairs a change touches, so what was stored before doesn't hold up other changes. What a service names comes before it in the change, whatever order the change lists them in. The agent checks again as it applies, for what was made outside Dockwell.

### Running, replacing and checking a container

- **Images are pulled when the server doesn't have them,** as Compose's default `pull_policy: missing` does. One already there runs as it is. The engine pulls by tag, as written; see the consequences for ADR 0013's digests.
- **A replace makes the new container first,** under a name of its own, then stops the old one, removes it, renames the new one and starts it, as Compose recreates. The pull and the create aren't downtime. Once the old one is gone, the rename and the start finish even if the call is cancelled, for up to 30 s. If the rename fails anyway, the new container is left under the name it was made with, `<old ID, 12 digits>_<name>`, with the service's labels: the next run takes it as the old one, so the volumes Docker made for the image move on with their data, and deleting the service removes it.
- **Downtime is stated as its most:** 12 seconds, the 10 s Docker gives a container to stop before it kills it, and 2 s to start. Changing only the restart policy is an update in place with none. A service's ports are down for that long; rolling deploys that avoid it are #28.
- **Volumes Docker made for an image's `VOLUME` paths move to the new container,** as Compose keeps them, so a replace loses no data. Removing a container leaves them for a prune.
- **Verify waits for health.** A container with a health check, its own or its image's, must turn healthy within 5 minutes; if the step's own time runs out first, as after a long pull, the error says that, not that the check took too long. One without must keep running for 2 s. One that exits or keeps restarting fails with how to see why, `docker logs <name>`, never with what it printed, which may hold secrets (ADR 0017).

### Reading back, and drift

`Observe` reads a container back into a spec, and the agent normalizes it as ADR 0009 requires. What Docker adds never reads as drift:
- IDs, start times, states, the volumes Docker made for the image, and Docker's default bridge network for a service that names none;
- Dockwell's and Compose's own labels;
- **what Docker fills in from the image.** Docker merges the image's environment, labels, command, entrypoint, user, working directory and health check into the container's. A value equal to the image's could come from either, so the container carries `dev.dockwell.set`, the list of what its spec set, such as `["command","env:API_KEY"]`, by key only. Reading back keeps those, and drops values equal to the image's otherwise. So neither a spec that repeats its image nor what the image adds shows as drift. A container without the label, made by hand, keeps only what differs from its image.
- options Docker adds to a network of its own, under `com.docker.network.`, unless the spec set them.

Anything else that differs is drift: a container started by hand with the name, an environment variable added, an image changed, or a container `docker compose up` made from an edited export. An exported file carries `dev.dockwell.set`, so a container compose makes from it reads back as the export, and only what was edited shows.

A stopped container isn't drift: the spec says what runs, not whether it is running, until health comes with #28.

### Deleting a volume

- **It destroys data, so the plan says so.** The step is marked as destroying data, the plan is destructive, and only a person who names its fingerprint can apply it (ADR 0017).
- **One in use can't be deleted.** A volume a container mounts, the service's own or one made by hand, is refused with which containers mount it, as a network with containers on it is: Docker would refuse to remove it, and they would go on using a volume the environment no longer has. The control plane refuses it first, when the change is proposed (above); the agent's check is for what was made outside Dockwell.
- **It is kept for 7 days.** Docker can't rename a volume or change its labels, so it can't move aside. The agent keeps it where it is, and lists it in `deleted-volumes.json` in its data directory. It reads the list once and keeps it in memory, and writes each change to the file before it counts. Reading the volume back finds it gone, and a service can't mount it. Creating a volume of that name again within 7 days, as undoing the deletion does, takes it off the list, data and all, if it is asked for as it was. Asked for another way, it is refused, with how to remove the old one first.
- **The agent prunes every hour** the volumes kept for 7 days. One a container still uses stays until none does. The prune holds the list while it removes each volume, and checks first that it is still listed, so an undo that brings a volume back as the prune reaches it keeps its data.

### Compose files

- **compose-go v2.16.2 parses them, in the control plane,** as `docker compose` does: its canonical forms of the short syntaxes, and its types. Not its loader, which validates against the Compose schema with a JSON Schema library that allocates 1.7 MB as every process that links it starts (see the consequences). Dockwell takes far less than the schema allows, and checks it on the file's YAML first: every field it takes must hold what the schema allows there, as compose-go's transforms panic on anything else, and the refusal says where, what Compose takes, and how to write it. What the loader would do after, Dockwell does too: variables named without a value, the default network for services that name none, and references between services, volumes and networks, cycles of `depends_on` included.
- **`ComposeService.ImportCompose`** (`POST /v1/compose:import`, `dockwell compose import`) plans a file into an environment as one change: its networks, then its volumes, then its services, each after those it `depends_on`. Each Compose service is a service, and each volume and network one of its own. With `remove_orphans`, services and networks of the environment that aren't in the file are deleted too; volumes stay, as `docker compose up --remove-orphans` leaves them.
- **What Dockwell can't run is refused by name, never dropped:** the file's top level takes `services`, `volumes`, `networks`, `name` and `version`; a service takes `image`, `command`, `entrypoint`, `environment`, `ports`, `restart`, `labels`, `volumes`, `networks`, `network_mode: bridge`, `user`, `working_dir`, `healthcheck`, `depends_on` and `expose`. `x-` extensions are allowed. `expose` changes nothing on one server, and `depends_on` orders the change but isn't kept. Each refusal says what to do, such as "the service web builds an image, which Dockwell can't do yet. Build and push the image, then name it with image".
- **Mounts take read-only and SELinux relabelling.** A server path can be mounted read-only, and relabelled as `:z` (shared with other containers) or `:Z` (private to this one) relabel it, in the short syntax or as `bind: {selinux: z}`. The spec carries it as `Mount.relabel`, Docker gets it as the bind's option, and a server that doesn't enforce SELinux ignores it. A volume can't be relabelled: Dockwell mounts volumes through Docker's mount API, which has no such option. Propagation modes, `nocopy`, and options Docker doesn't know are refused by name; compose-go drops options it doesn't know, so Dockwell reads them from the file itself. `cached`, `delegated` and `consistent` are taken and dropped, as `expose` is: Docker ignores them on Linux.
- **The control plane never reads a file for one.** `env_file`, `label_file`, `extends`, `include`, secrets and configs are refused on the file's YAML, and nothing that would read them runs. Bind mounts must be absolute: a path relative to the file means nothing on a server.
- **Variables** take values from the request and the file's defaults. One with neither is refused, rather than left blank as Compose would, at every level: Dockwell resolves them itself, with Compose's syntax, as compose-go's substitution leaves a variable blank inside a default, `${X:-${Y}}`, and after one in the same string, `${X:-a}-${Y}`. A default that isn't used needs nothing. CI's Docker job checks `docker compose config` resolves each form the same.
- **Restart** follows Compose: a service without `restart` doesn't restart, though Dockwell's own default is `unless-stopped`.
- **`ComposeService.ExportCompose`** (`GET /v1/compose`, `dockwell compose export`) writes an environment, or one service with the volumes and networks it uses, as a file with the project's name. `docker compose -f <file> ps` lists what Dockwell runs. The CLI prints the file alone, to pipe into `docker compose -f -`.

### Secrets, and environment values

- **An export never holds a secret.** A field marked `(sensitive)` is written by name alone, as `KEY: null`, for `docker compose` to take from its shell, and listed in `omitted`. Today every environment variable is, since `Container.env` is sensitive.
- **#29 brings plain values and secret references.** The export reads the field's sensitive option, not the field, so plain values are written once they aren't marked. On the agent, `environment()` in the container runtime is the one place a spec's environment becomes Docker's, which is where secret references resolve, in memory.

### Tests

- `internal/docker/dockertest` is a fake engine on a unix socket. It merges image config into a container as Docker does, makes volumes a container mounts when missing, refuses to remove a volume or network in use, and takes references as Docker does, ID prefixes included. The drivers' tests, the agent stream's and the API's run on it.
- `DOCKWELL_TEST_DOCKER=1` runs tests against a real engine, which skip without it and say so. CI's Docker job sets it: the drivers read back what real images fill in, `docker compose` lists and recreates what Dockwell made, and an edit through it reads back as that edit alone.
- `make docker-e2e` runs a built binary through the CLI on the machine's Docker: a service from an image runs, changes, undoes, exports to a file `docker compose` lists, a Compose file imports, and both go.

## Consequences

- **Memory.** Only the driver is in the agent build, which every server that joins runs ([ADR 0023](0023-a-full-build-and-an-agent-build.md)): `internal/docker`, `internal/id`, and the container, service, volume and network drivers. They bring no module or standard package the agent build didn't link already. compose-go and `internal/compose` are in the full build alone, so what they cost lands on the control server only. An idle agent of the agent build reads 19.6 MB, 0.7 MB more than main's at 1af231b, and a control plane with its own agent, of the full build, 39.1 MB, 2.4 MB more: medians of 5 runs after 180 s, each beside the other on one machine (ADR 0018). The agent build's binary is 0.3 MB larger, and allocates 14 KB more at init.
- **What compose-go costs** was measured before there was an agent build, when every server ran one binary. An idle agent then read 30.0 MB, 2.2 MB more than main at 2f50507, at its 30 MB warning line, and a control plane with its own agent 38.4 MB, 1.6 MB more. What that was made of, measured in pairs the same way, where a build measured twice read up to 0.3 MB apart:

  | Idle agent of the one binary, RSS | Without compose-go | With its parsing packages | With its loader |
  | --- | ---: | ---: | ---: |
  | This change | 28.1 MB | 29.7 MB | 31.7 MB |
  | Allocated at init by compose-go and what it brings | none | 0.57 MB | 2.32 MB |

  - The driver itself, the Docker client and the new kinds, cost an agent about 0.3 MB: without compose-go it read 28.1 MB, and main 27.8 MB.
  - compose-go's loader cost 3.6 MB, over the warning line. Most of that is `santhosh-tekuri/jsonschema`, which the loader's schema validation brings: it allocates 1.7 MB and spends 4 ms at init, in every process that links it. Calling compose-go's interpolation, canonical forms and types without the loader costs 1.5 MB.
  - Of what remains, `distribution/reference`, which compose-go's types import, allocates 0.41 MB at init.
  - **Something lighter would do.** A parser of the subset Dockwell takes, over the YAML library alone, would leave out compose-go's types and transforms, and `distribution/reference` with them. The YAML library adds 0.6 MB to the binary, against compose-go's 1.7 MB, so it would save about 1 MB of the 1.5, now the control plane's. It would mean writing the short syntaxes of ports, volumes and durations, and interpolation, which compose-go keeps in step with docker compose. If the control server needs the room, that is the place to find it.
- **Images are pulled by tag, not digest, so ADR 0013's digests and mirror map aren't here yet.** Resolving a tag to a digest when planning needs the revision to record the digest it resolved, so that a rollback runs the same image, which is a change to how the engine commits specs. Until then a server pulls a tag it doesn't have, and keeps the image it has.
- **Private registries** need credentials the agent sends with a pull, which come with secrets.
- **One container per service.** A service scaled by hand with `docker compose up --scale` isn't drift; replicas come with rollouts (#28).
- **Volumes and networks can't change in place.** Moving data between volumes is a backup's job, and moving services between networks a plan of several steps.
- **A Compose file's `depends_on` orders its import, but isn't kept,** so an export doesn't have it, and nothing waits on a dependency's health yet.
- Builds, `deploy`, `network_mode` other than bridge, tmpfs mounts, port ranges and ports Docker picks are refused until something needs them.
