# 0077. Dockwell's catalog in its own repository, followed by every install

- Status: Accepted. Amends [ADR 0055](0055-catalogs-from-a-git-url.md), for what a source offers, not for what runs.
- Date: 2026-11-12

## Context

Dockwell's own entries are built into the binary (`internal/catalog/entries`), and their artwork ships in the panel (`web/public/catalog-media`). A fix to an entry, a new release of an app, or a new app reaches an install only with a release of Dockwell. [ADR 0055](0055-catalogs-from-a-git-url.md) lets an install read a catalog from a public Git repository, pinned by commit, "never a branch", so a source never moves without a person either, and a source's entries show no artwork, as the only artwork an entry could name was the panel's own.

On 2026-11-12 the owner asked for the catalog to live in a public repository of its own, Dockwell/catalog, as the source every install starts with, and for catalogs to follow their repositories, so an install has the latest catalog with no person needed. They decided that new installs start with Dockwell's own catalog alone, and that catalogs follow their branch.

What an install runs must still be what a person chose (PRD principle 2). A catalog decides what can be installed and what an install can upgrade to, not what runs.

## Decision

### The line

**What the catalog offers may follow a source's branch. What runs never changes without a person's plan.** A source that follows a branch moves to the branch's new commit by a change Dockwell makes itself: its actor is Dockwell, it's on Activity, it can be undone, and its notes say what the commit adds, changes and leaves out, as a person's move says. Installed apps are untouched. Where the new entry has a higher release, the install shows "Upgrade available", as before, and the upgrade is a plan a person applies. A person can still pin a source to a commit. This amends ADR 0055's "never a branch" for the catalog's list, not for anything that runs.

### Dockwell/catalog

- **A public repository, github.com/Dockwell/catalog,** holds Dockwell's own entries, one `<id>.yaml` each in `entries/`, and the images they name in `entries/media/`, with their notices, as `web/public/catalog-media` held them.
- **Its licence is the entries'.** Each entry says `license: AGPL-3.0-only`, as Dockwell's own words and files are, and its `source` names the repository and file it lives in. The artwork is the apps' own, with its notices, as before; it isn't relicensed.
- **Its CI checks every entry as a server would,** by running `dockwell-catalog check entries` from a pinned commit of Dockwell, so an entry the control plane would refuse can't merge. The check is `package folder`, the same code a server reads a source with: the same limits, `catalog.Parse`, the Compose file read as an install reads it, every image pinned by digest, and every image checked by its bytes. A change to it reaches the catalog's CI when the catalog moves its pin, by a pull request there.
- **Every change to it is a pull request,** reviewed and checked before it reaches main, which is what every install follows.

### Media from a source

- **A source's images are the folder `media` in its folder of entries,** one level, no deeper. An entry names one as `media/<name>`, in its icon or a screenshot's path. A name is lowercase words with `-` or `_`, then `.webp`, `.png`, `.jpg` or `.jpeg`.
- **The same fetch reads it,** as `gitfetch` reads a folder: within limits of its own, 1,000 images, 1 MiB each, 32 MiB together, with only names that could be an image read at all. Over them, the commit is refused, as for its entries.
- **An image is what its first bytes say,** WebP, PNG or JPEG, and its extension must say the same. Never SVG, which can hold a script. One that isn't is left out, saying why, and an entry that names it, or names an image the commit hasn't, keeps its place without it, and the plan says so.
- **The control plane serves them,** from the commit kept for a source applied, never a plan's, at `/catalog-media/<key>/<commit>/<name>`, where the key is the source's repository and folder, as its commits are kept. It answers only a signed-in person who may read the catalog, with the type its bytes say, `X-Content-Type-Options: nosniff`, a `sandbox` content security policy, and a cache for good, as a commit's files never change. The panel's own policy, `img-src 'self'`, takes it as it is.
- **A source's entry still can't wear Dockwell's artwork:** a `/catalog-media/` path in a source's entry is dropped, saying so, as ADR 0055 has it. Its own images are its own.

### Following a branch

