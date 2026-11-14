// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  type Change,
  ChangeSchema,
  ChangeState,
  ContainerState,
  type HostPanelSchema,
  ListContainersResponseSchema,
  PanelKind,
  PlanAction,
  type PlanStopPanelRequest,
  ServerContainerSchema,
  Surface,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

function api(more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  return { servers: { watchServers: watch.stream }, ...more };
}

const running = (name: string, image: string) =>
  create(ServerContainerSchema, { name, image, state: ContainerState.RUNNING });

const containers = (...list: ReturnType<typeof running>[]) =>
  create(ListContainersResponseSchema, { containers: list });

const host = { kind: "server.host", scope: "srv_1", name: "host" };

/** The plan of stopping or starting a panel on hz-1, as the control plane answers it. */
function panelChange(p: MessageInitShape<typeof HostPanelSchema>, summary: string): Change {
  return create(ChangeSchema, {
    id: "chg_panel",
    summary,
    state: ChangeState.PLANNED,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(new Date()),
    plan: {
      fingerprint: "1a2b3c4d5e6f708192a3b4c5d6e7f809",
      hostAccess: true,
      resources: [{ id: "res_host", resource: host, action: PlanAction.UPDATE, servers: ["srv_1"], panel: p }],
      servers: [
        {
          serverId: "srv_1",
          serverName: "hz-1",
          steps: [{ resourceId: "res_host", resource: host, action: PlanAction.UPDATE, panel: p }],
        },
      ],
    },
  });
}

const stopCoolify = panelChange(
  {
    kind: "coolify",
    parts: [
      { name: "coolify", wasRunning: true, wasRestart: "always" },
      { name: "coolify-db", wasRunning: true, wasRestart: "always" },
    ],
    stays: "coolify-proxy",
  },
  "Stop Coolify on hz-1",
);

afterEach(() => {
  localStorage.clear();
});

describe("stopping the panel an import came from", () => {
  it("is offered where Coolify runs, and its plan says what stops, what keeps running, and that it can be undone", async () => {
    const user = userEvent.setup();
    const planStopPanel = vi.fn((_req: PlanStopPanelRequest) => ({ change: stopCoolify }));
    renderAt(
      "/servers/srv_1",
      api({
        containers: {
          listContainers: () =>
            containers(running("coolify", "ghcr.io/coollabsio/coolify:4.7.5"), running("ghost-k4s9", "ghost:5")),
        },
        imports: { planStopPanel },
      }),
    );
    const offer = await screen.findByText("Coolify runs on this server");
    expect(offer).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Stop Coolify" }));
    await waitFor(() => expect(planStopPanel).toHaveBeenCalledTimes(1));
    expect(planStopPanel.mock.calls[0]?.[0]).toMatchObject({ serverId: "srv_1", panel: PanelKind.COOLIFY });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await waitFor(() => expect(sheet).toHaveTextContent("Stop Coolify on hz-1"));
    for (const words of [
      "Stops coolify and coolify-db, which stay stopped when Docker or hz-1 restarts",
      "coolify-proxy keeps serving the domains of what was taken on",
      "What Coolify ran keeps running, and nothing of Coolify is deleted",
      "Undo for",
    ]) {
      expect(sheet).toHaveTextContent(words);
    }
    expect(sheet).not.toHaveTextContent("passkey");
    await expectAccessible(sheet);
  });

  it("says a start has no undo of its own", async () => {
    const user = userEvent.setup();
    const start = panelChange(
      {
        kind: "dokploy",
        start: true,
        parts: [{ name: "dokploy-postgres", swarm: true, wasRunning: true, wasReplicas: 1n }],
      },
      "Undo: Stop Dokploy on hz-1",
    );
    renderAt(
      "/servers/srv_1",
      api({
        containers: { listContainers: () => containers(running("dokploy.1.x7k2", "dokploy/dokploy:v0.31.2")) },
        imports: { planStopPanel: () => ({ change: start }) },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Stop Dokploy" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await waitFor(() => expect(sheet).toHaveTextContent("Start Dokploy again on hz-1"));
    expect(sheet).toHaveTextContent("Starts dokploy-postgres as each stood before the stop");
    expect(within(sheet).getByText("No undo")).toBeTruthy();
  });

  it("isn't offered where the panel isn't running", async () => {
    const stopped = create(ServerContainerSchema, {
      name: "coolify",
      image: "ghcr.io/coollabsio/coolify:4.7.5",
      state: ContainerState.EXITED,
    });
    const listContainers = vi.fn(() => containers(stopped, running("web", "nginx:1.27")));
    renderAt("/servers/srv_1", api({ containers: { listContainers } }));
    await waitFor(() => expect(listContainers).toHaveBeenCalled());
    await screen.findByText("web");
    expect(screen.queryByText("Coolify runs on this server")).toBeNull();
  });

  it("goes when someone says not now", async () => {
    const user = userEvent.setup();
    renderAt(
      "/servers/srv_1",
      api({ containers: { listContainers: () => containers(running("coolify", "ghcr.io/coollabsio/coolify:4.7.5")) } }),
    );
    await screen.findByText("Coolify runs on this server");
    await user.click(screen.getByRole("button", { name: "Not now" }));
    await waitFor(() => expect(screen.queryByText("Coolify runs on this server")).toBeNull());
  });
});
