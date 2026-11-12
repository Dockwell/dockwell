# 0078. Other stores' catalogs, read on the server

- Status: Accepted
- Date: 2026-11-12

## Context

On 2026-11-12 the owner asked whether Dockwell could take 1Panel's app store, Dokploy's templates and Coolify's templates as catalog sources directly, from a repository's link, with the conversion hidden, and keep up with each as it changes. They decided three things:

- **Other stores convert on the server.** A person pastes a store's link, or picks a preset, and the control plane converts it. They never see a step of it.
- **Dockwell publishes no converted copy** of another store, which waits for counsel on redistribution ([ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md)). Converting on each install, from the store's own repository, redistributes nothing.
- **Catalogs follow their source** (ADR 0077, in review with #543): what the catalog offers moves with the store's branch, and what runs never moves without a person's plan.

Two earlier records said otherwise. ADR 0029 turned down "reading each format in the control plane", as six parsers of strangers' files in a service that runs as root. [ADR 0055](0055-catalogs-from-a-git-url.md) has a source hold entries in Dockwell's format alone, converted first where the catalog is built. This record amends both, for the three stores the converter already reads, and says what makes it safe.

We measured the three stores on 11 and 12 Nov 2026, at 1Panel's `33c0c8ed`, Dokploy's `4f0ddfb5` and Coolify's `93c7e53e`:

| Store | One commit, fetched whole | What its adapter reads | Apps | Convert on the server |
| --- | ---: | ---: | ---: | ---: |
| 1Panel's app store | 152 MiB | 1,374 files, 3.5 MiB | 269 | 94 |
| Dokploy's templates | 17 MiB | 1,897 files, 4.1 MiB | 612 | 169 |
| Coolify's templates, in Coolify's own repository | 22 MiB | 384 files, 0.9 MiB | 384 | 78 |

1Panel's commit is over the 64 MiB a source fetches, mostly two versions of OpenResty's build archives and GeoIP databases. Every app left out is counted, with its reason, as the converter's report does.

## Decision

### A source has a format, told from its folders

- **A `catalog.source` names its format:** Dockwell's own entries, 1Panel's app store, Dokploy's templates or Coolify's templates. A source made before formats reads as Dockwell's own, as it did.
- **Dockwell tells the format from the commit's folders,** so a person never types it: 1Panel's `apps/<key>/data.yml`, Dokploy's `blueprints/<id>/template.toml`, Coolify's `templates/compose/<id>.yaml`, and Dockwell's `<id>.yaml`, at the folder's top or in `entries/`, whose folder the source then names. A store's layout outweighs a few YAML files beside it, as 1Panel's and Coolify's repositories keep such files at their top. Where the folders could be more than one, `SetCatalogSource` refuses with a choice naming each, which the panel shows as a picker and the CLI as a list for `--format`. A folder that holds none reads as Dockwell's own, whose plan says it reads no entries, as before.
- **`ListCatalogSourceRefs` says the layout of the default branch's commit,** with how many apps it looks to hold, so a form that was pasted a link shows what it is before any plan.

### The control plane converts, in the full build, and runs nothing

- **The control plane runs `internal/catalog/convert`'s adapters on the commit it fetched,** in memory. They're the offline converter's, read through a listing of the commit in place of a checkout. Only the full build links them: the agent build doesn't, and a test holds it to that ([ADR 0023](0023-a-full-build-and-an-agent-build.md)).
- **Nothing a store holds is ever run.** An app that needs a script on the server, a build, or files shipped beside its Compose file is left out, with the reason, as ever.
- **A converted entry passes the checks Dockwell's own do,** and reads as an install would, as ADR 0055 has every source's entries do. The plan's first steps, sign-in and the rest are the app page's to show, as ADR 0029 has it.

### A store is fetched in two rounds

- **The first round asks for the commit with `filter blob:limit=1`:** its folders, and no file of a byte or more, as Git's partial clone does. It's 16 MiB at most. The control plane walks it to the store's folders alone, 20,000 files and folders at most, 8 deep.
- **The second asks for exactly the files the adapter reads,** by their objects' names: 5,000 at most, 1 MiB each, 32 MiB together, in a pack of 64 MiB at most. A file over 1 MiB is left out of what's read, and the app that needs it is left out, saying so. The rest of the commit, archives included, is never sent.
- **Each object is checked against its name, from the commit down,** as for a source of Dockwell's own, and the pack is read within the same bounds ([the threat model](../security/catalog-sources.md)). A host that can't filter is fetched whole, within the 64 MiB a source always had.
- **Both rounds go through netguard,** with no proxy and no redirect, as ADR 0055 has it. All three stores read in under two seconds this way.

### Bounds for a stranger's store

A store now converts on the server, and refreshes with nobody watching once it follows a branch, so each part of it has a bound of its own, on top of ADR 0055's:

- **Files:** 256 KiB for a Compose file, a form or a template, where the largest of the three stores' was 38 KiB. Each nests 64 levels at most by its brackets and its indentation, counted before any parser reads it. That count is a rough one: a compact block sequence (`- - -`) nests with neither, and YAML's parser stops those at its own 10,000 levels.
- **Aliases:** expanding YAML's aliases writes out 256 KiB at most (what a Compose file may be once it's read, `compose.MaxFile`), counted as each value is copied, beside 100,000 nodes. An anchor of 100 KiB named through two levels of ten aliases is about 1,100 nodes and 10 MB, so a count of nodes alone let it by.
- **Dokploy's variables:** a variable's value comes to 4 KiB at most, a template's values to 256 KiB together, in 10,000 steps at most, as variables that each name the next ten times stand for gigabytes in a template of a few hundred bytes. Depth alone was bounded before.
- **Apps:** 2,000 a store at most, as a source holds 2,000 entries. Two apps that come to one ID, such as `my_app` and `my-app`, keep the first by its folder, and the other is left out, saying whose ID it is.
- **What a commit converts to:** 16 MiB at most, as a source of Dockwell's own entries reads 16 MiB of a commit; past it, the commit is refused. A licence's copyright line, which each entry repeats, is one line of 200 characters at most. What's kept of each left-out app is its ID and folder, 200 characters each, and a reason of 500.
- **What it asks Dockwell to make:** a secret any entry generates is 256 characters at most, checked for every source's entries, as one of a billion would cost the control plane a gigabyte at install. A template's helper that asks for more is left out.
- **Where its images are:** an image on a registry that's this server, a private network or a metadata address, by its name, is left out, as pinning it would have the server ask there before anyone has looked. Where a name leads is checked as the agent connects (below).
- **What it writes into what's kept:** a store's text reaches a kept entry only through the YAML encoder, and through the header's comments as one line, every control character and line or paragraph separator made a space, as YAML reads a carriage return, NEL, U+2028 and U+2029 as line breaks. A tree entry whose name holds a control character or a separator refuses the commit, in gitfetch.
- **Time:** fetching a store's commit takes a minute at most, and converting it another, each with a budget of its own. Each adapter checks the time once an app, and the converter between apps. One commit is fetched and converted at a time, and once per commit and converter.

