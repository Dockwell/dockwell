# 0094. Fleet actions

- Status: Accepted
- Date: 2026-11-14

## Context

The PRD's fleet row (P1) asks for fleet actions across a tag selector in canary waves: OS patches, and Docker and agent upgrades. Its UX principle 4 says every action targets a set, and one server is a set of one: the plan sheet stays the same, and waves and a stop condition show only when the set has more than one member. The mocks draw the flow:

- `Servers-List.dc.html` has checkboxes on the table and a "Run fleet action" bar for what's picked.
- `Fleet-Action.dc.html` has a target, an action and a rollout, then Review plan.
- `Fleet-Action-Running.dc.html` follows the waves as they run.

[ADR 0091](0091-server-labels.md) gave servers labels and one selector parser for this.

What exists already:

- `PlanInstallUpdates`, `PlanReboot` and `PlanSystemServiceAction` each take several servers and make one change, with a host edit on each server ([ADR 0064](0064-a-change-on-a-server-installing-updates-and-rebooting.md), [ADR 0082](0082-acting-on-a-servers-system-services.md)).
- The engine runs a change's steps as one queue, one step at a time, and a step that fails skips the rest.
- A reboot's step asks the agent, then waits for it to connect from another boot, failing after 15 minutes.
- Nothing upgrades an agent. The Updates tab gives a command to copy: the install script, which upgrades in place when run again and checks the release with a pinned cosign ([ADR 0010](0010-install-script-verifies-with-pinned-cosign.md)).

## Decision

### A fleet action is a change over a set

- **Each action stays the method it is,** and takes a set: `server_ids`, or a `selector` (ADR 0091), which the plan resolves to servers once, at planning. The change, its per-server plan, its passkey and its undo are what each action has today. Nothing new runs outside a change.
- **The first three actions:** apply OS updates (`PlanInstallUpdates`), restart, reload, start or stop a system service (`PlanSystemServiceAction`), and upgrade the agent (`PlanAgentUpgrade`, new). `PlanReboot` takes a set and a rollout too, as restarting servers is the same shape.
- **A server that's offline at planning isn't in the change.** Its agent can't plan its step, and the plan names it in a note ("hz-3 is offline, so it isn't in this change. Run it again once it's back"), rather than holding the change for it.

### A rollout: waves, a pause, and when to stop

- **`FleetRollout`** says how the change runs over its servers:
  - **waves:** a canary of one server first, on by default, then waves of `batch_size` servers, which defaults to a quarter of the set rounded up;
  - **a pause** between waves, which defaults to none;
  - **`keep_going`**, off by default: a step that fails stops the change there, and no server in a later wave is touched.
- **The plan holds the waves,** as server IDs in order: the canary is the first server by name, then the rest by name. The plan sheet shows them, and the fingerprint covers them, so what's applied is the order that was reviewed.
- **The engine runs the queue by wave.** Wave 2's steps start only once each step of wave 1 has passed and the pause is over.
  - While it pauses, the change is applying and says until when. A timer resumes it, and a control plane that restarts mid-pause resumes it at the time recorded.
  - The pause counts from the latest end of any wave before, by the plan's numbering of the waves and the steps' record, never the run's: a run that resumes partway, as after a restart or a server that came back, keeps it, and so does a wave a resume finds done after a later one. A step a resume finds done ends then.
  - A resume runs only the steps left in the reviewed waves: none that ended, and none on a server that took no step when the plan was reviewed, though it would now.
  - Within a wave, servers go one after another in this decision's first slice, as every change's steps go today. The second slice runs a wave's servers side by side, up to its batch, each server's own steps still in order. That's the engine's one real change: the queue runs one step at a time now, and a host step that waits (a reboot, an upgrade) suspends the run.
- **On a failure with `keep_going` off,** the change fails with the step that failed, and every step after it is skipped. What passed stays: an install or an upgrade can't be undone, and a unit action can (ADR 0082). The change's page says which servers passed, which failed and which weren't touched.
- **With `keep_going` on,** a failed step is recorded and the next server goes on. The change still ends failed, listing every server that failed.
  - It counts every failure on the change's record, from every run of it, as a resume runs only what's left.
  - A server that's gone before its turn is a failure like any other, and the change waits for no server whose steps have all ended.
  - A server that doesn't come back within the limit fails the change, and the steps that never began are skipped, as after any failure. A server asked beside it that isn't back yet ends failed too, as nothing reads its step once the change has ended.
- **A set of one** has no rollout, and its plan says nothing of waves, as today.

### Upgrading an agent

