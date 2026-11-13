# 0082. Acting on a server's system services

- Status: Proposed
- Date: 2026-11-12

## Context

[ADR 0067](0067-a-servers-system-services-and-its-system-log.md) put a server's systemd units and OpenRC services on its page, read only, and left acting on them to a third slice. On 2026-11-12 the owner dogfooded it (#567): the tab can only be looked at. Restarting `nginx.service` after fixing its config, starting what failed at boot, or stopping something from starting at boot still means SSH.

What bears on it:

- **PRD principle 2.** Every change to managed state is a revision with a plan, and the PRD names each exception. A restart is one, as a change with a plan and no revision ([ADR 0027](0027-health-checks-rollouts-and-rollback.md)), and so are installing updates and rebooting, as edits of the server's host, `server.host` ([ADR 0064](0064-a-change-on-a-server-installing-updates-and-rebooting.md)). Ending a process is outside it: a process isn't managed state ([ADR 0041](0041-processes-on-a-server.md)).
- **The server's packages own its units.** A package installs its unit files, and apt and dnf enable a unit as they install it, by its preset. ADR 0064 kept packages out of desired state for the same reason: Dockwell would fight the server's own package manager every night.
- **Some units are how anyone reaches the server.** Stopping `sshd`, the network, Docker or Dockwell's own agent cuts off the person, every app, or Dockwell itself, and from the panel there's no way back.
- **A unit's run state moves on its own.** systemd restarts a unit by its `Restart=`, a socket starts its service on the next connection, a timer starts its unit on schedule.
- **The agent runs the server's tools** by absolute path, without a shell, with a deadline (ADR 0043), and ADR 0067's reader runs `systemctl` and `rc-status` on a runner of its own.

## Decision

### A change with a plan and no revision

Each action on a unit is a flagged edit of the server's `server.host` resource, as ADR 0064's install and reboot are: `Edit{Resource: host on web-1, Unit: {Name: "nginx.service", Action: restart}}`. It records a change and its plan with `Revise`, makes no revision, and goes through the engine as every change does: the plan sheet, the fingerprint, holds, progress, resume, the role checks, Activity and the change's own page.

- **Starting, stopping, restarting and reloading** aren't desired state: systemd moves them on its own, as above, so a desired "running" would drift on every crash and every socket activation.
- **Starting at boot is lasting state, and still not Dockwell's.** Enabling, disabling, masking and unmasking change the server's unit links, which its packages also write: a desired "disabled" would show drift, or be put back over the package manager, each time apt installed or upgraded a package that enables its unit. As with packages (ADR 0064), Dockwell does what's asked, says what it did, and keeps no desired set.
- **Undoing it plans the opposite.** The plan records how the unit stood: running or not, and its at-boot word, such as `enabled` or `disabled`. Undoing a change that started, stopped, enabled, disabled, masked or unmasked a unit plans the action that puts that back, as a change of its own with its own plan, passkey and typed code. A restart and a reload can't be undone, and the plan says so.
- **The PRD gains a fifth exception** to principle 2, beside installing updates and rebooting.

Why not an action without a plan, as ending a process is: what a unit takes with it isn't visible from its row. Stopping `docker.socket` stops Docker, stopping a unit stops every unit that `Requires=` it, and a stopped service with a listening socket starts again on the next connection. A plan says that before it happens, and the engine already holds servers, follows progress and records who did what.

### The actions

| Action | systemd | OpenRC |
| --- | --- | --- |
| Start | `systemctl start -- NAME` | `rc-service NAME start` |
| Stop | `systemctl stop -- NAME` | `rc-service NAME stop` |
| Restart | `systemctl restart -- NAME` | `rc-service NAME restart` |
| Reload | `systemctl reload -- NAME`, where `CanReload=yes` | not offered |
| Enable at boot | `systemctl enable -- NAME` | `rc-update add NAME default` |
| Disable at boot | `systemctl disable -- NAME` | `rc-update del NAME default` |
| Mask | `systemctl mask -- NAME` | not offered |
| Unmask | `systemctl unmask -- NAME` | not offered |

