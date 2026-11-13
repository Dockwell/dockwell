# 0086. Registry logins for private images

- Status: Proposed
- Date: 2026-11-13

## Context

Dockwell sends no registry credentials (#583). An image a registry keeps private can't be pulled by a plan or a deploy, its update check reads "Can't check: private or not found" ([ADR 0079](0079-knowing-a-newer-version-of-an-app-is-out.md)), and the image field says "Not found, or private" ([ADR 0062](0062-reading-an-image-from-its-registry.md)). Both records left the credentials to a record of their own, and said a check and a read send what the pull sends once they come. People keep their own images on Docker Hub, GHCR, GitLab, Amazon ECR or a registry they run, and 1Panel takes a login per registry.

What bears on it:

- **Secrets are sealed at rest and reach only the servers that use them,** sealed to one agent stream, cleared after the step, and never in a plan, a log, an export or the record ([ADR 0022](0022-secrets-encrypted-at-rest.md)).
- **Every change to managed state is a revision with a plan** (PRD principle 2). A backup destination is an install-wide resource whose keys are sealed and sent to one server for one call ([ADR 0033](0033-backups-of-volumes-and-databases.md)): the shape this needs.
- **Install-wide objects that projects name** are seen below an admin by name and state alone ([ADR 0049](0049-roles-and-invites-by-project.md), class 2).
- **The agent on the app's server reads registries,** with the pull's network, and reaches only the reference's host and its token realm, through `netguard` for any host on the internet (ADR 0062).
- **A build pushes nowhere.** An image built on a server is loaded into its Docker, and reaches others over the agent stream ([ADR 0046](0046-builds-from-git.md)). What a build pulls is its base images, through BuildKit.

## Decision

### A login is an install-wide resource

`registry.login`, named like any resource, with this spec, sealed as ADR 0022 seals any kind with a `(sensitive)` field:

| Field | What it is |
| --- | --- |
| `registry` | The registry's host, as an image names it: `docker.io`, `ghcr.io`, `registry.gitlab.com`, `registry.example.com:5000`, or ECR's `<account>.dkr.ecr.<region>.amazonaws.com` |
| `username` | Who signs in |
| `password` | A password, or better a token that only reads images (sensitive) |
| `ecr` | For ECR, in place of the two above: an access key's ID and secret (sensitive) and the region |
| `token_host` | The host the registry sent a client to for its token when the login was checked, such as `auth.docker.io`: the one place beside the registry the login ever goes |
| `projects` | The projects whose apps may pull with it; empty for every project |

- **A revision with a plan.** Adding, changing or removing a login is a change an admin plans and applies, as for a backup destination: `registry.login` is an install-wide kind, so planning and applying it is an admin's. The plan masks the sensitive fields, as for every sealed kind, and says the rest. An update that leaves a sensitive field out keeps it, so changing which projects use a login never asks for its password again. It keeps it only while the login goes where it went: an update that moves it to another registry, token service or user needs the password again, so a kept password is never sent somewhere new, as an alert channel keeps its token (ADR 0044).
- **It's never exported.** A login belongs to no project, so no Compose export, backup of a project or plan of an app holds it; an app names it, by name.
- **On Activity** as any change: who planned and applied it, by its name.

### Which login a pull uses

- **What the app names.** A service's container gains `registry_login`, the name of a login. A plan refuses one that isn't there, or whose `projects` leave out the service's project, in words that don't say which: "There's no registry login ghcr-ci this app may use".
- **Or the one there is.** A container that names none pulls with the login for its image's registry, when its project may use exactly one. That's the default everywhere an image comes in, the deploy sheet, a Compose import, a catalog install and the API alike, so saving a login is all a private image needs. With several, it names none, and the panel asks which.
- **The plan says which,** on the step that pulls ("pulls with ghcr-ci"), and its fingerprint covers it, so a login added between the plan and its apply makes the plan stale rather than change what's applied.
- **A check and a read send what the pull sends.** An update check (ADR 0079) uses the login its app's pull would, and the image field reads with the login it would pull with.

### Where a login goes

- **Only to the server that needs it, for one call.** A step that pulls, a plan that asks whether an image can be pulled, an update check, a read for a form and a live check each carry the login for that image's registry, sealed to the agent's stream with HPKE as a step's values are (ADR 0022, "Delivery"), apart from them, and sealed again for the next stream when a call moves. The agent opens it in memory for that call and keeps it no longer. A server whose apps use no login never holds one, and an agent from before logins is sent none: the plan says to update it.
- **Only to the registry and its token service.** Before the agent uses a login, itself or through Docker, it reads the registry's challenge: a Basic challenge from the registry's own host, or a Bearer challenge whose realm is on the registry's own host or on the login's `token_host`. Any other realm, as a registry that now points elsewhere, is refused, and nothing is sent: "ghcr.io now sends clients to another host for its tokens, so Dockwell won't sign in there. Check the login again". A token service on another host is reached through `netguard`'s client for the internet alone. Dockwell's own client never sends a login to a mirror.
- **Docker gets it for that one pull.** The pull and the plan's `/distribution` ask carry it in `X-Registry-Auth`, after the challenge check, and only for an image on the login's own registry: Docker sends a login to the registry of the image it pulls, whatever the login says it's for, so a helper image a step pulls from elsewhere goes without it. Docker keeps none of it. A registry that doesn't answer the challenge check gets no login through Docker, and what the server has stands. A server whose Docker names `registry-mirrors` gets no Docker Hub login, as Docker would send it there too.
- **Never in a log, an error or a plan.** What the agent and the control plane say of a login is its name and its registry. A registry's own error is passed on as its words.
- **The agent's desired-state cache** ([ADR 0054](0054-the-agent-keeps-its-desired-state.md)) keeps no login. A container the agent starts again while the control plane is away runs the image the server pulled already; one that needs a new pull waits for the control plane, as an ECR token would have ended by then anyway.
- **What a login read stays its own.** The agent keeps what a read with a login found apart from what a read without it, or with another, finds, so a project that may not use a login never sees what it read. A token a registry gives for a login serves that login's next read alone, until it ends. Docker Hub's own API, a host of its own, gets no login: a private repository's tags come from the registry.

### Amazon ECR

ECR signs in with a token that lasts 12 hours, which an AWS key gets. **The key stays on the control plane:** it calls ECR's `GetAuthorizationToken` in the login's region, signed with SigV4 as backups sign S3, through `netguard`, keeps the token in memory until an hour before it ends, and sends agents the token alone, as user `AWS`. A login whose `registry` isn't an ECR host for its region and the key's account is refused.

### A live check

`ImageService.CheckRegistryLogin` signs in at a registry from a server's agent (the one named, or the only one), without saving anything: a token with no repository in its scope from the realm the registry names, or a Basic sign-in at `/v2/`. It answers **Signed in**, **Wrong username or password**, **Can't reach the registry**, or **Token service moved**, with the token host it found, which the panel puts in the plan. It takes the credentials as typed, or a saved login's name to check it again, as when its password is rotated. A password typed now may go to the token service the registry names, wherever it is, the first time; a saved one goes only to the one it was saved with, and a registry that moved its token service is answered as moved, with where, and nothing is sent there. It's an admin's, signed in, and changes nothing.

### Who may use a login

- **Admins save them.** Every method that plans or checks a login is an admin's, as `registry.login` is install-wide.
- **A project names the logins it may use.** `ListRegistryLogins` gives an admin every login, with everything but its secrets; below an admin it gives the logins whose `projects` include one of the caller's, by name and registry alone, as ADR 0049 gives a backup destination (reach "named", trimmed to `id`, `name` and `registry`). Narrowed to a project or an environment, it gives those its apps may pull with.
- **A developer's app may pull with a login its project may use,** which the plan checks, so an admin decides which projects reach which registry.

### The panel

- **Settings → Integrations → Registries,** as the mock draws it: a card for each login, with its registry, its name and which projects use it, and **Manage**; Docker Hub and GitHub's registry as cards to connect until they are. **Connect** opens a sheet that asks for the registry (Docker Hub, GitHub Container Registry, GitLab, Amazon ECR, or another host), the username and a token, with one line on what token each takes, and checks as you type, saying **Signed in** before **Review plan** goes ahead. ECR asks for the region and the key, and the check fills the registry from them. Its name and which projects may use it, every project unless narrowed, are under Advanced. Manage changes which projects use it without asking for the token again, or removes it, each as a plan.
- **The plan says which login a pull uses:** "pulls with ghcr-ci" beside how long it's down, and on each step where a fleet's plan lists them.
- **Where an image is typed** (New app and the deploy sheet): the control plane reads it anonymously first. On "Not found, or private", it reads again with the login its pull would use, and the field says **Found, with ghcr-ci**. Where the project may use several logins for that registry, a **Pull with** picker under it names the one the app pulls with. With none, an admin is offered **Add a login for ghcr.io**, which opens the sheet with the registry filled; below an admin it says the image may be private. A Compose import and a catalog install pull with the one login their project may use, by the same default.

### Builds

A build pushes nowhere (ADR 0046). What it pulls is its Dockerfile's images, through BuildKit, whose client answers the builder's ask for the login of each registry it pulls from out of Docker's `config.json`. Whoever writes the Dockerfile picks those images: a push to the branch a build follows can add `COPY --from=ghcr.io/acme/billing / /loot` and a step that sends it somewhere. So a build gets a login only where someone with the right to it names it, and the plan says what that gives:

- **Only the logins it names,** in `ImageBuild.registry_logins`, one for each registry at most; naming none gives none, whatever its project may use. New app reads the Dockerfile's base image as anyone, and where its registry keeps it private, offers the logins the project may use for that registry, **Pull ghcr.io images with**, the only one picked, and **No login**. Naming a login the project may not use, two for one registry, or one for a registry on the loopback refuses the plan.
- **The plan says what it gives,** on the step that builds: "its Dockerfile can read any image ghcr-ci can read". Its fingerprint covers which revision of which login each is, and the step carries those revisions to its apply, which refuses a login saved again since, as it refuses one the project may no longer use. An apply sends what its plan named, and no other.
- **Sealed to the stream, for that step** (`DesiredResource.build_registry_auths`), apart from the step's values and its pull's login. The agent opens them, drops a login whose registry is on the loopback, by its name or what it resolves to, checks each registry's challenge as for Docker's pull, and drops one that doesn't answer, or refuses the step for one that moved its token service. It checks them again when the build leaves the queue, as a build can wait for another.
- **Only for the registries the Dockerfile pulls from.** The agent reads the Dockerfile at the commit, as BuildKit reads it: parser directives, lines joined at the escape character, comments and heredocs skipped, build arguments put in. Every image its `FROM` lines, `COPY` and `ADD --from` and `RUN --mount from=` name, past its own stages, says a registry, and a login for any other stays out of the builder. A Dockerfile that names an image by a build argument it isn't given says nothing of what it pulls, and a build with logins refuses it.
- **Docker's own frontend alone.** A `# syntax` directive, or `BUILDKIT_SYNTAX`, hands the build to a frontend image the repository picks, which could ask for any image with the logins. A build with logins sets the Dockerfile frontend's `cmdline` option, which in BuildKit v0.34.1 keeps it from handing the build on and does nothing else, and refuses, in words, a Dockerfile whose syntax names anything but `docker/dockerfile`, in any form BuildKit reads one (`#`, `//`, JSON, past a `#!` line), and a `BUILDKIT_SYNTAX` build argument.
- **No registry on the loopback.** A build's steps share the builder's loopback, so a step listening on `localhost:5000` would be the registry BuildKit sends that login to. The plan refuses one, and the agent drops one, by name or by what it resolves to.
- **A cache of its own.** BuildKit serves an image pinned by digest from the content its cache holds, whatever pulled it, so a build given logins keeps its cache in a volume of its own, keyed by its service and the logins it was given, and no build without them reads it. A build with no login keeps to its environment's.
- **The whole Dockerfile, or none.** The reading says up to 1 MiB of it, in base64, and a longer one refuses a build with logins, as what it pulls past that would go unread.
- **In memory, read-only, for BuildKit's client alone:** the builder gets them as Docker's `config.json` in a folder in memory, as its secrets are, at `DOCKER_CONFIG`, and never in its environment, its command or its log, which hides each password, its `username:password` and that pair's base64, as it hides a secret. A server with nowhere in memory for them builds nothing that uses one.
- **Railpack's images are Railpack's.** A build Railpack makes plans with no login, even one it names, and its apply seals and checks none: its builder and runtime images are public, and a broken login can't stop them.
- **An agent from before** doesn't say `Hello.build_logins`, and a plan that would build with logins through it says to update it.
- **Tests' builds** keep their secrets and logins in the folder `DOCKWELL_TEST_BUILD_SECRETS` names, and only with `DOCKWELL_TEST`, as a test's machine may have nowhere in memory it can write, and reach the registry `DOCKWELL_TEST_REGISTRY` names over plain HTTP where it's named so, as a test's registry has no certificate BuildKit trusts. `dockwell run` warns as it starts when `DOCKWELL_TEST` is set under systemd or OpenRC.
- **Real BuildKit proves it:** `make docker-e2e` runs `registry:2` with htpasswd on the runner, keeps an image in it, and builds from it on the agent build's server: with no login named the registry refuses BuildKit, and with the login named the build pulls the base, from a `DOCKER_CONFIG` mounted read-only, and runs. BuildKit v0.34.1's client writes `.token_seed` there only for a registry that answers with a token service, and goes on without it on a read-only folder (`session/auth/authprovider/tokenseed.go`).
- **Not yet:** BuildKit could be handed a pull token scoped to the repositories the Dockerfile names (`registrytoken` in `config.json`), got with the realm-checked client, so the password never enters the builder. A token lasts minutes and a build can outlast it, so it waits; until then a named login reads what the login can, which the plan says.

### Numbers

The agent protocol takes 310 to 319: `DesiredResource.registry_auth` 310, for a plan's ask and a step's pull alike, `ImageUpdatesCall.registry_auth` 311, the image reads' 312, `CheckRegistryLoginCall` and its result 313, `Hello.registry_logins` 314, by which an agent says it signs in, `DesiredResource.build_registry_auths` 315 and `Hello.build_logins` 316, for builds. Fields this adds to other messages take 100 to 109: `Container.registry_login` 100, the resource oneof's `registry_login` 100, `GetImageRequest` and `ListImageTagsRequest`'s `registry_login` 100 and `anonymous` 101, their responses' `registry_login` 100, `PlanStep.registry_login` 100, `build_logins` 101 and `build_login_versions` 102, `ImageBuild.registry_logins` 100, and the enum values `IMAGE_ANSWER_SIGN_IN_FAILED` and `UPDATE_PROBLEM_SIGN_IN_FAILED` 100. The new messages number from 1.

### After the security review

An independent review of the first slice found these, and each is closed, with a test that fails without it:

- **Every challenge is read, as Docker reads them.** Docker reads every challenge in every `WWW-Authenticate` header, tries Bearer before Basic, and sends the login to the realm it names. So the check before Docker is handed a login reads them all, with a scheme followed by any whitespace, and one Bearer realm anywhere that isn't the registry's own host, or the saved token service over HTTPS, refuses the login. A challenge it can't read refuses it too, saying so, as Docker could send it where Dockwell can't see. A realm on another host goes over HTTPS alone, for Dockwell's own reads as well.
- **A private image one app pulled isn't another's to run.** The agent remembers which login pulled each image, by its ID, in a file of its own, names alone. Before a step runs a copy the server has that a login pulled, and the step doesn't pull with that same login, the registry is asked with the step's own pull, and a refusal fails the plan: a project that may not use a login can't run what it pulled.
- **The sheet asks nothing of a host half typed.** Another registry's host counts once its field is left, or on Enter. It's asked with no login first, and a token goes only to a registry that answers as one. Once the registry changes after a token is typed, the token is checked only when someone asks, with **Check with ghcr.io**.
- **The fingerprint covers which revision of which login** a pull uses, so a login saved again under its name makes a plan stale.
- **An ECR region is a region's name**, as it goes into the address of ECR's API.
- **The CLI takes no JSON on its command line that sets what signs in** to a registry login or its ECR key, as for an alert channel's token: it comes from a file or standard input.
- **Docker is handed no Docker Hub login behind registry mirrors,** as Docker sends that login to the mirrors its `daemon.json` names. Dockwell's own reads go to Docker Hub alone with it; Docker pulls from Docker Hub as anyone there.

## Alternatives considered

- **A login in each environment, as a secret is.** Any developer could add one, and the same login would be stored once for each environment. The issue asks for logins an admin saves, which projects then use.
- **`docker login` on each server.** It leaves the password in `~/.docker/config.json` on every server, for anything root runs to use and send, and Dockwell couldn't say which app uses it.
- **The control plane pulls or reads for the servers.** ADR 0062 turned that down: it reads from another network than the pull.
- **Hand Docker a bearer token of our own instead of the password.** It would keep the password from Docker, but a token from Docker Hub lasts five minutes, and a large pull outlives it with no way to renew it.

## Consequences

- A private image pulls, deploys, checks for updates and fills the form once an admin saves a login, with nothing more to do in each app.
- A login goes to the server that pulls, for that call, and from there to its registry and the token service it was saved with, and nowhere else.
- An admin decides which projects reach which registry.
- An ECR key never leaves the control plane.
- A registry that moves its token service has its logins refused until they're checked again.
- A build reads a private image only with a login someone named for it, from a registry its Dockerfile names, through Docker's own frontend.
- Slices: (1) logins, the check, pulls, update checks and form reads; (2) builds' base images, as above.
