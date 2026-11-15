# 0100. Dockwell's catalog keeps its apps current by itself

- Status: Proposed. Amends [ADR 0077](0077-dockwells-catalog-in-its-own-repository.md), for how a change reaches the catalog's main.
- Date: 2026-11-15

## Context

On 2026-11-15 the owner asked how Dockwell's own catalog keeps its apps up to date automatically. It doesn't, yet.

Dockwell/catalog (ADR 0077) holds 34 entries:
- **Each entry pins its images** as `repository:tag@sha256:<digest>`, says the app's `version`, and counts its `release`.
- **A new version arrives by hand.** It reaches the catalog only when someone notices it, edits the entry, and opens a pull request.
- **The only workflow checks entries** as a server reads them. Nothing looks for new releases.

So the catalog is as current as the last time someone looked, and so is every install that follows it.

What there is to build on:

- **[ADR 0079](0079-knowing-a-newer-version-of-an-app-is-out.md)** reads a tag's track, its newer tags and their digests from a registry, and classes each step as a patch, minor or major. #729 adds that a repository whose majors jump by more than 4 has changed how it numbers, as MySQL's year.month releases after 9.x.
- **[ADR 0080](0080-updating-an-app-safely-and-automatically.md)** upgrades an install to a higher release by a plan. Database series moves are its own kind of plan. An install can also opt into automatic updates.
- **`scripts/catalog-e2e.sh`** installs an entry on real Docker, writes data through the app, upgrades, and reads the data back.

ADRs 0079 and 0080 are both still Proposed. This record builds on them as they stand.

Renovate can bump a digest inside a Compose string. But it can't set `version` or `release`, and it doesn't share ADR 0079's tracks.

## Decision

### The line

**The catalog moves itself to new releases only when they're proven, small and corroborated. Everything else waits for a person.**

A bump changes what's offered. An install then moves in one of two ways:
- by a person's plan, as before;
- by the automatic updates a person turned on for it (ADR 0080), which take a catalog release only after a wait (see "On the install side").

So an automatic bump can reach running containers. That's why it has to clear every check below.

### The policy, beside the entries

The bot's policy lives at the repository's top, outside `entries/`, in `updates/<id>.yaml`.
- **It isn't part of the entry format.** A server never reads it, and an older Dockwell would refuse an entry with a field it doesn't know.
- **Changing it moves nothing.** It doesn't raise `release` or move any install's source.
- **Its defaults come from code,** so most entries need no file.
- **Its schema lives with `dockwell-catalog`,** not in `entry.proto`.

It's keyed by entry and Compose service, since the same image appears in several entries. Per service it says:

- **`level`:** the largest step that merges by itself.
  - `patch` is the default for a database engine's image wherever it appears: postgres, mysql, mariadb, mongo, redis, valkey and clickhouse.
  - `patch` is also the default for an app on 0.x or on calendar versions, such as `2026.11`.
  - `minor` is the default for an app at 1.0 or later on semantic versions.
  - `none` means only a person moves it.
  - `stays` means it never moves, as Plausible's ClickHouse, which follows what Plausible itself pins.

  A step ADR 0080 calls a database series move counts as a major, whatever ADR 0079's numbers say.
- **`gives`:** the service whose tag gives the entry's `version`. That's only needed where an entry has more than one image. A leading `v` and a suffix are dropped from it.
- **`rebuilds`:** whether the repository re-pushes the same tag by design. Docker Official Images do, and it's on by default for them.
- **`signedBy`:** the identity that signs the image. That's a cosign keyless issuer and subject, or a GitHub attestation's repository and workflow. A person confirms it once.
- **`majorsInOrder`:** for an app that can't skip a major, as Nextcloud.
- **`hold`:** versions to skip, each with why. Only a person's pull request changes it.

### The bump job

It runs daily at 10:00 UTC (2:00 AM PST, 3:00 AM PDT for Vancouver), and by hand, in two jobs:

1. **Read.** It runs `dockwell-catalog bump` from the commit in `.github/dockwell-commit`, with `contents: read` and no secret.
   - For each image it reads the registry with ADR 0079's code. It finds the newest version on the image's track that has aged past its wait, and any new digest of the tag it runs.
   - It writes the edits as a patch file. It changes the exact `image:`, `version:` and `release:` lines as text, so comments stay, and fails if any line isn't found exactly once.
   - Docker Hub's 429 is retried within the run. An image left unread for 3 days is named in the weekly report.
2. **Write.** It holds a GitHub App token and runs no Dockwell code.
   - The App is installed on Dockwell/catalog alone, with `contents: write` and `pull_requests: write`. Its key is in an environment secret only the scheduled workflow on `main` can read.
   - It applies the patch with `git` and opens or updates pull requests. A pull request from the App starts CI, which one made with the workflow's own token wouldn't.

Each entry gets up to two pull requests, updated in place:
- **`bump/<id>`** takes the newest step within the levels.
- **`bump/<id>-major`** takes the next step beyond them. For a major, that's the next major only, never two at once.

When one merges, the bot rebases the other and recomputes `release`. A closed bump counts as declined only until the next version.

A new digest of the same tag is a step of its own:
- `version` stays the same and `release` goes up by 1.
- It merges by itself only where `rebuilds` is on, after the wait, and at most once a week per entry.
- Anywhere else it waits for a person, labelled "tag pushed again".

