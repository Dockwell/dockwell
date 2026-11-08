# 0043. Package updates on a server, through its own package manager

- Status: Accepted
- Date: 2026-11-07

## Context

Issue #229 asks for a server's Updates tab (`design/mocks/Server-Updates.dc.html`) and the Updates card on the overview (`Overview-Single.dc.html`, `M-Overview-Single.dc.html`): which package updates wait, which are security fixes, whether the server needs a reboot, and Dockwell's own version against its newest release. The PRD has package updates on a server's page (P1). Keeping packages patched is part of what a panel does, and 1Panel's servers here were left months behind.

What bears on it:

- The agent runs as root, and so must a package manager that installs. It runs the maintainers' scripts of every package it touches, as root. That's the most power any program Dockwell starts has, so how it runs has to be narrow and plain.
- The agent stays under 40 MB at idle ([ADR 0018](0018-keep-the-budgets-in-rss.md)). apt, dnf and apk are separate programs, and dnf alone takes more than that while it reads its metadata.
- What a server reports live is read only while someone looks, as ADR 0040, on its own branch as this is written, has a server's live use. Refreshing a package list reaches the network, so it should happen no more than a server would do by itself.
- Every change to managed state is a revision with a plan ([ADR 0009](0009-resources-revisions-and-plans.md)). A server's packages aren't managed state: Dockwell holds no desired version of them, can't see them drift, and can't undo an upgrade. A restart is in the same place, and it's a change with a plan that makes no revision.

## Decision

### The distribution's own tool, and nothing else

The agent reads and installs updates with the package manager the distribution has: **apt** on Debian, Ubuntu and what's built on them, **dnf** 4 on AlmaLinux, Rocky Linux and RHEL, and **apk** on Alpine. It picks by `ID` and `ID_LIKE` in `os-release`, and runs the tool from its fixed path: `/usr/bin/apt-get` (with `dpkg-query` and `apt-cache` beside it), `/usr/bin/dnf` (with `rpm`), `/sbin/apk`. Anything else, and a server whose tool isn't at its path, says "Dockwell can't read updates on this server" with what it found. No `yum` alone, `zypper` or `pacman`: they can come later, each with its own tests on its own distribution. Fedora isn't read yet either: its dnf is dnf 5, whose output isn't dnf 4's, so a server whose `/usr/bin/dnf` leads to dnf 5 says so, rather than reading it wrong and marking no security fix.

### How it runs

- **As root, with no shell.** The agent starts the tool from its absolute path with a list of arguments, so nothing in a package name or a setting is ever parsed by a shell, and a name is refused unless it's one the tool itself listed.
- **With a clean environment.** `PATH=/usr/sbin:/usr/bin:/sbin:/bin`, `HOME=/root`, `LC_ALL=C` so the output reads the same on every server, and for apt `DEBIAN_FRONTEND=noninteractive` and `APT_LISTCHANGES_FRONTEND=none`. Nothing of the agent's own environment passes on. Standard input is `/dev/null`, so nothing can wait for an answer, and apt is told to keep a configuration file a person changed (`--force-confold`) rather than ask.
- **Bounded in time.** Each run has a deadline: 5 minutes to refresh the lists, 2 minutes to read what waits, and 30 minutes to install. It runs in a process group of its own, which gets SIGTERM at the deadline and SIGKILL 10 seconds later, so a hung mirror or a maintainer's script that waits for nothing can't hold the agent. apt waits at most 2 minutes for a lock the server's own updates hold, and says so.
- **One at a time.** A server runs one package manager run at a time for Dockwell. Two people looking share one read.
- **Reading changes nothing.** apt reads with `--simulate` and `Debug::NoLocking`, dnf with `--cacheonly`, and apk with `apk version`, so reading installs nothing, and apt's and apk's take no lock. dnf 4 takes its lock even reading from its cache, so a read waits while the server's own dnf runs. Only refreshing and installing write, and both are said as they happen.
- **Nothing it starts outlives it.** A tool that exits leaving a child holding its output is waited for the grace, and its process group is then killed, as it is at a deadline.

### What it reads

- **What waits:** each package with its installed version, the version it would get, where that comes from, and its one-line description. For apt, that's what `apt-get upgrade --with-new-pkgs` would do, so a new kernel that a metapackage pulls in is there, and nothing that would be removed. Packages apt holds back, such as Ubuntu's phased updates, are counted apart, as an upgrade wouldn't take them yet.
- **Which are security fixes,** as the distribution says: for apt, a version from a suite that ends in `-security`, such as `noble-security` or `bookworm-security`; for dnf, a package a security advisory names in `updateinfo`. Alpine doesn't mark them, so on Alpine none is, and the tab says so rather than guess.
- **Whether a reboot is needed now:** `/run/reboot-required` where Debian and Ubuntu write it, `dnf needs-restarting -r` where it's installed, and otherwise a kernel under `/lib/modules` newer than the one running.
- **What installing would need:** a reboot for a new kernel, libc, D-Bus, the CPU's microcode, or systemd under dnf, the core set `dnf needs-restarting` uses (Debian and Ubuntu re-execute systemd as it's upgraded), and a restart of Docker for Docker's own engine, which stops every container, and starts again only those set to restart, unless Docker keeps containers running across a restart (`live-restore`). Nothing else is claimed: which programs keep an old library until they restart isn't known before installing.

### Only when someone looks, and the lists at most daily

