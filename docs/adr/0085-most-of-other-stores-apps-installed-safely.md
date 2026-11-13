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

### A new build of a moving tag is an update, through the install's catalog

A store's file that names `latest` never changes when `latest` moves, so the catalog's releases can't say an app on it has moved on. Its registry can:

- **A catalog install that runs an image by a tag that moves is checked for a new build,** with the same HEAD of the tag ADR 0079 makes for any image, every 6 hours and by Check now. Its catalog's releases are read from its catalog, as before; nothing else about the image, such as a newer version tag, is an install's to take.
- **A new build is offered as an update of the install,** of the kind `digest`, at the release it runs, so the panel's Update, and automatic updates of that kind (ADR 0080), upgrade the install through its catalog and name no image.
- **Upgrading the install reads each moving tag again,** for the build it points at now: with no newer release, it's planned only when one of them has a new build, with the summary "Update moving to the newest build of nginx:latest"; with a newer release, each moving tag is pinned afresh too, as a person upgrading expects what's current. The plan shows each new digest, and backs up first, as any upgrade does. A companion added or taken away still keeps every digest.
- **The agent keeps the digest its check read beside a plan's reads,** for the 10 minutes it keeps those, so an update planned right after a check pins the build the check saw.

### "Converts" means "works", for a sample

- **Other stores on Docker installs a sample of each store's apps on real Docker,** through the CLI, as a person would: `scripts/store-e2e.sh` adds each store at its pinned commit, and each app installs from its converted entry, its plan pins each image, every container runs, healthy where its entry has a check, on loopback alone, the app answers on the port a person opens, and it's removed through a plan. The sample is small apps, at least one from each store: one on a moving tag, one named with no tag, and one by a version.
- **The job's agent reads registries as a server does,** under `DOCKWELL_TEST_REGISTRY=public`, while `DOCKWELL_TEST` keeps everything else it would reach off (ADR 0034). Without it a test still reads no image from a registry.

### Numbers

ADR 0085. No proto field yet: this slice adds `CatalogInstall.release` in Go alone, and `ImageUpdatesCall` as it is.

### As built, 12 Nov 2026

| Store | Apps | Converted | Rate |
| --- | ---: | ---: | ---: |
| 1Panel's app store | 269 | 99 | 36% |
| Dokploy's templates | 612 | 299 | 48% |
| Coolify's templates | 384 | 199 | 51% |

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
- Still to come in #570: files beside the Compose file, carried as data within bounds; 1Panel's scripts as steps Dockwell runs itself; a secret inside a longer value; the Compose keys Dockwell can't run yet; an app that runs on amd64 alone offered where such a server is; and another app's database, as a need (ADR 0084).
