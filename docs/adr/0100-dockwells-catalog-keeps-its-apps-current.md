# 0100. Dockwell's catalog keeps its apps current by itself

- Status: Proposed
- Date: 2026-11-15

## Context

On 2026-11-15 the owner asked how Dockwell's own catalog keeps its apps up to date automatically. It doesn't, yet. Dockwell/catalog ([ADR 0077](0077-dockwells-catalog-in-its-own-repository.md)) holds 34 entries. Each pins its images as `repository:tag@sha256:<digest>`, says the app's `version`, and counts its `release`. A new version reaches the catalog only when someone notices it, edits the entry, and opens a pull request. The catalog's only workflow checks entries as a server reads them. Nothing looks for new releases. So the catalog is as current as the last time someone looked, and every install that follows it is too.

What there is to build on:

- **[ADR 0079](0079-knowing-a-newer-version-of-an-app-is-out.md)** already reads a tag's track, its newer tags, and their digests from a registry. It also classes each step as a patch, minor or major from the repository's own tags. Its rules are what "an update" means to an installed app, and the catalog should mean the same.
- **[ADR 0080](0080-updating-an-app-safely-and-automatically.md)** upgrades an install to a higher release as a plan a person applies, with database majors as a move of their own.
- **The catalog's CI** checks every entry with `dockwell-catalog check`, pinned to a commit of Dockwell.
- **Dockwell's own CI** installs catalog apps on real Docker ("Catalog on Docker"): each starts, answers, and makes its users.
- **Every install follows Dockwell/catalog's main** within an hour, and what runs changes only with a person's plan (ADR 0077's line).

What others do: 1Panel, Runtipi and CasaOS keep their stores current with a bot or by maintainers' hand, and merge without starting the app. Renovate bumps a digest inside a file, but knows nothing of an entry's `version` or `release`, or of ADR 0079's tracks.

## Decision

### The line

**The catalog moves itself to new releases of its apps; installs still move only by a person's plan.** A bump changes what's offered, never what runs, as ADR 0077 has it. So it can merge without a person where it's proven safe. An install then shows "Upgrade available", and its owner plans the upgrade.

### What an entry says about its updates

An entry gains `updates`, read by the bot and ignored by servers:

- **`track`, per image,** is ADR 0079's track of its pinned tag by default: `ghost:6.72.0` follows `6.x.y`, and `wordpress:7.1.3-apache` follows `7.x.y-apache`. An entry may narrow it, such as a database held to its major.
- **`majors`** is `ask` (the default) or `never`. A new major always opens a pull request for a person, and never merges by itself.
- **`merge`** is the largest step that merges by itself once it's proven: `minor` for an app (the default), `patch` for a database engine (MySQL, MariaDB, PostgreSQL, Redis, Valkey, MongoDB), and `none` for an entry that always wants a person.
- **`hold`** names versions the bot skips, each with why, such as a release known to break. A held version is never offered, and the next one is.
- **`with`** ties images that move together, such as an app and a sidecar of the same project. They move in one bump, or not at all.

### The bump job

**A scheduled workflow in Dockwell/catalog, daily and by hand,** runs `dockwell-catalog bump entries` from the same pinned commit of Dockwell as the check:

- For each entry and image, it reads the registry with ADR 0079's code: the newest tag on the track, its kind of step, and its digest. Tags are taken as `sha256:` and 64 hex digits only.
- For each entry with something newer, it writes a branch `bump/<id>`. It rewrites the image's tag and digest, sets `version` to the app's new version, and adds 1 to `release`. Then it opens or updates one pull request per entry.
- **The pull request says** the old and new version, the kind of step, the release notes link ADR 0079 finds, and the digests. It keeps the wording rules: fact, consequence, what to do.
- **It's gentle with registries**, as ADR 0079 is: Docker Hub's API for its tags, HEADs for digests, and a back-off on 429. It needs no secret beyond the workflow's own token, as every image the catalog pins is public.
- **One pull request per entry, updated in place,** so a fast-moving app never opens a queue of them. A held or closed version isn't proposed again.

### Proving a bump

A bump merges by itself only once the catalog's CI has proved it:

1. **The entry checks** as a server reads it (`dockwell-catalog check`), as today.
2. **It installs on real Docker** with the catalog-on-Docker job Dockwell's CI runs. The job moves into a reusable workflow both repositories call, for the changed entries alone. The app must start, answer on its port, make its users where it has a need, and stay healthy for a minute.
3. **It upgrades on real Docker:** the entry's previous release is installed, given a little data where the job knows how, and upgraded to the new release, as ADR 0080 plans it. The data must still be there.

### Merging

- **A patch or minor within `merge` that passes all three merges by itself,** by GitHub's auto-merge on a required-checks branch rule. Every install then offers it within an hour.
- **Anything else waits for a person:**
  - a major;
  - a database step past `patch`;
  - an entry with `merge: none`;
  - a bump that fails any check.

  Its pull request says which and why, with a label, so a person can find them.
- **A bump that fails** stays open with the failing job linked, and the next day's run updates it. After 7 days, an issue names it.

### Dockwell's pinned commit

The bump job and the check run from `.github/dockwell-commit`. A weekly job opens a pull request that moves it to main's newest commit whose CI and Release passed, so the catalog's checks keep up with Dockwell's format.

## What we turned down

- **Renovate.** It can bump a digest inside a Compose string with a custom manager. But it can't set `version` or `release`, doesn't share ADR 0079's tracks, and would be a second definition of "an update".
- **Updating installs automatically.** What runs changes by a person's plan. Automatic upgrades for an install are ADR 0080's policy, a person's choice per app, not the catalog's.
- **Merging every bump without proof.** A new image that doesn't start would reach every install as "Upgrade available".
- **Merging majors by themselves.** A major often needs steps, and a database major moves data.
- **Bumping from the control plane.** Each install would write its own catalog. One reviewed and proven catalog is what installs follow.

## Slices

1. **`dockwell-catalog bump`** in Dockwell, with ADR 0079's code and the `updates` field, plus the scheduled workflow in Dockwell/catalog opening pull requests. Nothing merges by itself yet.
2. **Proof:** the reusable catalog-on-Docker workflow, run in the catalog's CI on changed entries, with the upgrade check.
3. **Auto-merge** by the rules above, the 7-day issue, and the weekly move of Dockwell's pinned commit.