- The control plane asks a server's agent what waits when someone has the server's page or the overview open, and keeps the answer in memory for 10 minutes. A restart of the control plane reads it again when someone next looks. Nothing asks with nobody looking.
- **Refreshing the lists** (`apt-get update`, `dnf makecache`, `apk update`) reaches the network, so the agent does it only when the lists are more than a day old, by when the server last refreshed them, whoever did: the stamps the server's daily update leaves (Ubuntu's `update-success-stamp`, Debian's `update-stamp`), or the agent's own last refresh. A list file's time is only a floor, as apt gives it the repository's own time. A server that refreshes its own lists daily, as Ubuntu's `apt-daily.timer` does, is never refreshed by Dockwell at all.
- **A refresh that fails holds off the next for an hour,** so a mirror that's down isn't asked on every look, and the tab says why meanwhile. The agent keeps when it last refreshed, and last tried to, in its data folder, so a restart forgets neither.
- **Check now** refreshes at once, for a person, and at most once a minute, a failed refresh or not.
- With `DOCKWELL_TEST=1`, the agent runs no package manager unless `DOCKWELL_TEST_PACKAGES` names a folder of stand-ins, so no test reaches a mirror or reads the machine it runs on.

### Output is read, not kept

- What a read prints is parsed and dropped. At most 4 MiB of it is read, and a tool that prints more fails the read. Of what it prints on standard error, the last 16 KiB is kept for the error's message. Nothing is written to disk or to the agent's log, beyond one line that a read or refresh failed and why.
- An install's output goes to the change as its step's progress, line by line, and the last lines go with a failure's message. Once the change ends, nothing more of it is kept.

### The budget

The agent holds nothing for updates between runs. A read parses into a list of at most a few hundred packages, which goes to the control plane and is dropped. The package manager is a process of its own, so it isn't in the agent's RSS, but it is in the server's memory while it runs: apt about 30 to 60 MB for a second or two, dnf 100 MB or more while it reads its metadata. That cost is why nothing runs while nobody looks.

### Dockwell's own version

The control plane reads the newest release at most daily, and only when someone looks. Each milestone's release is a pre-release (the release policy decided so), and GitHub's `latest/download` skips pre-releases, so with no mirror it reads GitHub's list of releases (`/repos/Dockwell/dockwell/releases?per_page=10`), and takes the newest by semver, pre-releases included and drafts not, within the same bounds: 30 seconds, and 1 MiB read. With a mirror (`DOCKWELL_MIRROR`, then `DOCKWELL_RELEASES_URL`, as the install script takes them), it reads `latest/download/checksums.txt` there, as a mirror copies releases and has no API. A read that fails is tried again within the hour, and logged once for each run of failures, not each time. The install script keeps a mirror it was given in the service's environment, so the control plane reads where the install came from. The panel's command to update builds on the newest release's own `install.sh`, from the same releases URL, run as root. It's a hint and nothing more: nothing is downloaded, and a mirror can serve an older release as the latest, so updating stays the install script's, which checks the signature ([ADR 0010](0010-install-script-verifies-with-pinned-cosign.md), [ADR 0013](0013-mirrors-for-releases-and-images.md)). With `DOCKWELL_TEST=1` it isn't read unless `DOCKWELL_RELEASES_URL` is set.

### Who may read and check

- **Reading a server's updates takes what reading the server takes:** any API token can, read-only included, as a server's inventory, its OS and its kernel, is already readable, and what waits says little more of how to attack it than those.
- **Check now takes a person** (`needs_session`): it has the server reach its mirrors, so a token that leaked can't keep a fleet running `apt-get update`, even at once a minute.
- **Why updates can't be read is a reason code** the panel words: `unsupported`, `agent_outdated`, `server_offline` and `read_failed`, with the server's own words as detail. With nothing read yet, the panel says it's checking, or why it can't, and never that a server is up to date.

### Installing, and rebooting

- **Installing is a change with a plan, which makes no revision,** as a restart is. The plan lists each package from its version to the new one, which need a reboot, and whether Docker restarts, and its fingerprint covers the versions, so a list that moved means reviewing it again. It installs only what it names, never removes a package (`--no-remove` for apt), and has nothing to undo: a package can't reliably go back, and a failed install leaves what it installed.
- **It takes a person and a passkey.** What it runs is root, so no API token applies it, and applying asks for the passkey a root change asks for (`STEP_UP_ACTION_APPLY_ROOT_CHANGE`). It goes on Activity and the server's timeline as any change does.
- **A reboot is a change of its own,** never a step that an install adds by itself. The Updates tab offers it once the server needs one. Its plan says that everything on the server goes offline, and for about how long, and it takes a person and a passkey.
- **Nothing installs or reboots by itself.** The mock's nightly schedule isn't built: the server's own automatic updates, such as `unattended-upgrades`, go on as they were, and the tab says whether they're on.

## Consequences

- Updates work on the distributions the PRD names, with the same three code paths everywhere and no shell. Another package manager is new code with its own tests, not a setting.
- The parsers rest on output the C locale makes stable: apt's `Inst` lines, `dnf check-update` and `updateinfo`, and `apk version`. Their tests run on output taken from each distribution, and CI's Docker job runs the reader on real Debian, Ubuntu, AlmaLinux and Alpine images, so a release that changes them fails there first.
- A server that refreshes its own lists is never refreshed for Dockwell, and one that doesn't is refreshed at most once a day when someone looks. What the tab shows is as old as the lists, and it says when they were refreshed.
- What a library update restarts isn't shown, as only the install can tell. The plan says what's known: a reboot, and Docker.
- Rebooting from the panel takes the panel down with the server when the control plane runs there. Its plan says so.
