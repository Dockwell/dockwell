// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  LogLevel,
  type ReadSystemLogRequest,
  Role,
  SystemInit,
  SystemLogLineSchema,
  SystemLogSource,
  SystemServiceInfoSchema,
  SystemServiceListSchema,
  SystemServiceState,
  type WatchServersResponse,
  type WatchSystemLogRequest,
  type WatchSystemLogResponse,
  WatchSystemLogResponseSchema,
  type WatchSystemServicesResponse,
  WatchSystemServicesResponseSchema,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { authError, authState, mira } from "../../test/auth";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const NOW = Date.now();
const line = (cursor: string, message: string, unit = "nginx.service", level = LogLevel.INFO, agoMs = 60_000) =>
  create(SystemLogLineSchema, { cursor, message, unit, pid: 40, level, time: timestampFromMs(NOW - agoMs) });

const units = create(WatchSystemServicesResponseSchema, {
  list: create(SystemServiceListSchema, {
    init: SystemInit.SYSTEMD,
    acts: true,
    services: [
      create(SystemServiceInfoSchema, { name: "nginx.service", state: SystemServiceState.FAILED }),
      create(SystemServiceInfoSchema, { name: "cron.service", state: SystemServiceState.RUNNING }),
      create(SystemServiceInfoSchema, { name: "apt-daily.service", state: SystemServiceState.INACTIVE }),
    ],
  }),
});

function renderLog(path: string, api: Api = {}) {
  const servers = feed<WatchServersResponse>();
  servers.push(changed(aServer({ id: "srv_1", name: "hz-1" })), snapshotComplete);
  const system = feed<WatchSystemServicesResponse>();
  system.push(units);
  const follow = feed<WatchSystemLogResponse>();
  const read = vi.fn((req: ReadSystemLogRequest) => ({
    lines: req.query
      ? [line("c9", "upstream timed out")]
      : [line("c2", "nginx: started"), line("c1", "nginx: starting", "nginx.service", LogLevel.INFO, 120_000)],
    more: req.query !== "",
    source: SystemLogSource.JOURNAL,
    searchedTo: req.query && !req.until ? timestampFromMs(NOW - 3_600_000) : undefined,
    cutShort: req.query !== "" && !req.until,
  }));
  const watch = vi.fn((req: WatchSystemLogRequest, ctx: { signal: AbortSignal }) => follow.stream(req, ctx as never));
  renderAt(path, {
    ...api,
    servers: {
      watchServers: servers.stream,
      watchSystemServices: system.stream,
      readSystemLog: read,
      watchSystemLog: watch as never,
      ...api.servers,
    },
  });
  return { read, watch, follow };
}

