// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { type Change, ChangeState, FieldOp, type Plan, PlanAction, type ServerPlan } from "@dockwell/api";
import type { PlanAction as DiffAction, PlanChange, PlanKind, PlanResource, PlanValue } from "@dockwell/ui";

/** How PlanDiff draws each of the plan's actions. Nothing to do reads as an update with a note saying so. */
const ACTIONS: Readonly<Record<PlanAction, DiffAction>> = {
  [PlanAction.UNSPECIFIED]: "update",
  [PlanAction.NONE]: "update",
  [PlanAction.CREATE]: "create",
  [PlanAction.UPDATE]: "update",
  [PlanAction.REPLACE]: "replace",
  [PlanAction.DELETE]: "destroy",
};

/** The kind PlanDiff draws a resource of the API's kind as. */
export function diffKind(kind: string): PlanKind {
  switch (kind) {
    case "project":
    case "environment":
    case "service":
    case "container":
      return kind;
    case "firewall.rule":
      return "firewall";
  }
  return "service";
}

const SECRET: PlanValue = { secret: true };

/**
 * A plan's resources as PlanDiff shows them: each one's action and field
 * changes. A sensitive field says it changed and never what it holds, which
 * the API blanks anyway.
 */
export function diffResources(plan: Plan | undefined, nothingToDo: string): PlanResource[] {
  return (plan?.resources ?? []).map((resource) => {
    const changes: PlanChange[] = resource.changes.map((change) => {
      const out: { path: string; from?: PlanValue; to?: PlanValue } = { path: change.path };
      if (change.op !== FieldOp.ADDED) out.from = change.sensitive ? SECRET : change.from;
      if (change.op !== FieldOp.REMOVED) out.to = change.sensitive ? SECRET : change.to;
      return out;
    });
    const out: PlanResource = {
      id: resource.id,
      action: ACTIONS[resource.action],
      kind: diffKind(resource.resource?.kind ?? ""),
      name: resource.resource?.name ?? resource.id,
      changes,
    };
    return resource.action === PlanAction.NONE ? { ...out, note: nothingToDo } : out;
  });
}

/** The longest any one server is down for, in ms: each server's steps run one at a time, servers side by side. */
export function longestDowntime(servers: readonly ServerPlan[]): number {
  let longest = 0;
  for (const server of servers) {
    const d = server.downtime;
    if (d) longest = Math.max(longest, Number(d.seconds) * 1000 + d.nanos / 1e6);
  }
  return longest;
}

/** The first characters of a plan's fingerprint, which a person types to confirm a destructive plan. */
export function shortFingerprint(plan: Plan | undefined): string {
  return (plan?.fingerprint ?? "").slice(0, 8);
}

/** True for a change that is over: applied, failed or discarded. */
export function isFinal(change: Change): boolean {
  return (
    change.state === ChangeState.APPLIED ||
    change.state === ChangeState.FAILED ||
    change.state === ChangeState.DISCARDED
  );
}

/**
 * True while a change can still be undone: applied, not undone already, and
 * within its undo window. A rollback is Dockwell's own repair, which undoing
 * would only break again, so it offers none.
 */
export function canUndo(change: Change | undefined, now: number): boolean {
  if (!change || change.state !== ChangeState.APPLIED || change.undoneBy !== "" || change.rollback) return false;
  return change.undoDeadline !== undefined && timestampMs(change.undoDeadline) > now;
}
