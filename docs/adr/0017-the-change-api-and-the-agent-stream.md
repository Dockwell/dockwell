# 0017. The change API, and the engine's calls over the agent stream

- Status: Accepted
- Date: 2026-11-04

## Context

[ADR 0009](0009-resources-revisions-and-plans.md) designed the engine: propose, plan, apply, roll back, undo, Resume and drift, for every kind. [ADR 0014](0014-object-model-and-store.md) put projects and environments on it and gave resources scopes. Both left the engine reachable only in process. Issue #20 asks for the rest:
- plans computed without side effects, and reproducible;
- apply streaming each step's progress to every surface;
- undo for every revision, as its own plan;
- secrets never in plans.

That needs an API in `dockwell.v1` that the panel (#24), the CLI (#22) and REST scripts share, following [ADR 0015](0015-rest-and-openapi-from-the-schema.md)'s paths and [ADR 0016](0016-scoped-api-tokens.md)'s effects. It also needs the engine's `resource.Agent` calls carried over the agent stream of [ADR 0007](0007-one-binary-two-roles.md), which so far carried only a hello and stats.

## Decision

### `ChangeService`

| Method | Effect | REST |
| --- | --- | --- |
| `ProposeChange` | plan | `POST /v1/changes` |
| `GetChange` | read | `GET /v1/changes/{id}` |
| `ListChanges` | read | `GET /v1/changes` |
| `ApplyChange` | apply | `POST /v1/changes/{id}:apply` |
| `WatchChange` | read, server stream | none |
| `UndoChange` | plan | `POST /v1/changes/{id}:undo` |
| `DiscardChange` | plan | `POST /v1/changes/{id}:discard` |
| `ListDrift` | read | `GET /v1/drift` |
| `RevertDrift` | plan | `POST /v1/drift:revert` |
| `AdoptDrift` | plan | `POST /v1/drift:adopt` |

**Apply returns once the change is committed; a watch follows the steps.**
- `ApplyChange` runs the checks of ADR 0009, commits the revisions, and returns the change as it starts applying. The steps run in the background, to an end, whether or not anyone waits.
- `WatchChange` sends the change as it stands, then each step's progress, then the change again whenever its state moves on. It ends once the change is applied, failed or discarded. Any number of clients can watch one change: the panel that applied it, the activity view, the CLI.
- A streaming apply was the other way. It would have given REST no apply at all, since streams have no REST path (ADR 0015), and a second client would still have needed a watch. With this split every surface applies the same way, and follows with the stream, or by polling `GetChange`, which carries the same progress.
- Each event carries the change's state and steps as the event left them. A client that reads the change later may find it further on; the events after say how. A failed change's last event comes once its rollback is on record, so it can name it in `rolled_back_by`.
- A watch hears only what is on record. A failure the store can't record isn't published: the change stays applying there, and Resume takes it up again.
- `ListChanges` gives each plan in brief: its fingerprint, counts, undo window and whether it is destructive. `GetChange` has it in full. A page reads what reverts its changes in one query.

**What changes when a plan can't be undone.** A plan whose steps destroy data or may cut off access is marked `destructive`. Applying one needs two things:
- the plan's fingerprint in the request, as the API's form of the PRD's typed confirmation: the client says which plan it reviewed;
- a signed-in session. ADR 0016 keeps every token off destructive methods; a method's effect is fixed in the schema, so `ApplyChange` stays `EFFECT_APPLY`, and the handler refuses a token for a destructive plan with `needs_session`.

Any client may send the fingerprint, and a change applies only if its plan has it.

**Discarding is a state.** A planned change can be discarded, and stays on record as discarded. The check on `changes.state` gains the value; SQLite rebuilds the table to change it (migration 0008).

**Edits carry specs in one message.** `dockwell.resource.v1alpha1.Spec` has a field for each kind's spec, so the schema, the OpenAPI document and the clients are typed, and a new kind is a new field. The edit's kind may be left out; the spec says it.

**Requests leave out what has no choice** (PRD principle 4, ADR 0014):
- An edit's scope may be an ID, a name, or empty. Empty means the organization for a project, the only project for an environment, the only environment for a service, and the only server for a host-level kind.
- Once there is a choice, an empty scope fails with reason `choice`, and the message names the options, for the CLI to ask which.
- An environment's name is unique only in its project, so an edit for a kind that lives in an environment, such as a service, can name the project beside it, in `project`, by name or ID. It needs to only once there are two projects; an environment's ID needs none. Other kinds have no use for it, and are refused one.
- A project and environments made in it can't come in one request yet, since the client can't name the new project's ID. A new project gets `production` in the same change, as ADR 0014 decided.

**Responses never carry a spec.** Plans show field changes, and a `(sensitive)` field shows that it changed, never its values, as `resource.Diff` already did. `resourcepb.Changes` blanks a sensitive field's values again on the way out, whatever made them. Drift shows the same diff. There is no method to read a resource's spec yet: one would need a way to mask secrets and still edit around them, and #22 and #24 can decide that together.

**Errors name a reason** under `Dockwell-Error`, as AuthService's do:
- `invalid` and `choice` for a request to fix;
- `not_found`;
- `plan_stale` and `plan_changed` for a plan to propose again;
- `not_planned`, `busy`, `cant_undo`, `no_drift`;
- `needs_confirmation` and `needs_session` for a destructive plan;
- `no_driver`, `agent_failed` and `agent_timeout` for a server's agent;
- `server_offline`;
- `stopping`, for an apply that arrives as the control plane stops.

A client that goes away is a cancelled call, whatever it waited on, never `agent_failed`. `ListDrift` lists each server it couldn't check, with one of these reasons and a message, and returns the others' drift all the same.

### Plans, as the plan sheet needs them

- **Each resource as a whole:** its action, its field changes from the latest revision, and where it runs before and after. The action comes from its steps on every server: a replace on any server makes it a replace; deleting it, from every server it runs on, a delete; a new resource that every step creates, a create; anything else that changes it, its spec or where it runs, an update. So a move that recreates a service on one server and takes it off another is a replace, not an update. A kind that runs on no server, such as a project, takes what its revision does.
- **The counts** of create, change, replace and destroy, by resource. A replace counts on its own, never as a change, as `PlanDiff` shows it.
- **Each server's steps**, with action, effect, downtime, data loss and warnings, and the server's name, so a removed server still has one.
- **Servers** that are offline or removed, the undo window, and whether the plan is destructive. An applied change has its undo deadline.
- **The fingerprint** covers what the plan does, and names a resource made by the change by its place in the change, not by the ID it was given, which differs every time. The same edits against the same state then give the same fingerprint.
- **Planning has no side effects.** Agents only observe to plan. Proposing records the planned change and an audit event, nothing else.

### Progress

- A step is queued, running, done, failed, skipped (a step before it failed), or waiting (a rollback waiting for its server). Each has a message for people, such as "Replacing container web on hz-1", or the step's error.
- The engine keeps a running change's progress in memory, and records it on the change (`changes.progress`) as each step finishes, in the transaction that records its deployment. A change read later shows how its steps went, and Resume carries progress over: steps done stay done, what remains is queued again, and a step no longer needed is done.
- **Commits take turns; planning and steps don't.** Applying claims the change's resources first, then plans against the agents with no lock held, then takes the commit lock to check again that nothing moved and to commit. The steps run outside it too. So a slow agent, which may take a minute to plan, holds up only the changes that touch its server, and changes to different resources don't wait for each other, as ADR 0009 says. Claims are let go on every path, failures included.
  - A change that touches a resource another change is still applying, or committing, is refused with `busy` until that one ends. The claim comes before planning, so such a change reads `busy`, not `plan_changed` because the other has moved the server. Base revisions catch most such pairs already, but not a change that makes no revision, such as reverting drift.
  - Resume skips a change that is already running in this process, or whose resources another holds. It runs again once that one lets go, and a rollback that ends waiting for a server that is connected by then asks for it too. So a server that reconnects while its rollback runs elsewhere isn't forgotten.
- **A rollback is planned and committed in one step,** with the plan it runs as its plan, and is never left planned. Its intent is the desired state before the failed change, so it isn't held to a fingerprint, and a server whose agent can't plan it is planned from its last known state. A step planned that way, as for an offline server, doesn't run: it waits for Resume to plan it against the server. A person's change still applies only as reviewed.

### Stopping

The control plane stops in an order that lets changes under way survive it:
1. The API stops, so no change starts.
2. The engine stops (`Engine.Stop`). It starts no change, Resume or step from then on, and lets the steps under way finish for up to 30 seconds. It then cancels them, and waits up to 10 seconds more for every run, Resume's included, to record where it stopped.
3. The agent listener closes, and this server's own agent stops, so the steps under way had their agents to the end.
4. The store closes.

A change the stop interrupts stays applying, with the step it was at queued again, and Resume finishes it on the next start, planning that step against its server to find it done or not. It isn't failed and isn't rolled back: a call that fails because of the stop, cancelled by it or lost as a stream closes, interrupts the change rather than failing its step. A rollback the stop interrupts waits the same way.

### The engine's calls over the agent stream

- **Messages.** The control plane sends a `Call` (plan, apply or observe) with an ID and a timeout, and the agent answers with a `CallResult` of the same ID, in any order. `CancelCall` stops a call whose caller went away. Specs travel encoded as their kind's message, so a kind needs nothing in the agent protocol, and fake drivers in tests use it as real ones will.
- **Deadlines.** A call's timeout is a duration, not a time, so a server whose clock is off still gets the time it was given. Without a deadline from its caller, planning and observing get a minute and applying ten.
- **Kinds.** An agent says in its hello which kinds it has drivers for. A plan for any other kind fails at planning, before reaching the agent: "the agent on hz-1 has no driver for service resources, so it can't run service web".
- **A stream that drops** fails the calls waiting on it at once, with `ErrLost`, saying the step may or may not have run. The engine treats that as a failed step, as ADR 0009 treats a server that goes offline: the change fails, its rollback commits at once and waits for the server, and Resume finishes it when the agent reconnects. The agent cancels its calls when its stream ends; the rollback plans again from what the server has. A call that never reached the agent, because its stream had ended, fails with `ErrNotConnected` instead: it didn't run, so a rollback's step waits for the server rather than failing.
- **A driver's failure names its server**, as `hz-1: the image doesn't exist`, so a step's error says where it failed.
- **Resume runs** on start and each time an agent connects, as ADR 0009 said, and again when a run that Resume skipped ends.
- **Renewing the certificate** moves the stream without failing a call:
  1. The agent stops taking calls and says so with `Draining`. A call that arrives after is refused as draining: it didn't run.
  2. The control plane answers with `Drained` and sends no more calls on that stream. Every call it sent before arrives first, so the agent answers each one.
  3. The agent lets the calls under way finish, for up to 15 minutes, still sending stats, then half-closes the stream and lets the control plane end it once it has read the last results.
  4. A call refused as draining, or sent while the agent drains, goes to the agent's next stream, within the call's own time. If the agent doesn't come back within 30 seconds of the old stream ending, the call fails with `ErrNotConnected`.

  When a stream ends, the agent refuses new calls first, then stops receiving, then cancels what still runs and waits for it, so no call starts while it waits.
- **Principle 5's cache of desired state** is #32. The agent tells `agent.Desired` about each step it applies and verifies; #32 implements it and reconciles from it.

### Audit and the timeline

Changes write `change.planned`, `change.applied`, `change.failed` and `change.discarded`. Each event names:
- the change, as its subject;
- the actor: whoever proposed or discarded it, or for the outcome, whoever applied it, which may be someone else;
- the surface it came from, in a new `surface` column: `panel` for a session, `api` for a token, `automatic` for Dockwell;
- the outcome, in its action.

A change also records who applied it, and from where. #21's timeline can read events and changes alone. The CLI and MCP surfaces get values of their own when #22 decides how a client says which it is.

### `dockwell run`

The control plane builds the engine with the kinds it knows: projects and environments, and the container, firewall rule and service types, which it checks and plans for offline servers. The agent in this build has no drivers, until M3 (#26, #27). So only projects and environments, which run on no server, apply end to end; a server-level kind fails at planning with `no_driver`.

## Consequences

- The panel's plan sheet, activity view and timeline (#24, #21), and the CLI's `apply` and `undo` (#22), build on one API with nothing UI-only. The CLI applies, then watches.
- REST clients can apply, undo and discard, and follow progress by polling. Pushed updates over REST wait for the server-sent events that ADR 0015 leaves to its own decision.
- A destructive plan can't be applied with a token. A script that needs one waits for a step-up flow, as ADR 0016 notes for every destructive method.
- A driver's errors reach plans, progress and responses as written, so drivers must keep secrets out of their messages. The fakes do, and #26 and #27's tests must check theirs.
- Each step writes one more column in the transaction it already had. A running change's progress lives in memory only while it runs.
- Revisions still hold specs, secrets included, as they must to apply them. Encrypting secrets at rest is the PRD's separate requirement.