- **The request names a unit and an action, and a set of servers,** as every action takes (the PRD's "Scaling the experience", rule 4): one host edit for each server. `ChangeService.PlanSystemServiceAction` is `EFFECT_PLAN`, `NEEDS_ADMIN` for both roles, `REACH_INSTALL` and `needs_session`, as installing updates and rebooting are: an action on the server itself is a person's, and a token can't list units to act on (ADR 0067). The CLI, which signs in with a token, is refused and sends a person to the panel, as for a reboot.
- **The name is checked twice.** The control plane checks it against ADR 0067's pattern. The agent asks systemctl about that one unit, never acts on one whose `LoadState` is `not-found`, and acts on the unit's own name (`Id`), so an alias such as Debian's `sshd.service` for `ssh.service` meets the same guard as the unit it names. Under OpenRC the name must be a script in `/etc/init.d`.
- **What isn't offered is refused, with why:** starting a unit with `CanStart=no` or `RefuseManualStart=yes` ("It only starts when another unit needs it"), stopping one with `CanStop=no` or `RefuseManualStop=yes`, reloading one with `CanReload=no`, enabling a unit with no install section (`static`, `indirect`, `generated`, `transient`: "It starts when another unit needs it, so it has nothing to enable"), and starting a masked unit ("It's masked. Unmask it first"). The list carries what's needed to offer only what applies: `start_refused`, `stop_refused`, `can_reload` and the guard, below.

### What the plan says

The agent plans the edit from `systemctl show` on that unit, with only these properties: `Id`, `Names`, `LoadState`, `ActiveState`, `SubState`, `UnitFileState`, `CanStart`, `CanStop`, `CanReload`, `RefuseManualStart`, `RefuseManualStop`, `RequiredBy`, `BoundBy`, `ConsistsOf` and `TriggeredBy`, then `ActiveState` of the units those name. Never `Environment` or `Exec*`.

- **How it stands now:** running or not, and its at-boot word. An action that changes nothing plans nothing ("nginx.service is already running"), as an edit with no difference does.
- **What goes with it:** for a stop or a restart, each running unit that stops or restarts with it, read from `RequiredBy`, `BoundBy` and `ConsistsOf`, as systemd propagates a stop or restart to those.
- **What starts it again:** for a stop, each active unit in `TriggeredBy`, such as `ssh.socket` for `ssh.service`, which starts it on the next connection or on schedule.
- **What stops when Docker restarts:** restarting `docker.service`, `docker.socket` or `containerd.service` says so, and the plan sheet lists each app, database and website on the server and whether Docker starts each again, as for an install that restarts Docker (#527). It holds the server as that install does: no other change runs steps there meanwhile.
- **That it can't be undone,** for a restart or a reload.

The fingerprint covers the unit, the action, how it stood and what goes with it, so a plan made before the unit moved goes stale.

### What Dockwell needs is guarded

Every unit falls in one guard. The agent decides it from the unit's `Id` and its `Names`, refuses at planning, and the list carries the guard so the panel offers each action, or shows it off with the reason in one line (ADR 0059).

| Guard | Units | Refused | Passkey | Typed code |
| --- | --- | --- | --- | --- |
| Dockwell | `dockwell.service`, `dockwell-access.service`; OpenRC's `dockwell` | every action: "Dockwell runs as this service on web-1" | | |
| Docker | `docker.service`, `docker.socket`, `containerd.service`; `docker`, `containerd` | stop, disable, mask: "Every app on web-1 runs in Docker" | restart, reload | restart |
| SSH | `ssh.service`, `sshd.service`, `ssh.socket`; `sshd` | stop, disable, mask: "It's how you reach web-1 over SSH" | restart, reload | |
| Network | `systemd-networkd.service`, `NetworkManager.service`, `networking.service`, `systemd-resolved.service`; `networking` | stop, disable, mask: "web-1 reaches the network through it" | restart, reload | restart |
| Firewall | `nftables.service`, `ufw.service`, `firewalld.service`, `netfilter-persistent.service`, `iptables.service`, `ip6tables.service`; `iptables`, `ip6tables` | | stop, disable, mask, restart, reload | stop, disable, mask |
| Others | everything else | | | |

- **A passkey** makes the plan as good as root (`Plan.AsGoodAsRoot`), so `ApplyChange` asks for `STEP_UP_ACTION_APPLY_ROOT_CHANGE`, bound to the change, which ADR 0075's window can stand in for. No new step-up action is needed. Starting, stopping or restarting any other unit takes no passkey: it runs what root installed, as root configured it, and grants nothing new, as restarting an app takes none.
- **The typed code** is ADR 0028's red sheet: for what stops everything on the server (Docker restarting), may cut the agent off (the network restarting), or opens every port the firewall closes (stopping or disabling it, as Debian's `nftables.service` flushes the ruleset when it stops). The plan flags it, and the panel's `needsConfirming` follows the flag.
- **SSH restarts only with a config that passes.** Before restarting or reloading an SSH unit, the agent runs `sshd -t`, as ADR 0052 does before it changes SSH, and refuses with sshd's own line when it fails ("sshd's config doesn't pass its own check, so restarting it would leave web-1 without SSH: …"). Sessions already open stay open across a restart.
- **The network restarts without waiting.** The agent runs `systemctl restart --no-block`, so its answer is sent before the network moves, and the step passes once systemd has the job. The list says how it came back.
- **What isn't detected:** a store in Postgres run by a unit on the control server, a VPN unit a person reaches the server through. These are "Others", and the plan names what goes with them.

### Applying, and after

- **It runs on the reader's own runner,** not the agent's shared one, with a deadline of 2 minutes: systemd's own start timeout is 90 s by default, and `systemctl` waits for the job.
- **The step checks what came of it.** After a start or a restart, the agent reads the unit again: a unit left `failed`, or not running, fails the step with the last lines its own process wrote to the journal (below), so the change says why without opening anything. A stop that left it running, or an enable that left it disabled, fails likewise.
- **Resume runs it again.** Each action stands as asked once done, so a resume plans nothing for one that finished: started is started. A restart or a reload interrupted after the call went out runs once more, which is harmless, as an app's restart is.
- **The list reads again at once.** As a unit's step ends, passed or failed, the control plane reads that server's system services again for every watch open (`Engine.OnUnit`), so the row shows how the unit came out within a second, not at the next 10 s read.
- **On Activity** as every change: `change.planned`, then `change.applied` or `change.failed`, with who planned and applied it, on the server's timeline, under the change's summary, such as "Restart nginx.service on web-1".

### Why a unit failed, and its journal

- **A failed unit says why in its row:** the last lines its own process wrote before it failed, not systemd's lines about it ("Main process exited, code=exited, status=1/FAILURE"), which only repeat its result. The control plane reads them once for each failure, by the unit's name and the time it failed, and keeps them while it stands, so a list read every 10 s asks the journal nothing more. At most ten failed units are read for a list; the rest show their result alone.
- **A unit's journal is one click away,** in its sheet: its last 50 lines, newest last, with the levels ADR 0067 gives (**Errors · Warnings and up · Everything**), more on scrolling back, read again every 5 s while the sheet is open. It's ADR 0067's `ServerService.ReadSystemLog`, built here narrowed to a unit: `journalctl -o json --system --unit=NAME --reverse` with only ADR 0067's fields, paged by `__CURSOR`, at most 200 lines a page. Following the whole system log, its page and its search are ADR 0067's slice 2, still to come.
- **Reading it goes on record** as ADR 0067 decided: `logs.read`, with the actor and the server's system log as its subject, at most once an hour for one reader and one server, never what was asked for. A reading that can't be recorded isn't served. The list's "why" lines are part of reading the list, which isn't recorded (ADR 0067); they're a unit's own last words, at most three lines each.
- **Without a journal,** as on Alpine with busybox's `syslogd`, it reads the end of `/var/log/messages` (its last 1 MiB), the lines whose program tag is the service's name.

### The panel

Following the interaction principles in design/system/README.md: the row acts, the sheet holds the rest, nothing is typed.

- **Each row has its one likely action:** Restart for a running or failed unit, Start for a stopped one, Unmask for a masked one, shown on the row's hover or focus, and always for a failed unit or one under way. A guard that refuses it shows it off, with the reason as its tooltip and accessible description. While the action plans, applies and the list reads again, the row says so ("Restarting…") and its other actions wait; the action's failure stays on the row with its journal line, Retry, and the change one step away.
- **The unit's sheet** says how it stands, then its actions (Start or Restart, Stop, Reload where it can, and **Start at boot** or **Don't start at boot**, as each is a plan rather than a setting that flips), its facts, its journal, and Mask or Unmask under **More**. A failed unit leads with why and Restart.
- **Every action opens the plan sheet,** as restarting an app does, so what goes with it, the passkey and the typed code come only when they apply.
- **On a phone,** the row opens the sheet, which holds every action.

