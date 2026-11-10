# 0062. Reading an image from its registry, for a form

- Status: Proposed
- Date: 2026-11-10

## Context

New app and Deploy take an image as typed text, such as `nginx:1.27`. Nothing says whether it exists, which versions there are, or what it needs, until the plan asks Docker and the apply pulls it. A newcomer has to know that nginx listens on 80, that Postgres keeps its data in `/var/lib/postgresql/data` and wants `POSTGRES_PASSWORD`, and type each by hand. The owner's verdict on #426: pick the version, see that it exists as it's typed, and have the form filled from the image.

All of that is in the registry. A tag resolves to a manifest, a manifest names a config, and the config holds what the image declares: `ExposedPorts`, `Env`, `Volumes`, `Healthcheck`, `User`, `WorkingDir`, `Cmd` and `Entrypoint`. A registry lists a repository's tags; Docker Hub also says when each was pushed and which digest it points at.

What Dockwell does with registries today:

- A plan asks the image's registry whether it can be pulled, through Docker's `/distribution`, from the agent on each server ([`internal/resource/container/registry.go`](../../internal/resource/container/registry.go)). Docker answers with a manifest GET, which Docker Hub counts as a pull and limits for an address that doesn't sign in, so the agent asks Docker Hub 20 times in 6 hours at most for plans, keeps a yes for 15 minutes, and skips a registry that didn't answer for 5 minutes.
- The agent sends no registry credentials, so an image a registry keeps private can't be pulled yet.
- Docker pulls through its own `registry-mirrors`, which cover Docker Hub; [ADR 0013](0013-mirrors-for-releases-and-images.md) plans mirrors for the other registries.

The options weighed for where the read happens:

- **The control plane reads registries itself.** It could cache one answer for the fleet, but it reads from a different network than the server that pulls: a registry on the server's own network or behind its VPN is out of reach, a server that pulls Docker Hub through a mirror sees what the control plane can't, and the fleet's reads all count against the control server's one address. It would also have the control plane fetch hosts that anyone who can make an app names.
- **Pull the image, and inspect it.** It's what the plan does in the end, but it takes minutes and gigabytes for a form someone may abandon, and leaves images behind.
- **The agent on the server the app will run on reads, and the control plane asks it.** Chosen.

## Decision

### The agent on the app's server reads

A read goes to the agent on the server the request names, over its stream to the control plane. The server can be left out while there is only one, as a service's placement can (PRD principle 4); once there are several, the form names the one the app will run on first. That server is where the image will be pulled, so it reads with the pull's network, DNS and mirrors, and its answer is what the pull will see.

It reads the cheapest source first:

1. **What the server has.** An image already on it is inspected through Docker, with no network: its config and its digest.
2. **The registry, by its API.** The agent speaks the registry API (`/v2/`) itself, anonymously, with the bearer token a registry hands any client, rather than through Docker's `/distribution`, which costs a pull on Docker Hub. It follows the server's Docker: Docker Hub through the first of Docker's `registry-mirrors` that answers, then Docker Hub itself; plain HTTP only to a registry Docker treats as insecure (`/info`'s `RegistryConfig`); ADR 0013's per-registry mirrors once they exist.
3. **Docker Hub's own API**, for a repository's tags with their push times and digests, and for search, through Docker's `/images/search`. Neither is a pull.

### What a form can ask

- **Search:** Docker Hub's images whose names hold what was typed, official ones first, with each one's description. Nothing else searches. An answer says nothing of the server it was read through, such as which images it has, which is the install's to know ([ADR 0048](0048-members-as-far-as-one-owner-goes.md)); the server's own images are only the cheapest source of a config.
- **Tags of a repository, newest first.** From Docker Hub, by push time, each with its digest, so the tags that point where `latest` does are marked. From any other registry, by version (numbers compared as numbers, a release before its pre-releases), with `latest` marked by comparing the digests of `latest` and the ten newest versions, each a HEAD.
- **One image:** whether it's found, its digest, and its config for the server's platform. "Found" rests on a HEAD of the manifest, which no registry counts as a pull, and the config takes one GET of the platform's manifest, by digest, and the config blob.