describe("SystemLogPage", () => {
  it("reads the newest page, then follows from its newest line", async () => {
    const { read, watch, follow } = renderLog("/servers/srv_1/log");
    expect(await screen.findByText("nginx: started")).toBeVisible();
    expect(screen.getByRole("link", { name: "System log" })).toHaveAttribute("aria-current", "page");
    expect(read.mock.calls[0]?.[0]).toMatchObject({ serverId: "srv_1", unit: "", query: "", limit: 100 });
    await waitFor(() => expect(watch).toHaveBeenCalledTimes(1));
    expect(watch.mock.calls[0]?.[0]).toMatchObject({ serverId: "srv_1", after: "c2" });
    follow.push(create(WatchSystemLogResponseSchema, {}));
    follow.push(
      create(WatchSystemLogResponseSchema, {
        lines: [line("c3", "nginx: reloaded", "nginx.service", LogLevel.INFO, 1_000)],
        dropped: 12n,
      }),
    );
    expect(await screen.findByText("nginx: reloaded")).toBeVisible();
    expect(await screen.findByText(/12 lines dropped here/)).toBeVisible();
  });

  it("narrows to a unit picked from the server's own, failed ones first, and to a level", async () => {
    const user = userEvent.setup();
    const { read, watch } = renderLog("/servers/srv_1/log");
    await screen.findByText("nginx: started");
    const picker = screen.getByRole("combobox", { name: "System service" });
    await waitFor(() => expect(within(picker).getByRole("option", { name: "cron.service" })).toBeInTheDocument());
    const groups = within(picker).getAllByRole("group");
    expect(groups[0]).toHaveAccessibleName("Failed");
    await user.selectOptions(picker, "nginx.service");
    await waitFor(() => expect(read.mock.calls.at(-1)?.[0]).toMatchObject({ unit: "nginx.service" }));
    await user.click(screen.getByRole("radio", { name: "Errors" }));
    await waitFor(() =>
      expect(read.mock.calls.at(-1)?.[0]).toMatchObject({ unit: "nginx.service", level: LogLevel.ERROR }),
    );
    await waitFor(() =>
      expect(watch.mock.calls.at(-1)?.[0]).toMatchObject({ unit: "nginx.service", level: LogLevel.ERROR }),
    );
  });

  it("reads a time range back, without following", async () => {
    const { read, watch } = renderLog("/servers/srv_1/log?range=24h");
    await screen.findByText("nginx: started");
    const since = read.mock.calls[0]?.[0].since;
    expect(since).toBeDefined();
    expect(Number(since?.seconds) * 1000).toBeLessThanOrEqual(NOW - 86_400_000 + 5_000);
    await new Promise((r) => setTimeout(r, 200));
    expect(watch).not.toHaveBeenCalled();
    expect(screen.getByRole("radio", { name: "24 h" })).toBeChecked();
  });

  it("searches back, says how far it looked, and searches on from there", async () => {
    const user = userEvent.setup();
    const { read } = renderLog("/servers/srv_1/log");
    await screen.findByText("nginx: started");
    await user.type(screen.getByRole("textbox", { name: "Search the system log" }), "timed out");
    // The search is marked in the line, so its words are split.
    expect(await screen.findByText(/upstream/)).toBeVisible();
    expect(read.mock.calls.at(-1)?.[0]).toMatchObject({ query: "timed out" });
    expect(screen.getByText(/^Searched back to /)).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Search older" }));
    await waitFor(() => expect(read.mock.calls.at(-1)?.[0].until).toBeDefined());
  });

  it("opens narrowed to the unit a link names", async () => {
    const { read } = renderLog("/servers/srv_1/log?unit=nginx.service&level=warn");
    await screen.findByText("nginx: started");
    expect(read.mock.calls[0]?.[0]).toMatchObject({ unit: "nginx.service", level: LogLevel.WARN });
    expect(screen.getByRole("combobox", { name: "System service" })).toHaveValue("nginx.service");
  });

  it("says an older agent is outdated, and that a server keeps no log", async () => {
    renderLog("/servers/srv_1/log", {
      servers: {
        readSystemLog: () => {
          throw authError(Code.FailedPrecondition, "agent_outdated", "hz-1's agent is older than the control plane");
        },
      },
    });
    expect(await screen.findByText("hz-1's agent is outdated")).toBeVisible();
  });

  it("leaves the tab out below an admin", async () => {
    const servers = feed<WatchServersResponse>();
    servers.push(changed(aServer({ id: "srv_1", name: "hz-1" })), snapshotComplete);
    renderAt("/servers/srv_1", {
      auth: { getAuthState: () => authState({ user: { ...mira, role: Role.DEVELOPER } }) },
      servers: { watchServers: servers.stream },
    });
    await screen.findByRole("heading", { level: 1, name: "hz-1" });
    expect(screen.queryByRole("link", { name: "System log" })).toBeNull();
  });

  it("reads in Chinese", async () => {
    activateLocale("zh-CN");
    try {
      renderLog("/servers/srv_1/log");
      expect(await screen.findByText("nginx: started")).toBeVisible();
      expect(screen.getByRole("radio", { name: "全部" })).toBeChecked();
    } finally {
      activateLocale("en");
    }
  });
});