### The API and numbers

- `ChangeService.PlanSystemServiceAction` with `PlanSystemServiceActionRequest` (`server_ids`, `name`, `action`, `summary`) and its response; `SystemServiceAction`, an enum; `HostUnit`, what the plan says of a unit action, on `ResourcePlan.unit` 70 and `PlanStep.unit` 70.
- `ServerService.ReadSystemLog` with `ReadSystemLogRequest` and `ReadSystemLogResponse` and `SystemLogLine`, as ADR 0067 named it.
- `SystemServiceInfo` gains `guard` 70, `why` 71, `start_refused` 72, `stop_refused` 73 and `can_reload` 74, and `SystemServiceList` gains `acts` 70, false through an older agent; `SystemServiceGuard` is an enum.
- The agent protocol takes Track S's block: `DesiredResource.unit` 280 (`UnitAction`), `Call.system_log` and its result 281, and `Hello.acts_on_units` 282, without which the control plane refuses to plan a unit action ("web-1's agent is older than the control plane"), as an older agent would ignore the edit and plan nothing. ADR 0067's 236 for the system log stays free.
- The engine's `Edit.Unit`, `ResourcePlan.Unit` and the host driver's `UnitActor` carry it, as `Reboot` does; `item.noRevision` covers it, and the plan JSON keeps it, so no migration is needed.

