# 0029. A catalog format of our own, fed from other catalogs

- Status: Accepted
- Date: 2026-11-05

## Context

The PRD asks for a curated catalog of 200+ apps at launch, with versioned templates and a backup before each upgrade. It also asks whether the catalog can start from 1Panel's app store, and whether its licence allows that (#38). Writing every entry from nothing would be a slow cold start, and several catalogs already exist.

We measured six of them on 4 Nov 2026, with a scan of each entry's Compose file. The scan matches patterns and runs nothing, so these figures are estimates.

| Catalog | Licence of the repository | Entries | What its entries depend on |
| --- | --- | ---: | --- |
| [Dokploy](https://github.com/Dokploy/templates) | MIT | 600 | `template.toml`, and `../files` in 93 |
| [Umbrel](https://github.com/getumbrel/umbrel-apps) | None declared | 396 | Umbrel's app proxy in 381 |
| [Coolify](https://github.com/coollabsio/coolify/tree/main/templates/compose) | Apache-2.0 | 387 | Coolify's `SERVICE_` variables in 377 |
| [1Panel](https://github.com/1Panel-dev/appstore) | GPL-3.0 | 268 | `PANEL_` variables in 256, and an external `1panel-network` in 260 |
| [Runtipi](https://github.com/runtipi/runtipi-appstore) | GPL-3.0 | 266 | Runtipi's variables in 259, and proxy labels in 236 |
| [CasaOS](https://github.com/IceWhaleTech/CasaOS-AppStore) | Apache-2.0 | 182 | `x-casaos` in all, and `/DATA/AppData` in 164 |

**No entry runs as it is.** Each depends on its own platform, as the table shows. The Docker driver (#71) also refuses `container_name`, external networks and paths relative to the file by name, and 219 of 1Panel's 268 apps use all three.

**1Panel's apps need different things.** Taking each app's newest version:

| What the app needs | Apps |
| --- | ---: |
| A Compose file and a form | 145 |
| Also ships config files | 28 |
| Runs a script on the host | 58 |
| Host access, or features the driver refuses, such as the host network or Docker's socket | 37 |

Some apps in the last row have scripts too, which makes 69 with any script. Of those, 49 only do simple file steps, such as `chown`, `mkdir` and `sed`. Even so, a plan can't show what a script will do, so running them would break PRD principle 2.

**Many entries are the same app.** The 2,099 entries come to about 1,200 apps: 1,181 to 1,300, depending on how they are matched. About 400 appear in two or more catalogs, and about 50 in five or six. No simple rule matches them correctly:

- **Names mislead in both directions.** Of the 408 groups a name match finds across catalogs, 85 use different images. Another 74 apps share an image or source repository under different names. 1Panel's `bitwarden` runs `vaultwarden/server`.
- **Images and repositories merge too much.** They join what isn't one app: `frpc` and `frps`, or Gitea and its runner.

**Images aren't pinned.** The PRD requires catalog images pinned by digest. Umbrel pins every image in all 396 of its entries. Across the other five catalogs, only 5 entries do.

## Decision

### Dockwell reads one format, its own

- A catalog entry's format is part of the schema, so it is Apache-2.0 ([ADR 0012](0012-apache-for-api-schema-and-clients.md)), and anyone can publish a catalog.
- It says what the other formats can't: typed inputs, secrets as references, domains, health checks, what to back up before an upgrade, the host access an app needs, and the names it expects from a connection, as the PRD asks.
- **An entry runs no script on the host.** It declares the files it needs and who owns them. An entry that needs more can't run until it is rewritten.
- **Images are pinned by digest,** as the PRD requires.
- An app installs through the same plan as any other change, into a project of its own, as a Compose file does (#77). The plan shows everything, and host access needs a person.

### Other catalogs are converted once, when the catalog is built

- A separate repository, `Dockwell/catalog`, runs a job that takes each source at a named commit, converts its entries, checks them, proposes matches and publishes a signed index of static files.
- The control plane reads only that index, so it can be mirrored and verified as releases are ([ADR 0013](0013-mirrors-for-releases-and-images.md)).
- A source that changes its format breaks the job, not anyone's server. An entry that can't be converted is left out, with the reason recorded.

### What the licences allow

This is our reading of the licences, not legal advice. Counsel confirms it before the catalog ships, alongside the review of the CLA ([ADR 0001](0001-license-agpl-and-cla.md)).

- **Reading a format copies nothing.** The importer for someone's existing 1Panel install, which the PRD requires at launch, reads their own files, so these licences don't limit it.
- **GPL-3.0 entries can be copied, adapted and distributed, including commercially.** What we convert from 1Panel and Runtipi stays GPL-3.0, keeps its notices and says it was modified.
- **They stay out of the binary.** They live in the catalog repository, and Dockwell downloads them as data, so they aren't part of it. Even shipped together, GPL-3.0 section 5 treats separate works like these as an aggregate. The paid editions can then combine the core with their own modules. The CLA can't relicense these entries.
- **Every entry carries its `license` and its `source`,** the repository and commit it came from. The catalog's CI refuses an entry without them.
- **Permissive sources come first:** Dokploy, Coolify and CasaOS. GPL-3.0 sources fill the gaps, such as 1Panel's coverage of apps popular in China.
- **Umbrel isn't redistributed,** as its repository declares no licence, which leaves no clear right to copy it. At most, a person adds it as their own source, read on their own server, once custom catalogs arrive and counsel has looked at it.
- **Some things no catalog's licence gives us:**
  - Logos belong to each project, and the GPL grants no trademark rights, so we use them only to name the app, and take them from the project.
  - Descriptions and READMEs may be copied from upstream under other terms, so we write our own.
  - "Imports from 1Panel" says what a feature does. Nothing implies a catalog's owner endorses Dockwell.
- **Entries point at images in their publishers' registries.** Pointing at an image doesn't redistribute it. Copying one to a mirror does, so check its licence first.

### One app, however many catalogs list it

- **A reviewed list says what is the same app.** It lives in the catalog repository, and holds each app's ID, source repository and image names, and what it isn't. A job proposes matches from images and repositories, and a person confirms them. Anything uncertain stays separate: a duplicate costs a card, and a wrong merge installs the wrong software.
- **Only the list joins an entry to an app,** so a template that calls itself `vaultwarden` and runs another image stays apart.
- **People choose between real differences,** not between catalogs:

| Difference | Example | Shown as |
| --- | --- | --- |
| Source only | One image and setup in three catalogs | One option, with its sources listed |
| Packaging | `jellyfin/jellyfin` or `linuxserver/jellyfin` | A choice |
| Setup | FreshRSS with MariaDB, MySQL or PostgreSQL | A choice |
| Edition | `gitea/gitea` or `commitgo/gitea-ee` | A choice, flagged as its licence may differ |
| Role | Gitea and its runner, or `frpc` and `frps` | Separate apps, linked |

- **No choice appears when there's one option** (PRD principle 2 for scaling from one server to a fleet). The default is the entry Dockwell has tested, then the official image, no host access and the latest update. People can set an order of sources, or turn one off.

### What an install keeps, and what we vouch for

- **An install is pinned** to its entry, source and commit. Updates come from there, and each is a plan.
- **Switching is a plan too,** and never reads as a simple update. Packagings can keep their data in different paths, so the plan says whether the data carries over, and says so plainly when it can't tell.
- **Entries have two levels.** *Tested* entries install and pass a health check on real Docker in the catalog's CI. These are the PRD's curated 200+. *Community* entries are converted and checked but not run, and stay behind a setting that's off by default. The plan protects both the same way.

### The first entries, before the index exists

*Added 6 Nov 2026, with #89.*

`Dockwell/catalog` and its signed index don't exist yet, and people need to install apps now. So the first entries ship in the binary, in `internal/catalog/entries`, in the same format the index will carry. When the index arrives it takes over from `catalog.Load`, and nothing else changes.

- **They're ours.** We wrote these entries, so no other catalog's terms reach the binary, which keeps to what this record says. Each file's licence is the repository's, and its `license` says so.
- **The format is `dockwell.catalog.v1alpha1.Entry`.** It's alpha until backups exist, as above. For now it has typed inputs, generated secrets, what a person opens, what to do first and the host access an app needs. Declared files, what to back up and connection names wait for an app that needs them.
- **A secret stays a reference.** An entry's Compose file writes `${POSTGRES_PASSWORD}`, as any file would. The control plane generates the value, seals it as the environment's secret, and has the service use it by name, so no response carries it and a rotation reaches the service. An entry can't put a secret inside a longer value, which a rotation couldn't reach.
- **A rotation goes no further than the service.** PostgreSQL, MySQL and Nginx Proxy Manager read their password from the environment on first start alone, so rotating one leaves the old password signing in, and the panel revealing a new one that doesn't. Dockwell doesn't rotate these yet. Each entry's first steps say so, and how to change the password in the app instead.
- **An install is pinned in its project's spec,** as `Project.catalog`: the entry, its release, its source and commit, and every input that isn't a secret. So it's a revision like anything else, and an undo takes it away. `release` is a number that goes up, as a version like `2026.10.3` doesn't order reliably across apps.
- **An install never changes what's there.** A project of that name is refused, where importing a Compose file would update it, and so is a name used before: a deleted project keeps its IDs, so its volumes would come back, with data a new password can't open. The name and the ports start from what's free among what Dockwell runs. Anything else on a server only its agent can see, so the plan refuses that by name.
- **A database publishes no port.** Its admin tool runs in the same project, and a person adds a port on the app's page if something else needs one.
- **A form asks only what it must.** Every app gets a name and its ports. Domains, memory limits and backups in the `Catalog-Install` mock wait: a domain has its own form on the app's page, with what a tunnel needs, and the other two have nothing to set yet.
- **An entry's text is English for now.** The panel's own words are translated, and an entry's are data, so translations belong in the format once the catalog is big enough to need them.

The first set is what the two real servers ran but for their AI agent runner, which comes with an entry of its own: an app that publishes with no account of its own belongs to whoever reaches it first, so an entry doesn't do that. Every app in the set is open source, which the catalog's page says and a test holds it to.

### Upgrades, before backups exist

*Added 6 Nov 2026, with #89.*

An upgrade is a plan: `CatalogService.UpgradeCatalogInstall` reads the install's pin and proposes the entry's newest release, with the pin in the same change, so an undo puts back both.

- **A newer release is a higher `release`.** The panel says so on the catalog's card, on the rail and on each of the app's pages, which holds the button.
- **The entry owns what's in its file.** What it sets changes, and what the file no longer has goes, but for volumes, as `remove_orphans` leaves them.
- **What a person added stays:** a port, a mount, what's in a service's environment and what it uses. Without this, the port a person added on an app's page would go at the next upgrade. A name the file sets stays the file's.
- **Its secrets stay as they are, and nothing reads them.** A secret the install has is filled in with a value that means nothing, which the service then uses by name, so the value is never opened. A secret a release adds is generated, or asked for.
- **There's no backup first, and the plan says so.** The issue asks for one, but backups don't exist yet (#88), so that box stays open. A note, `no_backup`, says none is taken, and that an undo puts the old version back but not what the new one changed in its data.
- **`DOCKWELL_CATALOG_DIR` names a directory of entries,** read each time, that take over from built-in ones of the same ID. Before a feed exists it's how a newer release reaches a server for a test, and how to try an entry. The panel's flow and the Docker end to end use it. A feed takes over from it, as from what's built in.

Choosing to stay on a version, skipping one, and upgrading a whole fleet in one go are later work. A release that adds an input needs its value from `dockwell catalog upgrade --values`, as the panel has no form for it yet.

### What we turned down

- **Running 1Panel's format directly.** It would tie the product to a format that can't express plans, secrets or backups, and it would run scripts on the host.
- **Reading each format in the control plane.** That means six parsers of files from strangers in a service that runs as root, and six ways for an upstream change to break a server.
- **Merging automatically, by name or image.** The figures above show both are wrong often enough to install the wrong software.

## Consequences

- The catalog can reach 200+ apps without a cold start, and most of it can stay free of GPL terms.
- Licensing stays simple for the core: no catalog's terms reach the binary or the paid editions.
- Adapters for five sources, a publishing job and the reviewed list are lasting work. The list needs a person every time a source adds apps.
- The format can't be final until secrets (#29), domains (#27), health checks (#28) and backups exist, as it has to express each of them.
- The `Catalog` and `Catalog-Install` mocks show no choice of packaging or source, so phase 2 designs one before it builds them.
- The PRD's P1 item for custom catalogs from a Git URL stays as it is. It reuses these adapters, and is how someone adds a source of their own.
- Still to decide: the entry's exact schema, the licence of the catalog repository's own tooling, how the index is signed, and which 200 apps come first. The figures here will age, so measure again when the job is built.
