# 0085. Most of other stores' apps, installed safely

- Status: Accepted
- Date: 2026-11-12

## Context

On 2026-11-12 the owner added 1Panel's app store as a source and found 179 of its 263 apps left out. They asked that most of 1Panel's, Dokploy's and Coolify's apps install and work (#570).

[ADR 0078](0078-other-stores-read-on-the-server.md) reads those stores on the server and converts what it can, running nothing a store holds. What it leaves out, it counts by kind. Nothing measured those counts over time, so nobody could say whether a change raised them or lowered them, and "converts" meant only that an entry passed the catalog's checks, not that the app ran.

We measured the three stores on 12 Nov 2026 through the control plane's own fetch and converter, at 1Panel's `55061b99`, Dokploy's `4f0ddfb5` and Coolify's `93c7e53e`:

| Store | Apps | Converted | Rate |
| --- | ---: | ---: | ---: |
| 1Panel's app store | 269 | 93 | 34% |
| Dokploy's templates | 612 | 169 | 27% |
| Coolify's templates | 384 | 78 | 20% |

The converter stops at an app's first reason, so the counts by kind hide what's behind each. We let each gate through in turn, in a scratch build, to see what each one holds back:

| Gate let through | 1Panel | Dokploy | Coolify |
| --- | ---: | ---: | ---: |
| None | 93 | 169 | 78 |
| A tag that moves, such as `latest` | 93 | 288 | 175 |
| and a secret inside a longer value, such as a connection URL | 97 | 387 | 233 |
| and files shipped beside the Compose file | 112 | 446 | 233 |
| and scripts on the server, taken as no-ops | 140 | 446 | 233 |
| and an app that runs on amd64 alone | 151 | 446 | 233 |

What's left after all five is mostly Compose keys Dockwell can't run yet (`hostname`, `ulimits`, `pull_policy`, `stdin_open`, `init` and a few more), Coolify's files written inline (`content:`), and values a store fills in from another app, such as 1Panel's `PANEL_DB_HOST`, which connectable apps (ADR 0084, #569) are to meet.

This record says what the converter now accepts and why each is safe. It grows a section for each later slice of #570.

## Decision

### What each store converts to is measured, in the repository

- **`dockwell-catalog rates` converts each store at a commit the repository pins, as a server does:** through `convert.Fetch`, the control plane's own two-round fetch, factored out of the catalog's API for both to use, and `convert.OnServer`. It prints, for each store, how many apps it holds, how many convert, the rate, and the rest counted by kind, the most first. `--list` names each app left out and why; `--show <format>/<id>` prints one app's converted entry, or its reason. All three stores convert in under ten seconds.
- **The record is `internal/catalog/convert/rates.json`:** each store's repository, its pinned commit, how many apps it holds, every app that converts by ID, and how many are left out of each kind.
- **CI fails when an app that converted no longer does,** naming each, and when the record doesn't say what converts now, so the record stays true. `make store-rates RECORD=1` writes it, and `PIN=1` moves each store to its default branch's commit first, a person's commit, so upstream changes never fail a pull request by themselves.
- **It runs in a CI job of its own, Other stores on Docker,** which reaches GitHub for the stores, as that job pulls images anyway. A fetch that fails is tried twice more, a few seconds apart. The job runs for any Go change, as Catalog on Docker does (ADR 0050).
- **What converts isn't quite what the Sources page shows.** The page leaves out an app whose ID Dockwell's own catalog has, as `taken`, and shows one that's plainly the same app as another choice on that app's card (ADR 0078). Those depend on Dockwell's own catalog, which moves on its own, so the record counts the converter alone. At the commit above, 1Panel's 99 apps that convert show, as its only source, as 90 cards of its own, 6 as another choice on a card of Dockwell's own, and 3 taken.

### A tag that moves is pinned when an install is planned

- **On the server, an image named by a tag that moves, such as `latest`, `stable` or `main`, converts,** as a version's tag does: the entry names it by its tag, and the plan pins the build it points at then, through the agent of the app's server, and shows the digest (ADR 0078). An image named with no tag is written out as `latest`, which is what Docker pulls for it, so the plan names the tag it pins. A 1Panel app whose only version folder is `latest` converts at version `latest`.
- **It's as safe as a version's tag, which can be pushed again too.** What runs is the digest a person saw in the plan and approved. No registry is asked at a refresh, and nothing about the image is trusted that a version's tag isn't.
- **The offline converter still leaves such an app out.** It pins each image as it converts, for a catalog that's published, where a moving tag would never move on.
- **The converter's version goes up with each slice that changes which apps convert** (`convert.Version`, 3 for moving tags and 4 for what follows, after the 2 that reads stores' logos and words), so a server converts a commit it kept again, and a store that's pinned shows the new apps once Dockwell is updated, not only once the store moves.

### A new build of a moving tag is an update, through the install's catalog

A store's file that names `latest` never changes when `latest` moves, so the catalog's releases can't say an app on it has moved on. Its registry can:

- **A catalog install that runs an image by a tag that moves is checked for a new build,** with the same HEAD of the tag ADR 0079 makes for any image, every 6 hours and by Check now. Its catalog's releases are read from its catalog, as before; nothing else about the image, such as a newer version tag, is an install's to take.
- **A new build is offered as an update of the install,** of the kind `digest`, at the release it runs, so the panel's Update, and automatic updates of that kind (ADR 0080), upgrade the install through its catalog and name no image.
- **Upgrading the install reads each moving tag again,** for the build it points at now: with no newer release, it's planned only when one of them has a new build, with the summary "Update moving to the newest build of nginx:latest"; with a newer release, each moving tag is pinned afresh too, as a person upgrading expects what's current. The plan shows each new digest, and backs up first, as any upgrade does. A companion added or taken away still keeps every digest.
- **Taking a new build takes that build alone,** never the store's newer release, which a person may have skipped: its update asks for a rebuild (`UpgradeServiceRequest.rebuild`, 90), and the panel's Update, Update several and automatic updates send it for an update of the kind `digest`. When the install's catalog has moved on past the release it runs, a rebuild alone is refused, and the release stays an update of its own, which brings the newest builds with it.
- **A release whose version stays, or names a tag that moves, is skipped by its number,** such as `release:7` (`AvailableUpdate.skip_as`, 90, which the panel sends as it is), so skipping one release of an app on `latest` never skips the next, nor a new build.
- **The agent keeps the digest its check read beside a plan's reads,** for the 10 minutes it keeps those, so an update planned right after a check pins the build the check saw.

### "Converts" means "works", for a sample

- **Other stores on Docker installs a sample of each store's apps on real Docker,** through the CLI, as a person would: `scripts/store-e2e.sh` adds each store at its pinned commit, and each app installs from its converted entry, its plan pins each image, every container runs, healthy where its entry has a check, on loopback alone, the app answers on the port a person opens, and it's removed through a plan. The sample is small apps, at least one from each store: one on a moving tag, one named with no tag, and one by a version.
- **Its first run found a store's own mistake:** Dokploy's CyberChef publishes port 80, where its image has listened on 8080 since it moved to an unprivileged web server, so it runs and answers nothing, in Dokploy as here. A plan could read the port an image declares, as a form already does (ADR 0062), and say when a store's differs; that's later work, and the sample takes another app.
- **The job's agent reads registries as a server does,** under `DOCKWELL_TEST_REGISTRY=public`, while `DOCKWELL_TEST` keeps everything else it would reach off (ADR 0034). Without it a test still reads no image from a registry.

### What a container asks of itself

*Added with the second slice of #570.*

After moving tags, the Compose keys Dockwell refused were the next gate: `hostname` in 23 apps, `ulimits` 17, `pull_policy` 13, `stdin_open` 9, `security_opt` 9 (mostly `no-new-privileges:true`), `read_only` 6, `init` 5, a health check's `start_interval` 5, `pids_limit` 4, `dns` 3, `tty` 3 and `stop_signal` 2. Each changes the container alone, and none reaches the server, so each is now a field of the container, as Compose names it:

- **A spec holds `hostname`, `init`, `tty`, `stdin_open`, `stop_signal`, `ulimits`, `read_only`, `pids_limit`, `dns` and `no_new_privileges`** (Container 90 to 99), checked as Docker would take them: a hostname as a DNS name, and none on the server's own network, which has the server's; a signal by its name or number, written as SIGQUIT; a limit Docker knows, its soft limit within its hard; a count of 0 or more; up to 8 DNS servers, by address. They read back from Docker as the spec has them, so none is drift: a hostname Docker gave for the ID, or a stop signal the image gives, isn't one the spec set. The Compose importer reads and exports each, and a container Dockwell takes on keeps them (ADR 0021).
- **A real-time priority is access a person approves.** A limit of `rtprio` above 0 lets the app's processes take the CPU ahead of everything on the server, its agent and control plane among them, whatever its cpus limit, on a kernel that doesn't group real-time tasks: it counts as `SYS_NICE`, as a plan shows it. A store's app that locks memory with no limit (`memlock: -1`) is left out, as locked memory can't be reclaimed from the server.
- **A DNS server is held to where a request a person names may go**, in netguard's words: loopback, link-local, unspecified, multicast and reserved addresses are refused.
- **Of `security_opt`, only `no-new-privileges` is taken.** It tightens what a container may do. `seccomp:unconfined`, `apparmor:unconfined` and `label:disable` loosen what confines it, and are refused by name, as before.
- **A pull policy and a health check's start interval are taken and dropped.** Dockwell pulls the digest a plan pins whenever a server hasn't it, so a policy says nothing it does; `build`, which builds, is refused as a build is. A start interval only says how often a check runs while the start period lasts, which Docker before 25 doesn't keep: Dockwell checks at the interval throughout, so a new container is seen healthy a little later at worst.
- **An agent says it sets them, in its `Hello` (`own_settings`, 300).** The control plane plans none of them on an agent that doesn't, which would leave them out: "the agent on hz-2 is from before Dockwell set a container's hostname, limits and the like, which web asks for, so it would leave them out. Update the agent on hz-2."
- **compose-go asserts what a value is in places,** and a limit written as text, as a variable gives one, made it panic. A file is a stranger's when a store refreshes with nobody watching, so the importer writes each limit as the number it is, refusing text that's none, and turns any panic in compose-go's decoding into a file it can't read.

### A secret inside a word of a command

*Added with the second slice of #570.*

The catalog refused a secret anywhere but a value of its own, as an upgrade stands something in for a secret it never opens. That holds for an environment's value, where `postgres://u:${PASSWORD}@db/x` would carry the stand-in. It doesn't for a command, an entrypoint or a health check's test, which keep a value by name, with the value written into each word as the container is made, and a rotation reaching it ([ADR 0022](0022-secrets-encrypted-at-rest.md)). So a secret may now stand anywhere in one of their words, as in `redis-server --requirepass ${PASSWORD}` or `--api-key=${KEY}`, which 19 of the stores' apps write. A test installs such an app from a converted store and upgrades it: the spec keeps the names, the secret comes by reference, the container gets the secret itself, and the upgrade keeps it.

In a shell's script, the word `sh -c` or a `CMD-SHELL` test reads, a name isn't written in: it stays for the shell to read from the container's environment, where the value is, quoted for where it stands (`"${PASSWORD}"` outside quotes, `'"${PASSWORD}"'` inside single ones). Written in, a value would show in the process's command line to every user of the server, and a typed one with a quote, a `$(` or a `;` would change what the script does. The references label says which word is a script, so the container reads back as its spec, and a container made before keeps the form it has. One docker compose makes from an export has the value written in, as Compose writes any value it's given, and reads back as the spec too. In any other word the value is the program's own argument, as it is for any program given a password on its command line.

A secret inside an environment's value, over 100 of the stores' apps and most of them a companion database's URL, is left for the need that writes it (ADR 0084), or a mechanism that writes an environment's value as a command's words are.

### Files beside the Compose file, as data

*Added with the third slice of #570.*

Many stores' apps mount a config file their store ships: 1Panel's beside the Compose file in the version's folder (`./conf/my.cnf:/etc/mysql/my.cnf`), Dokploy's as a template's `[[config.mounts]]`, which its Compose file mounts from `../files`, and Coolify's written into the Compose file, as a volume's `content:`. A file is text a container reads, which Dockwell can carry as data, never running it:

- **A container mounts a file Dockwell writes, as a `Mount` of the kind `file`** (`MountFile`, 90 in `Mount`'s source): its content and its mode. It's 256 KiB at most, a container mounts 64 at most and 1 MiB of them together, its mode has read and execute bits alone, and it's always read-only, with no `external`, `make_owner` or `relabel`. It isn't a secret, and a plan shows what it holds.
- **The agent keeps each file in a folder of its own,** `/var/lib/dockwell/agent/files`, which only root writes, named by its mode and the SHA-256 of its mode and content, and binds it into the container read-only, as a mount of type bind, which fails where the file is missing rather than have Docker make an empty folder in its place. Two containers with one file share it, a new version is a new file that no running container sees change under it, and nothing the file says chooses where it's written, nor in `/proc`, `/sys` or `/dev`. A file is read back for a container only from that folder, by a name its bytes still match, so drift is a container that mounts something else. A file nothing has mounted for an hour is removed: writing it again for a container counts the hour from then, and a prune that can't read what every container mounts removes nothing. An empty folder on a file's name gives way to the file.
- **An agent says it writes them, in its `Hello` (`writes_files`, 301).** The control plane plans no file mount on an agent that doesn't, which would mount nothing: "the agent on hz-2 is from before Dockwell wrote the files a container mounts, which web does, so it would run without them. Update the agent on hz-2."
- **The Compose importer reads Compose's own `configs:` with `content:`,** each as a file of every service that names it, at its `target` (or `/<name>`), with its `mode`. A config from a server's `file:`, the importer's `environment:`, or Docker's `external:` reaches outside the file, and is refused, as is a `uid` or `gid` other than root's. A variable inside a config's content lands as text in a file a plan shows, so the exposure warnings name each service that mounts it. An export writes each file back as a config.
- **The converter writes each store's files as configs.** It reads the files an app's Compose file binds, in a third round of the fetch, 500 at most a commit, each within ADR 0078's bounds; each must be text (UTF-8, with no NUL), and an app's come to 128 KiB at most. A file whose first line is `#!` gets mode 0555, so a container can run it, as 1Panel's `run.sh` for its language runtimes is: it runs inside that container, as its image does, never on the server. A shipped folder bound read-only becomes a file for each file in it. A shipped folder the app writes to is left out, saying Dockwell would copy it into the app's volume as it installs, and doesn't yet. A Dokploy file is worked out as its template's values are, each variable within 4 KiB and the file within 128 KiB, and a file that holds a value the template makes up is left out, as an upgrade couldn't reach it.
- **The catalog refuses a secret anywhere in a config,** even as all it holds: a file is what a plan shows.
- **The panel shows a file in Volumes and mounts** as "A file Dockwell writes, 12 lines", opening to what it holds, and editing the volumes and mounts keeps each file as it is.
- **The sample on real Docker takes 1Panel's Glance,** which won't start without the config file its store ships.

### Numbers

ADR 0085. `UpgradeServiceRequest.rebuild` (90) and `AvailableUpdate.skip_as` (90), `Container` 90 to 99, `Mount.file` (90) and its `MountFile`, and the agent's `Hello.own_settings` (300) and `Hello.writes_files` (301); `CatalogInstall.release` in Go alone, and `ImageUpdatesCall` as it is.

### As built, 13 Nov 2026

| Store | Apps | Moving tags | With a container's own settings and secrets in commands | With files | Rate |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1Panel's app store | 269 | 99 | 112 | 123 | 45% |
| Dokploy's templates | 612 | 299 | 331 | 374 | 61% |
| Coolify's templates | 384 | 199 | 224 | 240 | 62% |

## What we turned down

- **A record of rates alone, with a floor.** A count can stay the same while one app stops converting and another starts; the record lists each app that converts, so CI names the one that fell.
- **Measuring without the network,** from copies of the stores in the repository. The stores' files are their authors', under their licences, which waits for counsel (ADR 0029), and a copy would measure a converter that reads files, not the fetch a server makes.
- **Pinning a moving tag when a store is converted,** with a cache. It's ADR 0078's reason: a refresh would ask registries hundreds of times.
- **Taking a moving tag's new build at each refresh.** What runs never moves without a person's plan, or their automatic updates.
- **A moving tag's version read from the image,** such as a label. Few images say one, and the digest says exactly what runs.

## Consequences

- A change to the converter that loses an app fails its pull request, by name, and one that gains apps records them.
- Over 250 more apps from Dokploy and Coolify install, with each new build offered as an update.
- Each store's pinned commit is a person's to move. Until it moves, what upstream adds is measured only by a server reading the store.
- CI reaches GitHub, and the registries of the sample's images.
- Still to come in #570: a shipped folder the app writes to, copied into its volume; 1Panel's scripts as steps Dockwell runs itself; a secret inside an environment's value; an app that runs on amd64 alone offered where such a server is; and another app's database, as a need (ADR 0084).
