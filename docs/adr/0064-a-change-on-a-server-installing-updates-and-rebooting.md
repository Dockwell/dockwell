# 0064. A change on a server: installing updates and rebooting

- Status: Proposed
- Date: 2026-11-10
- Amended: 2026-11-11, updating packages one at a time or a selection (#521)

## Context

[ADR 0043](0043-package-updates-on-a-server.md) decided how a server's package updates are read, and that installing them and rebooting are each a change with a plan, applied by a person with a passkey. Only the read was built. The Updates tab says "Dockwell can't install them yet" and offers the command to copy (#460). Issue #426's audit ranks this sixth: a procedure a panel should be a button for, with what it will restart said first.

What bears on it:

- **Every change goes through the revision store.** PRD principle 2 says every change to managed state is a revision with a plan, and only `internal/revision` calls `store.DB.Revise` ([ADR 0009](0009-resources-revisions-and-plans.md)); a test fails on anything else that does.
- **The engine plans resources, and some edits make no revision.** A change is a list of edits, each a resource with a spec, stored as `change_items` rows that each name a resource. A restart and a restore are edits of a resource as it is, flagged `Restart` or `Restore`: they record a change and its plan and make no new revision, and there's nothing to undo (`item.noRevision`, [ADR 0027](0027-health-checks-rollouts-and-rollback.md), [ADR 0033](0033-backups-of-volumes-and-databases.md)). Holds, progress, resume and the plan's proto are all per resource.
- **A server's packages aren't Dockwell's.** The server's own `unattended-upgrades` moves them every night, Dockwell can't undo an upgrade, and ADR 0054 lists server updates among what isn't managed state.
- **The engine resumes.** At every start of the control plane, and every time an agent connects, `Engine.Resume` plans each unfinished item of an applying change again and carries on.
- **A tool the agent runs dies with its call.** ADR 0043's tools run in a process group that's stopped when the call's context ends, and the agent's unit stops its children when it restarts. That's right for a read, and wrong for `dpkg` partway through libc.

## Decision

Decided in the review of this record (2026-11-10): the shape below, a server-scoped `server.host` resource whose edits carry flags, and no desired package state. The owner can overrule either.

### A server's host is a resource, and installing and rebooting are flagged edits of it

- **Each server has one `server.host` resource,** named `host`, scoped to the server, with an empty spec at revision 1. Dockwell's own change makes it when a server joins, and gives one once to each server that has joined already, as ADR 0044 gives an install its default rules. Later server settings that are real desired state (automatic updates on or off, the time zone, swap) become fields of its spec.
- **Installing updates is `Edit{Resource: host on web-1, InstallUpdates: …}`, and rebooting is `Edit{…, Reboot: true}`,** as a restart is `Edit.Restart`. A change holds one of them for each server it reaches and nothing else, so a deploy that fails doesn't leave a kernel half installed beside it.
- **Stored as any edit is:** a `change_items` row for the host resource, with two new columns, `install` (the packages the plan fixed, encoded) and `reboot`, beside `pull`, `run` and `rotates`. `item.noRevision` covers both: they record a change and its plan with `Revise` and make no revision, as there's no spec to keep.
- **So the engine's own items carry it:** holds, the fingerprint, progress lines, the plan sheet (ADR 0028), the CLI's plan output, resume and where Activity places events (ADR 0049) work as they do for a restart. What has to learn the flags: the automatic rollback skips them (as for a restart); `UndoChange`, through the API and the sheet, refuses one ("Installing updates can't be undone"); drift ignores the `server.host` kind; the `change-failed` rule's message names what failed ("Installing updates on web-1 failed").
- **No desired package state.** The spec never lists packages or versions: they'd show drift every night, and a revision of them couldn't be undone.

### What the plan says

The control plane plans the edit by asking the server's agent (`PlanCall`), and the host's driver answers with what the exact command would do, not what was picked:

- **The install, as simulated.** The agent runs the install command with the packages picked, in simulation: `apt-get install --simulate --only-upgrade --no-remove name=version …`, `dnf upgrade --assumeno name-version …`, `apk upgrade --simulate`. The plan lists everything the simulation says: each package from the version installed to the one it gets, where it comes from, whether it's a security fix, and each package it pulls in that isn't installed, such as a new kernel. The panel offers the security updates (the default), every update waiting, each package on its own row, a selection of rows, and Docker's engine with what else restarts it; on Alpine, which marks no security fixes (ADR 0043), it offers every update and says why. Nothing is typed, and the agent refuses a name its tool didn't list (see "Packages one at a time", below).
- **What it needs after,** by ADR 0043's rule: a reboot for a new kernel, libc, D-Bus, the CPU's microcode, or systemd under dnf. The plan says the server will need one, which is a change of its own.
- **What stops.** When Docker's engine is among the packages (`docker-ce`, `docker.io`, `moby-engine`, and `containerd.io`, `containerd` or `runc` where their upgrade restarts `dockerd`), its restart stops every container on the server, Caddy's included (ADR 0024): the plan lists each service, and each domain and website served there (the panel's domain too, on the control server when Caddy serves it), and whether Docker starts each again by its restart policy. `live-restore` keeps containers running only across patch releases of the same `YY.MM`; for a larger jump the plan says they stop. On the control server with its store in Postgres there, in a container, it says the store stops with Docker.
- **Nothing else restarts.** Ubuntu's `needrestart` runs after apt, and recent releases restart what it finds; the install runs it with `NEEDRESTART_MODE=l`, so it only lists, and the outcome shows its list. Which programs keep an old library until they restart can't be known before installing (ADR 0043).

The fingerprint covers the simulation's whole list, Docker's restart, and the services and routes on the server, so a deploy between the plan and the apply makes it stale. As the install step first starts, the agent simulates again and refuses if anything differs: "The updates waiting on web-1 changed since this plan. Plan again."

A reboot's plan lists everything on the server as going offline, with each service and whether Docker starts it again; it says the firewall's confirmed table loads at boot (ADR 0052). It lists what a reboot puts at risk: a server path a resource mounts that's on a filesystem in neither `/etc/fstab` nor a mount unit (it would come back an empty folder, and Docker would start on it), how old each volume's last off-server backup is, and on the control server the last control-plane backup (#356). It says how long the last reboot took once Dockwell has seen one (below), and nothing before. On the control server it also says that the panel and the API stop with it and come back on their own, and that the other servers keep serving meanwhile (PRD principle 5).

### How it's confirmed

- **The plan is as good as root.** The host driver's plan carries host access `root` for both edits, so `Plan.AsGoodAsRoot()` holds. `ApplyChange` then asks for the passkey, `STEP_UP_ACTION_APPLY_ROOT_CHANGE`, bound to the change's ID, which only a person signed in can give: a token, read-write or not, can't apply a plan for an install or a reboot, whoever planned it.
- **Only an admin.** `server.host` is an install-wide kind, so `checkRoles` and `CheckApply` ask for an admin to plan or apply it, as for a firewall rule.
- **A reboot of the control server is flagged as one that stops the panel,** as a plan that destroys data is, so the sheet asks to confirm it as well as the passkey.

### Holds

- **The server is held while the step applies, not while it's planned.** A plan can sit for days; the fingerprint catches one that went stale. As the step starts, the engine claims the host resource, and a step of another change for any resource on that server waits while it's claimed; the host's step waits for any step running there to end. A change for another server isn't held.
- **The agent keeps one durable mark for the server,** `<data>/agent/busy`, with the change's ID, written as the step starts and removed as it ends, whatever it came to. While it's there, the agent refuses another step ("web-1 is installing updates, change chg_…; this runs once it ends"), ADR 0054's reconcile does nothing, scheduled tasks (ADR 0053) due meanwhile wait and run once as it lifts, and the agent holds its own restart and self-update.
- **The step holds that server's alerts.** While it runs, `server-unreachable` and the service rules for that server are muted by a mute bound to the change, on Activity, and lifted as the step ends. If the step fails, `change-failed` says so.

### Applying, its progress, and what's on Activity

- **Downloading, safe to stop.** The step first fetches every package into the cache: `apt-get install --download-only …`, `dnf upgrade --downloadonly …`, or on Alpine `apk cache download` into a cache folder for the change under the agent's data folder (`--cache-dir`). It runs as ADR 0043 says, with a 30-minute deadline, and nothing on the server changes until it's done, so a dropped stream or a restart stops nothing half done.
- **Installing, in a unit of its own.** The agent then starts the install from that cache, with no network and no deadline, in a cgroup of its own: `systemd-run --unit=dockwell-install-<change ID> --collect`, or under OpenRC `start-stop-daemon --background` with a pidfile and `setsid`. Its environment is clean (`DEBIAN_FRONTEND=noninteractive`, `NEEDRESTART_MODE=l`), with `--no-download`, `--no-remove` and `--force-confold` for apt, `--cacheonly` for dnf, `--no-network` for apk. A dropped stream, a control-plane restart, an agent restart or crash, or the change's own deadline doesn't stop it: the cgroup is the unit's, not the agent's, and it also holds the children a maintainer script starts with `setsid()`, which a process group misses.
- **Its output** goes to `<data>/agent/install/<change ID>.log`. The agent follows the file and sends each line as the step's progress, which the sheet shows as it comes, and after an agent restart it finds the unit by its name and follows on. The file is deleted when the change ends, so ADR 0043's "nothing of the output is kept" still holds. The step passes when every package reached its version, and fails with the last lines and what it did install. Past 30 minutes it says it's still running, and goes on.
- **After Docker restarts,** the step waits up to 2 minutes for each service the plan said comes back to be healthy, as the health monitor reads it, and its outcome lists any that didn't, each linked, with **Restart** one step away. It starts nothing on its own.
- **Rebooting.** Before `ApplyCall` goes out, the engine records the step as started with the server's current boot ID, with `Revise`. The agent writes the step's ID and its boot ID to `<data>/agent/reboot`, its own file, not ADR 0054's journal, answers that it's rebooting, and once the answer is sent asks the init system to: `systemctl reboot`, or `reboot` under OpenRC. The step passes when the agent connects from another boot (`Hello.boot_id`, read from `/proc/sys/kernel/random/boot_id` with the reader #357 uses) and says the marker's step; then the step waits up to 5 minutes for each service the plan said comes back to be healthy, and lists what stayed down, as after Docker restarts. The time from the step's start to that hello is kept on the step, and the next reboot's plan says it.
- **On Activity and the server's timeline:** `change.planned`, then `change.applied` or `change.failed`, as any change, with the server, so who installed what and when stays on record. The server's timeline also says a reboot started, so its page says why it went offline, and how long it took to come back.

### Resuming each step

`Engine.Resume` runs at every start and every reconnect, and plans each unfinished item again. For these:

- **Download** runs again: it's safe to repeat.
- **Install.** If the agent still has the run (its unit, or its log once it ended), Resume reconnects to it by the change's ID and follows it. Otherwise the agent reads what's installed: packages at their planned versions count as done, and the rest go ahead only if their planned versions are still the candidates. The "changed since this plan" refusal applies at the step's first start, never to a resume, or every interrupted install would fail. When `dpkg --audit` reports work a stopped run left, the step runs `dpkg --configure -a` first. The same state found any other time shows on the Updates tab with **Finish the install**, a plan of its own.
- **Reboot, at most once.** An agent from another boot passes the step. An agent from the recorded boot whose marker names the step got the call, so Resume never sends it again, and waits. Only an agent from the recorded boot with no marker for the step never got it, and gets it once more.
- **The 15-minute limit has an owner.** Nothing in the engine gives up on a change that waits for a server: Resume returns `ErrPending` while it's offline. The engine's overdue sweep, every minute and as the control plane starts, fails a reboot step whose recorded start is more than 15 minutes old with no agent from another boot: "web-1 hasn't come back after its reboot. Check its console." Its time is the step's recorded start, so a control-plane restart doesn't reset it.

### Rebooting the control server

The panel, the API and the control server's own agent are one process ([ADR 0007](0007-one-binary-two-roles.md)), which the service manager starts at boot. The engine stops with it, so its reboot step first waits for every other applying change in the install to end, and holds new applies meanwhile. Then it records the step as started, with `Revise`, so the change is unfinished in the store. As the control plane starts again, `Engine.Resume` finds the step, its own agent connects from another boot, and the step passes. A panel that was open says the panel is restarting with web-1, and reconnects by itself; sessions are in the store, so nobody signs in again.

If the control server doesn't come back, nothing is left to say so but the person's own eyes. Its plan says that, and how to reach it: its console, or SSH.

### A set of servers

An edit takes a set of servers, as every action does (the PRD's "Scaling the experience", rule 4): one host edit for each, applied a server at a time in the plan's order, stopping at the first that fails. Waves and their stop condition come with Fleet actions. A reboot of a set takes the control server last, after the others have come back, as rebooting it first would leave nobody to watch the rest.

### Where this meets ADR 0054

- **The host resource is in the agent's desired-state cache** like any other, with an empty spec, and its reconcile has nothing to put back for it. Nothing there needs to know packages moved.
- **The busy mark is ADR 0054's `unfinished` guard for a whole server,** durable across an agent restart, which plans, applies and the reconcile all check. Main's mark is per step and in memory, so slice 1 doesn't depend on #312's cache.
- **A reboot completes through main's `Engine.Resume`,** with `Hello.boot_id` and the agent's reboot marker. From #357 it takes only the boot ID reader; the marker stays out of its journal, which the control plane turns into `server.reconciled_*` events.
- **After a reboot,** Docker starts what its restart policies say, and when the control plane isn't back yet, as when the control server rebooted itself, the agent's cache puts back what Docker can't after 60 seconds without a stream (ADR 0054, slice 2). The busy mark of a reboot lifts as the agent starts on another boot, so that reconcile isn't held.

### What this changes in ADR 0043

ADR 0043's "a change with a plan, which makes no revision" stays: the host edit is planned and applied as every change is, and makes no resource revision. Its reads run as it says. The install itself runs in a unit of its own after a download, as above, not in the read's process group.

### Packages one at a time, or a selection

Added on 2026-11-11 (#521), after the owner asked for an Update button on every package and every state of an update designed.

- **The request names packages, from the list.** `PlanInstallUpdatesRequest.packages` names the packages to install, as `GetServerUpdates` lists them, in place of `all`; both together are refused. The control plane checks each is a package's name (letters, digits and `+._~-`, an architecture after a colon), at most 1000, each once, and the change's summary names them ("Update openssl on web-1", or "Update 5 packages on web-1" past three).
- **The agent picks only from its own read.** It reads what waits as for the tab, and refuses a name that read doesn't list ("nginx isn't among the updates waiting on this server, so Dockwell won't install it"), whatever it's sent. It simulates exactly the packages named, pinned, so the plan lists them and whatever they need, upgraded or new, as apt finds it, and nothing else waiting. A package new to the server, such as the next kernel, installs on its own; named beside upgrades that don't pull it in, it's refused, as apt names upgrades with `--only-upgrade`, which would leave it out quietly.
- **The plan says what was asked.** `PackageInstall.asked` lists the names asked for, so the sheet puts them first and marks every other package as pulled in. Applying, resuming and the fingerprint work from the plan's packages, as before: the names matter only at planning.
- **An older agent is refused, not trusted.** The names reach the agent as `InstallUpdates.names`, which an agent from before this ignores, planning the security fixes instead. A new agent says the names back in `asked`; a plan for names that comes back without them is refused ("web-1's agent installs only the security fixes or every update, not packages picked one by one"), so nobody applies more than they picked.
- **The tab follows an install, whoever started it.** While an install's step runs on a server, `ServerUpdates.install_change_id` names its change, as the control plane's session with the agent sees it, and the panel follows the change with `WatchChange`. Each row the install takes says where it is: waiting, downloading, installing (with dpkg's line for that package as it comes), installed, then "Updated to" its version, after which it leaves the list. Other rows' Update is off while it runs, saying why, and so is Check now; an install applied anyway, as from the CLI, waits for the server as the holds above say.
- **What's read is current after an install.** No read starts while an install runs, as it would race dpkg, and a refresh would wait on its lock. Once one ends, the next look reads again, whatever the read's age, so the counts and the summary move without a reload, rather than after the 10 minutes ADR 0043 keeps a read.
- **A failure stays on its rows,** with dpkg's or apt's last line, Retry (the same request again) and the change one step away. More than three rows failing say it once, above the list.

### Methods and numbers

- `UpdateService.PlanInstallUpdates`, taking the servers and which updates (the security ones, or all), and `ServerService.PlanReboot`, taking the servers, each make the host edits and answer a planned change, applied with `ChangeService.ApplyChange`, as `RestartService` does. Both are `EFFECT_PLAN`, `needs_session`, `NEEDS_ADMIN`, `REACH_INSTALL`, with lines in the panel's `methods.ts`, and the CLI's `dockwell updates install` and `dockwell servers reboot`. `Edit` gains `install_updates` and `reboot` at its next free numbers.
- The agent protocol takes this area's block: `Hello.boot_id` 231, and `DesiredResource.install_updates` 232 and `DesiredResource.reboot` 233, which `PlanCall` and `ApplyCall` carry as they carry `restart`.
- Picking packages (2026-11-11) adds `PlanInstallUpdatesRequest.packages` 4, `PackageInstall.asked` 5, `ServerUpdates.install_change_id` 18, and `InstallUpdates.names` 4 in the agent protocol, a field of a message this area owns. The CLI's `dockwell updates install` takes `--packages`.

### PRD principle 2

The PRD names principle 2's exceptions one by one, so this adds installing a server's updates and rebooting it there: each is a change with a plan and no revision, as a restart is.

## Alternatives considered

- **(a) A second kind of item, `Proposal.Acts` beside `Edits`.** Every part of the engine is built around resources: `change_items`, resume, holds, progress, the passkey and role checks, the plan's proto, rollback, undo, the sheet, the CLI and Activity's placing would each need a branch. It's the bulk of the work for nothing the host resource doesn't give.
- **(b) The server's packages as desired state.** A desired version would drift every night as `unattended-upgrades` runs, a revision would hold versions the archive drops within weeks, and undoing one can't work.
- **(c) A host resource with flagged edits,** chosen: the existing items carry it, and later server settings have a home.

## Consequences

- Security updates install from the panel with one plan that says, before anything runs, what each package goes to, what it pulls in, what restarts, and whether a reboot follows; Activity keeps who did it.
- None of it can be undone, and the plan says so. A failed install leaves what it installed, as running the command by hand would, and an interrupted one is finished, not left half configured.
- The install outlives its call: a dropped stream, a restart of the control plane or the agent, or a deadline doesn't stop `dpkg` midway.
- A reboot is the first change whose step outlives the agent's process, and on the control server, the control plane's own. It leans on the store recording the step and the boot before the reboot, and on `Engine.Resume`.
- Slices: (1) the host resource, and installing on one server; (2) rebooting, with `Hello.boot_id`; (3) a set of servers, with Fleet actions.
