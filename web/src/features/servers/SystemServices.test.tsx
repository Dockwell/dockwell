// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  Role,
  SystemInit,
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
import { describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, authState, mira } from "../../test/auth";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const MB = 1024 ** 2;
const ago = (ms: number) => timestampFromDate(new Date(Date.now() - ms));

const unit = (s: MessageInitShape<typeof SystemServiceInfoSchema>) => create(SystemServiceInfoSchema, s);

/** A systemd server's services, as the agent sorts them: failed first, then running, then the rest. */
const services = [
  unit({
    name: "nginx.service",
    description: "A high performance web server",
    state: SystemServiceState.FAILED,
    detail: "failed",
    atBoot: "enabled",
    since: ago(25 * 60_000),
    result: "exit-code",
    exitStatus: 1,
  }),
  unit({
    name: "docker.service",
    description: "Docker Application Container Engine",
    state: SystemServiceState.RUNNING,
    detail: "running",
    atBoot: "enabled",
    since: ago(2 * 86_400_000),
    mainPid: 1024,
    memoryBytes: BigInt(84 * MB),
    needed: true,
  }),
  unit({
    name: "cron.service",
    description: "Regular background program processing daemon",
    state: SystemServiceState.RUNNING,
    detail: "running",
    atBoot: "enabled",
    since: ago(2 * 86_400_000),
    mainPid: 790,
  }),
  unit({
    name: "redis-server.service",
    description: "Advanced key-value store",
    state: SystemServiceState.INACTIVE,
    detail: "dead",
    atBoot: "disabled",
  }),
];

const listOf = (init: SystemInit, list: SystemServiceInfo[]) =>
  create(WatchSystemServicesResponseSchema, {
    list: create(SystemServiceListSchema, { init, services: list, acts: true }),
  });

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

async function renderServices(api: Api = {}) {
  const servers = feed<WatchServersResponse>();
  servers.push(changed(hz1), snapshotComplete);
  const system = feed<WatchSystemServicesResponse>();
  const rendered = renderAt("/servers/srv_1/services", {
    ...api,
    servers: { watchServers: servers.stream, watchSystemServices: system.stream, ...api.servers },
  });
  await screen.findByRole("heading", { level: 1, name: "hz-1" });
  return { ...rendered, system };
}

const rows = () =>
  within(screen.getByRole("table", { name: "System services on hz-1" }))
    .getAllByRole("row")
    .slice(1);

