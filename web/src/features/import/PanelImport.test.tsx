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
  ImportWaitsFor,
  ListContainersResponseSchema,
  PanelKind,
  PlanAction,
  type PlanPanelDomainsRequest,
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

const coolifyRuns = () =>
  create(ListContainersResponseSchema, {
    containers: [
      create(ServerContainerSchema, {
        name: "coolify",
        image: "ghcr.io/coollabsio/coolify:4.7.5",
        state: ContainerState.RUNNING,
      }),
    ],
  });

function api(more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  return { servers: { watchServers: watch.stream }, containers: { listContainers: coolifyRuns }, ...more };
}

const found = create(ImportCoolifyResponseSchema, {
  found: { kind: PanelKind.COOLIFY, version: "4.7.5", knownVersion: true, folder: "/data/coolify" },
  steps: [
    {
      kind: ImportKind.APP,
      name: "blog",
      summary: "a service from Coolify's templates, in personal / production",
      action: ImportAction.DONE,
      becomes: "the project blog, taken on already",
    },
    {
      kind: ImportKind.DOMAIN,
      name: "blog.example.com",
      summary: "a domain of blog, sent to ghost-k4s9",
      action: ImportAction.MAKE,
      becomes: "a route to ghost on port 2368",
      waitsFor: ImportWaitsFor.PORTS,
    },
    {
      kind: ImportKind.PANEL,
      name: "Coolify",
      summary: "Coolify itself 4.7.5: coolify, coolify-proxy",
      action: ImportAction.LEAVE,
      reason: "It goes on running beside Dockwell, as nothing of it is changed.",
    },
  ],
});

const host = { kind: "server.host", scope: "srv_1", name: "host" };
const route = { kind: "route", scope: "env_1", name: "blog-example-com" };
const proxy = {
  kind: "coolify",
  proxy: true,
  parts: [{ name: "coolify-proxy", wasRunning: true, wasRestart: "always" }],
};
const handover = create(ChangeSchema, {
  id: "chg_domains",
  summary: "Hand over Coolify's domains on hz-1",
  state: ChangeState.PLANNED,
  proposedVia: Surface.PANEL,
  createTime: timestampFromDate(new Date()),
  plan: {
    fingerprint: "1a2b3c4d5e6f708192a3b4c5d6e7f809",
    hostAccess: true,
    notes: [{ reason: "panel_domains_unguarded", message: "", names: ["blog.example.com"] }],
    resources: [
      { id: "res_host", resource: host, action: PlanAction.UPDATE, servers: ["srv_1"], panel: proxy },
      { id: "res_route", resource: route, action: PlanAction.CREATE, servers: ["srv_1"] },
    ],
    servers: [
      {
        serverId: "srv_1",
        serverName: "hz-1",
        steps: [
          { resourceId: "res_host", resource: host, action: PlanAction.UPDATE, panel: proxy },
          { resourceId: "res_route", resource: route, action: PlanAction.CREATE },
        ],
      },
    ],
  },
});

describe("moving in from Coolify", () => {
  it("is a tab where Coolify runs, reads what it runs without planning, and hands over its domains as a plan", async () => {
    const user = userEvent.setup();
    const importCoolify = vi.fn((_req: { serverId: string; skipPlan?: boolean }) => found);
    const planPanelDomains = vi.fn((_req: PlanPanelDomainsRequest) => ({ change: handover }));
    renderAt("/servers/srv_1/import", api({ imports: { importCoolify, planPanelDomains } }));
    expect((await screen.findByRole("link", { name: "Move in" })).getAttribute("href")).toBe("/servers/srv_1/import");
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    await waitFor(() =>
      expect(importCoolify).toHaveBeenCalledWith(expect.objectContaining({ skipPlan: true }), expect.anything()),
    );
    const ports = await screen.findByRole("list", { name: "Later, once Coolify's proxy hands over ports 80 and 443" });
    expect(ports).toHaveTextContent("blog.example.com");
    expect(ports).toHaveTextContent("a route to ghost on port 2368");
    const stays = screen.getByRole("list", { name: "Stays with Coolify" });
    expect(within(stays).getByRole("button", { name: "Stop Coolify" })).toBeTruthy();
    await expectAccessible(document.body);

    await user.click(screen.getByRole("button", { name: "Hand over ports 80 and 443" }));
    await waitFor(() => expect(planPanelDomains).toHaveBeenCalledTimes(1));
    expect(planPanelDomains.mock.calls[0]?.[0]).toMatchObject({ serverId: "srv_1", panel: PanelKind.COOLIFY });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await waitFor(() =>
      expect(sheet).toHaveTextContent("Stop coolify-proxy on hz-1, so Dockwell takes ports 80 and 443"),
    );
    expect(sheet).toHaveTextContent("its domains come as the routes below");
    expect(sheet).toHaveTextContent("No login or address limit found on the old proxy");
    expect(sheet).toHaveTextContent("blog.example.com");
    expect(sheet).toHaveTextContent("Undo for");
  });

  it("offers the proxy's start where it's stopped and no domain is a route", async () => {
    const user = userEvent.setup();
    const planStartPanel = vi.fn((_req: { serverId: string; panel: PanelKind; proxy: boolean }) => ({
      change: handover,
    }));
    const stopped = () =>
      create(ListContainersResponseSchema, {
        containers: [
          create(ServerContainerSchema, {
            name: "coolify",
            image: "ghcr.io/coollabsio/coolify:4.7.5",
            state: ContainerState.RUNNING,
          }),
          create(ServerContainerSchema, { name: "coolify-proxy", image: "traefik:v3.1", state: ContainerState.EXITED }),
        ],
      });
    renderAt(
      "/servers/srv_1/import",
      api({ containers: { listContainers: stopped }, imports: { importCoolify: () => found, planStartPanel } }),
    );
    await user.click(await screen.findByRole("button", { name: "Read what Coolify runs" }));
    expect(screen.queryByRole("button", { name: "Hand over ports 80 and 443" })).toBeNull();
    await user.click(await screen.findByRole("button", { name: "Start coolify-proxy again" }));
    await waitFor(() => expect(planStartPanel).toHaveBeenCalledTimes(1));
    expect(planStartPanel.mock.calls[0]?.[0]).toMatchObject({
      serverId: "srv_1",
      panel: PanelKind.COOLIFY,
      proxy: true,
    });
  });
});