### What only the offline converter has stays there

The offline converter has a person's overlays: our own summary and category for each app, and what they looked at of what it reaches. They don't come to the server.

- **An entry's summary is the store's own line about the app,** cut to 160 characters, shown as the store wrote it, as what a source says is shown as text. Its category is the converter's guess from the store's tags, or `other`. Overlays carry our own words for a catalog that's published, as ADR 0029 asks; a converted copy on one install isn't published.
- **What an app reaches on its server is written from what the converter finds,** in its `host_access`, rather than leaving it out until a person looks. The plan is where a person looks: host access needs their approval, and a passkey where it's as good as root, as for any install.
- **An app that runs on amd64 alone is still left out,** as the catalog is one for the whole fleet. Offering it per server is later work.

### Images are pinned when an install is planned

- **A converted entry names its images by tag,** written plainly. Refreshing a store asks no registry: pinning 400 images at each refresh would take minutes and hundreds of calls.
- **When an install or upgrade is planned, the control plane asks the agent of the server it goes on for each tag's digest** ([ADR 0062](0062-reading-an-image-from-its-registry.md)), and the plan installs `image@digest`. The plan shows each digest, and says it's the build the tag points at now.
- **It's one call, `ImageCall.digests`, a HEAD each,** which no registry counts as a pull, 20 images a call, an app of more asking in calls of 20, through the reader and the short-lived digests a form's reads keep. Detecting updates (ADR 0079, in review with #545) is to use the same call and the same digests, so there's one cache of them.
- **Only what the app's services run is pinned:** each service's own `image`, as the file is parsed and as an install reads it. An `image:` line elsewhere, such as a variable in a service's environment, was never checked as an image, so the agent is never asked about it.
- **Where an image's registry is, is checked again when the plan is made,** by its name, as at conversion. And the agent reads a registry the server's Docker doesn't name in its configuration at public addresses alone, through netguard, over HTTPS, wherever its name resolves: a store's file can't have it reach this server or its networks. A registry Docker names, such as an insecure one in daemon.json, is read as a pull would reach it.
- **A digest is checked for its shape,** `sha256:` and 64 hex digits, in the registry client and again before it's pinned.
- **What runs never moves without a plan.** An upgrade keeps the digest an install runs for an image whose tag the release didn't change, and pins only a tag that moved. A companion added or taken away keeps every digest. A tag pushed again upstream changes nothing until a person plans an upgrade.
- **An upgrade is offered only where the entry's file changes,** read without the digests the install chose, so a refresh offers nobody an upgrade of an app it didn't touch.

### No hold-back on a store's images

The offline converter holds a build back until it's 14 days old, as the repository holds back every dependency. The server doesn't, for a store's entries:

- The store chose that version, and its own users run it from the day it's published.
- An image's age is when its tag was last pushed, and official images are pushed again for each base image's update, so a tag that's rebuilt every few weeks would almost never be installable, with nothing a person could do about it. A registry keeps no tag's earlier builds to fall back to.
- A source's entries are its adder's to trust (ADR 0055), and the plan shows the digest it installs. Its age can show there once update detection reads release dates.

Dockwell's own catalog keeps its 14 days, where it's built.

### Licence and source

- **A converted entry keeps its store's licence and source.** The licence is read from the repository's own `LICENSE`, `LICENSE.md`, `LICENSE.txt` or `COPYING`, by its title, and matched to the GPL-3.0, AGPL-3.0, Apache-2.0 or MIT; one that's none of them plainly is `NOASSERTION`, and the plan says Dockwell can't tell it. Its `source` is the repository, the app's folder and the commit.
- **Its notes say where it's from as it is.** From the store's own repository, "From 1Panel's app store, under GPL-3.0-only". From any other repository laid out the same way, the repository and the layout, "From github.com/acme/appstore, in 1Panel's format, under GPL-3.0-only", as anyone can copy a store's layout.
- **What's kept on disk says it was converted:** each entry is kept as the offline converter writes one, with a header naming its source and licence and that it was modified, and when.

### What's kept, and what a plan says

- **A converted commit is kept as its entries,** each `<id>.yaml`, beside a record of the apps it left out and why, by the converter's version. A newer converter converts a kept commit again, and what the older one kept stays, and is read, until that succeeds, so a repository that's gone leaves what's installed from it its upgrades.
- **What's read of sources and layouts is kept in memory within bounds:** 64 reads of sources, let go of whole past that, and 64 layouts of commits, which a form asks for as a link is typed, read one at a time.
- **A store's plan says how many apps it brings, at which commit, as which store, under which licence,** and in one note how many it leaves out and why, counted by kind, as "It leaves out 175 of 269 apps: 60 run scripts on the server, 25 ship files beside their Compose file, …". Each app and its reason is a step away: `ListCatalogSources` lists them, each with its kind, such as `scripts`, for the panel to count and say in its reader's language.
- **An entry whose ID a catalog before it has is left out,** as ADR 0055 says, counted as `taken`, until the same app in several catalogs shows as one card.

### Releases

- **A converted entry's release is when its commit was made,** in seconds, so a later commit's entry is a higher release, whichever commit Dockwell read first. Pinning an older commit gives a lower one, which no upgrade goes to. A commit dated after now, as a fork can date one, counts from when it's converted, so it can't outrank every later commit.

### The same app in several catalogs

*Added with the second slice of #544.*

WordPress from Dockwell's own, from 1Panel's app store and from Coolify's templates is one app, and the catalog shouldn't show it three times.

- **An entry of an ID a catalog before it has, with the same name, in any case, and the same image repository for the service a person opens, is the same app.** It's kept as another choice of that app, not left out. Anything less certain stays out as `taken`, as ADR 0055 has it: a missed match costs a card, a wrong one installs the wrong software (ADR 0029). Entries of different IDs are never joined, so 1Panel's `bitwarden`, which runs `vaultwarden/server`, stays apart from a `vaultwarden`.
- **The card is listed from the first catalog that has the app,** Dockwell's own first, then each source by name, and names the others: "Dockwell's own · also in Coolify and 1Panel". `ListCatalogEntries` returns them as `also`, and `ListCatalogSources` says which apps each source offers that way.
- **A catalog is said as a person says it, never by its source's ID:** a store's own name for a store read at its own repository ("1Panel", "Dokploy", "Coolify"), and for any other source its repository's owner and name ("acme/appstore"), with its folder where two would read alike. The card, the app's page, the install sheet and the Sources page say the same; the ID is in a tooltip, and in the CLI.
- **The app's page picks the catalog,** where there's more than one, as a choice of the catalogs `GetCatalogEntry` names, and shows that catalog's entry: its version, licence and what it installs. The install sheet installs from the one picked, and the install upgrades from that catalog alone, as ADR 0055 has it. A source's plan says which apps it offers as another choice.

### One click for each store

- **The Sources page offers 1Panel's app store, Dokploy's templates and Coolify's templates** as buttons, in its empty state and above the list, each one no source reads yet. One opens the source form with the store's link, format and a name filled in, so the next thing a person sees is the plan.
- **A pasted link says how it reads** as a line under it, such as "1Panel's app store · 269 apps", from `ListCatalogSourceRefs`. Only where the folders could be more than one does the form ask, as a choice of those, never as a field.
- **What a store leaves out is one step away,** counted by why, the most first, each kind opening to its apps and their reasons.

## What we turned down

- **Publishing converted copies,** in Dockwell/catalog or anywhere, which waits for counsel, as the owner decided.
- **Pinning images at conversion, with a cache.** A refresh of 1Panel's store would ask registries about 400 times, and a plan would wait minutes for it. A plan pins the few images it installs, in seconds.
- **Resolving digests in the control plane.** It would read from another network than the server that pulls, and a second cache beside the agent's. ADR 0062 chose the agent for the same reasons.
- **Fetching a store's commit whole.** 1Panel's is 152 MiB, over what a source reads, almost all of it files no adapter reads.
- **Bringing overlays to the server as data.** They carry our own words for a published catalog and a record of what a person looked at; on one install, the plan is where a person looks.
- **Running 1Panel's scripts,** for the reason ADR 0029 gives: a plan can't show what a script will do.

## Consequences

- ADR 0029's "the control plane reads no stranger's format" and ADR 0055's "a source holds entries in Dockwell's format alone" no longer hold, for these three stores. A fourth format is a new adapter, its bounds, and a row in this record.
- The control plane carries three adapters and their parsers (YAML, TOML, JSON), in the full build alone, and a refresh runs them on whatever a store's branch says next, within the bounds above.
- A store's change that breaks its adapter leaves the source where it was, as a commit that can't be read does, rather than breaking anyone's server.
- A plan for a converted entry needs its server's agent connected, to pin its images.
- Still to build: a store's own icons once a source reads media (ADR 0077), following each preset's branch once sources follow one (ADR 0077), and an app that runs on amd64 alone offered for amd64 servers.
