// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  AppsOnRemoval,
  type Change,
  ChangeSchema,
  ChangeState,
  DeploymentState,
  PlanAction,
  type PlanRemoveServerRequest,
  Surface,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { anApp } from "../../test/apps";
import { feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1", controlServer: true });
const hz2 = aServer({ id: "srv_2", name: "hz-2" });
const web = { kind: "service", scope: "env_shop", name: "web" };
const host = { kind: "server.host", scope: "srv_2", name: "host" };

/** Removing hz-2, as its plan has it. */
function removal(compromised: boolean): Change {
  return create(ChangeSchema, {
    id: "chg_remove",
    summary: compromised ? "Remove hz-2, which may be compromised" : "Remove hz-2",
    state: ChangeState.PLANNED,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(new Date()),
    plan: {
      fingerprint: "1a2b3c4d5e6f708192a3b4c5d6e7f809",
      destructive: true,
      removes: { serverId: "srv_2", serverName: "hz-2", compromised },
      resources: [
        { id: "svc_web", resource: web, action: PlanAction.DELETE, previousServers: ["srv_2"], letsGo: compromised },
        { id: "res_host", resource: host, action: PlanAction.DELETE, previousServers: ["srv_2"], letsGo: true },
      ],
      servers: [
        {
          serverId: "srv_2",
          serverName: "hz-2",
          removed: compromised,
          steps: compromised
            ? []
            : [{ resourceId: "svc_web", resource: web, action: PlanAction.DELETE, hidesLogs: true }],
        },
      ],
      notes: [
        {
          reason: "removal_final",
          message: "hz-2 can't come back once it's removed: it joins again with a new ticket, as a new server",
          names: ["hz-2"],
        },
      ],
    },
  });
}

/** Removing hz-2 with nothing on it: the removal alone, which edits nothing (ADR 0090). */
function emptyRemoval(): Change {
  const c = removal(false);
  if (c.plan) {
    c.plan.resources = [];
    c.plan.servers = [{ ...c.plan.servers[0], removed: true, steps: [] } as (typeof c.plan.servers)[number]];
  }
  return c;
}

function api(planRemoveServer: (req: PlanRemoveServerRequest) => { change: Change }, apps = true) {
  const servers = feed<WatchServersResponse>();
  servers.push(changed(hz1), changed(hz2), snapshotComplete);
  return {
    servers: { watchServers: servers.stream },
    changes: {
      planRemoveServer,
      getChange: () => ({ change: removal(false) }),
      watchChange: feed<WatchChangeResponse>().stream,
    },
    services: {
      listServices: () => ({
        services: apps
          ? [
              anApp(
                "web",
                "nginx:1.27",
                {},
                { deployments: [{ serverId: "srv_2", serverName: "hz-2", state: DeploymentState.LIVE }] },
              ),
            ]
          : [],
      }),
    },
  };
}

describe("Removing a server", () => {
  it("asks what becomes of the apps that run there, then plans it, with compromised one switch away", async () => {
    const user = userEvent.setup();
    const plan = vi.fn((req: PlanRemoveServerRequest) => ({ change: removal(req.compromised) }));
    renderAt("/servers/srv_2", api(plan));
    await user.click(await screen.findByRole("button", { name: "Remove from fleet" }));
    const sheet = await screen.findByRole("dialog", { name: "Remove hz-2" });
    expect(sheet).toHaveTextContent("1 app runs on it");
    expect(within(sheet).getByRole("radio", { name: /Leave them running/ })).toBeChecked();
    expect(within(sheet).getByRole("radio", { name: /Move them to hz-1/ })).toBeVisible();
    await user.click(within(sheet).getByRole("radio", { name: /Stop them/ }));
    await user.click(within(sheet).getByRole("button", { name: "Plan removal" }));
    await waitFor(() => expect(plan).toHaveBeenCalledTimes(1));
    expect(plan.mock.calls[0]?.[0]).toMatchObject({ serverId: "srv_2", apps: AppsOnRemoval.STOP, compromised: false });

    const review = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(review).findByRole("button", { name: "Remove hz-2" })).toBeVisible();
    expect(within(review).getByText("No undo")).toBeVisible();
    expect(within(review).getByText("Applying this asks for your passkey")).toBeVisible();
    expect(review).toHaveTextContent("hz-2 can't come back once it's removed");
    // What it says of the server itself, what stays as it is, and nothing of an undo it hasn't.
    expect(review).toHaveTextContent("hz-2 leaves the fleet for good");
    expect(review).toHaveTextContent("It's revoked once its steps have passed");
    expect(within(review).getByText("Let go of")).toBeVisible();
    expect(review).not.toHaveTextContent(/undo this until|Undoing this brings/);

    // A compromised server takes no step, so its apps are left running, not stopped.
    await user.click(within(review).getByRole("switch", { name: /It may be compromised/ }));
    await waitFor(() => expect(plan).toHaveBeenCalledTimes(2));
    expect(plan.mock.calls[1]?.[0]).toMatchObject({ serverId: "srv_2", apps: AppsOnRemoval.LEAVE, compromised: true });
  });

  it("plans it at once for a server that runs nothing, and the control server has no such way", async () => {
    const user = userEvent.setup();
    const plan = vi.fn((_req: PlanRemoveServerRequest) => ({ change: emptyRemoval() }));
    const { unmount } = renderAt("/servers/srv_2", api(plan, false));
    await user.click(await screen.findByRole("button", { name: "Remove from fleet" }));
    await waitFor(() => expect(plan).toHaveBeenCalledTimes(1));
    expect(plan.mock.calls[0]?.[0]).toMatchObject({ serverId: "srv_2", apps: AppsOnRemoval.LEAVE, compromised: false });
    expect(screen.queryByRole("dialog", { name: "Remove hz-2" })).toBeNull();
    // A plan with nothing on the server to change still has it leave: it's no plan of nothing.
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(within(sheet).getByRole("button", { name: "Remove hz-2" })).toBeVisible();
    expect(sheet).toHaveTextContent("hz-2 can't come back once it's removed");
    unmount();

    renderAt("/servers/srv_1", api(plan));
    await screen.findByRole("region", { name: "Agent" });
    expect(screen.queryByRole("button", { name: "Remove from fleet" })).toBeNull();
  });
});
