// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  type Change,
  ChangeSchema,
  ChangeState,
  type HostUnitSchema,
  LogLevel,
  PlanAction,
  type PlanSystemServiceActionRequest,
  type ReadSystemLogRequest,
  Surface,
  SystemInit,
  SystemLogSource,
  SystemServiceAction,
  SystemServiceGuard,
  type SystemServiceInfo,
  SystemServiceInfoSchema,
  SystemServiceListSchema,
  SystemServiceState,
  type WatchServersResponse,
  type WatchSystemServicesResponse,
  WatchSystemServicesResponseSchema,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const ago = (ms: number) => timestampFromDate(new Date(Date.now() - ms));
const unit = (s: MessageInitShape<typeof SystemServiceInfoSchema>) => create(SystemServiceInfoSchema, s);

const nginx = unit({
  name: "nginx.service",
  description: "A high performance web server",
  state: SystemServiceState.FAILED,
  detail: "failed",
  atBoot: "enabled",
  since: ago(25 * 60_000),
  result: "exit-code",
  exitStatus: 1,
  canReload: true,
  why: ["nginx: [emerg] bind() to 0.0.0.0:80 failed (98: Address already in use)"],
});
const docker = unit({
  name: "docker.service",
  state: SystemServiceState.RUNNING,
  atBoot: "enabled",
  needed: true,
  guard: SystemServiceGuard.DOCKER,
});
const ssh = unit({
  name: "ssh.service",
  state: SystemServiceState.RUNNING,
  atBoot: "enabled",
  guard: SystemServiceGuard.SSH,
});
const dockwell = unit({
  name: "dockwell.service",
  state: SystemServiceState.RUNNING,
  atBoot: "enabled",
  guard: SystemServiceGuard.DOCKWELL,
});
const redis = unit({ name: "redis-server.service", state: SystemServiceState.INACTIVE, atBoot: "disabled" });
const apt = unit({ name: "apt-daily.service", state: SystemServiceState.INACTIVE, atBoot: "static" });

const all = [nginx, docker, ssh, dockwell, redis, apt];

function listOf(list: SystemServiceInfo[], acts = true) {
  return create(WatchSystemServicesResponseSchema, {
    list: create(SystemServiceListSchema, { init: SystemInit.SYSTEMD, services: list, acts }),
  });
}

const host = { kind: "server.host", scope: "srv_1", name: "host" };

/** The plan of an action on a unit of hz-1, as the control plane answers it. */
function unitChange(u: MessageInitShape<typeof HostUnitSchema>, summary: string): Change {
  return create(ChangeSchema, {
    id: "chg_unit",
    summary,
    state: ChangeState.PLANNED,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(new Date()),
    plan: {
      fingerprint: "1a2b3c4d5e6f708192a3b4c5d6e7f809",
      hostAccess: true,
      resources: [{ id: "res_host", resource: host, action: PlanAction.UPDATE, servers: ["srv_1"], unit: u }],
      servers: [
        {
          serverId: "srv_1",
          serverName: "hz-1",
          steps: [{ resourceId: "res_host", resource: host, action: PlanAction.UPDATE, unit: u }],
        },
      ],
    },
  });
}

async function renderServices(list: SystemServiceInfo[], api: Api = {}, acts = true) {
  const servers = feed<WatchServersResponse>();
  servers.push(changed(aServer({ id: "srv_1", name: "hz-1" })), snapshotComplete);
  const system = feed<WatchSystemServicesResponse>();
  system.push(listOf(list, acts));
  renderAt("/servers/srv_1/services", {
    ...api,
    servers: {
      watchServers: servers.stream,
      watchSystemServices: system.stream,
      readSystemLog: () => ({ lines: [], more: false, source: SystemLogSource.JOURNAL }),
      ...api.servers,
    },
    services: { listServices: () => ({ services: [] }) },
    websites: { listWebsites: () => ({ websites: [] }) },
    databases: { listDatabases: () => ({ databases: [] }) },
    containers: { listContainers: () => ({ containers: [] }) },
  });
  return screen.findByRole("table", { name: "System services on hz-1" });
}

const row = (table: HTMLElement, name: string) =>
  within(table).getByRole("row", { name: new RegExp(name.replace(".", "\\.")) });

