# 0009. One resource model, with revisions and plans for every kind

- Status: Accepted
- Date: 2026-11-04

## Context

PRD principle 2 says every change is a revision: clicks, CLI calls and pushes all make a versioned revision with a diff, previewed as a plan, applied, and reversible in one action. That covers firewall rules, Caddy config and env vars, not only deploys. The plan sheet in the mocks (`design/mocks/Plan-Review.dc.html`) shows:
- each resource with an action (create, update, replace, delete) and its changed fields;
- the servers affected and the expected downtime;
- when to apply;
- an undo that lasts 7 days.

Principle 5 says agents hold their own desired state and keep serving without the control plane, and that local changes come back as drift.

Features in M2 and later (apps, websites, databases, ingress, firewall, cron jobs, packages) all need this. If each one builds its own history and preview, the product gets several half-safe paths instead of one safe one. Issue #9 asked for a prototype that drives two unlike kinds through one engine first. This record is the design that prototype proved: `internal/resource`, `internal/revision`, and the container and firewall-rule kinds under `internal/resource/`.

## Decision

**A resource is a kind, a place, a name and a spec.**
- A kind is a name such as `container` or `firewall.rule`, and each kind has a protobuf spec message.
- The place is the server the resource lives on. Project and environment scopes come with services in M2, on the same model.
- The spec is the desired state. It holds intent only, never what the server reports back, such as a container ID or start time.
- Specs are Protobuf, like the API (ADR 0003). The panel, the CLI and the MCP server then see one typed schema, and diffs work on any kind without code for each kind.
- While the model settles, specs live in `dockwell.resource.v1alpha1`, which the breaking-change check exempts.

**A revision is an immutable snapshot of one resource's spec.** Revisions are numbered from 1 for each resource, and a deletion is a revision too, with no spec. History is never rewritten: undoing or rolling back writes a new revision that holds an older spec.

**A change groups the revisions one action makes.** Every write goes through one path:
1. *Propose:* the new specs for some resources, each with the revision it starts from.
2. *Plan:* a preview of what applying would do on the servers.
3. *Apply:* carry it out.

Applying commits the revisions and their change together in one transaction, so history only ever records changes that were approved. A change records:
- who made it and why;
- the plan as approved;
- the change it undoes or rolls back, if any;
- its state: planned, applying, applied or failed.

**Each kind has a driver, and drivers run where the resource is.** The control plane validates specs and diffs one desired spec against another. A driver on the agent compares the desired spec with what it observes on the server, and decides the action for each step:
- *create*, *update in place*, *replace* (such as recreating a container) or *delete*;
- its expected downtime;
- whether it destroys data;
- a word for the panel, such as "Recreate" or "Reload".

A driver has five methods:
- `Validate` normalizes a spec, so equal intents compare equal.
- `Observe` returns the actual state, projected onto the spec.
- `Plan` turns a diff into a step.
- `Apply` carries out a step.
- `Verify` confirms the result is healthy.

The agent reaches its drivers through an `Agent` interface. The prototype calls it in process, and M2 carries it over the agent stream (#11).

**A plan answers what will happen, where, and for how long.** It lists every step, grouped by server:
- the action, the field-level changes, the downtime and any data loss.

Field changes are found by walking the protobuf messages. A field marked `(sensitive)` shows that it changed, but never its value, so secrets stay out of plans, logs and history (PRD, non-functional requirements).

The plan also sums the downtime for each server, as steps on one server run in order. It notes any server that is offline, because its part of the plan is based on the state it last reported.

The plan carries a fingerprint, a hash of its steps.

**Apply checks that nothing moved, then commits, then acts.**
1. Apply refuses if:
   - any resource has a newer revision than the change started from;
   - a fresh plan against the servers no longer matches the approved fingerprint.

   In both cases the fix is to review the plan again.
2. It commits the revisions, so the desired state is on record before anything runs. If the control plane stops mid-apply, agents reconcile toward that state rather than toward a half-applied one.
3. It runs the steps, server by server, in a fixed order. Creates, updates and replaces go in proposal order, then deletes in reverse.
4. It verifies each step after applying it.

If a step fails, apply stops. It then rolls back the steps it already applied, as a new change that undoes the failed one and is applied at once. The history shows both: the deploy that failed and the rollback that followed.

**Undo is a change like any other.** Undoing change C proposes, for each resource C touched, the spec it had before C, or a deletion if C created it. It then plans and applies like any edit. Undo is refused when:
- a resource has changed again since C. The message names the later change, so the reader can undo that first or edit it directly. A partial undo that quietly keeps some of C is worse than none.
- C is older than the undo window, 7 days, which matches the plan sheet.

Destructive steps keep data recoverable for 7 days where the kind allows it (PRD, non-functional requirements). For example, a deleted volume moves aside rather than being erased. Drivers say which of their steps destroy data, and the plan shows it.

**Drift is a difference between the desired spec and what is observed.**
- Agents observe each managed resource on a schedule and compare it with the latest revision. The same diff that powers plans describes the drift, so it reads the same way.
- Drift is reported, never silently reverted.
- The panel offers two plans for it:
  - *Revert* re-applies the desired spec.
  - *Adopt* writes the observed state as a new revision, and changes nothing on the server.
- A change made with `dockwell local` while the control plane is away comes back the same way.

## Consequences

- Every feature from M2 onward is a kind: a spec message and a driver. It gets history, plans, undo, drift and audit without writing them. A path that writes managed state without a change is a bug (see `CLAUDE.md`).
- The plan sheet, the timeline's Undo and the drift view are one UI each, for every kind.
- Drivers decide downtime and data loss. Their estimates have to be honest and tested. A driver that says "no downtime" for a restart breaks the plan sheet's promise.
- Specs and drivers have to separate intent from observed state carefully, or drift fires on noise. Each driver's tests cover a resource that was created and then observed unchanged.
- Changes to different resources don't block each other. Two changes to the same resource are serialized by their base revisions: the second must be planned again.
- Applying runs steps one at a time on each server. Steps across servers can run in waves later, with a stop condition, as the PRD describes for fleets. A dependency graph between resources, such as a route needing its container, comes with references in M2. Until then the order is the proposal order.
- The prototype's container and firewall drivers run against in-memory backends that behave like Docker and nftables. The real backends, the Docker Engine API and nftables, implement the same small interfaces in M2. So does the agent's local cache of desired state, which principle 5 needs.
- For now, a plan that touches a server whose agent isn't connected can be reviewed but not applied. M2 decides whether such a change commits at once and reaches the server when it reconnects, which principle 5 suggests, or waits.
- Rollback after a failed apply works only when the earlier spec can be re-applied. Data migrations and other one-way steps need a forward fix, and their drivers must mark them as such in the plan.
