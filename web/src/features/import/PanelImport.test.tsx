// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  ChangeSchema,
  ChangeState,
  ContainerState,
  ImportAction,
  ImportCoolifyResponseSchema,
  ImportKind,
  ListContainersResponseSchema,
  PanelKind,
  type PlanStartPanelRequest,
  type PlanStopPanelRequest,
  ServerContainerSchema,
  Surface,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

const coolify = (state: ContainerState) => () =>
  create(ListContainersResponseSchema, {
    containers: [create(ServerContainerSchema, { name: "coolify", image: "ghcr.io/coollabsio/coolify:4.7.5", state })],
  });

function api(more: Api = {}, state = ContainerState.RUNNING): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  return { servers: { watchServers: watch.stream }, containers: { listContainers: coolify(state) }, ...more };
}

const found = (withPlan = false) =>
  create(ImportCoolifyResponseSchema, {
    found: { kind: PanelKind.COOLIFY, version: "4.7.5", knownVersion: true, folder: "/data/coolify" },
    steps: [
      {
        kind: ImportKind.APP,
        name: "blog",
        summary: "a service from Coolify's templates, in personal / production",
        action: ImportAction.TAKE_ON,
        becomes: "the project blog, taken on as it runs, with no restart, its volumes where they are",
      },
      {
        kind: ImportKind.PANEL,
        name: "Coolify",
        summary: "Coolify itself 4.7.5: coolify, coolify-proxy",
        action: ImportAction.LEAVE,
        reason: "It goes on running beside Dockwell, as nothing of it is changed.",
      },
    ],
    change: withPlan
      ? create(ChangeSchema, {
          id: "chg_import",
          summary: "Take on what Coolify runs on hz-1",
          state: ChangeState.PLANNED,
          proposedVia: Surface.PANEL,
          createTime: timestampFromDate(new Date()),
          plan: { fingerprint: "feedfacefeedfacefeedfacefeedface", summary: { change: 1 } },
        })
      : undefined,
  });

const host = { kind: "server.host", scope: "srv_1", name: "host" };
const panelChange = (start: boolean) =>
  create(ChangeSchema, {
    id: "chg_panel",
    summary: start ? "Start Coolify again on hz-1" : "Stop Coolify on hz-1",
    state: ChangeState.PLANNED,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(new Date()),
    plan: {
      fingerprint: "1a2b3c4d5e6f708192a3b4c5d6e7f809",
      hostAccess: true,
      servers: [
        {
          serverId: "srv_1",
          serverName: "hz-1",
          steps: [
            {
              resourceId: "res_host",
              resource: host,
              panel: { kind: "coolify", start, parts: [{ name: "coolify", wasRunning: true, wasRestart: "always" }] },
            },
          ],
        },
      ],
    },
  });

describe("moving in from Coolify", () => {
  it("is a tab where Coolify runs, reads what it runs without planning, and has the sheet make the plan", async () => {
    const user = userEvent.setup();
    const importCoolify = vi.fn((req: { serverId: string; skipPlan?: boolean }) => found(!req.skipPlan));
    const planStopPanel = vi.fn((_req: PlanStopPanelRequest) => ({ change: panelChange(false) }));
    renderAt("/servers/srv_1/import", api({ imports: { importCoolify, planStopPanel } }));
    expect((await screen.findByRole("link", { name: "Move in" })).getAttribute("href")).toBe("/servers/srv_1/import");
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    await waitFor(() =>
      expect(importCoolify).toHaveBeenCalledWith(expect.objectContaining({ skipPlan: true }), expect.anything()),
    );
    expect(await screen.findByRole("list", { name: "In this plan" })).toHaveTextContent("blog");
    const stays = screen.getByRole("list", { name: "Stays with Coolify" });
    await expectAccessible(document.body);

    await user.click(within(stays).getByRole("button", { name: "Stop Coolify" }));
    await waitFor(() => expect(planStopPanel).toHaveBeenCalledTimes(1));
    expect(planStopPanel.mock.calls[0]?.[0]).toMatchObject({ serverId: "srv_1", panel: PanelKind.COOLIFY });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await waitFor(() => expect(sheet).toHaveTextContent("Stop Coolify on hz-1"));
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Review plan" })).toBeNull());

    await user.click(screen.getAllByRole("button", { name: "Review the plan" })[0] as HTMLElement);
    await waitFor(() =>
      expect(importCoolify).toHaveBeenCalledWith(expect.objectContaining({ skipPlan: false }), expect.anything()),
    );
  });

  it("offers Coolify's start where it's stopped", async () => {
    const user = userEvent.setup();
    const planStartPanel = vi.fn((_req: PlanStartPanelRequest) => ({ change: panelChange(true) }));
    renderAt(
      "/servers/srv_1/import",
      api({ imports: { importCoolify: () => found(), planStartPanel } }, ContainerState.EXITED),
    );
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    await user.click(await screen.findByRole("button", { name: "Start Coolify again" }));
    await waitFor(() => expect(planStartPanel).toHaveBeenCalledTimes(1));
    expect(planStartPanel.mock.calls[0]?.[0]).toMatchObject({ serverId: "srv_1", panel: PanelKind.COOLIFY });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await waitFor(() => expect(sheet).toHaveTextContent("Start Coolify again on hz-1"));
    expect(within(sheet).getByText("No undo")).toBeTruthy();
  });
});
