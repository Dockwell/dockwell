// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  GetProcessResponseSchema,
  ListSocketsResponseSchema,
  ProcessDetailSchema,
  ProcessListSchema,
  ProcessSchema,
  SocketConnectionSchema,
  type WatchProcessesResponse,
  WatchProcessesResponseSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { anApp, ENV } from "../../test/apps";
import { feed, renderAt } from "../../test/render";
import { aServer, changed, history, snapshotComplete } from "../../test/servers";

const MB = 1024 ** 2;
const started = (msAgo: number) => timestampFromDate(new Date(Date.now() - msAgo));

const hz1 = aServer({
  id: "srv_1",
  name: "hz-1",
  stats: history(23.4).at(-1),
  inventory: { cpuCount: 4, osName: "Ubuntu" },
});

const procList = create(WatchProcessesResponseSchema, {
  processes: create(ProcessListSchema, {
    total: 2,
    cpuWindow: durationFromMs(2_000),
    processes: [
      create(ProcessSchema, {
        pid: 100,
        parentPid: 1,
        name: "nginx",
        command: "/usr/sbin/nginx -g daemon off;",
        user: "root",
        uid: 0,
        threads: 4,
        cpuPercent: 1.5,
        memoryBytes: BigInt(48 * MB),
        startTime: started(3_600_000),
        startTicks: 500n,
        unit: "nginx.service",
      }),
      create(ProcessSchema, {
        pid: 200,
        parentPid: 100,
        name: "api",
        command: "node server.js --api-key …",
        user: "app",
        uid: 1000,
        threads: 8,
        cpuPercent: 4.2,
        memoryBytes: BigInt(180 * MB),
        startTime: started(120_000),
        startTicks: 900n,
        container: {
          id: "c".repeat(64),
          name: "shop-api-1",
          composeProject: ENV,
          composeService: "api",
          restartPolicy: "unless-stopped",
        },
        containerMain: true,
      }),
    ],
  }),
});

describe("Processes detail sheet and Connections tab", () => {
  it("opens process detail on row click with full redacted command, parent link, and sockets", async () => {
    const user = userEvent.setup();
    const servers = feed<WatchServersResponse>();
    servers.push(changed(hz1), snapshotComplete);
    const processes = feed<WatchProcessesResponse>();

    renderAt("/servers/srv_1/processes", {
      servers: { watchServers: servers.stream },
      services: { listServices: () => ({ services: [anApp("api", "node:22")] }) },
      processes: {
        watchProcesses: processes.stream,
        getProcess: (req) =>
          create(GetProcessResponseSchema, {
            process: create(ProcessDetailSchema, {
              process: procList.processes?.processes.find((p) => p.pid === req.pid),
              fullCommand:
                req.pid === 200
                  ? "node server.js --api-key … DB_PASSWORD=… --port 8080"
                  : "/usr/sbin/nginx -g daemon off;",
              parentName: req.pid === 200 ? "nginx" : "systemd",
              threads: req.pid === 200 ? 8 : 4,
              state: "sleeping",
              socketsTotal: 1,
              sockets: [
                create(SocketConnectionSchema, {
                  protocol: "tcp",
                  localAddress: "0.0.0.0",
                  localPort: req.pid === 200 ? 8080 : 80,
                  remoteAddress: "0.0.0.0",
                  remotePort: 0,
                  state: "LISTEN",
                  pid: req.pid,
                }),
              ],
            }),
          }),
      },
    });

    await screen.findByRole("heading", { level: 1, name: "hz-1" });
    processes.push(procList);
    const table = await screen.findByRole("table", { name: "Processes on hz-1" });

    // Clicking the api row opens the detail sheet.
    const apiRow = within(table).getByText("api").closest("tr");
    expect(apiRow).not.toBeNull();
    await user.click(apiRow as HTMLElement);

    const dialog = await screen.findByRole("dialog", { name: "api" });
    expect(await within(dialog).findByText("node server.js --api-key … DB_PASSWORD=… --port 8080")).toBeVisible();
    expect(within(dialog).getByText(/0\.0\.0\.0:8080/)).toBeVisible();
    expect(within(dialog).getByText("8")).toBeVisible();

    // Clicking the parent switches the sheet to PID 100 (nginx).
    await user.click(within(dialog).getByRole("button", { name: /nginx \(100\)/ }));
    await screen.findByRole("dialog", { name: "nginx" });
    expect(await screen.findByText("/usr/sbin/nginx -g daemon off;")).toBeVisible();
  });

  it("switches to Connections tab and filters by protocol, state, and search text", async () => {
    const user = userEvent.setup();
    const servers = feed<WatchServersResponse>();
    servers.push(changed(hz1), snapshotComplete);
    const processes = feed<WatchProcessesResponse>();

    renderAt("/servers/srv_1/processes", {
      servers: { watchServers: servers.stream },
      services: { listServices: () => ({ services: [anApp("api", "node:22")] }) },
      processes: {
        watchProcesses: processes.stream,
        listSockets: () =>
          create(ListSocketsResponseSchema, {
            total: 2,
            connections: [
              create(SocketConnectionSchema, {
                protocol: "tcp",
                localAddress: "0.0.0.0",
                localPort: 80,
                remoteAddress: "0.0.0.0",
                remotePort: 0,
                state: "LISTEN",
                pid: 100,
                startTicks: 500n,
                processName: "nginx",
                user: "root",
                unit: "nginx.service",
              }),
              create(SocketConnectionSchema, {
                protocol: "udp",
                localAddress: "127.0.0.1",
                localPort: 53,
                remoteAddress: "0.0.0.0",
                remotePort: 0,
                state: "UNCONN",
                pid: 200,
                startTicks: 900n,
                processName: "api",
                user: "app",
              }),
            ],
          }),
      },
    });

    await screen.findByRole("heading", { level: 1, name: "hz-1" });
    processes.push(procList);

    await user.click(screen.getByRole("radio", { name: "Connections" }));
    const connTable = await screen.findByRole("table", { name: "Connections on hz-1" });
    expect(within(connTable).getByText("0.0.0.0:80")).toBeVisible();
    expect(within(connTable).getByText("127.0.0.1:53")).toBeVisible();

    // Filter by protocol = udp.
    await user.selectOptions(screen.getByRole("combobox", { name: "Filter by protocol" }), "udp");
    expect(within(connTable).queryByText("0.0.0.0:80")).toBeNull();
    expect(within(connTable).getByText("127.0.0.1:53")).toBeVisible();

    // Reset protocol and filter by port text "80".
    await user.selectOptions(screen.getByRole("combobox", { name: "Filter by protocol" }), "");
    await user.type(screen.getByRole("searchbox", { name: "Filter connections" }), ":80");
    expect(within(connTable).getByText("0.0.0.0:80")).toBeVisible();
    expect(within(connTable).queryByText("127.0.0.1:53")).toBeNull();
  });
});