The control plane passes on what the agent parsed: names, tags, times, digests, ports, variable names with their defaults, paths, the health check and the command. It never passes a registry's raw answer.

### What's kept, and where

- **The control plane keeps nothing.** Each read goes to the agent, so one person's read never answers another's from a cache that held a credential's view, once there are credentials.
- **The agent keeps what costs to ask again:** a config by its digest, which never changes, for 64 digests at most; a tag's digest, a repository's tags and a search for 10 minutes each. All of it goes after 30 minutes without a read, so an idle agent keeps none of it ([ADR 0060](0060-what-an-idle-agent-keeps-in-memory.md)).
- **"Not found" isn't kept,** as the fix is to push the image and look again.

### Docker Hub's limits

A config read is a manifest GET, which Docker Hub counts as a pull from the server's address. Reads get an allowance of their own beside the plans' 20 in 6 hours: 10 in 6 hours per server, counted only for a GET a cache didn't spare. Past it, the field still says Found (a HEAD), and the form says it couldn't read the image's settings this time, with every field still there to fill by hand. Found checks and tags are free, and typed references are checked only once the typing pauses.

### Private registries

The agent has no registry logins, as a pull has none ([`internal/docker/images.go`](../../internal/docker/images.go), `NotPullable`). A registry answers an image it keeps private as it answers one it hasn't got, so the read says **"Not found, or private"**, and the reference can still be typed: the plan says what the pull finds. When registry logins come, in their own record, the read sends what the pull sends, so it never says Found for what the pull can't get.

### Time

Each request to a registry has 5 seconds, and a whole read 8, inside the control plane's 10 for the agent's answer. A registry that doesn't answer isn't asked again for 5 minutes, as plans leave one alone, and a read through it meanwhile says it couldn't reach it.

### What a registry read may reach

The agent reaches only the host a reference names, its token realm, and the server's mirrors; over `/v2/` and Docker Hub's API alone; with GET and HEAD; and reads at most 4 MiB of a manifest or config, and 10 pages of tags. A link-local or metadata address is refused (`netguard`), as no registry lives there. A loopback or private address is allowed, as a registry on the server or its network is common and the pull reaches it anyway.

### In a test

A test asks nothing beyond the machine it runs on. With `DOCKWELL_TEST` set, the agent reads every registry and Docker Hub's API from the one `DOCKWELL_TEST_REGISTRY` names, such as the fake engine's (`FAKEDOCKER_REGISTRY`), and reads none without it.

### Who may ask

Anyone who may propose an app in an environment: a developer in its project, and above (`REACH_PROJECT` through the environment named), as `CheckCompose` is. The server is named by ID, as a service's placement names it, and the answer says nothing of the server beyond what its pull would see. It's a read: nothing changes, so there's no plan.

### What the panel does with it

- The image field offers matches as it's typed, then the repository's tags newest first, with `latest` named for the version it points at, and any reference can still be typed.
- Beside the field: Checking, Found, "Not found, or private", or "Can't reach the registry".
- On Found, the form fills what the person hasn't touched: the port in the app from `EXPOSE`, with a free server port; a volume row for each `VOLUME` path; and the variables the image declares, as rows with their defaults. Changing the image refills only those.

## Consequences

- A newcomer picks `nginx`, then `1.27`, and the form says Found, fills port 80 with a free server port, and shows where it will answer, with nothing typed beyond the name.
- The agent gains a small registry client and an image cache that empties when idle, and its protocol a request and an answer for each of the three reads (field numbers 200 to 209).
- New API methods, each with its role and reach declared: `SearchImages`, `ListImageTags` and `GetImage`, whose answers belong to no project and aren't narrowed (`requestBound`).
- The agent build links `internal/registry` and `internal/netguard`.
- Docker Hub sees at most 10 more counted pulls in 6 hours from each server, and only when someone reads an image no cache had.
- Private images read as "Not found, or private" until registry logins come.
- Deploy can offer the newer tags of what an app runs, from the same read.
