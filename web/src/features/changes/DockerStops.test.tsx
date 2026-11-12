// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  type Change,
  ChangeSchema,
  ChangeState,
  ContainerState,
  DeploymentState,
  GetServerUpdatesResponseSchema,
  PackageEffect,
  PackageManager,
  PlanAction,
  RestartPolicy,
  ServerUpdatesSchema,
  type ServiceInfo,
  ServiceInfoSchema,
  Surface,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { startsAgain, stoppingOn } from "./DockerStops";

const NOW = new Date(2026, 10, 12, 9, 0);

afterEach(() => activateLocale("en"));

function service(id: string, name: string, scope: string, restart: RestartPolicy, server = "srv_1"): ServiceInfo {
  return create(ServiceInfoSchema, {
    id,
    resource: { kind: "service", scope, name },
    spec: { runs: { case: "app", value: { image: `${name}:1`, restart } } },
    deployments: [{ serverId: server, serverName: "web-1", state: DeploymentState.LIVE }],
  });
}

const services = [
  service("svc_api", "api", "env_shop", RestartPolicy.UNSPECIFIED),
  service("svc_worker", "worker", "env_shop", RestartPolicy.ON_FAILURE),
  service("svc_db", "shop-db", "env_shop", RestartPolicy.ALWAYS),
  service("svc_blog", "blog", "env_blog", RestartPolicy.UNLESS_STOPPED),
  service("svc_cron", "cron", "env_tools", RestartPolicy.NO),
  // Elsewhere: not on web-1.
  service("svc_far", "far", "env_shop", RestartPolicy.NO, "srv_2"),
];

const websites = [
  { id: "prj_blog", name: "blog", domain: "blog.example.com", environmentId: "env_blog", serverId: "srv_1" },
];

describe("what stops while Docker restarts", () => {
  it("starts again what Docker's own rules start again as its daemon comes back", () => {
    expect(startsAgain(RestartPolicy.UNSPECIFIED)).toBe("yes");
    expect(startsAgain(RestartPolicy.UNLESS_STOPPED)).toBe("yes");
    expect(startsAgain(RestartPolicy.ALWAYS)).toBe("yes");
    // on-failure starts it again only if it exits with an error as the daemon stops it, as it often does.
    expect(startsAgain(RestartPolicy.ON_FAILURE)).toBe("maybe");
    expect(startsAgain(RestartPolicy.NO)).toBe("no");
  });

  it("lists each app, database and website on the server, what stays stopped first, then what may", () => {
    const got = stoppingOn("srv_1", services, websites, new Set(["svc_db"]));
    expect(got.map((s) => [s.name, s.kind, s.back])).toEqual([
      ["cron", "app", "no"],
      ["worker", "app", "maybe"],
      ["api", "app", "yes"],
      ["blog.example.com", "website", "yes"],
      ["shop-db", "database", "yes"],
    ]);
    // A website is as sure to come back as the least sure of its services.
    const site = stoppingOn(
      "srv_1",
      [...services, service("svc_php", "php", "env_blog", RestartPolicy.ON_FAILURE)],
      websites,
      new Set(),
    ).find((s) => s.kind === "website");
    expect(site?.back).toBe("maybe");
  });

  it("names them in the plan for an install that restarts Docker, linked, with whether each comes back", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW });
    const user = userEvent.setup();
    const watch = feed<WatchServersResponse>();
    watch.push(changed(aServer({ id: "srv_1", name: "web-1" })), snapshotComplete);
    renderAt("/servers/srv_1/updates", {
      servers: { watchServers: watch.stream },
      updates: {
        getServerUpdates: () =>
          create(GetServerUpdatesResponseSchema, {
            updates: create(ServerUpdatesSchema, {
              serverId: "srv_1",
              manager: PackageManager.APT,
              marksSecurity: true,
              readTime: timestampFromDate(NOW),
              listsTime: timestampFromDate(NOW),
              packages: [
                {
                  name: "docker-ce",
                  installed: "5:27.3.1-1~ubuntu.24.04~noble",
                  available: "5:27.4.0-1~ubuntu.24.04~noble",
                  security: true,
                  origin: "noble-security",
                  effect: PackageEffect.DOCKER_RESTART,
                },
              ],
            }),
          }),
      },
      changes: {
        planInstallUpdates: () => ({ change: dockerPlan() }),
        watchChange: feed<WatchChangeResponse>().stream,
      },
      services: { listServices: () => ({ services }) },
      websites: { listWebsites: () => ({ websites }) },
      databases: { listDatabases: () => ({ databases: [{ id: "svc_db", engine: 1 }] }) },
      containers: {
        listContainers: () => ({
          containers: [
            { id: "c1", name: "portainer", state: ContainerState.RUNNING },
            { id: "c2", name: "old", state: ContainerState.EXITED },
          ],
        }),
      },
    });
    await user.click(await screen.findByRole("button", { name: "Install security updates" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const list = await within(sheet).findByRole("list", { name: "What stops on web-1" });
    expect(
      within(sheet).getByText("What stops on web-1 until Docker is back · 1 stays stopped after · 1 may stay stopped"),
    ).toBeVisible();
    const rows = within(list).getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual([
      "cron App Stays stopped",
      "worker App May stay stopped",
      "api App Starts again",
      "blog.example.com Website Starts again",
      "shop-db Database Starts again",
    ]);
    // What stays stopped leads to where its restart policy is set, and says why on hover.
    expect(within(rows[1] as HTMLElement).getByRole("link", { name: "worker" })).toHaveAttribute(
      "href",
      "/apps/svc_worker/settings",
    );
    expect(within(rows[1] as HTMLElement).getByText("May stay stopped")).toHaveAttribute(
      "title",
      "It starts again only if it exits with an error as Docker stops it",
    );
    expect(within(rows[0] as HTMLElement).getByText("Stays stopped")).toHaveAttribute(
      "title",
      "Its restart policy never starts it again",
    );
    expect(within(rows[3] as HTMLElement).getByRole("link", { name: "blog.example.com" })).toHaveAttribute(
      "href",
      "/websites/prj_blog",
    );
    expect(within(rows[4] as HTMLElement).getByRole("link", { name: "shop-db" })).toHaveAttribute(
      "href",
      "/databases/svc_db",
    );
    expect(sheet).toHaveTextContent("1 container Dockwell didn't make stops too, and its own restart policy decides.");
    expect(within(sheet).getByRole("link", { name: "See them" })).toHaveAttribute("href", "/servers/srv_1");
    vi.useRealTimers();
  });
});

/** Installing docker-ce on web-1, which restarts Docker and stops what runs there. */
function dockerPlan(): Change {
  const host = { kind: "server.host", scope: "srv_1", name: "host" };
  const install = {
    packages: [
      {
        name: "docker-ce",
        installed: "5:27.3.1-1~ubuntu.24.04~noble",
        available: "5:27.4.0-1~ubuntu.24.04~noble",
        security: true,
        effect: PackageEffect.DOCKER_RESTART,
      },
    ],
    dockerRestarts: true,
  };
  return create(ChangeSchema, {
    id: "chg_docker",
    summary: "Install security updates on web-1",
    state: ChangeState.PLANNED,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(NOW),
    plan: {
      fingerprint: "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
      hostAccess: true,
      resources: [{ id: "res_host", resource: host, action: PlanAction.UPDATE, servers: ["srv_1"], install }],
      servers: [
        {
          serverId: "srv_1",
          serverName: "web-1",
          steps: [{ resourceId: "res_host", resource: host, action: PlanAction.UPDATE, install }],
        },
      ],
    },
  });
}
