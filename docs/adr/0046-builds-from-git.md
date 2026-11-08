# 0046. Builds from Git, as a step of the deploy that needs one

- Status: Accepted
- Date: 2026-11-07

## Context

Issue #253 asks Dockwell to deploy from Git: build an app from a repository, deploy on a push, and work with a CI that builds. The PRD makes builds from Git, push-to-deploy and CI integration P0 at launch (Deploy pillar), and signature experience 1 starts from "a single New sheet that accepts a repo URL". Until now an app ran an image someone else built, or a Compose file.

The mocks say how it looks:
- `Apps-New.dc.html` and `Onboarding-FirstDeploy.dc.html` take a repository and a branch, show how it builds ("Dockerfile"), and end in a plan: "Nothing runs until you apply the plan". The plan lists `source` and `build` as fields of the app.
- `App-Deployments.dc.html` lists revisions by commit, with who pushed it and what triggered it, and says of each build how long it took, how much came from the cache, which server built it and the image it made.
- `App-Settings.dc.html` has "Source and build" as part of the app, and "Build on" as a choice of servers.

What earlier records settled, which this has to keep:
- **Every change is a revision with a plan** (PRD principle 2, [ADR 0009](0009-resources-revisions-and-plans.md)). Only `internal/revision` writes revisions.
- **A new build needs a tag of its own** ([ADR 0021](0021-the-docker-driver.md#what-pull-means)). An image built on the server has no digest, so only its tag says what ran, and a build under a tag that's there is refused, or a rollback would have nothing to go back to.
- **A failed step leaves what ran** (ADR 0021, [ADR 0027](0027-health-checks-rollouts-and-rollback.md)): a pull that fails before anything stops marks its step untouched, and the change's rollback has nothing to do.
- **Secrets reach only the servers that use them, sealed for the step** ([ADR 0022](0022-secrets-encrypted-at-rest.md)), and never a plan, a log or the record.
- **The agent build links only what an agent runs, under 40 MB idle** ([ADR 0023](0023-a-full-build-and-an-agent-build.md)), and a server may have 1 GB of memory in all.
- **Log lines pass through and sit nowhere** ([ADR 0025](0025-logs-per-deploy.md)), for the logs of what runs.

## Decision

### A build is a step of the deploy that needs it

The source and how to build it are part of the service's spec, as the mocks draw them, in `Service.build`. A change that needs a new image pins what it builds as it is planned, and the step that replaces the app builds it first:

1. **Propose.** The control plane reads what the branch or tag names now, and writes the commit into the spec, with the image the build will make, under a tag of its own (below). The plan says what it builds: "Builds github.com/acme/web at 8be41d2 (main)". The fingerprint covers it.
2. **Apply.** Applying commits the revision, as for any change, then runs its steps. The service's step builds the image on its server, then replaces the app with it as a deploy of an image does, health check, rollout and all.
3. **A build that fails, or is cancelled, fails its step before anything is touched.** The step says so (`untouched`), the change fails, and its rollback has nothing to do: what ran goes on running, and the app's page says "web stays on r11". The revision this change made reads failed, as a failed pull's does.

So a build is never a change of its own, and nothing about a build bypasses the revision store: the revision names what is built before it is built, and the record never has to be corrected after.

Two things follow:
- **A rollback to a revision whose image is gone builds it again,** from the commit that revision pinned, as its spec says how. A commit that's gone from the repository since, as after a force-push, fails that rollback's step, and says so.
- **Undo and rollback put back the spec as it was,** image tag included, so they build nothing when the server still has that image, which it keeps (below).

The other ways we considered:
- **A build as an object outside revisions,** with a deploy of its image proposed once it finishes. A failed build could never touch what runs. But the plan would come after the build, where the mocks put it first, and a push would leave a build and a separate change to read together. A step already gives "a failure touches nothing" (`untouched`), and the plan says what it will build.
- **Building as the plan is made,** so the plan names a finished image. Planning would take minutes, a plan discarded would have built for nothing, and plans are meant to change nothing on a server.

### The spec

```proto
message Service {
  oneof runs { Container app = 1; }
  ...
  ImageBuild build = 130;
}
message ImageBuild {
  oneof source { GitSource git = 1; }
  string context = 2;               // the folder the build starts from, such as "web"
  string dockerfile = 3;            // relative to context: "Dockerfile" unless set
  map<string, string> args = 4;     // as docker build's --build-arg
  map<string, string> secrets = 5;  // BuildKit secret ID to the environment's secret
  string target = 6;                // the stage to build: the last unless set
}
message GitSource {
  string url = 1;     // "https://github.com/acme/web.git"
  string ref = 2;     // a branch or a tag: the repository's default branch unless set
  string commit = 3;  // set as a change is planned, unless the edit names one
}
```

- **A URL is HTTPS, or plain HTTP to a private network,** with a host and a path, and nothing that signs in: a user, a password or a token in it is refused, as the spec is in plans, history and the container's labels. Private repositories, SSH and providers' own credentials come with #253's third slice. `github.com/acme/web`, as people paste it, reads as `https://github.com/acme/web`. Plain HTTP takes only an address on a private network (RFC 1918's, IPv6's unique local ones, or this machine), or a `.local` or `localhost` name, which the building server resolves to a private address again before it builds: anyone between a server and a repository over plain HTTP elsewhere could choose the commit a plan pins.
- **The control plane's lookups dial only where a repository can be.** A plan's lookup of a branch refuses the control server itself, link-local addresses such as a cloud's metadata service, and, over plain HTTP, anything but a private network, at dial time, so no token can make the control plane ask its own ports or map a network by what answers. A test's Git server, on its own machine, takes `DOCKWELL_TEST`.
- **A ref is a branch or a tag** as Git names them, or a commit's 40 hex digits. Empty takes the repository's default branch, as `git clone` does.
- **`context` and `dockerfile` are relative paths with no `..`,** so a build can't reach outside its repository.
- **Build arguments aren't secret.** They end up in the image's history, so plans and the panel show them, and the plan's help says to use a secret for a credential. Up to 64, each value up to 4 KiB.
- **Build secrets are the environment's secrets, by name,** as the app's own are (`Service.secrets`). The ID is what a `RUN --mount=type=secret,id=…` names.
- **The app's image is Dockwell's to set** on a service with a build. An edit that leaves it empty asks for a build. One that names an image Dockwell didn't build is refused, saying to remove the build to run an image instead.

### What the plan pins

- **The commit.** The control plane asks the repository what its refs name, over Git's smart HTTP protocol (`info/refs?service=git-upload-pack`), with a 10 s limit and 32 MiB of answer at most. A branch or tag it hasn't got refuses the change, naming what it has nearest. An annotated tag is read as the commit it points at. A ref of 40 hex digits is taken as the commit, unasked. **A build follows a branch or a tag, and nothing else a repository advertises:** a pull request's head, `refs/pull/7/head`, is whatever anyone with a fork pushed, so a ref under `refs/` other than `refs/heads/` and `refs/tags/` is refused, and a short name is read only as a branch, then a tag. The plan writes the ref by its full name, `refs/heads/main`, into the spec, and the build fetches exactly that, as Git's own short names reach further (`pull/7/head` finds `refs/pull/7/head`) and take a tag before a branch of the same name. People read it as `main`.
- **The image, by a tag that names the build:** `dockwell.invalid/<service ID>:<commit, 12 digits>-<change ID>`, such as `dockwell.invalid/svc_01kbz…:8be41d2c3f4a-01kbz…`. Every build has a tag no other has, as ADR 0021 needs. `.invalid` is a name that never resolves (RFC 2606): a name with no host, such as `dockwell/web`, would be Docker Hub's, and a pull of it, after the server lost the image, would run whatever someone published there.
- **When a change builds:** when the app's image is empty, as a new app, a deploy, or the panel after a change to the build leaves it; and when an edit changes the build's settings, or its commit, and keeps the image it had, as a script that edits the spec would. An undo, a rollback and adopting drift build nothing they don't have to: they carry the image a revision had.
- **`DeployService` on a service with a build builds the newest commit of its ref,** with nothing else changed, whether or not that commit is new: a build can come out differently, as when a Dockerfile installs what's newest. Naming an image there is refused, saying the app builds from Git. Deploying a given commit comes with CI integration, in the fifth slice.
- **Only one server, for now.** A plan that places a service with a build on more than one server is refused, saying it runs on one until one build can reach the others (below).

### Where a build runs

**On the server the app runs on.** It needs no other server, which a person on one server hasn't got, and the image is where it runs. The mocks' "Build on" a pool of servers is the milestone's later issue for build servers.

**How an image built on one server reaches others:** over the agent stream, not a registry, as the PRD says an external registry is optional. The control plane asks the server that built it for what the other hasn't got (`docker save` of the image, less the layers the other has, which `docker load` takes as it is), and passes it on. It comes with build servers, and until then a service that builds runs on one server. Building on each server instead would build twice, and two builds of one commit can run different code, as a Dockerfile that installs what's newest does.

### BuildKit, through the server's Docker

**A build runs BuildKit in a container of its own, one per build,** from `moby/buildkit:v0.34.1`, by the digest of its multi-platform index, as Caddy's image is pinned (ADR 0024), and pulled through the same path. The container:
- runs the Dockerfile frontend, `dockerfile.v0`, on a checkout of the commit (below), and writes the image to standard out as Docker's image archive, which the agent passes to Docker's `POST /images/load` as it comes. Nothing of it touches the disk on the way;
- is privileged, as BuildKit's own sandbox for each `RUN` step, runc, needs to make namespaces and cgroups. What a Dockerfile runs stays in that sandbox: no `--privileged` step and no `network=host` (BuildKit's entitlements are off), no mount of the server, and Docker's default seccomp profile. Each step also runs under Docker's `docker-default` AppArmor profile where the server has it loaded, as Docker's own builder applies it: the script mounts securityfs in the builder, whose own profile is none, and gives BuildKit the profile in `buildkitd.toml`; where AppArmor can't be reached, the build's log says its steps run under seccomp alone;
- runs on `dockwell-build`, a bridge network of its own with inter-container traffic off, so a `RUN` step, which shares the builder's network, reaches no container on Docker's default bridge, where hand-run containers live. It still reaches the server's own addresses and the internet, as a build that installs packages must. Dropping the cloud metadata address from that network is a rule in `DOCKER-USER`, which waits for the firewall's work (ADR 0052) rather than writing rules there alone;
- has its logging off, as what it prints is the image, and Docker would keep a copy;
- is removed as the build ends, whatever happened, and one a crash left is removed when the agent next starts, by its label.

The other ways we considered:
- **Docker's own builder** (`POST /build?version=2`), BuildKit inside `dockerd`, which makes the image in Docker's store with nothing copied. It can't be held to a limit: its solver runs in `dockerd`'s own memory, and a `RUN` step's cgroup comes from `dockerd`'s cgroup driver, which differs from server to server. Secrets would need BuildKit's session protocol, gRPC served back over a hijacked connection. A server of 1 GB would have nothing to stop a build taking it down.
- **A BuildKit daemon that stays up,** as `docker buildx create` makes. It would start faster and share one cache, but it holds 30 to 50 MB of the server's memory while it waits, for every server, including the many that build once a week.
- **`docker buildx build` from the agent.** It needs Docker's CLI and its buildx plugin on every server, which Dockwell otherwise doesn't, and drives the same builder.

**The builder is a trust boundary.** A privileged container is root on the server, so an escape from BuildKit's sandbox, as the runc escapes of 2024 were, is root on the server from code any repository a build names. Where only the owner deploys, that is no more than the owner's own code running as root in the app's container. Once people other than the owner can build, as roles (#299) and tokens bring, naming a repository is running code as root on the server, which is a person's decision: a token can deploy a service that builds, through CI (#253's fifth slice), only where the service reaches nothing that is as good as root, as `runsSomethingElse` already holds for the whole spec, build included. BuildKit's rootless image, `moby/buildkit:<v>-rootless`, unprivileged with seccomp, AppArmor and systempaths unconfined, would lift the boundary to the server's user namespaces; it needs unprivileged user namespaces, which Ubuntu 24.04 restricts by AppArmor (`kernel.apparmor_restrict_unprivileged_userns`), so it waits until it can be tried on the servers Dockwell runs on.

What this costs: pulling BuildKit's image once, 111 MB for amd64; starting `buildkitd` for each build, about a second; and loading the image into Docker each time, which Docker writes to a temporary directory before it takes the layers it hasn't got. `make docker-e2e` measures a build end to end on CI's Docker.

### Builds without a Dockerfile: Railpack

#253's second slice builds a repository that has no Dockerfile with **Railpack** (railpack.com), which reads what the source is, such as a Node app from its `package.json`, and writes a plan of what to install, build and start. Nixpacks, its predecessor, is in maintenance; buildpacks need a lifecycle image per language and their own registry protocol. Railpack's BuildKit frontend builds its plan in the same builder as a Dockerfile, so everything above holds for it: the container, its limits, the cache, the network, the disk floor, the log and its masking.

- **What builds it is the build's `builder`:** unset, the Dockerfile when the commit has one where the build says, and Railpack when it hasn't; or the Dockerfile, which the commit must then have; or Railpack, which takes no Dockerfile or stage. With Railpack, `build_command` and `start_command` stand in for what it finds. They are the code that runs as much as a Dockerfile is, so they count in what writes a folder (`WriterOf`), and so do the service's own `command` and `entrypoint`, which take the place of the start command in the image. A Dockerfile build refuses them, and so does a plan that finds the commit has a Dockerfile when the builder is unset, as they would do nothing.
- **Railpack never runs in the builder.** `railpack prepare` reads a stranger's repository, outside BuildKit's sandbox, and the builder is privileged: the build's secrets, BuildKit's state and the server's devices are in reach there. So a build that may use Railpack reads its commit first, as a plan does (below), unprivileged; the reading writes Railpack's plan, and the agent hands that file to the builder, read-only, from memory, where the builder builds it with Railpack's frontend. The builder runs git, `buildkitd` and `buildctl`, as for a Dockerfile, and never mounts what Railpack runs from. Railpack reads symlinks out of the checkout as files (a `Procfile` that points elsewhere becomes the start command); that happens in the reading, which holds nothing but the checkout.
- **Pinned:** Railpack 0.43.0, by the digest of its frontend's multi-platform index (`ghcr.io/railwayapp/railpack-frontend`), whose image holds the `railpack` binary too. The agent pulls it, the first time a reading needs Railpack, and copies the binary into a volume, `dockwell-railpack`, in a container that reaches no network. Then, once, it has Railpack fetch `mise`, which it reads versions with, into the same volume, in a container that reads a `package.json` of its own: nothing of anyone's repository runs while the volume can be written. **mise isn't pinned**: Railpack fetches it from GitHub by its version, over TLS, with no checksum. A reading mounts the volume read-only, and links mise into a folder of its own in memory, so nothing a reading does reaches a later one; without mise in the volume, as on a server that couldn't fetch it, a reading fetches its own, and the agent tries again after 10 minutes. A Dockerfile build never pulls Railpack. The plan Railpack writes names its own builder and runtime images by tag, which BuildKit pulls, as Railpack releases them.
- **Reading a commit is a call of its own (`DetectBuild`),** which the panel's New app makes as you type, and which a plan makes for every build that builds with Railpack, or may, and which a build makes as it starts: a container of BuildKit's image on the build network, unprivileged, with 1 GiB of memory, checks out the commit as a build does, and either finds the Dockerfile, with the image its last stage starts from, or has Railpack write its plan. It builds nothing.
  - **What it holds is in memory:** the checkout (512 MiB at most), Railpack's and mise's files (256 MiB) and the plan, all counted against its 1 GiB, so a repository that checks out larger, as a 1.7 MB one of a repeated line checks out to 257 MB, fails as "larger than reading it may take" rather than fill the disk the server's databases share.
  - **Few at once:** a server reads two commits at a time, and the control plane reads a request that's already being read once, for everyone who asks; one that went away, as a form does as you type on, leaves the next to read it anew. A plan reads its builds at once, not one after another.
  - **Where it builds:** with more than one server and none named, New app reads nothing until you choose where the app runs, as a reading pulls Railpack, and checks out, on the server it reads on.
  - **What it found is in the plan,** "With Railpack, as Node 22. Runs npm install, npm run build, then starts with npm run start", with what Railpack warns of; a commit Railpack can't build refuses the plan before anything runs, with what to do by what Railpack said: a source it can't tell the language of, "add a Dockerfile, or set both a build command and a start command"; a version it can't find, "change the version, or set `RAILPACK_<TOOL>_VERSION` as a build argument"; nothing that starts the app, "set the build's start command", as Railpack is asked to refuse it (`--error-missing-start`) unless the service starts with its own command, since an image with no start command runs bash, which exits at once. A reading that couldn't reach where versions are listed doesn't refuse the plan: like a server that can't be asked, it leaves the plan without the reading, and the build reads it again. A reading is kept 15 minutes, by server and settings, so a form's reading and the plan after it read the commit once. The build records what it found (`Build.detection`), with its secrets masked.
- **Build arguments and secrets** reach Railpack as its variables, which it hands its steps as BuildKit secrets: in the build, each in a file in the build's directory in memory, a build argument as `a-NAME` and a secret as `s-NAME`, named to `buildctl --secret`. The reading that writes the plan has each build argument, and each secret by its name with a stand-in for its value, as Railpack's plan names secrets and holds none of their values; so a secret's value never reaches Railpack's reading, and what Railpack decides from a value, as PHP's `CACHE_DRIVER=redis`, comes from a build argument. A secret's ID is a variable's name, so a build that uses Railpack takes only those, and not `PATH` or `HOME`; and a build that may use Railpack takes no secret Railpack reads as its own settings, `RAILPACK_…` or `NIXPACKS_…`, such as `RAILPACK_START_CMD`, which would change what it installs and starts out of the plan's sight: as a build argument, it's seen, and counted in the writer. Railpack's `secrets-hash`, which makes a step that uses them build again when one changes, is an HMAC keyed with a key the agent keeps in its data directory, so it says nothing of the values; and its `cache-key`, which names its cache mounts, is an HMAC of the service under the same key, so no Dockerfile of another service in the environment can name one.
- **Steps are counted as Railpack's frontend names them,** such as `npm install`, and BuildKit's own vertices, such as loading the context, aren't.

### What a build may use

A build gets a container's limits, which the kernel holds it to, so a build can't take a small server down:
- **Memory:** what the server has available as the build starts, less 256 MiB kept for what runs, at least 512 MiB and at most 8 GiB, with no swap. On a server of 1 GB running a few apps, that is 512 MiB.
- **First to go:** its `oom_score_adj` is 1000, so if the server runs short anyway, the kernel kills the build before anything else.
- **CPU:** every CPU but one, and at least one, at a quarter of the weight of an ordinary container (`cpu-shares` 256), so on a busy server what runs comes first and the build takes what's left.
- **Processes:** 4,096 at most.

A build that runs out of memory fails with words that say so: "The build ran out of the 512 MB it may use on this server". A setting for a server's build limits, or an app's, comes when someone needs one.

**One build at a time on a server.** BuildKit's cache can't be shared by two daemons, and a small server can't take two builds at once. A second build waits for the first, and its step says so, as `queued`. A newer push replacing an older one that waits is the fourth slice's deploy queue.

**A build has 30 minutes,** on top of the time any step has, and its call's timeout grows by that much. One that takes longer fails, saying how long it had.

### The checkout

The container checks out the commit itself, with the `git` in BuildKit's image. **It fetches the branch or tag the build names, by its full name, and checks out the commit only if that branch has it,** deepening the fetch from 1 commit to 200, then to the branch's whole history without its files, until it finds it. GitHub serves any commit of a fork through its parent's URL, so a build that fetched a commit by its ID could build a stranger's code under the repository's name; one not on its branch fails, saying to deploy again. A commit the build names as its ref is fetched as it is, and is code of its own (below). It then reads the commit's subject and author for the record, checks the Dockerfile is there, and builds from that folder as a local context. BuildKit's own Git source would fetch without a checkout, but it gives neither the subject nor a place to find what to build without a Dockerfile, which #253's second slice needs.

### The cache, and the images builds make

- **Each environment's builds keep their cache in a volume of their own, `dockwell-build-cache-<environment>`,** so a `RUN --mount=type=cache` one app's Dockerfile fills, which takes its ID from its path, is never read by a build of another environment, and no project's build plants files in another's image. The bound, a tenth of the disk Docker keeps its data on, between 1 and 10 GiB, is split between the server's build caches, each at least 512 MiB; BuildKit collects its garbage after each build, and `buildctl prune --keep-storage` holds its cache to its share once the build ends. A server keeps the caches of the eight environments that built last, and removes the one that built longest ago past that. A cache whose build didn't prune it, as one cancelled, stopped or killed by a crash, prunes before its next build.
- **A build leaves Docker's disk a floor:** a twentieth of it, between 1 and 10 GiB. A build doesn't start under it, and the agent reads the disk every 250 ms while the build runs and stops it as soon as it falls under, saying so, as a `RUN` step that writes tens of GB, a checkout's history or `docker load`'s copy could otherwise fill the disk the server's databases write to; at 0.5 GB/s a write lands in the page cache faster than a slower poll would see it. What a step wrote is in BuildKit's snapshots, in the environment's cache, not the builder's own layer, so a build the disk stopped removes that cache, saying so, and the next build starts from nothing. A build refused under the floor, whose environment's cache a stopped or crashed build left unpruned, removes that cache first, as its next build couldn't start to prune it. The cache's bound alone says nothing of a build under way.
- **A service keeps the image it runs and its three newest builds.** Once a build is running, the agent removes the service's older built images, as Docker can't count them against anything. A rollback further back builds again, from its commit.
- **Cache hit** in the panel is the share of the build's steps BuildKit took from its cache.

### Secrets in a build

- **A build secret comes as a BuildKit secret, never as an argument, a variable or a label.** The control plane seals its value into the step's values, as for the app's secrets, under `build:<ID>`, so it reaches only the server that builds, and the agent opens it in memory for that step (ADR 0022). The agent writes each to a file in a directory in memory, `/run/dockwell/builds/<random>`, with mode 0600, mounts that read-only into the builder, and names each file to `buildctl` with `--secret id=<ID>,src=…`. The directory is removed when the build ends. BuildKit mounts a secret only into the `RUN` steps that ask for it, and never writes it into the image or its cache. A server with no such directory in memory refuses a build with secrets, saying so.
- **The build's log hides each value as it comes, before anything keeps it,** as a health check's output hides the app's ([ADR 0027](0027-health-checks-rollouts-and-rollback.md)): every line, a step's error and its name, and the error the build ends with. A value of more than one line, as a key file is, is hidden line by line, and one stored with a newline at its end without it. A step that writes a secret into a file of the image puts it there, as it would with any secret.
- **A secret's revision is pinned** like the app's, so rotating one is a plan. It builds nothing: the next build reads the new value. BuildKit leaves a secret out of a step's cache key, so a step that uses one comes from the cache after a rotation, unless something before it changed; deploy with a change to the Dockerfile, or prune the cache, to run it again with the new value.

### What a build's log keeps

A build's log differs from an app's: it's the output of Dockwell's own process, it ends, and it's needed most after the fact, when the build failed and its container and perhaps its server are gone. So it is kept, within bounds:
- **The agent reads it as it comes:** the checkout's lines, and BuildKit's progress as JSON (`--progress rawjson`), which says which step each line is from, when each step started and finished, and which came from the cache. It hides secrets, cuts each line at 4 KiB, and sends what it has at most every 250 ms, 64 KiB at a time, on its stream (`ConnectRequest.build`). Past 256 KiB/s it drops lines and says how many, so a build can't crowd out the calls on that stream.
- **The control plane keeps 10,000 lines of a build,** the first 1,000 and the last 9,000, and says how many it left out between, as a failure is near the end and what happened first is at the start. That is about 2 MiB at most. It keeps the logs of a service's 20 newest builds, and the record of every build for as long as its revision is kept.
- **A build's state is the agent's word:** queued, building, done, failed or cancelled, with the step it is on, the failing step and its last line. A server that goes away during a build leaves it as it was, as `lost`, which the step says (ADR 0028). The control plane keeps a build only for a step its change has for that service on that server, as an agent speaks for its own server's steps. Steps that run at once hold at most 1 MiB of lines with no end yet between them, the longest ended first.
- **The failing step's last line is log content,** for a person or a token with the logs grant, as `failed_line` of `ListBuilds`, `GetBuild` and a revision's build. The error a build fails with, which goes on its change for anyone who reads changes, names the step, quoted, and says its log marks what it printed last.

### How a failure reads

- **Words for each way people hit first,** in `internal/build`: the commit or ref not there, the commit not on its branch, the repository not reachable, no Dockerfile at the path, a step that failed, which names the step ("The build failed at step 4 of 9, "RUN npm ci": its log marks what it printed last"), running out of memory, out of time, the disk under its floor, and cancelled. The failure's reason is `build`, or `cancelled`, so the panel can say "Cancelled" and a form can point at its field.
- **A plan that can't read the repository is refused at the form,** with the field it's about: "github.com/acme/web has no branch main. Its branches include trunk and dev".

### The API, the CLI and the panel

- **`BuildService`:** `ListBuilds` for a service (`GET /v1/services/{id}/builds`), `GetBuild` (`GET /v1/builds/{id}`), `ReadBuildLog` (`GET /v1/builds/{id}/log`), `WatchBuild`, a stream of a build's state and lines until it ends, and `CancelBuild` (`POST /v1/builds/{id}:cancel`). A revision says which build made its image (`ServiceRevision.build`). So `dockwell builds list web`, `dockwell builds follow bld_…` and `dockwell builds cancel bld_…` do what the panel does. A build's log is a read of the logs grant, as any log is ([ADR 0016](0016-scoped-api-tokens.md)), since a step can print what an app shouldn't.
- **Cancelling** asks the server to stop the build, which fails its step as cancelled: the change's rollback, which has nothing to do, follows as for any failure. It is a step of a change someone applied, so it needs the same scope that applied it, and is in the audit log with who cancelled.
- **The panel** has a Git source in New app, first as the mocks have it, and builds in an app's Deployments: each revision by its commit, its build's state, how long, its cache hits, the server and the image, and the log, live while it runs and kept after.

## Consequences

- **Memory.** The agent build gains `internal/build`, which brings no module or standard package the agent didn't link already. A build's work happens in BuildKit's container; the agent passes the image through and keeps at most one batch of lines. What `make budgets` and `make docker-e2e` measure is in the pull request that adds it.
- **A service that builds runs on one server** until the image can be copied between servers, with build servers.
- **Every build loads its whole image into Docker,** as an archive, even when only a layer changed. Where Docker keeps images in containerd, BuildKit could write into that store directly, which would save it. That waits for a server that needs it.
- **A deploy builds even when the commit is the same.** It costs a build mostly from cache, which is what "Redeploy" means elsewhere too.
- **Private repositories, providers, push-to-deploy, and CI** are #253's next slices, on this. A Compose file's `build:` stays refused until the image can be built for it the same way.
- **What the cache keeps is per environment,** so two environments of one project build the same layers twice, and one environment's apps share a cache, and can fill each other's cache mounts. That is one project's code, which its members can change anyway.
- **What a built service writes is written by its code** (#252): its repository and how it builds, the folder, Dockerfile, stage and build arguments, as `ARG BASE` in `FROM $BASE` reaches any image (`WriterOf`, which the plan's access counts by). A plan names it as the repository and a digest of the rest, `github.com/acme/web@build:5d41402abc4b2a76`: 64 bits, which no one can choose a build's settings to match. A new commit of the branch it follows is the same code, as a new tag of an image is, since the build checks the commit is on the branch; a commit named as the ref is code of its own. So changing the repository, or any of how it builds, over a folder approved for root asks a person again. A service that builds runs only images its own builds made.
- **A build that fails or is cancelled makes a rollback revision,** as any failed change does: the failed revision stays as the record of what was tried, and the rollback is what runs, so the revision store's latest is what runs (ADR 0009). The mock's failed revision with nothing after it reads as if the store's latest were a spec that never ran.
