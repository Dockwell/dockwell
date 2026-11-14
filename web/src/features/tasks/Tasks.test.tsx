// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  Connection,
  type ProposeChangeRequest,
  type TaskInfo,
  TaskInfoSchema,
  TaskKind,
  TaskRunReason,
  TaskRunState,
} from "@dockwell/api";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { ENV, places, SERVER } from "../../test/apps";
import { applied, moved, webPlanned } from "../../test/changes";
import { type Api, onPhone, renderAt, renderWithApi } from "../../test/render";
import { useTaskWords } from "./useTasks";

const now = Date.now();
const at = (ms: number) => timestampFromDate(new Date(now + ms));

function aTask(init: MessageInitShape<typeof TaskInfoSchema> = {}): TaskInfo {
  const base: MessageInitShape<typeof TaskInfoSchema> = {
    id: "tsk_report",
    resource: { kind: "task", scope: ENV, name: "report" },
    kind: TaskKind.APP,
    spec: {
      displayName: "Nightly sales report",
      schedule: "0 4 * * *",
      timeZone: "Europe/Amsterdam",
      timeoutSeconds: 600,
      retries: 1,
      runs: { case: "app", value: { service: "api", command: "/app/api report sales --day=yesterday" } },
    },
    servers: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        connection: Connection.CONNECTED,
        nextRun: at(14 * 3_600_000),
        upcoming: [at(14 * 3_600_000)],
        lastRun: { id: "run_2", state: TaskRunState.SUCCEEDED, startTime: at(-60_000), endTime: at(-60_000 + 108_000) },
        recent: [TaskRunState.SUCCEEDED, TaskRunState.FAILED],
      },
    ],
  };
  return create(TaskInfoSchema, { ...base, ...init } as MessageInitShape<typeof TaskInfoSchema>);
}

const runs = [
  {
    id: "run_2",
    state: TaskRunState.SUCCEEDED,
    reason: TaskRunReason.SCHEDULE,
    startTime: at(-60_000),
    endTime: at(-60_000 + 108_000),
    attempts: [{ exitCode: 0 }],
    output: [
      { time: at(-60_000), text: "412 orders, 18,240.50 EUR gross" },
      { time: at(-1_000), text: "Done" },
    ],
  },
  {
    id: "run_1",
    state: TaskRunState.TIMED_OUT,
    reason: TaskRunReason.SCHEDULE,
    startTime: at(-86_400_000),
    endTime: at(-86_400_000 + 600_000),
    attempts: [{ exitCode: -1, timedOut: true }],
    error: "it took longer than its timeout, 10m0s, and was stopped",
  },
];

