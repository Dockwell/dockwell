// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import type { HandlerContext } from "@connectrpc/connect";
import {
  type Change,
  ChangeSchema,
  ChangeState,
  DeploymentState,
  GetServerUpdatesResponseSchema,
  PackageManager,
  PlanAction,
  type PlanRebootRequest,
  RestartPolicy,
  ServerUpdatesSchema,
  ServiceInfoSchema,
  StepState,
  Surface,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { feed, renderAt, renderWithApi, untilAborted } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { ChangeSheetProvider, type Opening, useChangeSheet } from "../changes/ChangeSheet";

function Opener({ opening }: { opening: Opening }) {
  const { open } = useChangeSheet();
  useEffect(() => open(opening), [open, opening]);
  return null;
}

const NOW = new Date(2026, 10, 12, 9, 0);

afterEach(() => vi.useRealTimers());

const host = { kind: "server.host", scope: "srv_1", name: "host" };

/** Rebooting web-1, the control server, which last took 1 min 42 s, as it stands at state with its step. */
function rebootChange(
  state: ChangeState,
  step?: { state: StepState; message?: string; start?: Date; finish?: Date },
): Change {
  const reboot = { controlServer: true, lastTook: { seconds: 102n, nanos: 0 } };
  return create(ChangeSchema, {
    id: "chg_reboot",
    summary: "Reboot web-1",
    state,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(NOW),
    plan: {
      fingerprint: "1a2b3c4d5e6f708192a3b4c5d6e7f809",
      hostAccess: true,
      resources: [{ id: "res_host", resource: host, action: PlanAction.UPDATE, servers: ["srv_1"], reboot }],
      servers: [
        {
          serverId: "srv_1",
          serverName: "web-1",
          steps: [{ resourceId: "res_host", resource: host, action: PlanAction.UPDATE, reboot }],
        },
      ],
    },
    steps: step
      ? [
          {
            serverId: "srv_1",
            resourceId: "res_host",
            resource: host,
            action: PlanAction.UPDATE,
            state: step.state,
            message: step.message ?? "",
            startTime: step.start ? timestampFromDate(step.start) : undefined,
            finishTime: step.finish ? timestampFromDate(step.finish) : undefined,
          },
        ]
      : [],
  });
}

/** A change's watch every watcher hears, each from the change as it last stood. */
function watched() {
  const subs = new Set<(c: Change) => void>();
  let last: Change | undefined;
  return {
    push(c: Change) {
      last = c;
      for (const sub of subs) sub(c);
    },
    async *watchChange(_req: unknown, ctx: HandlerContext): AsyncGenerator<WatchChangeResponse> {
      const queue: Change[] = last ? [last] : [];
      let wake = () => {};
      const sub = (c: Change) => {
        queue.push(c);
        wake();
      };
      subs.add(sub);
      try {
        for (;;) {
          const next = queue.shift();
          if (next) {
            yield {
              $typeName: "dockwell.v1.WatchChangeResponse",
              event: { case: "change", value: next },
            } as WatchChangeResponse;
            continue;
          }
          if (ctx.signal.aborted) return;
          await Promise.race([new Promise<void>((resolve) => (wake = resolve)), untilAborted(ctx.signal)]);
        }
      } finally {
        subs.delete(sub);
      }
    },
  };
}

function api(planReboot: (req: PlanRebootRequest) => { change: Change }, watch = watched()) {
  const servers = feed<WatchServersResponse>();
  servers.push(changed(aServer({ id: "srv_1", name: "web-1", controlServer: true })), snapshotComplete);
  return {
    servers: { watchServers: servers.stream },
    updates: {
      getServerUpdates: () =>
        create(GetServerUpdatesResponseSchema, {
          updates: create(ServerUpdatesSchema, {
            serverId: "srv_1",
            manager: PackageManager.APT,
            marksSecurity: true,
            readTime: timestampFromDate(NOW),
            listsTime: timestampFromDate(NOW),
            rebootRequired: true,
            rebootPackages: ["linux-image-6.8.0-49-generic"],
          }),
        }),
    },
    changes: {
      planReboot,
      watchChange: watch.watchChange,
      getChange: () => ({ change: rebootChange(ChangeState.PLANNED) }),
    },
    services: {
      listServices: () => ({
        services: [
          create(ServiceInfoSchema, {
            id: "svc_api",
            resource: { kind: "service", scope: "env_shop", name: "api" },
            spec: { runs: { case: "app", value: { image: "api:1", restart: RestartPolicy.UNSPECIFIED } } },
            deployments: [{ serverId: "srv_1", serverName: "web-1", state: DeploymentState.LIVE }],
          }),
        ],
      }),
    },
    websites: { listWebsites: () => ({ websites: [] }) },
    databases: { listDatabases: () => ({ databases: [] }) },
    containers: { listContainers: () => ({ containers: [] }) },
    backups: {
      listBackups: () => ({
        backups: [{ id: "bk_1", volume: "pgdata", volumeId: "vol_1", serverId: "srv_1", state: 2 }],
      }),
    },
  };
}

describe("rebooting a server", () => {
  it("is one step from the server needing it, with a plan of what goes offline and the panel stopping too", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    const user = userEvent.setup();
    const planReboot = vi.fn((_req: PlanRebootRequest) => ({ change: rebootChange(ChangeState.PLANNED) }));
    renderAt("/servers/srv_1/updates", api(planReboot));
    const lead = (await screen.findByText("web-1 needs a reboot")).closest(".dw-callout") as HTMLElement;
    await user.click(within(lead).getByRole("button", { name: "Reboot" }));
    await waitFor(() => expect(planReboot).toHaveBeenCalledTimes(1));
    expect(planReboot.mock.calls[0]?.[0]).toMatchObject({ serverIds: ["srv_1"] });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await waitFor(() => expect(sheet.querySelector(".plan-step-line")).toHaveTextContent("Reboot web-1"));
    // One server has no others to keep serving.
    expect(sheet).toHaveTextContent("The panel and the API stop with web-1 and come back on their own");
    expect(sheet).not.toHaveTextContent("the other servers keep serving");
    expect(sheet).toHaveTextContent("Its last reboot took 1 min 42 s");
    const offline = await within(sheet).findByRole("list", { name: "What goes offline on web-1" });
    expect(within(offline).getByRole("link", { name: "api" })).toBeVisible();
    expect(sheet).toHaveTextContent("What goes offline on web-1 until it's back · all of it starts again");
    const risk = await within(sheet).findByRole("list", { name: "Backups a reboot of web-1 puts at risk" });
    expect(risk).toHaveTextContent("pgdata Backed up only on web-1");
    // As good as root: the passkey, a typed confirmation, and nothing to undo.
    expect(within(sheet).getByText("Applying this asks for your passkey")).toBeVisible();
    expect(within(sheet).getByRole("textbox", { name: /to apply it/ })).toBeVisible();
    expect(within(sheet).getByText("No undo")).toBeVisible();
    expect(within(sheet).getByText(/everything there is offline until it's back/)).toBeVisible();
  });

  it("counts while the server is away, and says how long it took once it's back", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    const watch = watched();
    const started = new Date(NOW.getTime() - 40_000);
    watch.push(
      rebootChange(ChangeState.APPLYING, {
        state: StepState.WAITING,
        message: "web-1 is restarting; this passes once it's back",
        start: started,
      }),
    );
    const a = api(() => ({ change: rebootChange(ChangeState.PLANNED) }), watch);
    a.changes.getChange = () => ({ change: rebootChange(ChangeState.APPLYING) });
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={{ kind: "change", id: "chg_reboot" }} />
      </ChangeSheetProvider>,
      a,
    );
    const sheet = await screen.findByRole("dialog", { name: "Rebooting web-1" });
    expect(await within(sheet).findByText("web-1 is restarting; this passes once it's back")).toBeVisible();
    await waitFor(() => expect(sheet.querySelector(".plan-step-time")).toHaveTextContent(/4[0-3] s/));
    watch.push(
      rebootChange(ChangeState.APPLIED, {
        state: StepState.DONE,
        message: "web-1 is back after its reboot",
        start: started,
        finish: new Date(started.getTime() + 102_000),
      }),
    );
    const back = await screen.findByRole("dialog", { name: "web-1 is back after its reboot" });
    await waitFor(() => expect(back.querySelector(".plan-step-time")?.textContent).toBe("1 min 42 s"));
  });

  it("is on the server's page, and the overview's Updates card, once it needs one", async () => {
    const planReboot = vi.fn((_req: PlanRebootRequest) => ({ change: rebootChange(ChangeState.PLANNED) }));
    const user = userEvent.setup();
    const { unmount } = renderAt("/servers/srv_1", {
      ...api(planReboot),
      servers: { ...api(planReboot).servers, getServerDiskUse: () => ({}) },
    });
    const callout = (await screen.findByText("web-1 needs a reboot")).closest(".dw-callout") as HTMLElement;
    expect(callout).toHaveTextContent("For what was installed to take effect");
    await user.click(within(callout).getByRole("button", { name: "Reboot" }));
    await waitFor(() => expect(planReboot).toHaveBeenCalledTimes(1));
    unmount();
    renderAt("/", api(planReboot));
    const card = await screen.findByRole("region", { name: "Updates" });
    await user.click(await within(card).findByRole("button", { name: "Reboot" }));
    await waitFor(() => expect(planReboot).toHaveBeenCalledTimes(2));
  });
});