- **To the control plane's version, and no other,** as an agent works best with the control plane it was built with. A server whose agent runs it already takes no step, and the plan says so. The control server is never in an agent upgrade: its agent is the control plane, which upgrades by running the install script on it, as now.
- **An agent from before fleet upgrades can't upgrade itself,** as it doesn't run the script a change carries. It's left out of the change, which says so, and the install script on its server upgrades it once; later upgrades come from the panel. An agent that runs the version already, or a newer one, takes no step.
- **A build that isn't a release can't be upgraded to.** `PlanAgentUpgrade` refuses it, saying the control plane runs a development build, and the panel doesn't offer the action.
- **The script is the control plane's own.** The full build embeds `scripts/install.sh` from its own source, which the release ships unchanged. The plan carries that script to each agent, with the version and the mirrors the control plane uses, of the release and of cosign. The install script keeps both in the service's environment, so each upgrade passes them on. The script then checks the release with its pinned cosign, as an install does.
  - Nothing fetches a script from the network.
  - The agent build carries no verifier of its own, and so gains no memory.
- **The agent runs it outside its own service,** since the script restarts that service:
  - `systemd-run --collect` as a transient unit under systemd, or a new session under OpenRC;
  - its output goes to a file in the agent's data directory;
  - a mark there holds the change and the version asked, as a reboot's does.
- **The step passes once the agent connects at the version asked,** as a reboot's passes once its agent connects from another boot.
  - A script that ends with the agent at its old version fails the step, with the script's last lines, which the agent keeps beside its status until the step reads them.
  - An agent that's away for 15 minutes fails the step too, as `RebootLimit` does.
  - One upgrade runs at a time on a server: another change's is refused while an earlier script hasn't left its status, unless that one can't be running: it started on an earlier boot, as its mark records, at a time still to come, as after a clock set back, or more than 30 minutes ago, as a script killed before it ended never leaves a status. Under systemd, the transient unit's name holds the same line.
- **It's as good as root,** so applying takes a person's passkey and no token, as installing updates does. An upgrade can't be undone, and the plan says so.

### Picking servers in the panel

- **Servers** gains checkboxes on its table, and a bar for what's picked ("3 selected", Clear, Run fleet action), as Servers-List has them.
- **Fleet actions** (`/fleet-actions`, in the rail today with nothing behind it) takes the New fleet action screen, as Fleet-Action draws it:
  - **Target:** the selector, or the servers picked on Servers, with the matching tiles and how many match.
  - **Action:** cards for the three actions. Each says what it would do to this set, such as "All 38 already run 0.9.4".
  - **Rollout:** waves as segments ("1 server · 10 · 10 · Rest"), the batch size, the pause, and Stop on the first failure.
  - **A summary,** then Review plan, which opens the plan sheet with its waves.
- **A running action** is its change. The change's page groups its steps by wave and shows each server as a tile, as Fleet-Action-Running does. Pausing a rollout by hand, and stopping one partway, come later.

### Numbers

- **Change API:** this area takes 150 to 159.
  - `FleetRollout`, `PlanRollout`, `RolloutWave`, `PlanAgentUpgradeRequest` and `HostAgentUpgrade` are new messages, numbered from 1.
  - `selector` 150 and `rollout` 151 on `PlanInstallUpdatesRequest`, `PlanSystemServiceActionRequest` and `PlanRebootRequest`.
  - `Plan.rollout` 150, `StepProgress.wave` 150, and `agent_upgrade` 150 on `PlannedResource` and `PlanStep`.
- **Agent protocol:** this area takes 350 to 359.
  - `Hello.upgrades_itself` 350.
  - `DesiredResource.upgrade_agent` 351, an `UpgradeAgent`: the change, the version, the script's SHA-256, and, on an apply alone, the script and the mirror.

## Alternatives considered

- **One `PlanFleetAction` method with an action in a oneof:** a second way to plan each action, which the PRD's "one server is a set of one" rules out. The actions already take several servers.
- **The agent verifies a release with its own Sigstore code,** as ADR 0010's option 4: no script, and a smaller dependency than cosign, but sigstore-go in every agent, past the agent's memory budget. It may come later. Then the step's shape stays, and only what the agent runs changes.
- **The agent fetches `install.sh` for the version from the release:** the same script, from the network, trusted as `curl | sh` is. The control plane's own copy is the same bytes, without the fetch.
- **Waves by percentage, as the mock's 1 → 10% → 50% → rest:** a fixed shape that a batch size covers more plainly. The panel can still offer such presets as batch sizes.
- **Holding the change for offline servers until they're back:** it would leave a change applying for as long as a server is away, and hold every other change there meanwhile.

## Consequences

- Every host action already planned for one server (an install, a reboot, a unit action) takes a set and a rollout, and the CLI's `--selector` comes with each.
- The engine learns waves, which the second slice extends to a wave's servers running side by side. That step makes the queue concurrent, and gets its own review.
- Stopping on unhealthy apps or an error rate, draining before a reboot, upgrading Docker, rotating SSH keys and running a command are left for later. Each is an action or a stop condition on this rollout.
