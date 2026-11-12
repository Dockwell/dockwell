# 0079. Knowing a newer version of an app is out

- Status: Proposed
- Date: 2026-11-12

## Context

On 2026-11-12 the owner asked what upgrading an app is like: can Dockwell tell that a new version is out, and upgrade it, smoothly and safely (#545, #546). Today:

- **A catalog install** shows "Upgrade available" when its catalog has a higher release, but only on the catalog entry's own page. The app's page, the Apps list and the Overview say nothing.
- **An app from an image, a Compose file or Git** is never checked. Nothing says that postgres:16.4 is out while 16.3 runs, or that `latest` moved.

This record is about knowing: what to check, how, how often, what is kept, and where it shows. ADR 0080 (#546) is about upgrading, on what this finds.

What others do, which sets the bar:

- **Watchtower** and **Unraid** ("update ready") compare the digest the same tag points at with what runs. They never see a newer version tag, so postgres:16.3 stays "up to date" for ever. Watchtower updates by itself unless told only to watch.
- **Diun** only notifies, on a digest or on new tags that match a pattern a person writes. **What's Up Docker** reads version tags as semver and classes them as major, minor and patch, again with patterns a person writes per container.
- **Portainer** and **Synology Container Manager** mark an image whose tag moved, on the container's own page.
- **Coolify** and **Dokploy** don't check; a redeploy pulls the tag again.
- **1Panel** offers its store's newer versions on an installed app, with "ignore this version", and knows nothing of a container it didn't install.

None reads version tags with no pattern to write, keeps a suffix's track, links release notes, and shows the result on every surface a person looks at. That's the bar.

What Dockwell has to build on:

- [ADR 0062](0062-reading-an-image-from-its-registry.md): the agent on a server reads an image from its registry with that server's network, mirrors and insecure registries, through `netguard`, mindful of Docker Hub's limits. A HEAD of a manifest says the digest a tag points at, and no registry counts it as a pull.
- Each service's deployment records the digest it ran (`ServiceDeployment.digest`), from Docker's `RepoDigests`, which is what a HEAD of the same tag answers while the tag hasn't moved.
- A catalog install's pin says where it came from and which release it runs ([ADR 0055](0055-catalogs-from-a-git-url.md)), and the catalog says whether there is a higher one (`catalog.Upgrades`).
- [ADR 0046](0046-builds-from-git.md)'s builds read what a branch names over Git's smart HTTP, from the control plane, with no checkout.
- Alerts and their channels ([ADR 0044](0044-alerts.md)); an alert's mute as an audited action that isn't managed state.

## Decision

### What is checked, per service

Each service is checked on its own, and a Compose stack's update is the set of its services'. What's checked depends on where the service's image comes from (`ServiceUpdate.source`):

- **A catalog install** (its project has a pin): its catalog has a higher release of its entry, the rule `CatalogInstall.upgrade` follows today, for a source that follows a branch too (ADR 0077, #543). It's read from the catalog when it's asked for, so it's never staler than the catalog. Its images are pinned by digest, so no registry is asked.
- **An image whose tag is a version**, such as `16.3`, `1.27.3-alpine` or `v1.2`: newer tags in the same track, read from the registry's list of tags, and whether the tag it runs points at a new image.
- **Any other tag**, such as `latest`, `stable` or `edge`: whether it points at a new image, by digest. A tag that isn't a version gets only that check.
- **An app built from Git**: whether the branch it builds has a newer commit than the one that runs, from the same read of the repository's refs that planning a build makes. It isn't counted in commits, as counting needs the history, which a fetch would take. A tag or a commit named as the ref never moves, and isn't checked.
- **An app from a folder on the server** ([ADR 0058](0058-runtimes-an-app-from-a-folder.md)): whether its language's image has a new build, by digest. A newer version of the language is the runtime's choice, which ChangeServiceRuntime already offers.

### Versions, and how a step is classed

A tag is a version where it reads as one: numbers with dots, an optional `v` before them, and an optional suffix after a `-`, `+`, `_` or `.`, as ADR 0062 sorts tags. The numbers are compared as numbers.

- **Its track** is its `v`, how many numbers it has, and its suffix with each run of digits taken as any number. A newer tag is only one in the same track: `16.3-alpine` moves to `16.4-alpine` or `16.4-alpine3.21`, never to `16.4` or `16.4-bookworm`, and `1.27` to `1.28`, never to `1.27.4`, which a tag of two numbers already follows.
- **Pre-releases** (`-rc1`, `-beta`, `-alpha` and the words ADR 0062 names) are left out, unless what runs is one, when a newer pre-release or the release itself is offered.
- **The kind of step** reads from the first number that differs. The first is the major. The last is the patch. With three numbers or more, the second is the minor. How many numbers a version has is read from the repository's tags with the same major in that track, as projects number differently: PostgreSQL has no third number in 16.x, so 16.3 to 16.4 is a patch, as PostgreSQL means it, while nginx has 1.27.3, so 1.27 to 1.28 is a minor. A tag of one number, such as `16`, moves only by majors.
- **What's offered** is at most one of each kind, the newest: the newest patch, the newest minor and the newest major, plus a new image for the tag it runs (`UPDATE_KIND_DIGEST`). Smallest step first: digest, patch, minor, major. The panel offers the first and shows the rest one step away, so a person on 16.3 sees "16.4 · patch" first and "17.2 · major" beside it.
- **A catalog install's** step compares its entry's app versions the same way, with the two versions alone to read the places from. A release that changes how the same version runs is `UPDATE_KIND_RELEASE`.

### Where and how it's read

- **On the agent of a server the service runs on**, as ADR 0062 reads: through that server's mirrors, its insecure registries, and `netguard`. The control plane schedules the checks, dedupes them, keeps what they found, and words it. A service on several servers is checked through the first of them that's connected.
- **One call per image a server runs**, shared by every service on that server that runs it. The agent asks the registry with a HEAD of the tag's manifest for its digest, which no registry counts as a pull. For a version tag it also reads the repository's tags, once per repository per check: Docker Hub's 100 most recently pushed, from its own API, which isn't a pull, filtered by the suffix where there is one; another registry's `/v2/` list, paged within ADR 0062's bounds. Of the newer tags it keeps the newest of each kind and asks each one's digest with a HEAD, at most three, where Docker Hub's list hasn't said it.
- **From the server's own copy of what runs**, with no network, it reads the image's `org.opencontainers.image.source` label, for release notes.
- **Every 6 hours per server**, jittered by a tenth so a fleet's servers don't ask at once, and the first time a few minutes after the control plane starts, so a restart doesn't ask anything that was checked in the last 6 hours again. **Check now** checks an app, or every app on a server, at once, at most once a minute each.
- **Gently.** A registry that answers 429 goes unasked by that server for an hour, then two, doubling to a day, and one that doesn't answer for an hour; a check meanwhile keeps what it found before and says why it's late. An agent asks two registries at a time at most, as its reads for a form do (ADR 0062), so a check never takes a form's turn for long.
- **Private images.** Dockwell has no registry logins yet (ADR 0062), so an image a registry keeps private reads as "Can't check: private or not found", which isn't an error a person needs to act on. When logins come, a check sends what the pull sends.
- **A server that's offline** keeps what was found before, and is checked when its agent reconnects, if a check is due. An agent too old to check says so, and the server's page offers to update it.

### Release notes and dates

- **Notes:** an image's `org.opencontainers.image.source`, on GitHub as its releases page and on GitLab as its `/-/releases`; another HTTPS source as itself. A catalog entry's `app_source`, the same way. Only `https://` addresses are kept.
- **Dates:** Docker Hub says when each tag was last pushed, which is when its version came out or its image was rebuilt. Another registry says nothing without reading a config, which isn't worth a GET, so its date is when Dockwell first found it (`found_time`), which the panel says as such.

### What's kept, and where

The control plane keeps each service's last result in the store (`service_updates`), so a restart forgets nothing and the panel shows it at once:

- when it was last checked and when it's checked next;
- what was found, with when each was first found;
- why it couldn't be checked, as a reason the panel words (`private_or_not_found`, `unreachable`, `rate_limited`, `server_offline`, `agent_outdated`, `not_running`) and the registry's or server's own words;
- what a person skipped or snoozed.

What a check found is the control plane's record, as an alert is, and not managed state: nothing runs differently for it, so it has no revisions. The agent keeps nothing between checks, so an idle agent holds nothing for them ([ADR 0060](0060-what-an-idle-agent-keeps-in-memory.md)).

### Skip and snooze

- **Skip this version** sets one exact version aside for one app: a skipped 2.0 doesn't show, and 2.0.1 does. It's kept per app, as a version, a digest or a commit, for the last 16 skipped.
- **Snooze** sets all of an app's updates aside for a day, a week or a month: they show on its page as snoozed, and count nowhere else.
- Both are audited actions of a developer of the app's project, as acknowledging an alert is, not changes with a plan, as nothing that runs changes. Undoing either is the same action. ADR 0080's automatic updates never apply a version a person skipped, nor an app's updates while they're snoozed.

### What the API says

`ServiceInfo.update` carries a service's `ServiceUpdate` on every row ListServices returns, so a list and a page need no second call:

- `check`: up to date, available, failed or not checked, and unset before the first check;
- `available`: what it can move to, smallest step first, each with its kind, the version and image it runs and would run, both digests where known, when it came out and was found, its notes, the catalog release for an install, the commit for a Git app, and whether a person skipped it;
- `check_time`, `next_check_time`, `problem` and `problem_detail`, `source`, `snooze_time`.

`ServiceUpdateService` lists services with their updates, checks now, skips and snoozes, and the CLI's `dockwell updates` takes the same. Reading takes a viewer of the project; checking, skipping and snoozing a developer, as deploying does ([ADR 0049](0049-roles-and-invites-by-project.md)).

### Where it shows

- **Apps:** a quiet Update state on the row, and on a stack's row the count of its services', a count in the header, and a filter, Updates available.
- **The app's page:** a card, "16.3 → 16.4 · patch · out 3 days · Notes · Update", whose Update opens ADR 0080's plan. Skip and snooze are one step away, under its menu. A catalog install has the same card, no longer only on the catalog page.
- **Overview:** "3 updates" with Review, which opens Apps filtered.
- **The rail:** a count beside Apps, only while there's one to show.
- **Alerts:** a rule, An update is available, for any update or majors only, through the existing channels, one alert per app that resolves when it's updated, skipped or snoozed.

### What we turned down

- **Reading registries from the control plane.** One answer for a fleet, but from the wrong network: a server behind a mirror or a VPN sees what the control plane can't, and every read would count against one address. ADR 0062 weighed the same.
- **Pulling to check.** Exact, but minutes and gigabytes per app, and a pull Docker Hub counts.
- **Patterns a person writes per app,** as Diun and What's Up Docker ask for. Tracks and kinds read from the tags themselves cover the common shapes with nothing to write, and a tag that doesn't read as a version still gets the digest check.
- **Counting new commits.** It needs the history, a fetch of the repository each time, where the refs alone say the branch moved.

### Numbers

ADR 0079; `ServiceInfo` 20 to 29; the agent protocol's `Call` and `CallResult` 250 to 259; `service_update.proto` numbers its own messages from 1.

## Consequences

- A person sees what's waiting where they already look, and understands the step before taking it: its kind, its date and its notes.
- Each server asks each registry it uses a HEAD per image and, for a version tag, a list of tags, every 6 hours. Docker Hub counts none of it as a pull.
- The store gains a table, and the control plane a loop that wakes each minute and checks a server every 6 hours.
- A private image can't be checked until registry logins come.
- A version scheme the rules misread, such as a date written as one number, reads every step as a major, which says too much rather than too little; skipping it is one step away.
- What's found is as old as its check: a tag pushed a minute after a check shows within 6 hours, or at once with Check now.
