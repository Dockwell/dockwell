# 0100. Dockwell's catalog keeps its apps current by itself

- Status: Accepted. Amends [ADR 0077](0077-dockwells-catalog-in-its-own-repository.md), for how a change reaches the catalog's main.
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
- **`gives`:** the service whose tag gives the entry's `version`. A leading `v` and a suffix are dropped from it. An entry with more than one image and no `gives` gets no bumps until a person sets it. Where `version` and the tag disagree today, as pgadmin's `9.18` against its `9.18.0`, a person makes them agree once, first.
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

- **It has aged.** It's been out for 3 days for a patch, and 7 for a minor or a new digest. On Docker Hub, that's counted from when the tag was pushed, which Docker Hub says. Elsewhere, such as GHCR, it's counted from when the bot first saw the version, which the bump's pull request records when it's opened. An image's own build date doesn't count, as whoever builds it sets it. The bot offers the newest version past its wait, so a fast-moving app still moves. A person can merge sooner, such as for a security fix.
- **It's corroborated.** Registry credentials alone can't fake this:
  - for the image that gives the entry's `version`, the same version is a published, non-draft, non-pre-release GitHub release or tag of the entry's `appSource`;
  - for a Docker Official Image, such as a `postgres` beside an app, the tag is in that image's file in github.com/docker-library/official-images, which Docker builds it from. That file names no digests, so until slice 3 finds a channel that does, the digest is held by its wait from Docker Hub's own `tag_last_pushed`, which Docker's server sets; docker-library/repo-info isn't a second channel, as Docker's bot fills it from the registry (amended after slice 1's review);
  - anywhere else, the bump waits for a person.
- **It's signed, where the image was.** With `signedBy` set, the new digest must be signed by that identity. It waits for a person if it's unsigned where the one before was signed, or signed by someone else.
- **It keeps its platforms.** The new digest's index lists every platform the old one did. A release that drops arm64 waits for a person.
- **Its entry has settled.** At most one version an entry merges by itself every 4 days, release changes beside it included, as a rebuild's or a `-major` merge's rebase. An install's automatic update takes a release only once its source has held it for 3 days, and each release starts that again (ADR 0080), so an entry that merged faster would never reach its installs on their own. A person's merge is never held (the release-wait review, M3).

### Proving a bump

These are required checks on every bump. Their runs get `contents: read` and no secret.

1. **The entry checks** as a server reads it, as today.
2. **It installs and upgrades on real Docker,** with `scripts/catalog-e2e.sh` turned into a reusable workflow both repositories call:
   - The previous release is the entry as the pull request's base has it. The hand-kept `previous` map in Dockwell goes.
   - **Each app's check moves to the catalog,** as `checks/<id>.sh`. It writes something through the app, such as a user, a folder or a row. After the upgrade it reads it back, and signs in with the generated secrets.
   - **An entry with no check** gets a floor: a sentinel row or key per database need, written and read back with the engine's own client. An entry with only the floor doesn't merge by itself.
   - **It also upgrades from the first release of the current major,** as an install that waited months would. The format can't yet say "upgrades only from release N or later", so an app that needs that waits for a person, and a later record adds it.
   - It runs on x86, and on arm64 (`ubuntu-24.04-arm`) for entries with no check yet.
   - Pulls go through the CI mirror. A run proves at most 5 entries, as the organization's 20-job cap is shared with Dockwell's CI.
3. **`bump scope`** runs from `main`'s own definition on every pull request, reading the diff only and never running the head. It posts its result as a commit status on the head, which a ruleset can require. It passes a pull request that doesn't come from the App, and leaves it to the review rule. For a pull request from the App, it fails unless every one of these holds:
   - the pull request comes from the App on a `bump/*` branch;
   - it changes one entry, and only an image's tag and digest, `version` and `release`;
   - `release` is exactly the base's plus 1;
   - every image keeps its repository;
   - it leaves `updates/`, `checks/` and `.github/` alone;
   - its step is within the levels, past its wait, corroborated, signed where it should be, and keeps its platforms.

   A maintainer's approval of an App bump that's out of scope runs it again, and then it passes, so a person can merge a major or a held step.

### Merging

Two rulesets on `main`, which GitHub applies together:
- **One requires a review.** The App may bypass it.
- **One requires the entry check, the proof and `bump scope`.** Nobody bypasses it.

