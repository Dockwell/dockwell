// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type EndProcessRequest,
  ProcessListSchema,
  ProcessSchema,
  StepUpAction,
  type WatchProcessesResponse,
  WatchProcessesResponseSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { anApp, ENV } from "../../test/apps";
import { authError, stubPasskeys } from "../../test/auth";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, history, snapshotComplete } from "../../test/servers";

const MB = 1024 ** 2;
const started = (msAgo: number) => timestampFromDate(new Date(Date.now() - msAgo));

const list = (measured: boolean) =>
  create(WatchProcessesResponseSchema, {
    processes: create(ProcessListSchema, {
      total: 4,
      ...(measured ? { cpuWindow: durationFromMs(2_000) } : {}),
      processes: [
        create(ProcessSchema, {
          pid: 1184,
          name: "postgres",
          command: "postgres -D /var/lib/postgresql/data",
          user: "postgres",
          cpuPercent: 6.1,
          memoryBytes: BigInt(312 * MB),
          startTime: started(41 * 86_400_000),
          startTicks: 5000n,
          container: {
            id: "c".repeat(64),
            name: "shop-db-1",
            composeProject: ENV,
            composeService: "db",
            restartPolicy: "unless-stopped",
          },
          containerMain: true,
        }),
        create(ProcessSchema, {
          pid: 812,
          name: "dockerd",
          command: "/usr/bin/dockerd -H fd://",
          user: "root",
          cpuPercent: 2.2,
          memoryBytes: BigInt(72 * MB),
          startTime: started(41 * 86_400_000),
          startTicks: 300n,
          unit: "docker.service",
          stops: "containers",
        }),
        create(ProcessSchema, {
          pid: 731,
          name: "dockwell",
          command: "/usr/local/bin/dockwell agent",
          user: "root",
          cpuPercent: 1.1,
          memoryBytes: BigInt(38 * MB),
          startTime: started(41 * 86_400_000),
          startTicks: 290n,
          unit: "dockwell.service",
          protected: "dockwell",
        }),
        create(ProcessSchema, {
          pid: 48213,
          name: "worker",
          command: "worker --queue default",
          user: "nonroot",
          cpuPercent: 0.3,
          memoryBytes: BigInt(301 * MB),
          startTime: started(12_000),
          startTicks: 99000n,
        }),
      ],
    }),
  });

const hz1 = aServer({
  id: "srv_1",
  name: "hz-1",
  stats: history(23.4).at(-1),
  inventory: { cpuCount: 4, osName: "Ubuntu" },
});

async function renderProcesses(api: Api = {}) {
  const servers = feed<WatchServersResponse>();
  servers.push(changed(hz1), snapshotComplete);
  const processes = feed<WatchProcessesResponse>();
  const rendered = renderAt("/servers/srv_1/processes", {
    ...api,
    servers: { watchServers: servers.stream },
    services: { listServices: () => ({ services: [anApp("db", "postgres:17")] }) },
    processes: { watchProcesses: processes.stream, ...api.processes },
  });
  await screen.findByRole("heading", { level: 1, name: "hz-1" });
  return { ...rendered, processes };
}