### The agent build

The actions and the journal read live in `internal/agent/system` and `internal/resource/host`, both already linked by the agent build (ADR 0023); nothing new is listed in `build_test.go`. Nothing runs while nobody acts or reads: no process, no buffer.

## Alternatives considered

- **(a) Starting at boot as desired state** on the `server.host` spec, put back as ADR 0054's drift would. It gives a revision and drift for the units a person changed, and fights the package manager each time it enables a unit it installs; undoing it by the opposite action gives the same one-step way back without that.
- **(b) An audited action without a plan,** as ending a process (ADR 0041): one click, but nothing says beforehand what stops with the unit or starts it again, and holds, progress and resume would be built a second time beside the engine.
- **(c) A resource for each unit,** `server.unit` named by the unit: actions on two units of one server could run at once, but each unit would need a revision to exist before its first action, which (a) is.
- **(d) A host edit with a plan and no revision,** chosen.

## Consequences

- A unit is started, stopped, restarted, reloaded, enabled, disabled, masked and unmasked from the panel, with a plan that says what goes with it first, and Activity keeps who did it.
- What Dockwell needs to reach the server and run its apps can't be stopped or disabled from the panel, and what may cut access asks for a passkey, and for the typed code where it stops everything.
- Two actions on one server's units run one after the other: each is an edit of the one host, which a second change waits for. The panel says the next waits.
- A failed unit says why in its row, and its journal is in its sheet.
- Following the whole system log, its page and its search remain ADR 0067's slice 2.