GitHub's own auto-merge waits for the review rule even for an actor that may bypass it, so it isn't used. Instead, a small workflow on `main` merges: once every required check on a bump passes, it merges at the head the checks ran on, through the merge API, with the App's token. It runs no Dockwell code. Slice 3 first proves this on a scratch repository.

Anything else waits for a person, with a label saying why:
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

ADR 0080's automatic update of a catalog install takes a release only once it's been on the install's source for 3 days, counted from when the source moved to it. A person's plan can take it at once. ADR 0080 says so, as amended on 2026-11-16.

v0.3.0 already updates catalog installs automatically, with no such wait. So nothing in the catalog merges by itself until a Dockwell release with this wait is out. Until then, the catalog's own wait is the only one.

### When a bad release gets through

**A person withdraws it.** Their pull request adds the version to `hold`. If no install could have moved its data forward, the next release pins the version before. Otherwise the catalog waits for the upstream's fix.

**ADR 0080 refuses a higher release of a lower version as a move down,** as amended on 2026-11-16. It offers it only to new installs.

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
3. **Merging by itself:** the merge workflow, proven first on a scratch repository, which runs `bump scope` itself rather than trust a status on the head; the two rulesets; signatures; digest bumps; and the weekly report. It turns on only once a Dockwell release with ADR 0080's install-side wait is out. It requires that an entry merges at most one version by itself every 4 days, its last merge read from main's history, so each settles for its installs' 3 days ("Its entry has settled", above).

### Slice 1, as built

*2026-11-16.*

- **`dockwell-catalog bump --catalog <checkout> --out <folder>`** reads each entry and its policy, and writes a patch for each lane with `bumps.json`: each pull request's branch, title, body and labels, which the write job reads with `jq` alone. `--schema` prints the policy's JSON Schema, and `dockwell-catalog check` refuses a policy in `updates/` that names no entry, a service its entry hasn't, or a field it doesn't know.
- **The track** is ADR 0079's, with #729's rule that a repository's own numbering ends where its majors jump by more than 4. `registry.AllNewer` gives every newer version of a track, not only the newest of each kind, so a lane takes the newest that's past its wait.
- **The lane beyond** takes the smallest kind of step past the level: a minor where the level is `patch` and there's one, else the next major, its newest version, and never one past it. With `majorsInOrder`, a next major that isn't the one right after the current one waits, saying so. A held version is skipped. A step across a database's series, as ADR 0080's `Series` reads it, is a major.
- **The wait** counts from Docker Hub's `tag_last_pushed`. Elsewhere it counts from when an open bump first saw the version: its body records it on a line of its own, and the read job reads the open bumps' bodies back before it bumps. That takes `pull-requests: read` beside `contents: read`, and the job's own read-only token, for GitHub's rate as it reads each app's releases; it still holds no secret. A version first seen today waits its whole wait from today. The body records every version the run read of each image it moves, not only the one it offers, so a version passed over for a newer one keeps its time, and an app on GHCR that releases faster than its wait still moves (the review's M3). Each is keyed by its repository, tag and digest: a tag pushed again with another image starts its wait again, labelled "tag pushed again" (M4).
- **Corroboration** for what gives the version is a release of `appSource`, tagged with or without a `v`, that isn't a draft or a pre-release, else a tag. For a Docker Official Image it's the tag on a `Tags` or `SharedTags` line of its file in docker-library/official-images. That file names no digests, so the digest itself isn't corroborated there: its wait is what stands for it.
- **Not yet:** signatures (a policy's `signedBy` is read and checked for its shape, and a bump of a signed image waits, saying its signature isn't checked yet), a new digest of the same tag as a step of its own, and the weekly report. `bumps.json` lists the images a run couldn't read. A bump branch with an open pull request that a run proposes nothing for is left as it is, for a person to close.
- **The workflow** is `.github/workflows/bump.yml` in Dockwell/catalog, on a branch until the App is made. `.github/bump-app.md` there says what an owner sets up: the App `dockwell-catalog-bump` with contents and pull requests write, installed on Dockwell/catalog alone, and an environment `bump` kept to `main` holding its key as `BUMP_APP_PRIVATE_KEY` and its client ID as `BUMP_APP_CLIENT_ID`.
- **After its review:**
  - Both jobs take only the App's own pull requests from this repository: a fork's from a branch named `bump/` can't say when a version was first seen, and the App never edits one (H1).
  - The write job pushes a patch only where it changes `entries/<id>.yaml` and nothing else, as git reads the staged change (M2).
  - The lane beyond goes by a database's series where its data moves by series: MariaDB 12.4 comes before 13.0, one series at a time, and nothing past the level goes unsaid (M1).
  - Each bump opens on its own, a failing one named and the rest going on, onto main as it is or the commit its patch was made from (L2), and only a bump's own labels, made once where missing (L4).
  - Waiting on Docker Hub's 429s has a budget, and the run a deadline below the job's, naming what it leaves unread (L3). A comment on `version:` stays (L1), a policy is one YAML document (L5), and a hold names a version or its first parts (L6).