A tag that isn't a version, such as `latest`, isn't allowed in Dockwell's own catalog.

### The wait, and a second channel

A version merges by itself only once all of these hold:

- **It has aged.** It's been out for 3 days for a patch, and 7 for a minor or a new digest, counted from when the tag was pushed. The bot offers the newest version past its wait, so a fast-moving app still moves. A person can merge sooner, such as for a security fix.
- **It's corroborated.** The same version is a published, non-draft, non-pre-release GitHub release or tag of the entry's `appSource`. Registry credentials alone can't fake that. Where `appSource` isn't on GitHub, the bump waits for a person.
- **It's signed, where the image was.** With `signedBy` set, the new digest must be signed by that identity. It waits for a person if it's unsigned where the one before was signed, or signed by someone else.
- **It keeps its platforms.** The new digest's index lists every platform the old one did. A release that drops arm64 waits for a person.

### Proving a bump

These are required checks on every bump. Their runs get `contents: read` and no secret.

1. **The entry checks** as a server reads it, as today.
2. **It installs and upgrades on real Docker,** with `scripts/catalog-e2e.sh` turned into a reusable workflow both repositories call:
   - The previous release is the entry as the pull request's base has it. The hand-kept `previous` map in Dockwell goes.
   - **Each app's check moves to the catalog,** as `checks/<id>.sh`. It writes something through the app, such as a user, a folder or a row. After the upgrade it reads it back, and signs in with the generated secrets.
   - **An entry with no check** gets a floor: a sentinel row or key per database need, written and read back with the engine's own client. An entry with only the floor doesn't merge by itself.
   - **It also upgrades from the first release of the current major,** as an install that waited months would.
   - It runs on x86, and on arm64 (`ubuntu-24.04-arm`) for entries with no check yet.
   - Pulls go through the CI mirror. A run proves at most 5 entries, as the organization's 20-job cap is shared with Dockwell's CI.
3. **`bump scope`** is required, and runs from `main`'s own definition. It reads the diff only, never running the head. It fails unless every one of these holds:
   - the pull request comes from the App on a `bump/*` branch;
   - it changes one entry, and only an image's tag and digest, `version` and `release`;
   - `release` is exactly the base's plus 1;
   - every image keeps its repository;
   - it leaves `updates/`, `checks/` and `.github/` alone;
   - its step is within the levels, past its wait, corroborated, signed where it should be, and keeps its platforms.

   A bump that fails `bump scope` can still merge, but only with a person's review.

### Merging

Two rulesets on `main`:
- **One requires a review.** The App may bypass it.
- **One requires the entry check, the proof and `bump scope`.** Nobody bypasses it.

So a bump within the rules merges by itself through GitHub's auto-merge. Anything else waits for a person, with a label saying why:
- a major or a series move;
- a step past its level;
- a version still within its wait;
- missing corroboration or signature;
- a dropped platform;
- a failed check.

A bump that keeps failing is named in an issue after 7 days.

Moving `.github/dockwell-commit` changes the code the catalog's CI runs, including the read job. A weekly pull request proposes main's newest commit whose CI and Release passed, and it always waits for a person.

This amends ADR 0077: a change reaches the catalog's main when it's "reviewed, or proven and in scope". ADR 0077's reason for no hold-back ("a move changes what's offered, never what runs") no longer holds alone, because of automatic updates. So the wait above is the hold-back.

### On the install side

ADR 0080's automatic update of a catalog install takes a release only once it's been on the install's source for 3 days, counted from when the source moved to it. A person's plan can take it at once. This needs a change to ADR 0080, as a follow-up.

### When a bad release gets through

**A person withdraws it.** Their pull request adds the version to `hold`. If no install could have moved its data forward, the next release pins the version before. Otherwise the catalog waits for the upstream's fix.

**ADR 0080 must refuse a higher release of a lower version as a move down.** It offers it only to new installs. That's also a follow-up to ADR 0080.

### A weekly report

It opens one issue for entries that may have moved on without the bot noticing:
- an entry whose `appSource` has had a newer GitHub release than its `version` for 14 days, with no bump open, as when a vendor changes registry, numbering or suffix;
- an image left unread for 3 days.

The bot never changes an image's repository. That's a person's pull request.

## What we turned down

- **Renovate.** It can't set `version` or `release`, and would be a second meaning of "an update" beside ADR 0079's.
- **The policy inside the entry.** Every install on an older Dockwell would refuse the whole catalog.
- **Merging every proven bump.** A proof says an image works, not that it's the upstream's own.
- **Merging majors and series moves by themselves.** A major often needs steps, and a database series move moves data.
- **The workflow's own token.** Its pull requests start no CI.
- **Bumping from each control plane.** Each install would write its own catalog. One reviewed and proven catalog is what installs follow.

## Slices

1. **`dockwell-catalog bump` and the two-job workflow,** with `updates/`, the wait, corroboration and the platform check. It opens pull requests; nothing merges by itself.
2. **Proof:** the reusable install-and-upgrade workflow, checks moved to `checks/`, the floor, arm64, and `bump scope`.
3. **Auto-merge** by the two rulesets, signatures, digest bumps, the weekly report, and the follow-ups to ADR 0080.