- **A source names a branch to follow,** `CatalogSource.follow`, or none, and stays at its commit until a person moves it, as before. Following with no commit named starts at the commit the branch is at now.
- **The control plane looks at each following source every hour,** give or take six minutes, so installs started together don't ask a host at once, and first within five minutes of starting. A look lists the repository's branches, as `ListCatalogSourceRefs` does, through the same guarded client.
- **"Check now" looks at once,** `CheckCatalogSource`, an admin's, as it reads the network.
- **After a look that failed, the next waits longer:** five minutes, then ten, twenty and so on, at most six hours, and an hour again after one that passed.
- **When the branch has moved, Dockwell moves the source** by a change of its own, planned as a person's `SetCatalogSource` is, with the same notes, and one more saying what runs doesn't change, applied at once.
- **A commit that can't be read, or whose entries all fail when the commit before had some, leaves the source where it is.** The Sources page says why, and so does an alert, `CONDITION_CATALOG_SOURCE_STUCK`, which every install gets once as a default rule, held for an hour, as a failing host is often back by then.
- **Undoing Dockwell's move puts the old commit back, and it stays there until the branch moves again.** The commit Dockwell last moved a source to is kept, as a setting, and the hourly look never moves the source back to it. "Check now" does, as the person asks for the branch's commit, and so does following again, which plans the branch's commit as it is.
- **No hold-back for a commit just pushed.** A move changes what's offered, never what runs, and an upgrade is still a person's plan, so a commit that's soon replaced costs a look at a plan at most. The hourly look gathers a burst of pushes anyway. Dockwell/catalog's own main is reviewed and checked before it moves.
- **A test asks no host it doesn't run:** under `DOCKWELL_TEST=1` the hourly look is off, and a source moves only when a test asks.

### The default source

- **A new install gets the source `dockwell`, following Dockwell/catalog's main at `entries`,** made by Dockwell's own first change, as [ADR 0044](0044-alerts.md) gives an install its first alert rules. An existing install gets it once, the same way. A setting records that it was made, so one a person removes stays removed. If a source already reads Dockwell/catalog, that one is Dockwell's own, and none is added.
- **Dockwell's own catalog comes first,** as it did built in: the source that reads Dockwell/catalog's `entries`, then the rest by name. `CatalogSourceInfo.own` says which it is.
- **An install from the built-in catalog moves to the `dockwell` source** in the same change: its pin names the source, its repository, folder and commit, and keeps the build's commit it came from, `CatalogPin.built_in_commit`. The change's notes name each install it moves, so its upgrades come from the source from then on. Undoing the change puts the pins back, with no catalog to upgrade from, and its plan says so.
- **The binary keeps a snapshot of Dockwell/catalog** as the fallback when GitHub can't be reached, as on a first boot offline, or an install with no way out. It's made by a make target from Dockwell/catalog at a pinned commit, and CI fails if what's embedded isn't what that commit holds, so the catalog repository is the one place entries are written. The `dockwell` source is made at the snapshot's commit, which the control plane reads from the snapshot, so a first boot needs no network. Once GitHub answers, the source follows main from there. `internal/catalog/entries` and `web/public/catalog-media` stop being where entries are written.

## What we turned down

- **Upgrading installed apps as the catalog moves.** What runs changes only with a person's plan; a catalog that follows a branch changes only what's offered.
- **A built-in catalog beside the `dockwell` source.** Two of Dockwell's own, one in the binary and one from the repository, would give each app two origins and two upgrade paths. The snapshot is the source's own commit instead.
- **Following a tag.** A tag is a fixed point; a source pinned to one stays.
- **Holding a just-pushed commit back for some minutes.** It would delay every catalog update to guard against a commit that changes nothing installed.
- **Images from an entry's own host, or any URL.** Browsing the catalog would ask strangers' hosts, and the panel's policy would have to allow them.
- **A source field naming the media folder.** One fixed place keeps the form to the repository's link, and what a converted store brings fits it.
- **Serving a source's images to anyone.** The panel's own files are public; a source's are a stranger's files on the control plane's disk, so only a person who may read the catalog sees them.

## Consequences

- An entry's fix, a new release or a new app reaches every install within about an hour of reaching Dockwell/catalog's main, with no Dockwell release, as a change on Activity each install can undo.
- The catalog's own CI and a server run the same check, so what merges is what installs show.
- A source's repository owner can now change what an install offers without a person on that install. Adding a source that follows is the trust, and the plan and the threat model say so. What runs still waits for a person.
- [The threat model](../security/catalog-sources.md) adds images, following and the default source.

## As built

- **Pull request 1 (#543):** images from a source, following a branch, `CheckCatalogSource`, the stuck alert, `dockwell-catalog check`, and the Sources page's follow line, "Check now" and "Pin here".
- **Pull request 2 (#543):** the Dockwell/catalog repository, the `dockwell` source and its first change, moving the built-in pins, and the embedded snapshot with its make target and CI check.
