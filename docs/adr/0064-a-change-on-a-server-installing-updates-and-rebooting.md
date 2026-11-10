# 0064. A change on a server: installing updates and rebooting

- Status: Proposed
- Date: 2026-11-10

## Context

[ADR 0043](0043-package-updates-on-a-server.md) decided how a server's package updates are read, and that installing them and rebooting are each a change with a plan, applied by a person with a passkey. Only the read was built. The Updates tab says "Dockwell can't install them yet" and offers the command to copy (#460). Issue #426's audit ranks this sixth: a procedure a panel should be a button for, with what it will restart said first.

What bears on it:

- **Every change goes through the revision store.** PRD principle 2 says every change to managed state is a revision with a plan, and only `internal/revision` calls `store.DB.Revise` ([ADR 0009](0009-resources-revisions-and-plans.md)); a test fails on anything else that does. `Revise` writes changes, revisions and deployments.
- **The engine plans resources.** A change is a list of edits, each a resource with a spec. A restart and a restore are edits too, of a resource as it is, flagged `Restart` or `Restore`: they record a change and its plan and make no new revision, as no spec changed, and there's nothing to undo ([ADR 0027](0027-health-checks-rollouts-and-rollback.md), [ADR 0033](0033-backups-of-volumes-and-databases.md)).
- **A server's packages are no resource.** Dockwell holds no desired version of them, can't see them drift, and can't undo an upgrade (ADR 0043). A reboot has no resource either. Neither fits an edit.
- **The firewall did fit.** [ADR 0052](0052-a-servers-firewall-ssh-and-fail2ban.md) made its rules and policy resources (`firewall.rule`, `firewall.policy`), so they ride the existing model, and nothing here changes them. Its boot unit loads the last confirmed table at boot.
- **The agent keeps its desired state.** ADR 0054, on its own branch as this is written (#312, and #357, which is changing the engine's agent side now), has the agent keep what each resource should run, put back what goes missing while the control plane is away, mark a resource `unfinished` while a step acts on it, and the control plane resume a change left half done when it comes back. A reboot is where all of that matters at once.

## Decision

### An act: an item of a change that is a server, not a resource

- **`Proposal` gains `Acts` beside `Edits`.** An act names a server and what to do there: `install_updates`, with each package and the version it goes from and to, or `reboot`. A change holds edits or acts, never both: a plan says one kind of thing, so a deploy that fails doesn't leave a kernel half installed beside it.
- **It goes through the revision store like everything else.** Planning records the change and its plan with `Revise`, applying records each step's start and end and the change's end with `Revise`, and nothing else writes it. It is held, fingerprinted, resumed and listed exactly as a change of resources is.
- **It makes no resource revision,** as a restart makes none: there is no spec to keep. So it has nothing to undo, and the change sheet offers no undo, as for a restart. Its plan says so, and that a failed install leaves what it installed.

### What the plan says

The control plane plans an act by asking the server's agent, as a step's plan does, and the agent reads what waits now (ADR 0043's read: its 10-minute answer, or fresh if that's older than the plan's own read). An install's plan lists:

- **Each package,** from the version installed to the one it gets, where that comes from (the suite or repository), and whether it's a security fix. The panel offers the security updates (the default) or every update waiting, from the agent's list; nothing is typed, and the agent refuses a name its tool didn't list.
- **What it needs after,** by ADR 0043's rule: a reboot for a new kernel, libc, D-Bus, the CPU's microcode, or systemd under dnf. The plan says the server will need one and that the reboot is a change of its own; an install never adds one.
- **What stops.** When Docker's engine is among the packages, its restart stops every container on the server: the plan lists each service and website there, whether Docker starts it again by its restart policy, or not (a policy of `no`, or a container someone stopped), and how long it was down at its last restart. With `live-restore` on, it says nothing stops. Nothing else is claimed: which programs keep an old library until they restart can't be known before installing (ADR 0043).

The plan's fingerprint covers the server, each package's name and both versions, and whether Docker restarts. As the step starts, the agent reads again, and refuses if any of it moved: "The updates waiting on web-1 changed since this plan. Plan again." So a person never applies a list they didn't see.

A reboot's plan lists everything on the server as going offline, for about as long as its last boot took (the time from the boot to its agent's first hello, which the control plane keeps for each server), with each service and whether it comes back by itself: by Docker's restart policy, by the agent's own reconcile when the control plane is away (ADR 0054), or not. It says the firewall's confirmed table loads at boot (ADR 0052). On the control server it also says that the panel and the API stop with it and come back on their own, and that the other servers keep serving meanwhile (PRD principle 5).

### How it's confirmed

- **Only a person applies it,** signed in to the panel or the CLI with a session (`needs_session`), never an API token, and only an admin: an act reaches a whole server, and runs as root.
- **Applying asks for the passkey,** `STEP_UP_ACTION_APPLY_ROOT_CHANGE`, bound to the change's ID, as ADR 0043 says: a package's maintainer scripts run as root, and a reboot takes everything on the server down.
- **A reboot of the control server is flagged as one that stops the panel,** as a plan that destroys data is, so the sheet asks to confirm it as well as the passkey.

### Holds

- **An act holds its server.** While it's planned and applying, no step of another change runs on that server, and its own step waits for any that is running. An install that restarts Docker under a deploy would leave both half done. A change for another server isn't held.
- **The agent marks every resource on the server `unfinished`** (ADR 0054) for the act's step, so neither the agent's reconcile, if the stream drops while Docker restarts, nor a step that arrives meanwhile acts on containers the package manager is moving. The marks go as the step ends, whatever it came to.

### Applying, its progress, and what's on Activity

- **Installing.** The control plane sends the agent `InstallUpdatesCall` with each name and version. The agent runs the tool as ADR 0043 says (its absolute path, no shell, a clean environment, `--no-remove` and `--force-confold` for apt, 30 minutes at most) and sends each line it prints as the step's progress, which the change sheet shows as it comes. It answers with what's installed now, which packages need a reboot, and whether Docker restarted. The step passes when every package reached its version, and fails with the last lines printed and what it did install. Nothing of the output is kept once the change ends.
- **After Docker restarts,** the step waits up to 2 minutes for each service the plan said comes back to be healthy, as the health monitor reads it, and its outcome lists any that didn't, each linked. It starts nothing on its own: what to do about a service that stayed down is the person's, through a plan.
- **Rebooting.** The control plane sends `RebootCall`. The agent writes the step's ID and the boot it's on (`/proc/sys/kernel/random/boot_id`) to its journal, answers that it's rebooting, and once the answer is sent asks the init system to reboot: `systemctl reboot`, or `reboot` under OpenRC. The step passes when the server's agent connects again from another boot (`Hello.boot_id`), and the agent's journal, read as it connects, says which step that was, so an answer lost to the reboot doesn't leave the step waiting. Past 15 minutes the step fails: "web-1 hasn't come back after its reboot. Check its console."
- **On Activity and the server's timeline:** `change.planned`, then `change.applied` or `change.failed`, as any change, with the server, so who installed what and when stays on record. The server's timeline also says a reboot started, so its page says why it went offline, and how long it took to come back.

### Rebooting the control server

The panel, the API and the control server's own agent are one process ([ADR 0007](0007-one-binary-two-roles.md)), which the service manager starts at boot. Before the control plane asks its own agent to reboot, the engine records the step as started, with `Revise`, so the change is unfinished in the store. As the control plane starts again it resumes unfinished changes, as it does now (ADR 0054): the step reads the boot its own agent connects from, and passes. A panel that was open says the panel is restarting with web-1, and reconnects by itself; sessions are in the store, so nobody signs in again.

If the control server doesn't come back, nothing is left to say so but the person's own eyes. Its plan says that, and how to reach it: its console, or SSH.

### A set of servers

An act takes a set of servers, as every action does (PRD principle 4): one act for each, applied a server at a time in the plan's order, stopping at the first that fails. Waves and their stop condition come with Fleet actions. A reboot of a set takes the control server last, after the others have come back, as rebooting it first would leave nobody to watch the rest.

### Where this meets ADR 0054

- **An act writes no record in the agent's desired-state cache,** as it changes no resource. Nothing there needs to know packages moved.
- **It takes the `unfinished` mark** for every resource on its server, as above, which is ADR 0054's own guard against a reconcile racing a step.
- **A reboot's step completes through the resume path #357 is reworking:** an agent that connects says what it finished, and the control plane resumes the change. This adds a kind of step to that path rather than a path of its own, so the reboot slice waits for #357 to land.
- **After a reboot,** Docker starts what its restart policies say, and when the control plane isn't back yet, as when the control server rebooted itself, the agent's cache puts back what Docker can't after 60 seconds without a stream (ADR 0054, slice 2).

### What this changes in ADR 0043

ADR 0043's "a change with a plan, which makes no revision" stays: such a change is recorded by the revision store, planned and applied as every change is, and makes no resource revision. Everything else it says about how the tool runs, what's read, and who may act holds as it is.

### Methods and numbers

- `UpdateService.PlanInstallUpdates`, taking the servers and which updates (the security ones, or all), and `ServerService.PlanReboot`, taking the servers, each answer a planned change, applied with `ChangeService.ApplyChange`. Both are `EFFECT_APPLY`, `needs_session`, admin, `REACH_INSTALL`, with lines in the panel's `methods.ts`, and the CLI's `dockwell updates install` and `dockwell servers reboot`.
- The agent protocol takes this area's block, 230 to 239: `Call.install_updates` and `CallResult.install_updates` 232, `Call.reboot` and `CallResult.reboot` 233, and `Hello.boot_id` 231 (230 is `pages_listings`, #459).

## Consequences

- Security updates install from the panel with one plan that says, before anything runs, what each package goes to, what restarts, and whether a reboot follows; Activity keeps who did it.
- None of it can be undone, and the plan says so. A failed install leaves what it installed, as running the command by hand would.
- The engine gains a second kind of item, which planning, holding, applying and resuming each have to know: that is the bulk of the work, and its tests, on SQLite and Postgres.
- A reboot is the first change whose step outlives the agent's process, and on the control server, the control plane's own. It leans on the store recording the step before the reboot and on #357's resume.
- Slices: (1) installing, on one server, once this is accepted; (2) rebooting, after #357 lands; (3) a set of servers, with Fleet actions.