### Slice 2, as built

*2026-11-16.*

- **One proof, two callers.** `.github/workflows/catalog-proof.yml` is a reusable workflow: it checks out Dockwell at a commit and the catalog with its history, builds Dockwell, and runs `scripts/catalog-e2e.sh` for 1 to 5 entries, through the CI mirror, on x86, and again on `ubuntu-24.04-arm` for the entries proven with the floor alone. Dockwell's CI calls it for each group of its snapshot's entries; the catalog's `proof.yml` calls it for the entries a change moves, or whose check it changes, at the commit `.github/dockwell-commit` names, which `proof.yml`'s `uses:` must match. "Proof, every entry" is the one check a ruleset requires, and passes where nothing needed proving.
- **Earlier releases come from the catalog's history.** `dockwell-catalog earlier` reads, from the checkout's git history, the release before (the pull request's base where there is one, else the newest earlier commit whose version differs) and the first release of the current major, where it's neither. Each installs, passes its check, and upgrades to the entry in one plan. The `previous` table and `scripts/catalog-e2e-before/` are gone. A check can say the earliest release that runs on CI's runners at all (`earliest[mongo]=8.0.38`), below which nothing is proven from.
- **Checks live in the catalog** as `checks/<id>.sh`, which a process of the harness's own sources: `check_<id> first` and `check_<id> upgraded`, with what a person gives the install (`given`, `given_env`, `companions`) beside it. The catalog's Check holds each to an entry and its function, and runs shellcheck over them.
- **The floor** is the harness's own probes, which judge every entry after its check and are all an entry with no check gets: what its `web` service publishes answers, and each database it keeps on a volume (PostgreSQL, MySQL or MariaDB, Redis or Valkey, MongoDB, by image) holds a sentinel through the upgrade, written and read back with the engine's own client inside its container. The proof outputs which entries had the floor alone, for slice 3's merge to leave to a person. An input a person must give, with no check to give it, gets a stand-in fitting its type and what its input takes: an email and a host at `.invalid`, which never resolves, a port, text, or for a secret a token in the shape apps such as cloudflared read, of no account anywhere (#745). A stand-in is never a real credential, but it may make an app reach its upstream's service, as cloudflared's tunnel dials Cloudflare's edge, which refuses it. A secret's value reaches Dockwell's CLI from a file of mode 0600, never as an argument.
  - **A service that reads a secret's stand-in** can't pass a health check that needs the real thing, so the copy of the entry the proof installs runs it with no health check, before and after the upgrade alike. It's held instead to running for 120 s after each change, past the 90 s cloudflared's own check allowed, with no restart and the same start time. An exit fails it, whatever its code, as a bad token's looks like a broken release's: "can't prove tunnel: it stops on a stand-in. Give checks/<id>.sh".
  - **An entry proven with stand-ins is never fully proven.** Stand-ins go only to an entry with no check, which is floored and never merges by itself; Dockwell's own CI, under `floor: false`, gives none. The floored list the person merging reads says, beside each such entry, what its stand-ins left unproven, such as "tunnel not held to a health check, as it reads a stand-in for TUNNEL_TOKEN".
- **`bump scope`** is `dockwell-catalog bump scope`, run by the catalog's `scope.yml` on `pull_request_target` from main's definition. It fetches the head's commit as objects, never checking it out, reads the diff with Dockwell at main's pinned commit, and writes the commit status "bump scope" on the head. The App's pull request passes where its branch is `bump/<id>`, it changes `entries/<id>.yaml` alone, only images' tags and digests, `version` and `release` up by one, each image keeping its registry and repository, and its head is exactly the within lane's step the bump makes from its base now, with nothing waiting (read again from the registries, with first-seen times from its own body). `bump/<id>-major` never passes by itself. A maintainer's approval of the head passes it: `review.yml`, on a review, does nothing but end, and `scope.yml` runs again on its `workflow_run`, as a review's own run would take its workflow from the pull request.
- **A commit is judged, not a pull request** (the review's H1). A commit status belongs to the commit, which every pull request with it as its head shows, so a fork's pull request of the App's own head could have earned it a person's pass. `bump scope-whose` judges a commit as the App's bump wherever it's the head of an open App pull request from this repository, or of a `bump/` branch here, whichever pull request started the run. And slice 3's merge workflow doesn't trust any status or check run on the head: it runs `bump scope` itself, on the bump it's about to merge, and merges only where it passes then.
- **The order of landing,** held by the code: the catalog's `checks/` lands first, on main alone (`t3/checks`, nothing runs it); this slice moves `internal/catalog/snapshot/commit` to that commit and copies `checks/` beside the snapshot's entries, and `catalog-snapshot.sh --check` fails until the commit is on GitHub; the harness refuses a catalog with no `checks/`, and Dockwell's CI passes `floor: false` and `require-upgrade: true`, so an entry without a check, or without an upgrade it proves, fails rather than passing on the floor (the review's M1, M2). Then the catalog's `proof.yml` and `scope.yml` land, with `.github/dockwell-commit` moved to a Dockwell commit that has `catalog-proof.yml` and `bump scope`.
- **After its review:**
  - The floor holds every database that kept a sentinel to running that engine on a volume after the upgrade, and fails where one is gone (M3). A check can't redefine the harness's functions, as they're read-only; an entry whose every earlier release is before its check's `earliest` is reported `not-upgraded` (M4). The proof builds without setup-go's cache (M5).
  - `earlier` takes the newest earlier release whose file differs, reads main's own history by its first parents, and falls back to the history where a pull request's base has the head's entry (M1, L2, L3). A check names its release before by version and image digests, `before[<id>]`, for a catalog history that holds none, and `upgrade_waived[<id>]` says why one proves none; Dockwell's CI requires the upgrade, and its test fails where a tried entry has neither, outside a short list (M1).
  - `bump scope` diffs from the merge base and says where main moved the entry since (L1), and an unread registry gives `error`, to run again (L5). In the catalog, scope.yml's runs are keyed by the pull request's number and queued (L4), and it reads in a job that can't post, posting from one that reads nothing (L7); proof.yml picks from the merge's first parent and holds every pin to `.github/dockwell-commit` (L6).
- **After its re-check:**
  - `scope.yml` and `proof.yml` run only for pull requests into main, so a copy of them on any other branch, a `bump/` branch cut before a fix included, never judges a commit (N1).
  - **What a check reaches by files and its shell is closed** (N3). The harness reads every entry, check and earlier release it proves before any check runs, and runs from a copy of itself. Each phase of a check runs as its own process: a fresh copy of the harness, as it was then, sources a fresh copy of the check, and keeps nothing for the next phase but `kept` and the plans it made. The entry then passes on the harness's own probes, in the process that judges it, not on the check's exit alone: its containers healthy on the images its entry names, what it opens answering, each database holding the harness's sentinel through the upgrade, and its secrets sealed. The lists of entries with the floor alone or with no upgrade stay in that process, and are written once at the end. So a check that rewrites the checkout, the harness, another check or those lists, shadows a command, or exits early reaches neither its own verdict nor the next entry.
  - **What it still reaches is the machine.** A check runs as the job's user, who has `sudo` and Docker on the runner, so it's root there. In the same job it can still put a program before the harness's own on the `PATH` or replace Docker, leave a process or a container running, change the Dockwell the run proves, or the harness's own process, and a root process can fake what the harness's probes read. Only a runner of its own for each entry closes that. The catalog proves at most 5 entries a run and Dockwell's CI its snapshot's in groups of 5, from checks a person reviewed; an App bump proves one entry, so slice 3's merges aren't reached.