describe("acting on system services", () => {
  it("restarts a failed unit from its row, which says why it failed, through a plan with nothing to type", async () => {
    const user = userEvent.setup();
    const plan = vi.fn((_req: PlanSystemServiceActionRequest) => ({
      change: unitChange(
        { name: "nginx.service", action: SystemServiceAction.RESTART, wasState: "failed", wasAtBoot: "enabled" },
        "Restart nginx.service on hz-1",
      ),
    }));
    const table = await renderServices(all, { changes: { planSystemServiceAction: plan } });
    const failed = row(table, "nginx.service");
    expect(failed).toHaveTextContent("nginx: [emerg] bind() to 0.0.0.0:80 failed (98: Address already in use)");
    await user.click(within(failed).getByRole("button", { name: "Restart nginx.service" }));
    await waitFor(() => expect(plan).toHaveBeenCalledTimes(1));
    expect(plan.mock.calls[0]?.[0]).toMatchObject({
      serverIds: ["srv_1"],
      name: "nginx.service",
      action: SystemServiceAction.RESTART,
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await waitFor(() => expect(sheet).toHaveTextContent("Restart nginx.service on hz-1"));
    expect(sheet).toHaveTextContent("what it serves pauses while it restarts");
    expect(sheet).toHaveTextContent("No undo");
    expect(within(sheet).queryByRole("textbox")).toBeNull();
    expect(sheet).not.toHaveTextContent("passkey");
  });

  it("offers each action off, with why, where Dockwell needs the unit or the init refuses", async () => {
    const user = userEvent.setup();
    const table = await renderServices(all);
    expect(
      within(row(table, "dockwell.service")).getByRole("button", { name: "Restart dockwell.service" }),
    ).toHaveAccessibleDescription("Dockwell runs as this service");
    expect(
      within(row(table, "docker.service")).getByRole("button", { name: "Restart docker.service" }),
    ).not.toHaveAttribute("aria-disabled");
    await user.click(row(table, "docker.service"));
    const sheet = await screen.findByRole("dialog", { name: "docker.service" });
    expect(within(sheet).getByRole("button", { name: "Stop" })).toHaveAccessibleDescription(
      "Every app on hz-1 runs in Docker",
    );
    expect(within(sheet).getByRole("button", { name: "Don't start at boot" })).toHaveAccessibleDescription(
      "Every app on hz-1 runs in Docker",
    );
    await expectAccessible(sheet);
    await user.keyboard("{Escape}");
    await user.click(row(table, "apt-daily.service"));
    const static_ = await screen.findByRole("dialog", { name: "apt-daily.service" });
    expect(within(static_).queryByRole("button", { name: "Start at boot" })).toBeNull();
    expect(within(static_).getByRole("button", { name: "Start" })).not.toHaveAttribute("aria-disabled");
  });

  it("says a restart of SSH asks for the passkey, and one of Docker the typed code too", async () => {
    const user = userEvent.setup();
    let next = unitChange(
      { name: "ssh.service", action: SystemServiceAction.RESTART, guard: SystemServiceGuard.SSH, passkey: true },
      "Restart ssh.service on hz-1",
    );
    const table = await renderServices(all, { changes: { planSystemServiceAction: () => ({ change: next }) } });
    await user.click(within(row(table, "ssh.service")).getByRole("button", { name: "Restart ssh.service" }));
    let sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(sheet).findByText("Applying this asks for your passkey")).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Review plan" })).toBeNull());

    next = unitChange(
      {
        name: "docker.service",
        action: SystemServiceAction.RESTART,
        guard: SystemServiceGuard.DOCKER,
        passkey: true,
        confirm: true,
        dockerRestarts: true,
      },
      "Restart docker.service on hz-1",
    );
    await user.click(within(row(table, "docker.service")).getByRole("button", { name: "Restart docker.service" }));
    sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(sheet).findByRole("textbox")).toBeVisible();
    expect(sheet).toHaveTextContent("apps and websites stop while Docker restarts");
  });

  it("reads a unit's journal in its sheet, by level", async () => {
    const user = userEvent.setup();
    const read = vi.fn((req: ReadSystemLogRequest) => ({
      lines:
        req.level === LogLevel.ERROR
          ? [
              {
                cursor: "c2",
                unit: "nginx.service",
                pid: 40,
                level: LogLevel.ERROR,
                message: "bind() failed",
                time: ago(60_000),
              },
            ]
          : [
              {
                cursor: "c2",
                unit: "nginx.service",
                pid: 40,
                level: LogLevel.ERROR,
                message: "bind() failed",
                time: ago(60_000),
              },
              {
                cursor: "c1",
                unit: "nginx.service",
                pid: 40,
                level: LogLevel.INFO,
                message: "starting",
                time: ago(120_000),
              },
            ],
      more: false,
      source: SystemLogSource.JOURNAL,
    }));
    const table = await renderServices(all, { servers: { readSystemLog: read } });
    await user.click(row(table, "nginx.service"));
    const sheet = await screen.findByRole("dialog", { name: "nginx.service" });
    const log = within(sheet).getByRole("region", { name: "System log of nginx.service" });
    expect(await within(log).findByText("starting")).toBeVisible();
    expect(read.mock.calls[0]?.[0]).toMatchObject({ serverId: "srv_1", unit: "nginx.service", limit: 50 });
    await user.click(within(log).getByRole("radio", { name: "Errors" }));
    await waitFor(() => expect(within(log).queryByText("starting")).toBeNull());
    expect(within(log).getByText("bind() failed")).toBeVisible();
    expect(read.mock.calls.at(-1)?.[0]).toMatchObject({ level: LogLevel.ERROR });
    // A failed unit leads with why, and Restart.
    expect(
      within(sheet).getByText("nginx: [emerg] bind() to 0.0.0.0:80 failed (98: Address already in use)"),
    ).toBeVisible();
    expect(within(sheet).getAllByRole("button", { name: "Restart" })).toHaveLength(1);
  });

  it("offers nothing to do through an older agent, and says why", async () => {
    const table = await renderServices(all, {}, false);
    expect(
      within(row(table, "redis-server.service")).getByRole("button", { name: "Start redis-server.service" }),
    ).toHaveAccessibleDescription("Its agent is older than the control plane, so it can't act on system services");
  });
});