const rows = () =>
  within(screen.getByRole("table", { name: "Processes on hz-1" }))
    .getAllByRole("row")
    .slice(1);

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("ProcessesPage", () => {
  it("lists what runs, busiest first, with what each belongs to", async () => {
    const { processes } = await renderProcesses();
    expect(screen.getByRole("link", { name: "Processes" })).toHaveAttribute("aria-current", "page");
    processes.push(list(false));
    await screen.findByRole("table", { name: "Processes on hz-1" });
    // Until a second list comes, there's nothing to measure CPU against.
    expect(rows()[0]).toHaveTextContent("postgresApp db1184postgres–312 MB41 days");
    processes.push(list(true));
    await waitFor(() => expect(rows()[0]).toHaveTextContent("6.1%"));
    expect(rows()[1]).toHaveTextContent("dockerdService docker812root2.2%72 MB");
    expect(rows()[2]).toHaveTextContent("dockwellDockwell");
    expect(within(rows()[2] as HTMLElement).getByText("Protected")).toBeVisible();
    expect(within(rows()[2] as HTMLElement).queryByRole("button")).toBeNull();
    expect(rows()[3]).toHaveTextContent(/^workerworker --queue default48213nonroot0\.3%301 MB1\d sStop$/);
    expect(screen.getByText("Live")).toBeVisible();
    expect(screen.getByText(/All 4 processes\./)).toBeVisible();
    const totals = screen.getByRole("region", { name: "Totals" });
    expect(totals).toHaveTextContent("4 processes");
    expect(within(totals).getByRole("meter", { name: "CPU" })).toHaveAttribute("aria-valuenow", "23");
    expect(totals).toHaveTextContent("Load average 0.84 / 0.91 / 0.88 on 4 vCPU");
    const services = screen.getByRole("region", { name: "Services" });
    expect(services).toHaveTextContent("2 systemd units");
    expect(services).toHaveTextContent("docker1 process · 2.2% · 72 MB");
  });

  it("sorts by a column, either way, and finds by name, PID, user or what it belongs to", async () => {
    const user = userEvent.setup();
    const { processes } = await renderProcesses();
    processes.push(list(true));
    await screen.findByRole("table", { name: "Processes on hz-1" });
    const memory = screen.getByRole("button", { name: "Memory" });
    await user.click(memory);
    expect(memory.closest("th")).toHaveAttribute("aria-sort", "descending");
    expect(rows().map((r) => r.querySelector(".procs-name")?.textContent)).toEqual([
      "postgres",
      "worker",
      "dockerd",
      "dockwell",
    ]);
    await user.click(memory);
    expect(memory.closest("th")).toHaveAttribute("aria-sort", "ascending");
    expect(rows()[0]).toHaveTextContent("dockwell");

    const filter = screen.getByRole("textbox", { name: "Filter processes" });
    await user.type(filter, "shop-db");
    await waitFor(() => expect(rows()).toHaveLength(1));
    expect(rows()[0]).toHaveTextContent("postgres");
    await user.clear(filter);
    await user.type(filter, "nonroot");
    await waitFor(() => expect(rows()[0]).toHaveTextContent("worker"));
    await user.clear(filter);
    await user.type(filter, "nothing-like-it");
    await waitFor(() => expect(screen.getByText("Nothing that runs matches.")).toBeVisible());
  });

  it("groups by container and by service", async () => {
    const user = userEvent.setup();
    const { processes } = await renderProcesses();
    processes.push(list(true));
    await screen.findByRole("table", { name: "Processes on hz-1" });
    await user.click(screen.getByRole("radio", { name: /Containers/ }));
    const containers = await screen.findByRole("table", { name: "Containers on hz-1" });
    expect(containers).toHaveTextContent("shop-db-1App db16.1%312 MB");
    await user.click(screen.getByRole("radio", { name: /Services/ }));
    const services = await screen.findByRole("table", { name: "Services on hz-1" });
    expect(services).toHaveTextContent("docker12.2%72 MB");
    expect(services).toHaveTextContent("dockwell11.1%38 MB");
  });

  it("stops a process with a passkey, once it has said what that does", async () => {
    stubPasskeys();
    const user = userEvent.setup();
    const ended: EndProcessRequest[] = [];
    const steps: { action: StepUpAction; target: string }[] = [];
    const { processes } = await renderProcesses({
      auth: {
        beginStepUp: (req) => {
          steps.push({ action: req.action, target: req.target });
          return { ceremonyId: "cer_1", optionsJson: JSON.stringify({ challenge: "AAAA", allowCredentials: [] }) };
        },
        finishStepUp: () => ({ proof: "proof-1" }),
      },
      processes: {
        endProcess: (req) => {
          ended.push(req);
          if (!req.proof) throw authError(Code.PermissionDenied, "needs_step_up", "this needs your passkey just now");
          return { killed: false };
        },
      },
    });
    processes.push(list(true));
    await screen.findByRole("table", { name: "Processes on hz-1" });
    await user.click(screen.getByRole("button", { name: "Stop postgres (PID 1184)" }));
    const sheet = await screen.findByRole("dialog", { name: "Stop postgres?" });
    expect(sheet).toHaveTextContent("It gets SIGTERM");
    expect(sheet).toHaveTextContent(
      "It's the main process of app db, which Docker starts again, as its restart policy says. To stop the app, stop it on its own page.",
    );
    expect(sheet).toHaveTextContent("Commandpostgres -D /var/lib/postgresql/data");
    expect(sheet).toHaveTextContent("It asks for your passkey, and goes on Activity");
    await user.click(within(sheet).getByRole("button", { name: "Stop process" }));
    expect(await screen.findByRole("dialog", { name: "Stopped postgres" })).toHaveTextContent("postgres stopped.");
    expect(steps).toEqual([{ action: StepUpAction.END_PROCESS, target: "srv_1:1184:5000" }]);
    expect(ended.map((r) => [r.pid, r.startTicks, r.proof])).toEqual([
      [1184, 5000n, ""],
      [1184, 5000n, "proof-1"],
    ]);
  });

  it("says Docker starts a container's process again only as its restart policy says, and only its main one", async () => {
    const user = userEvent.setup();
    const cases: [string, Partial<{ restartPolicy: string; containerMain: boolean }>][] = [
      [
        "isn't its main process, so nothing starts it again, and the container goes on running",
        { containerMain: false },
      ],
      ["which stops with it and stays stopped: its restart policy is no", { restartPolicy: "no" }],
      ["may stay stopped: Docker restarts it only after an error", { restartPolicy: "on-failure" }],
    ];
    for (const [words, change] of cases) {
      const { processes, unmount } = await renderProcesses();
      const l = list(true);
      const pg = l.processes?.processes[0];
      if (!pg?.container) throw new Error("the fixture has no postgres");
      if (change.restartPolicy) pg.container.restartPolicy = change.restartPolicy;
      if (change.containerMain !== undefined) pg.containerMain = change.containerMain;
      processes.push(l);
      await screen.findByRole("table", { name: "Processes on hz-1" });
      await user.click(screen.getByRole("button", { name: "Stop postgres (PID 1184)" }));
      const sheet = await screen.findByRole("dialog", { name: "Stop postgres?" });
      expect(sheet).toHaveTextContent(words);
      expect(sheet).not.toHaveTextContent("which Docker starts again");
      unmount();
    }
  });

  it("says what else stops with Docker, and why the SSH server is protected", async () => {
    const user = userEvent.setup();
    const { processes } = await renderProcesses();
    const l = list(true);
    l.processes?.processes.push(
      create(ProcessSchema, {
        pid: 700,
        name: "sshd",
        command: "sshd: /usr/sbin/sshd -D",
        user: "root",
        startTicks: 200n,
        protected: "ssh",
      }),
    );
    processes.push(l);
    await screen.findByRole("table", { name: "Processes on hz-1" });
    const sshd = rows().find((r) => r.textContent?.includes("sshd"));
    expect(sshd).toHaveTextContent("This SSH server takes new logins, a way back onto the server.");
    await user.click(screen.getByRole("button", { name: "Stop dockerd (PID 812)" }));
    const sheet = await screen.findByRole("dialog", { name: "Stop dockerd?" });
    expect(sheet).toHaveTextContent("Every container on hz-1 stops with it, as Docker needs it to run them.");
  });

  it("says why a process couldn't be stopped, and keeps the sheet open", async () => {
    stubPasskeys();
    const user = userEvent.setup();
    const { processes } = await renderProcesses({
      processes: {
        endProcess: () => {
          throw authError(
            Code.NotFound,
            "process_gone",
            "that process isn't running any more, so there's nothing to end. The list shows what runs now",
          );
        },
      },
    });
    processes.push(list(true));
    await screen.findByRole("table", { name: "Processes on hz-1" });
    await user.click(screen.getByRole("button", { name: "Stop worker (PID 48213)" }));
    const sheet = await screen.findByRole("dialog", { name: "Stop worker?" });
    await user.click(within(sheet).getByRole("button", { name: "Stop process" }));
    expect(await within(sheet).findByText(/isn't running any more/)).toBeVisible();
    expect(within(sheet).getByRole("button", { name: "Stop process" })).toBeEnabled();
  });

  it("says why it can't list, and passes axe, wide and on a phone", async () => {
    const { processes, container, unmount } = await renderProcesses();
    processes.fail(new ConnectError("hz-1 can't list its processes now: the agent is busy", Code.Unavailable));
    expect(await screen.findByText(/the agent is busy/)).toBeVisible();
    unmount();

    const again = await renderProcesses();
    again.processes.push(list(true));
    await screen.findByRole("table", { name: "Processes on hz-1" });
    await expectAccessible(again.container);
    again.unmount();
    void container;

    onPhone();
    const phone = await renderProcesses();
    phone.processes.push(list(true));
    const listed = await screen.findByRole("list", { name: "Processes" });
    expect(within(listed).getAllByRole("listitem")[0]).toHaveTextContent("postgres1184 · postgres · App db6.1%312 MB");
    await expectAccessible(phone.container);
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    const { processes } = await renderProcesses();
    processes.push(list(true));
    await screen.findByRole("table", { name: "hz-1 上的进程" });
    expect(screen.getByRole("textbox", { name: "筛选进程" })).toBeVisible();
    expect(screen.getByRole("button", { name: "停止 postgres（PID 1184）" })).toBeVisible();
    expect(screen.getByRole("region", { name: "总计" })).toHaveTextContent("平均负载 0.84 / 0.91 / 0.88，共 4 个 vCPU");
  });
});