describe("SystemServicesPage", () => {
  it("lists a server's system services, failed first, with when and why one failed", async () => {
    const { system } = await renderServices();
    expect(screen.getByRole("link", { name: "System services" })).toHaveAttribute("aria-current", "page");
    system.push(listOf(SystemInit.SYSTEMD, services));
    await screen.findByRole("table", { name: "System services on hz-1" });
    expect(rows()[0]).toHaveTextContent(
      /^nginx\.serviceA high performance web serverFailed25 min ago · exit-code, status 1Starts––Restart$/,
    );
    expect(rows()[1]).toHaveTextContent("docker.serviceDockwell needs itDocker Application Container EngineRunning");
    expect(rows()[1]).toHaveTextContent("84 MB1024");
    expect(rows()[3]).toHaveTextContent("redis-server.serviceAdvanced key-value storeInactiveDoesn't start––");
    expect(screen.getByText("Live")).toBeVisible();
  });

  it("shows the failed or running ones, and finds by name or description", async () => {
    const user = userEvent.setup();
    const { system } = await renderServices();
    system.push(listOf(SystemInit.SYSTEMD, services));
    await screen.findByRole("table", { name: "System services on hz-1" });
    const show = screen.getByRole("radiogroup", { name: "Show" });
    expect(show).toHaveTextContent("All, 4Failed, 1Running, 2");
    await user.click(within(show).getByRole("radio", { name: /Failed/ }));
    expect(rows().map((r) => r.textContent?.split(".service")[0])).toEqual(["nginx"]);
    await user.click(within(show).getByRole("radio", { name: /Running/ }));
    expect(rows().map((r) => r.textContent?.split(".service")[0])).toEqual(["docker", "cron"]);
    await user.click(within(show).getByRole("radio", { name: /All/ }));
    await user.type(screen.getByRole("textbox", { name: "Filter system services" }), "key-value");
    await waitFor(() => expect(rows()).toHaveLength(1));
    expect(rows()[0]).toHaveTextContent("redis-server.service");
    await user.clear(screen.getByRole("textbox", { name: "Filter system services" }));
    await user.type(screen.getByRole("textbox", { name: "Filter system services" }), "nothing like it");
    expect(await screen.findByText("No system service matches.")).toBeVisible();
  });

  it("opens a service's facts from its row", async () => {
    const user = userEvent.setup();
    const { system } = await renderServices();
    system.push(listOf(SystemInit.SYSTEMD, services));
    await screen.findByRole("table", { name: "System services on hz-1" });
    await user.click(rows()[1] as HTMLElement);
    const sheet = await screen.findByRole("dialog", { name: "docker.service" });
    expect(sheet).toHaveTextContent("Docker Application Container Engine");
    expect(sheet).toHaveTextContent("Main process1024");
    expect(sheet).toHaveTextContent("Memory84 MB");
    expect(sheet).toHaveTextContent("At bootStarts enabled");
    expect(sheet).toHaveTextContent("DockwellNeeds it on hz-1");
    await expectAccessible(sheet);
  });

  it("says when the server's init isn't one Dockwell reads", async () => {
    const { system } = await renderServices();
    system.push(listOf(SystemInit.OTHER, []));
    expect(
      await screen.findByText(
        "hz-1 starts its services with an init Dockwell doesn't read. It reads systemd's and OpenRC's.",
      ),
    ).toBeVisible();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("says an older agent is outdated, with the way to update it, and stops asking", async () => {
    let asked = 0;
    await renderServices({
      servers: {
        // biome-ignore lint/correctness/useYield: it fails before it would send
        watchSystemServices: async function* () {
          asked++;
          // What the control plane sends for an agent that doesn't know the call.
          throw authError(
            Code.FailedPrecondition,
            "agent_outdated",
            "hz-1's agent is older than the control plane, and can't list system services. Update its agent, then look again",
          );
        },
      },
    });
    expect(await screen.findByText("hz-1's agent is outdated")).toBeVisible();
    expect(screen.getByText("It's older than the control plane, so it can't list system services.")).toBeVisible();
    expect(screen.getByRole("link", { name: "Update its agent" })).toHaveAttribute("href", "/servers/srv_1/updates");
    await new Promise((resolve) => setTimeout(resolve, 1_200));
    expect(asked).toBe(1);
  });

  it("says nothing has failed when nothing has, and what nothing matches", async () => {
    const user = userEvent.setup();
    const { system } = await renderServices();
    system.push(
      listOf(
        SystemInit.SYSTEMD,
        services.filter((s) => s.state !== SystemServiceState.FAILED),
      ),
    );
    await screen.findByRole("table", { name: "System services on hz-1" });
    await user.click(within(screen.getByRole("radiogroup", { name: "Show" })).getByRole("radio", { name: /Failed/ }));
    expect(await screen.findByText("Nothing has failed on hz-1.")).toBeVisible();
    expect(screen.queryByText("No system service matches.")).toBeNull();
  });

  it("lists them as rows on a phone, each opening its facts", async () => {
    onPhone();
    const user = userEvent.setup();
    const { system } = await renderServices();
    system.push(listOf(SystemInit.SYSTEMD, services));
    const list = await screen.findByRole("list", { name: "System services" });
    const first = within(list).getAllByRole("listitem")[0] as HTMLElement;
    expect(first).toHaveTextContent("nginx.service");
    await user.click(within(first).getByRole("button"));
    expect(await screen.findByRole("dialog", { name: "nginx.service" })).toBeVisible();
  });

  it("leaves the tab out below an admin", async () => {
    const servers = feed<WatchServersResponse>();
    servers.push(changed(hz1), snapshotComplete);
    renderAt("/servers/srv_1", {
      auth: { getAuthState: () => authState({ user: { ...mira, role: Role.DEVELOPER } }) },
      servers: { watchServers: servers.stream },
    });
    await screen.findByRole("heading", { level: 1, name: "hz-1" });
    expect(screen.queryByRole("link", { name: "System services" })).toBeNull();
  });

  it("reads in Chinese", async () => {
    activateLocale("zh-CN");
    try {
      const { system } = await renderServices();
      system.push(listOf(SystemInit.SYSTEMD, services));
      await screen.findByRole("table");
      expect(screen.getByText("失败", { selector: ".dw-badge *, .dw-status *" })).toBeVisible();
    } finally {
      activateLocale("en");
    }
  });
});
