// SPDX-License-Identifier: AGPL-3.0-only

import { clone, create, type MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  ActorKind,
  ActorSchema,
  type Change,
  ChangeSchema,
  ChangeState,
  type Event,
  EventKind,
  EventSchema,
  FieldOp,
  PlanAction,
  Surface,
  type WatchChangeResponse,
  WatchChangeResponseSchema,
  type WatchEventsResponse,
  WatchEventsResponseSchema,
} from "@dockwell/api";

export const mira = create(ActorSchema, { kind: ActorKind.USER, id: "usr_1", name: "Mira Chen" });

/**
 * A copy of a change with some fields set, which must be messages already:
 * create would take a change spread into it for a message, and leave plain
 * objects inside it unconverted.
 */
export function moved(change: Change, fields: Partial<Change>): Change {
  return Object.assign(clone(ChangeSchema, change), fields);
}

/** A planned change that creates the project shop and its production environment, on no server. */
export function shopPlanned(): Change {
  return create(ChangeSchema, {
    id: "chg_shop",
    summary: "Create the project shop",
    state: ChangeState.PLANNED,
    proposedBy: mira,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(new Date()),
    plan: {
      fingerprint: "3f9a1c2b7d004e11aa22bb33cc44dd55",
      summary: { create: 2 },
      undoWindow: durationFromMs(7 * 86_400_000),
      resources: [
        {
          id: "prj_shop",
          resource: { kind: "project", scope: "org_1", name: "shop" },
          action: PlanAction.CREATE,
          changes: [{ path: "display_name", op: FieldOp.ADDED, to: "Shop" }],
        },
        {
          id: "env_prod",
          resource: { kind: "environment", scope: "prj_shop", name: "production" },
          action: PlanAction.CREATE,
        },
      ],
    },
  });
}

/** A planned change that replaces web on hz-1, with 2 s of downtime and a secret that changes. */
export function webPlanned(): Change {
  return create(ChangeSchema, {
    id: "chg_web",
    summary: "Update web",
    state: ChangeState.PLANNED,
    proposedBy: mira,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(new Date()),
    plan: {
      fingerprint: "a1b2c3d4e5f60718293a4b5c6d7e8f90",
      summary: { replace: 1 },
      undoWindow: durationFromMs(7 * 86_400_000),
      resources: [
        {
          id: "res_web",
          resource: { kind: "container", scope: "srv_1", name: "web" },
          action: PlanAction.REPLACE,
          changes: [
            { path: "image", op: FieldOp.MODIFIED, from: "nginx:1.27", to: "nginx:1.28" },
            { path: "env[API_KEY]", op: FieldOp.MODIFIED, sensitive: true },
          ],
          servers: ["srv_1"],
          previousServers: ["srv_1"],
        },
      ],
      servers: [
        {
          serverId: "srv_1",
          serverName: "hz-1",
          downtime: durationFromMs(2_000),
          steps: [
            {
              resourceId: "res_web",
              resource: { kind: "container", scope: "srv_1", name: "web" },
              action: PlanAction.REPLACE,
              downtime: durationFromMs(2_000),
            },
          ],
        },
      ],
    },
  });
}

/** An event for a change, timed now unless init says otherwise. */
export function changeEvent(change: Change, init: MessageInitShape<typeof EventSchema> = {}): Event {
  const action = {
    [ChangeState.UNSPECIFIED]: "change.planned",
    [ChangeState.PLANNED]: "change.planned",
    [ChangeState.APPLYING]: "change.applying",
    [ChangeState.APPLIED]: "change.applied",
    [ChangeState.FAILED]: "change.failed",
    [ChangeState.DISCARDED]: "change.discarded",
  }[change.state];
  return create(EventSchema, {
    id: change.id,
    kind: EventKind.CHANGE,
    action,
    time: change.applyTime ?? change.createTime ?? timestampFromDate(new Date()),
    actor: change.appliedBy ?? change.proposedBy,
    surface: change.appliedBy ? change.appliedVia : change.proposedVia,
    change,
    ...init,
  });
}

/** An event from the audit log. */
export function auditEvent(init: MessageInitShape<typeof EventSchema>): Event {
  return create(EventSchema, {
    kind: EventKind.ACCESS,
    time: timestampFromDate(new Date()),
    actor: mira,
    surface: Surface.PANEL,
    ...init,
  });
}

/** The change as it is now, for a watch of it. */
export function watched(change: Change): WatchChangeResponse {
  return create(WatchChangeResponseSchema, { event: { case: "change", value: change } });
}

/** A watch's snapshot of the timeline. */
export function snapshot(events: Event[], nextPageToken = ""): WatchEventsResponse {
  return create(WatchEventsResponseSchema, { events, snapshot: true, nextPageToken });
}

/** What happened since, for a watch of the timeline. */
export function since(events: Event[], removed: string[] = []): WatchEventsResponse {
  return create(WatchEventsResponseSchema, { events, removed });
}

/** A change applied a moment ago, with an undo window of 7 days. */
export function applied(change: Change, at = new Date()): Change {
  return moved(change, {
    state: ChangeState.APPLIED,
    appliedBy: mira,
    appliedVia: Surface.PANEL,
    applyTime: timestampFromDate(at),
    undoDeadline: timestampFromDate(new Date(at.getTime() + 7 * 86_400_000)),
  });
}