function api(tasks: TaskInfo[], more: Api = {}): Api {
  return {
    resources: places(),
    tasks: { listTasks: () => ({ tasks }), listTaskRuns: () => ({ runs }) },
    ...more,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("scheduled tasks", () => {
  it("says what a task is before there's one", async () => {
    renderAt("/scheduled-tasks", api([]));
    expect(await screen.findByRole("heading", { level: 2, name: "No scheduled tasks yet" })).toBeVisible();
    expect(screen.getAllByRole("button", { name: /New task/ }).length).toBeGreaterThan(0);
  });

  it("lists each task with its schedule in words and as cron, where it runs, and how its last run went", async () => {
    const ping = aTask({
      id: "tsk_ping",
      resource: { kind: "task", scope: ENV, name: "ping" },
      kind: TaskKind.REQUEST,
      spec: {
        schedule: "*/5 * * * *",
        runs: { case: "request", value: { url: "https://status.example.com/health", method: "GET" } },
      },
      servers: [
        {
          serverId: SERVER,
          serverName: "hz-1",
          connection: Connection.CONNECTED,
          lastRun: { id: "run_p", state: TaskRunState.SUCCEEDED, startTime: at(-60_000), endTime: at(-60_000 + 180) },
        },
      ],
    });
    const { container } = renderAt("/scheduled-tasks", api([aTask(), ping]));
    const table = await screen.findByRole("table", { name: "Scheduled tasks" });
    const rows = within(table).getAllByRole("row");
    expect(rows[1]).toHaveTextContent("Nightly sales report");
    expect(rows[1]).toHaveTextContent("Command in api");
    expect(rows[1]).toHaveTextContent("Every day at 04:00");
    expect(rows[1]).toHaveTextContent("0 4 * * *");
    expect(rows[1]).toHaveTextContent("Succeeded");
    expect(rows[2]).toHaveTextContent("Every 5 minutes");
    expect(rows[2]).toHaveTextContent("status.example.com");
    expect(rows[2]).toHaveTextContent("180 ms");
    expect(screen.getByText(/2 tasks on hz-1/)).toBeVisible();
    expect(screen.getByRole("region", { name: "Nightly sales report" })).toBeVisible();
    await expectAccessible(container);
  });

  it("shows the chosen task's command, its last runs and what it printed", async () => {
    renderAt("/scheduled-tasks?task=tsk_report", api([aTask()]));
    const panel = await screen.findByRole("region", { name: "Nightly sales report" });
    expect(within(panel).getByText("/app/api report sales --day=yesterday")).toBeVisible();
    expect(await within(panel).findByText("412 orders, 18,240.50 EUR gross")).toBeVisible();
    expect(within(panel).getByText("1 succeeded · 1 failed")).toBeVisible();
    expect(within(panel).getByRole("list", { name: "Last runs, oldest first" }).children).toHaveLength(2);
    // The strip's ends by their day, as Scheduled-Tasks labels them: a day ago, and a minute ago.
    const dayOf = (ms: number) => {
      const d = new Date(now + ms);
      const today = new Date(now);
      const days = Math.round(
        (new Date(today.getFullYear(), today.getMonth(), today.getDate()).getTime() -
          new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()) /
          86_400_000,
      );
      return days === 0 ? "Today" : "Yesterday";
    };
    const ends = panel.querySelector(".tasks-strip-ends");
    expect(ends?.firstElementChild).toHaveTextContent(dayOf(-86_400_000));
    expect(ends?.lastElementChild).toHaveTextContent(dayOf(-60_000));
    expect(panel).toHaveTextContent("10 min, then retry once");
    expect(panel).toHaveTextContent("Europe/Amsterdam");
    expect(panel).toHaveTextContent("timed out: it took longer than its timeout");
    // How the last run ended, in the line above what it printed.
    expect(panel).toHaveTextContent("exit 0");
  });

  it("runs it now", async () => {
    const user = userEvent.setup();
    const ran = vi.fn(() => ({ run: { id: "run_3", state: TaskRunState.RUNNING } }));
    renderAt(
      "/scheduled-tasks",
      api([aTask()], {
        tasks: { listTasks: () => ({ tasks: [aTask()] }), listTaskRuns: () => ({ runs }), runTask: ran },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Nightly sales report" });
    await user.click(within(panel).getByRole("button", { name: "Run now" }));
    await waitFor(() =>
      expect(ran).toHaveBeenCalledWith(
        expect.objectContaining({ id: "tsk_report", server: SERVER }),
        expect.anything(),
      ),
    );
  });

  it("runs it now on the server chosen, and an edit keeps it on every server it's on", async () => {
    const user = userEvent.setup();
    const two = aTask({
      kind: TaskKind.SHELL,
      spec: {
        displayName: "Rotate logs",
        schedule: "30 2 * * *",
        runs: { case: "shell", value: { command: "logrotate" } },
      },
    });
    two.servers.push({ ...two.servers[0], serverId: "srv_2", serverName: "hz-2" } as TaskInfo["servers"][number]);
    const ran = vi.fn(() => ({ run: { id: "run_3", state: TaskRunState.RUNNING } }));
    const proposed = vi.fn((req: ProposeChangeRequest) => {
      void req;
      return { change: { id: "chg_1" } };
    });
    renderAt(
      "/scheduled-tasks",
      api([two], {
        tasks: { listTasks: () => ({ tasks: [two] }), listTaskRuns: () => ({ runs }), runTask: ran },
        changes: { proposeChange: proposed },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Rotate logs" });
    await user.selectOptions(within(panel).getByLabelText("Run it now on"), "hz-2");
    await user.click(within(panel).getByRole("button", { name: "Run now" }));
    await waitFor(() =>
      expect(ran).toHaveBeenCalledWith(expect.objectContaining({ server: "srv_2" }), expect.anything()),
    );

    await user.click(within(panel).getByRole("button", { name: "Edit" }));
    const sheet = await screen.findByRole("dialog");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    // No servers keeps the ones it's on.
    expect(proposed.mock.calls[0]?.[0].edits[0]?.servers).toEqual([]);
  });

  it("says how a run went in its own words, and what a request answered", async () => {
    const ping = aTask({
      id: "tsk_ping",
      resource: { kind: "task", scope: ENV, name: "ping" },
      kind: TaskKind.REQUEST,
      spec: {
        displayName: "Ping the shop",
        schedule: "*/5 * * * *",
        timeZone: "Pacific/Chatham",
        runs: { case: "request", value: { url: "https://shop.example.com/health", method: "GET", expectStatus: 200 } },
      },
    });
    const pings = [
      {
        id: "run_c",
        state: TaskRunState.SUCCEEDED,
        startTime: at(-60_000),
        endTime: at(-60_000 + 180),
        skippedSlots: 2,
        attempts: [{ startTime: at(-60_000), endTime: at(-60_000 + 180), httpStatus: 200, responseBytes: 22n }],
        output: [
          {
            time: at(-60_000 + 180),
            text: "Dockwell: GET https://shop.example.com/health answered 200 OK in 180ms, 22 bytes.",
          },
        ],
      },
      {
        id: "run_b",
        state: TaskRunState.INTERRUPTED,
        startTime: at(-3_600_000),
        endTime: at(-3_600_000 + 1_000),
        error: "the agent stopped while it ran, so how it ended isn't known",
      },
      {
        id: "run_a",
        state: TaskRunState.FAILED,
        startTime: at(-7_200_000),
        endTime: at(-7_200_000 + 300),
        attempts: [{ httpStatus: 503 }],
        error: "it answered 503, and the task expects 200",
      },
    ];
    renderAt(
      "/scheduled-tasks",
      api([ping], { tasks: { listTasks: () => ({ tasks: [ping] }), listTaskRuns: () => ({ runs: pings }) } }),
    );
    const panel = await screen.findByRole("region", { name: "Ping the shop" });
    expect(
      await within(panel).findByText("GET https://shop.example.com/health answered 200 in 180 ms, 22 bytes."),
    ).toBeVisible();
    expect(panel).not.toHaveTextContent("Dockwell:");
    expect(panel).toHaveTextContent("Its schedule came round 2 times while it ran");
    expect(panel).toHaveTextContent("interrupted: the agent stopped while it ran");
    expect(panel).toHaveTextContent("failed: it answered 503, and the task expects 200");
    // Its schedule reads in its own zone, which isn't the viewer's.
    const table = screen.getByRole("table", { name: "Scheduled tasks" });
    expect(within(table).getAllByRole("row")[1]).toHaveTextContent("Every 5 minutes, Pacific/Chatham time");
  });

  it("keeps a run's time on the timeline near the end of the day", async () => {
    const late = aTask();
    const on = late.servers[0];
    if (on) {
      on.nextRun = at(23 * 3_600_000);
      on.upcoming = [at(23 * 3_600_000)];
    }
    const { container } = renderAt("/scheduled-tasks", api([late]));
    await screen.findByRole("table", { name: "Scheduled tasks" });
    const label = await waitFor(() => {
      const found = container.querySelector<HTMLElement>(".tasks-lane-label");
      if (!found) throw new Error("no label yet");
      return found;
    });
    // Before its mark, measured from the right, so it ends on the track.
    expect(label).toHaveClass("is-before");
    expect(label.style.right).not.toBe("");
    expect(label.style.left).toBe("");
  });

  it("gives the start of the axis to Now, and hides an hour that would stand on it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      // Five minutes to six: 06:00 is under one percent along the track.
      vi.setSystemTime(new Date(2026, 10, 10, 5, 55));
      const { container } = renderAt("/scheduled-tasks", api([aTask()]));
      await screen.findByRole("region", { name: "Next 24 hours" });
      const labels = [...container.querySelectorAll<HTMLElement>(".tasks-axis-label")];
      // All four stay in the page, so the track's hours keep their places.
      expect(labels).toHaveLength(4);
      expect(labels.filter((l) => l.classList.contains("is-hidden")).map((l) => l.textContent)).toEqual(["06:00"]);
    } finally {
      vi.useRealTimers();
    }
  });

  it("draws no card around the card that says there are no tasks", async () => {
    const { container } = renderAt("/scheduled-tasks", api([]));
    const heading = await screen.findByRole("heading", { level: 2, name: "No scheduled tasks yet" });
    expect(heading.closest(".empty-state")).not.toBeNull();
    expect(container.querySelector(".dw-panel")).toBeNull();
  });

  it("doesn't count the runs of a task that hasn't run", async () => {
    renderAt("/scheduled-tasks", {
      ...api([aTask()]),
      tasks: { listTasks: () => ({ tasks: [aTask()] }), listTaskRuns: () => ({ runs: [] }) },
    });
    expect(await screen.findByText("It hasn't run yet. Run it now to see what it does.")).toBeVisible();
    expect(screen.getByText("None yet")).toBeVisible();
    expect(screen.queryByText("Last 0 runs")).toBeNull();
  });

  it("says a server that isn't connected runs its tasks all the same", async () => {
    const away = aTask();
    const first = away.servers[0];
    if (first) first.connection = Connection.UNREACHABLE;
    renderAt("/scheduled-tasks", api([away]));
    expect(await screen.findByText(/hz-1 runs its tasks all the same, on its own clock/)).toBeVisible();
  });

  it("plans a new shell command from what it runs, named after it, every day at 04:00", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((req: ProposeChangeRequest) => {
      void req;
      return { change: { id: "chg_1" } };
    });
    renderAt("/scheduled-tasks", api([], { changes: { proposeChange: proposed } }));
    await user.click((await screen.findAllByRole("button", { name: /New task/ }))[0] as HTMLElement);
    const sheet = await screen.findByRole("dialog", { name: "New task" });
    expect(within(sheet).getByText(/as root, so applying it takes your passkey/)).toBeVisible();
    // Only what to run and when are asked; the rest is a line under Advanced.
    expect(within(sheet).queryByLabelText("Timeout, in minutes")).toBeNull();
    await user.type(within(sheet).getByLabelText("Command"), "find /var/log/app -mtime +7 -delete");
    expect(within(sheet).getByRole("button", { name: "Advanced" })).toHaveTextContent(
      "Named find · in / · stops after 10 min · no retries",
    );
    expect(within(sheet).getByText(/^Next, in UTC: /)).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource?.kind).toBe("task");
    expect(edit?.resource?.name).toBe("find");
    const spec = edit?.spec?.kind.case === "task" ? edit.spec.kind.value : undefined;
    expect(spec?.schedule).toBe("0 4 * * *");
    expect(spec?.runs.case === "shell" && spec.runs.value.command).toBe("find /var/log/app -mtime +7 -delete");
    expect(spec?.timeoutSeconds).toBe(600);
  });

  it("picks a schedule from lists, and says when it next runs", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((req: ProposeChangeRequest) => {
      void req;
      return { change: { id: "chg_1" } };
    });
    renderAt("/scheduled-tasks", api([], { changes: { proposeChange: proposed } }));
    await user.click((await screen.findAllByRole("button", { name: /New task/ }))[0] as HTMLElement);
    const sheet = await screen.findByRole("dialog", { name: "New task" });
    await user.type(within(sheet).getByLabelText("Command"), "/usr/local/bin/backup-photos");
    await user.type(within(sheet).getByLabelText(/^Name/), "Back up photos");
    await user.selectOptions(within(sheet).getByLabelText("Schedule"), "week");
    await user.selectOptions(within(sheet).getByLabelText("On"), "5");
    // A time input takes the whole time at once, as a browser's picker gives it.
    fireEvent.change(within(sheet).getByLabelText("At"), { target: { value: "21:30" } });
    expect(within(sheet).getByText("30 21 * * 5")).toBeVisible();
    expect(within(sheet).getByText(/^Next, in UTC: Fri/)).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource?.name).toBe("back-up-photos");
    const spec = edit?.spec?.kind.case === "task" ? edit.spec.kind.value : undefined;
    expect(spec?.schedule).toBe("30 21 * * 5");
    expect(spec?.displayName).toBe("Back up photos");
  });

  it("takes cron for the rest, and refuses a name or a schedule the control plane wouldn't take", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn();
    renderAt("/scheduled-tasks", api([], { changes: { proposeChange: proposed } }));
    await user.click((await screen.findAllByRole("button", { name: /New task/ }))[0] as HTMLElement);
    const sheet = await screen.findByRole("dialog", { name: "New task" });
    await user.type(within(sheet).getByLabelText("Command"), "true");
    await user.selectOptions(within(sheet).getByLabelText("Schedule"), "cron");
    const cron = within(sheet).getByLabelText("Cron");
    expect(cron).toHaveValue("0 4 * * *");
    await user.clear(cron);
    await user.type(cron, "every day");
    expect(within(sheet).getByText("That isn't cron Dockwell reads.")).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Advanced" }));
    const short = within(sheet).getByLabelText("Short name");
    await user.clear(short);
    await user.type(short, "Rotate Logs");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(await within(sheet).findByText(/Use lowercase letters, digits and hyphens/)).toBeVisible();
    expect(within(sheet).getByText(/Use cron's five fields/)).toBeVisible();
    expect(proposed).not.toHaveBeenCalled();
  });

  it("refuses a schedule no date matches, as the control plane would", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn();
    renderAt("/scheduled-tasks", api([], { changes: { proposeChange: proposed } }));
    await user.click((await screen.findAllByRole("button", { name: /New task/ }))[0] as HTMLElement);
    const sheet = await screen.findByRole("dialog", { name: "New task" });
    await user.type(within(sheet).getByLabelText("Command"), "true");
    await user.selectOptions(within(sheet).getByLabelText("Schedule"), "cron");
    // Custom's one field stands in for the time picker of the shape it came from.
    expect(within(sheet).queryByLabelText("At")).toBeNull();
    const cron = within(sheet).getByLabelText("Cron");
    await user.clear(cron);
    await user.type(cron, "0 0 30 2 *");
    expect(within(sheet).getByText("No date matches it, so it never runs.")).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(within(sheet).getAllByText("No date matches it, so it never runs.")).toHaveLength(1);
    expect(proposed).not.toHaveBeenCalled();
  });

  it("leaves out the next runs in a zone the browser doesn't know, and opens all the same", async () => {
    const user = userEvent.setup();
    const odd = aTask();
    if (odd.spec) odd.spec.timeZone = "Mars/Olympus_Mons";
    renderAt("/scheduled-tasks", api([odd]));
    const panel = await screen.findByRole("region", { name: "Nightly sales report" });
    await user.click(within(panel).getByRole("button", { name: "Edit" }));
    const sheet = await screen.findByRole("dialog");
    expect(within(sheet).getByLabelText("Schedule")).toBeVisible();
    expect(within(sheet).queryByText(/^Next, in /)).toBeNull();
  });

  it("names the folder a shell command runs in its own words, in Chinese too", async () => {
    activateLocale("zh-CN");
    try {
      const user = userEvent.setup();
      renderAt("/scheduled-tasks", api([]));
      await user.click((await screen.findAllByRole("button", { name: /新建任务/ }))[0] as HTMLElement);
      const sheet = await screen.findByRole("dialog");
      await user.type(within(sheet).getByLabelText("命令"), "find /var/log/app -mtime +7 -delete");
      const advanced = within(sheet)
        .getAllByRole("button")
        .find((b) => b.textContent?.includes("在 / 中运行"));
      expect(advanced).toBeDefined();
      expect(sheet.textContent).not.toContain("/后");
    } finally {
      activateLocale("en");
    }
  });

  it("opens the task it made once the plan applies, where Run now is", async () => {
    const user = userEvent.setup();
    let tasks: TaskInfo[] = [];
    const made = aTask({
      id: "tsk_find",
      resource: { kind: "task", scope: ENV, name: "find" },
      kind: TaskKind.SHELL,
      spec: { schedule: "0 4 * * *", runs: { case: "shell", value: { command: "find /tmp -delete" } } },
    });
    const planned = moved(webPlanned(), { id: "chg_task", summary: "Run find on a schedule" });
    renderAt(
      "/scheduled-tasks",
      api([], {
        tasks: { listTasks: () => ({ tasks }), listTaskRuns: () => ({ runs: [] }) },
        changes: {
          proposeChange: () => ({ change: planned }),
          applyChange: () => {
            tasks = [made];
            return { change: applied(planned) };
          },
        },
      }),
    );
    await user.click((await screen.findAllByRole("button", { name: /New task/ }))[0] as HTMLElement);
    const sheet = await screen.findByRole("dialog", { name: "New task" });
    await user.type(within(sheet).getByLabelText("Command"), "find /tmp -delete");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await user.click(await screen.findByRole("button", { name: /Apply plan/ }));
    await user.click(await screen.findByRole("button", { name: "Open find" }));
    const panel = await screen.findByRole("region", { name: "find" });
    expect(within(panel).getByRole("button", { name: "Run now" })).toBeEnabled();
  });

  it("is a list of rows on a phone", async () => {
    onPhone();
    renderAt("/scheduled-tasks", api([aTask()]));
    expect(await screen.findByRole("button", { name: /Nightly sales report/ })).toBeVisible();
  });

  it("reads in Chinese", async () => {
    activateLocale("zh-CN");
    try {
      renderAt("/scheduled-tasks", api([aTask()]));
      expect(await screen.findByRole("heading", { level: 1, name: "计划任务" })).toBeVisible();
    } finally {
      activateLocale("en");
    }
  });
});

describe("a schedule in words", () => {
  it("says the common ones, and nothing for the rest", () => {
    let words: ReturnType<typeof useTaskWords> | undefined;
    function Read() {
      words = useTaskWords();
      return null;
    }
    renderWithApi(<Read />, { resources: places() });
    const say = (cron: string) => words?.schedule(cron);
    expect(say("* * * * *")).toBe("Every minute");
    expect(say("*/5 * * * *")).toBe("Every 5 minutes");
    expect(say("0 * * * *")).toBe("Every hour");
    expect(say("@hourly")).toBe("Every hour");
    expect(say("15 * * * *")).toBe("Every hour at :15");
    expect(say("30 2 * * *")).toBe("Every day at 02:30");
    expect(say("0 4 * * 1-5")).toBe("Every weekday at 04:00");
    expect(say("0 4 * * 1")).toBe("Every Monday at 04:00");
    expect(say("0 0 * * 7")).toBe("Every Sunday at 00:00");
    expect(say("0 4 1 * *")).toBe("On day 1 of every month at 04:00");
    expect(say("0 */6 * * *")).toBe("Every 6 hours");
    expect(say("15 */2 * * *")).toBe("Every 2 hours at :15");
    expect(say("@yearly")).toBe("Every year on January 1 at 00:00");
    expect(say("0 4 * * 1,3")).toBe("");
  });
});
