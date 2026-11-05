// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ListLogSourcesResponseSchema,
  LogGapReason,
  LogKept,
  LogLevel,
  type LogSource,
  LogSourceSchema,
  LogSourceState,
  ServerLogState,
  type WatchLogsRequest,
  type WatchLogsResponse,
  WatchLogsResponseSchema,
} from "@dockwell/api";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { type Api, onPhone, renderAt, untilAborted } from "../../test/render";
import { deploysOf, replicaLabel } from "./LogsPage";
import { instanceName, withLabels } from "./words";

const SERVICE = "svc_01TEST";
const URL = `/apps/${SERVICE}/logs`;
const T0 = 1_793_832_000n;

const running = create(LogSourceSchema, {
  id: "aaaaaaaaaaaa",
  serverId: "srv_1",
  serverName: "hz-1",
  replica: 1,
  revision: 3n,
  changeId: "chg_3",
  state: LogSourceState.RUNNING,
  containerName: "env-web-1",
});
const second = create(LogSourceSchema, { ...running, id: "bbbbbbbbbbbb", serverId: "srv_2", serverName: "hz-2" });

const failed = create(LogSourceSchema, {
  ...running,
  id: "cccccccccccc",
  revision: 2n,
  changeId: "chg_2",
  state: LogSourceState.KEPT,
  kept: LogKept.FAILED,
});
const sourcesOf = (...sources: (typeof running)[]) =>
  create(ListLogSourcesResponseSchema, {
    service: { id: SERVICE, name: "web", environment: "production" },
    sources,
  });

function line(text: string, seconds = 0, level = LogLevel.UNSPECIFIED) {
  return { time: { seconds: T0 + BigInt(seconds), nanos: 0 }, text, level };
}

function message(init: Parameters<typeof create<typeof WatchLogsResponseSchema>>[1]): WatchLogsResponse {
  return create(WatchLogsResponseSchema, init);
}

/** A stream a test feeds by hand: each call of it ends or fails once, and the next goes on. */
function logFeed() {
  const queue: WatchLogsResponse[] = [];
  let failure: unknown;
  let ended = false;
  let wake = () => {};
  return {
    send(m: WatchLogsResponse) {
      queue.push(m);
      wake();
    },
    fail(err: unknown) {
      failure = err;
      wake();
    },
    end() {
      ended = true;
      wake();
    },
    async *watch(signal: AbortSignal): AsyncGenerator<WatchLogsResponse> {
      for (;;) {
        const next = queue.shift();
        if (next !== undefined) {
          yield next;
          continue;
        }
        if (failure !== undefined) {
          const err = failure;
          failure = undefined;
          throw err;
        }
        if (ended) {
          ended = false;
          return;
        }
        if (signal.aborted) return;
        await Promise.race([new Promise<void>((resolve) => (wake = resolve)), untilAborted(signal)]);
      }
    },
  };
}

/** An API with one service, whose stream sends what a test feeds it. */
function api(sources = sourcesOf(running)) {
  const stream = logFeed();
  const requests: WatchLogsRequest[] = [];
  const logs: Api["logs"] = {
    listLogSources: () => sources,
    watchLogs: (req, ctx) => {
      requests.push(req);
      return stream.watch(ctx.signal);
    },
  };
  return { logs, stream, requests };
}

afterEach(async () => {
  await act(() => activateLocale("en"));
});

describe("LogsPage", () => {
  it("shows lines as they come, from one source without a source column", async () => {
    const { logs, stream, requests } = api();
    const { container } = renderAt(URL, { logs });
    await screen.findByRole("log");
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0]?.id).toBe(SERVICE);
    expect(requests[0]?.tail).toBeGreaterThan(0);
    await act(() =>
      stream.send(
        message({
          sources: [running],
          servers: [{ serverId: "srv_1", serverName: "hz-1", state: ServerLogState.READING }],
          chunks: [
            {
              sourceId: running.id,
              lines: [line("GET /healthz 200"), line("slow query", 1, LogLevel.WARN), line("boom", 2, LogLevel.ERROR)],
            },
          ],
          caughtUp: true,
        }),
      ),
    );
    await screen.findByText("GET /healthz 200");
    expect(container.querySelector(".dw-log-src")).toBeNull();
    expect(screen.getByRole("log")).toHaveAttribute("aria-live", "off");
    expect(within(screen.getByRole("log").closest(".dw-log") as HTMLElement).getByText("Following")).toBeVisible();
    await expectAccessible(container);
  });

  it("names both containers of a rolling deploy for as long as it reads both, and where the new one begins", async () => {
    const before = create(LogSourceSchema, { ...running, id: "dddddddddddd", containerName: "env-web-2" });
    const after = create(LogSourceSchema, { ...running, id: "eeeeeeeeeeee", revision: 4n, containerName: "env-web-3" });
    const { logs, stream } = api(sourcesOf(before));
    const { container } = renderAt(URL, { logs });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({
          sources: [before],
          chunks: [{ sourceId: before.id, lines: [line("what ran says")] }],
          caughtUp: true,
        }),
      ),
    );
    await screen.findByText("what ran says");
    // One container, whatever number it has reached: its server says which.
    expect(container.querySelector(".dw-log-src")).toBeNull();
    await act(() =>
      stream.send(
        message({ sources: [before, after], chunks: [{ sourceId: after.id, lines: [line("the new one says", 1)] }] }),
      ),
    );
    await screen.findByText("the new one says");
    await waitFor(() => expect(container.querySelector(".dw-log-src")).not.toBeNull());
    expect(screen.getAllByText("web-2").length).toBeGreaterThan(0);
    expect(screen.getAllByText("web-3").length).toBeGreaterThan(0);
    await screen.findByText("web-3 started r4");
    expect(screen.queryByText(/stopping_/)).toBeNull();
  });

  it("says how many run now, before, during and after a rolling deploy, and after several", async () => {
    const web = (id: string, n: number, state = LogSourceState.RUNNING) =>
      create(LogSourceSchema, { ...running, id, revision: BigInt(n), state, containerName: `env-web-${n}` });
    const { logs, stream } = api(sourcesOf(web("aaaa00000002", 2)));
    const { container } = renderAt(URL, { logs });
    await screen.findByRole("log");
    const head = () => container.querySelector(".dw-log-title")?.textContent ?? "";
    const hear = (sources: LogSource[], text?: string) =>
      act(() =>
        stream.send(
          message({
            sources,
            chunks: text ? [{ sourceId: sources[sources.length - 1]?.id ?? "", lines: [line(text)] }] : [],
            caughtUp: true,
          }),
        ),
      );
    await hear([web("aaaa00000002", 2)], "before");
    await screen.findByText("before");
    expect(head()).toContain("web · production");
    expect(head()).not.toMatch(/container|replica/);

    // During the move, both run, and it says so.
    await hear([web("aaaa00000002", 2), web("aaaa00000003", 3)], "during");
    await screen.findByText("during");
    await waitFor(() => expect(head()).toContain("2 containers"));
    expect(head()).not.toMatch(/replica/);
    expect(screen.getByRole("radio", { name: "All containers" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "web-3" })).toBeVisible();

    // After it, one runs, though this view read two, and a filter has nothing to choose.
    await hear([web("aaaa00000002", 2, LogSourceState.KEPT), web("aaaa00000003", 3)], "after");
    await screen.findByText("after");
    await waitFor(() => expect(head()).not.toMatch(/container/));
    expect(screen.queryByRole("radio", { name: "All containers" })).toBeNull();
    // What it read of the one that went still says which wrote it.
    expect(container.querySelector(".dw-log-src")).not.toBeNull();

    // After several, one that went with nothing kept among them, it's still only what runs.
    await hear(
      [
        web("aaaa00000003", 3, LogSourceState.GONE),
        web("aaaa00000004", 4, LogSourceState.KEPT),
        web("aaaa00000005", 5, LogSourceState.KEPT),
        web("aaaa00000006", 6),
      ],
      "several later",
    );
    await screen.findByText("several later");
    await waitFor(() => expect(head()).not.toMatch(/container/));
    expect(head()).not.toMatch(/replica/);
    expect(screen.queryByRole("radio", { name: "All containers" })).toBeNull();
  });

  it("doesn't count what a search found as running, once what's read live says otherwise", async () => {
    const web = (id: string, n: number, state = LogSourceState.RUNNING) =>
      create(LogSourceSchema, { ...running, id, state, containerName: `env-web-${n}` });
    const was = web("aaaa00000001", 1);
    const lost = web("aaaa00000002", 2);
    const { logs, stream } = api(sourcesOf(was));
    const readLogs = vi.fn(() => ({ sources: [was, lost], chunks: [] }));
    const user = userEvent.setup();
    const { container } = renderAt(URL, { logs: { ...logs, readLogs } });
    await screen.findByRole("log");
    await act(() =>
      stream.send(message({ sources: [was], chunks: [{ sourceId: was.id, lines: [line("before")] }], caughtUp: true })),
    );
    await screen.findByText("before");
    const field = screen.getByRole("textbox", { name: "Search logs" });
    await user.type(field, "far");
    await user.click(await screen.findByRole("button", { name: "Search all the servers keep" }));
    await waitFor(() => expect(readLogs).toHaveBeenCalled());
    await user.click(screen.getByRole("radio", { name: "Lines loaded" }));
    await user.clear(field);
    const head = () => container.querySelector(".dw-log-name")?.textContent ?? "";
    // What the search saw and still runs goes on counting, as what's read live says so.
    const now = web("aaaa00000003", 3);
    await act(() =>
      stream.send(
        message({ sources: [was, now], chunks: [{ sourceId: now.id, lines: [line("during")] }], caughtUp: true }),
      ),
    );
    await screen.findByText("during");
    await waitFor(() => expect(head()).toContain("2 containers"));
    // Since, one the search saw has gone with nothing said of it, and the other was kept.
    await act(() =>
      stream.send(
        message({
          sources: [web("aaaa00000001", 1, LogSourceState.KEPT), now],
          chunks: [{ sourceId: now.id, lines: [line("after")] }],
          caughtUp: true,
        }),
      ),
    );
    await screen.findByText("after");
    await waitFor(() => expect(head()).toContain("web · production"));
    await waitFor(() => expect(head()).not.toMatch(/container/));
    expect(screen.queryByRole("radio", { name: "All containers" })).toBeNull();
  });

  it("forgets a filter on a name that has gone, as a deploy moves on from it", async () => {
    const { logs, stream } = api(sourcesOf(running));
    renderAt(`${URL}?replica=web-9`, { logs });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({
          sources: [running],
          chunks: [{ sourceId: running.id, lines: [line("still here")] }],
          caughtUp: true,
        }),
      ),
    );
    await screen.findByText("still here");
  });

  it("has a source column once there are two sources", async () => {
    const { logs, stream } = api(sourcesOf(running, second));
    const { container } = renderAt(URL, { logs });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({
          sources: [running, second],
          chunks: [
            { sourceId: running.id, lines: [line("from one")] },
            { sourceId: second.id, lines: [line("from two", 1)] },
          ],
          caughtUp: true,
        }),
      ),
    );
    await screen.findByText("from one");
    await waitFor(() => expect(container.querySelector(".dw-log-src")).not.toBeNull());
    expect(screen.getAllByText("hz-2").length).toBeGreaterThan(0);
    // Two replicas give a choice of them.
    expect(screen.getByRole("radio", { name: "All containers" })).toBeChecked();
    await userEvent.setup().click(screen.getByRole("radio", { name: "hz-2" }));
    await waitFor(() => expect(screen.queryByText("from one")).toBeNull());
    expect(container.querySelector(".dw-log-src")).toBeNull();
  });

  it("says when there are no containers yet, and when nothing was written", async () => {
    const empty = api(sourcesOf());
    const first = renderAt(URL, { logs: empty.logs });
    await screen.findByRole("log");
    await act(() => empty.stream.send(message({ caughtUp: true })));
    await screen.findByRole("heading", { name: "web has no containers yet" });
    first.unmount();

    const quiet = api();
    renderAt(URL, { logs: quiet.logs });
    await screen.findByText("Reading logs");
    await act(() => quiet.stream.send(message({ sources: [running], caughtUp: true })));
    await screen.findByRole("heading", { name: "No logs yet" });
    await screen.findByText(/web hasn't written anything/);
  });

  it("states a server that goes offline, and takes it back", async () => {
    const { logs, stream } = api();
    renderAt(URL, { logs });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({
          servers: [
            {
              serverId: "srv_1",
              serverName: "hz-1",
              state: ServerLogState.OFFLINE,
              message: "hz-1 isn't connected, so its logs can't be read",
            },
          ],
        }),
      ),
    );
    await screen.findByText("hz-1 is offline");
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Its agent isn't connected, so its logs can't be read now. Its containers keep running and logging. Check the server. Lines written meanwhile arrive when it reconnects, as far as Docker kept them.",
    );
    await act(() =>
      stream.send(message({ servers: [{ serverId: "srv_1", serverName: "hz-1", state: ServerLogState.READING }] })),
    );
    await waitFor(() => expect(screen.queryByText("hz-1 is offline")).toBeNull());
  });

  it("says when its container stopped", async () => {
    const stopped = create(LogSourceSchema, {
      ...running,
      state: LogSourceState.STOPPED,
      exitCode: 137,
      finishTime: timestampFromMs(Number(T0) * 1000),
    });
    const { logs, stream } = api(sourcesOf(stopped));
    renderAt(URL, { logs });
    await act(() => stream.send(message({ sources: [stopped], caughtUp: true })));
    await screen.findByText("web isn't running on hz-1");
    await screen.findByText(/with exit code 137/);
  });

  it("refuses without permission, with no stream of lines", async () => {
    const logs: Api["logs"] = {
      listLogSources: () => {
        throw new ConnectError("no", Code.PermissionDenied);
      },
      watchLogs: async function* () {
        yield* [];
        throw new ConnectError("no", Code.PermissionDenied);
      },
    };
    const { container } = renderAt(URL, { logs });
    await screen.findByText("You can't read these logs");
    expect(screen.queryByRole("log")).toBeNull();
    await expectAccessible(container);
  });

  it("marks what a search finds, and says how much of what is loaded matched", async () => {
    const { logs, stream } = api();
    const { container, router } = renderAt(URL, { logs });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({
          sources: [running],
          chunks: [{ sourceId: running.id, lines: [line("POST /cart 500"), line("GET /orders 200", 1)] }],
          caughtUp: true,
        }),
      ),
    );
    await screen.findByText("GET /orders 200");
    const user = userEvent.setup();
    await user.type(screen.getByRole("textbox", { name: "Search logs" }), "cart");
    await waitFor(() => expect(container.querySelector("mark")).toHaveTextContent("cart"));
    expect(screen.queryByText("GET /orders 200")).toBeNull();
    await screen.findByText(/1 of the 2 lines loaded matches/);
    await waitFor(() => expect(router.state.location.search).toMatchObject({ q: "cart" }));
    await user.clear(screen.getByRole("textbox", { name: "Search logs" }));
    await user.type(screen.getByRole("textbox", { name: "Search logs" }), "zzz");
    await screen.findByText('Nothing in the 2 lines loaded has "zzz".');
    expect(screen.getByRole("button", { name: "Search all the servers keep" })).toBeVisible();
    await user.keyboard("{Escape}");
    await screen.findByText("GET /orders 200");
    expect(screen.getByRole("textbox", { name: "Search logs" })).toHaveValue("");
  });

  it("searches all the servers keep on request, and says how far it reached", async () => {
    const { logs, stream } = api();
    const readLogs = vi.fn(() => ({
      chunks: [{ sourceId: running.id, lines: [line("found far back")] }],
      sources: [running],
      searchedLines: 1500n,
      searchedBackTo: { seconds: T0 - 86_400n, nanos: 0 },
    }));
    renderAt(URL, { logs: { ...logs, readLogs } });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({ sources: [running], chunks: [{ sourceId: running.id, lines: [line("here")] }], caughtUp: true }),
      ),
    );
    const user = userEvent.setup();
    const field = screen.getByRole("textbox", { name: "Search logs" });
    await user.type(field, "far");
    // Nothing loaded has it: the way on is named, and so is what was looked for.
    await screen.findByText('Nothing in the 1 lines loaded has "far".');
    await user.click(await screen.findByRole("button", { name: "Search all the servers keep" }));
    // Marked where it matched, so split across elements.
    await waitFor(() => expect(screen.getByRole("log")).toHaveTextContent("found far back"));
    expect(readLogs.mock.calls.length).toBe(1);
    await screen.findByText(/1 match in 1,500 lines, back to/);
    await screen.findByText(/That is all the servers keep\./);
    expect(screen.getByRole("radio", { name: "All the servers keep" })).toBeChecked();
    // Typing doesn't search again, nor go back to the lines loaded: Enter does the one.
    await user.type(field, "ther");
    await screen.findByText("Press Enter to search all the servers keep.");
    // Past how long a search of what is loaded waits for the next key.
    await act(() => new Promise((resolve) => setTimeout(resolve, 200)));
    expect(readLogs.mock.calls.length).toBe(1);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(readLogs.mock.calls.length).toBe(2));
    await user.click(screen.getByRole("radio", { name: "Lines loaded" }));
    await waitFor(() => expect(screen.getByRole("log")).not.toHaveTextContent("found far back"));
  });

  it("marks what is missing, in the stream and beside it", async () => {
    const { logs, stream } = api();
    renderAt(URL, { logs });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({
          sources: [running],
          chunks: [
            {
              sourceId: running.id,
              gap: { reason: LogGapReason.TOO_FAST, lines: 1204n, endTime: { seconds: T0, nanos: 0 } },
              lines: [line("after the gap", 1)],
            },
          ],
        }),
      ),
    );
    await screen.findByText(/1,204 lines from hz-1 not shown: it wrote faster than a live view can carry/);
    await screen.findByText(/About 1,200 lines not shown/);
    await screen.findByText(/this app wrote faster than a live view can carry/);
    expect(screen.getByRole("button", { name: "Go to the last" })).toBeVisible();
  });

  it("shows a deploy that failed, by the link Activity gives", async () => {
    const { logs, stream, requests } = api(sourcesOf(running, failed));
    renderAt(`${URL}?change=chg_2`, { logs });
    await screen.findByRole("log");
    await waitFor(() => expect(requests[0]?.changeId).toBe("chg_2"));
    expect(requests[0]?.once).toBe(true);
    await screen.findByText("failed at", { exact: false });
    // By the name it had, as Docker has forgotten it.
    expect(screen.getByRole("alert")).toHaveTextContent("It ran as web-1.");
    await act(() =>
      stream.send(
        message({
          sources: [failed],
          chunks: [{ sourceId: failed.id, lines: [line("ERROR no database", 0, LogLevel.ERROR)] }],
          caughtUp: true,
        }),
      ),
    );
    await screen.findByText("ERROR no database");
    // It isn't following anything.
    expect(screen.queryByText("Following")).toBeNull();
    expect(screen.getByRole("radio", { name: /r2\s*failed/ })).toBeChecked();
  });

  it("says when a deploy's logs are gone", async () => {
    const { logs } = api();
    const { router } = renderAt(`${URL}?change=chg_old`, { logs });
    await screen.findByText("This deploy's logs are gone");
    await userEvent.setup().click(screen.getByRole("button", { name: "Show the current logs" }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
  });

  it("reads a time range to its end, and stops being live", async () => {
    const { logs, stream, requests } = api();
    renderAt(URL, { logs });
    await screen.findByRole("log");
    await userEvent.setup().click(screen.getByRole("radio", { name: "1 h" }));
    await waitFor(() => expect(requests.some((r) => r.once && r.last?.seconds === 3600n)).toBe(true));
    await act(() => stream.end());
    await screen.findByRole("heading", { name: "Nothing was written in this time" });
    expect(screen.queryByText("Paused")).toBeNull();
  });

  it("carries on after the last line when its stream drops", async () => {
    const { logs, stream, requests } = api();
    renderAt(URL, { logs });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({
          sources: [running],
          chunks: [{ sourceId: running.id, lines: [line("before")] }],
          resumeToken: "place-1",
          caughtUp: true,
        }),
      ),
    );
    await screen.findByText("before");
    await act(() => stream.fail(new ConnectError("gone", Code.Unavailable)));
    await screen.findByText("Reconnecting to the control server");
    await waitFor(() => expect(requests.at(-1)?.resumeToken).toBe("place-1"), { timeout: 3000 });
  });

  it("speaks Chinese", async () => {
    await act(() => activateLocale("zh-CN"));
    const { logs, stream } = api();
    const { container } = renderAt(URL, { logs });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({
          sources: [running],
          chunks: [{ sourceId: running.id, lines: [line("boom", 0, LogLevel.ERROR)] }],
          caughtUp: true,
        }),
      ),
    );
    await screen.findByRole("textbox", { name: "搜索日志" });
    expect(screen.getByRole("button", { name: "警告" })).toBeVisible();
    expect(screen.getByRole("button", { name: "错误" })).toBeVisible();
    expect(screen.getByRole("radio", { name: "1 小时" })).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("has a phone's layout, with targets of 44 px, and one level at a time", async () => {
    onPhone();
    try {
      const { logs, stream } = api(sourcesOf(running, second));
      const { container, router } = renderAt(URL, { logs });
      await screen.findByRole("log");
      await act(() =>
        stream.send(
          message({
            sources: [running, second],
            chunks: [{ sourceId: running.id, lines: [line("hello")] }],
            caughtUp: true,
          }),
        ),
      );
      await screen.findByRole("button", { name: "All levels" });
      const user = userEvent.setup();
      // All of the screen, with the search behind the header's button.
      expect(screen.queryByRole("navigation", { name: "Main" })).toBeNull();
      expect(screen.queryByRole("navigation", { name: "App sections" })).toBeNull();
      expect(screen.queryByRole("textbox", { name: "Search logs" })).toBeNull();
      await user.click(screen.getByRole("button", { name: "Search logs" }));
      expect(await screen.findByRole("textbox", { name: "Search logs" })).toHaveFocus();
      await user.click(screen.getByRole("button", { name: "Close the search" }));
      await screen.findByRole("button", { name: "All levels" });
      // What to show is a tap away.
      await user.click(screen.getByRole("button", { name: "Live" }));
      expect(await screen.findByRole("radio", { name: "15 min" })).toBeVisible();
      await user.click(screen.getByRole("button", { name: "Close" }));
      await user.click(screen.getByRole("button", { name: "Errors" }));
      await waitFor(() => expect(router.state.location.search).toMatchObject({ levels: "error" }));
      expect(screen.getByRole("button", { name: "All levels" })).toHaveAttribute("aria-pressed", "false");
      expect(screen.getByRole("button", { name: "hz-2" })).toBeVisible();
      expect(screen.getByRole("button", { name: "Download" })).toBeVisible();
      await expectAccessible(container);
    } finally {
      vi.unstubAllGlobals();
    }
  });
});

/** Renders the logs with lines in them, and the means to go on. */
async function withLines(lines: ReturnType<typeof line>[], extra: Partial<NonNullable<Api["logs"]>> = {}, url = URL) {
  const { logs, stream, requests } = api();
  const view = renderAt(url, { logs: { ...logs, ...extra } });
  await screen.findByRole("log");
  await act(() =>
    stream.send(message({ sources: [running], chunks: [{ sourceId: running.id, lines }], caughtUp: true })),
  );
  return { ...view, stream, requests, user: userEvent.setup() };
}

describe("reading a failed deploy", () => {
  const trace = [
    line("starting up", 0),
    line("ERROR no database", 1, LogLevel.ERROR),
    line("    at connect (db.go:42)", 1, LogLevel.ERROR),
    line("ERROR still no database", 5, LogLevel.ERROR),
    line("giving up", 6),
  ];

  it("opens at the first error, and goes through them", async () => {
    const { logs, stream } = api(sourcesOf(running, failed));
    const { container } = renderAt(`${URL}?change=chg_2`, { logs });
    await screen.findByRole("log");
    await act(() => stream.send(message({ sources: [failed], chunks: [{ sourceId: failed.id, lines: trace }] })));
    await act(() => stream.end());
    await waitFor(() =>
      expect(container.querySelector(".dw-log-line.is-current")).toHaveTextContent("ERROR no database"),
    );
    // A stack trace is one error. Nothing pulls the view away from it to the end.
    const group = screen.getByRole("group", { name: "Errors in this log" });
    expect(group).toHaveTextContent("1 of 2");
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Next error" }));
    await waitFor(() =>
      expect(container.querySelector(".dw-log-line.is-current")).toHaveTextContent("ERROR still no database"),
    );
    expect(group).toHaveTextContent("2 of 2");
    await user.click(screen.getByRole("button", { name: "Next error" }));
    expect(group).toHaveTextContent("1 of 2");
    await user.click(screen.getByRole("button", { name: "Previous error" }));
    expect(group).toHaveTextContent("2 of 2");
    await expectAccessible(container);
  });

  it("says when and why it failed, with the revision that took over, and not how long logs stay", async () => {
    const dead = create(LogSourceSchema, {
      ...failed,
      image: "shop:crash",
      exitCode: 1,
      finishTime: timestampFromMs(new Date(2026, 10, 5, 1, 19).getTime()),
    });
    vi.setSystemTime(new Date(2026, 10, 5, 12, 0));
    try {
      const { logs } = api(sourcesOf(create(LogSourceSchema, { ...running, revision: 3n }), dead));
      renderAt(`${URL}?change=chg_2`, { logs });
      const alert = await screen.findByRole("alert");
      expect(alert).toHaveTextContent("r2 failed at 01:19 and was rolled back to r3.");
      expect(alert).toHaveTextContent("shop:crash exited with code 1.");
      expect(alert).not.toHaveTextContent("30 days");
    } finally {
      vi.useRealTimers();
    }
  });

  it("gives the day when it wasn't today", async () => {
    const dead = create(LogSourceSchema, {
      ...failed,
      finishTime: timestampFromMs(new Date(2026, 9, 1, 1, 19).getTime()),
    });
    const { logs } = api(sourcesOf(running, dead));
    renderAt(`${URL}?change=chg_2`, { logs });
    expect(await screen.findByRole("alert")).toHaveTextContent(/1 Oct, 01:19/);
  });

  it("labels a deploy with its revision in mono and how it ended in words, leaving the machine's name as it is", async () => {
    const { logs } = api(sourcesOf(running, failed));
    const { container } = renderAt(`${URL}?change=chg_2`, { logs });
    const option = await screen.findByRole("radio", { name: /r2\s*failed/ });
    expect(option.querySelector("code")).toHaveTextContent("r2");
    expect(option.querySelector("svg")).not.toBeNull();
    expect(screen.getByRole("radio", { name: /Running now · r3/ })).toBeVisible();
    expect(container.textContent).not.toContain("R2");
  });

  it("is named by the app it was of, once there is no app", async () => {
    const { logs } = api(sourcesOf(failed));
    renderAt(URL, { logs });
    expect(await screen.findByRole("heading", { level: 1, name: "web" })).toBeVisible();
    expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toHaveTextContent("web");
  });
});

describe("what a view says of itself", () => {
  it("doesn't call a deployed app empty because its server is offline", async () => {
    const list = create(ListLogSourcesResponseSchema, {
      service: { id: SERVICE, name: "worker", environment: "production" },
      servers: [{ serverId: "srv_2", serverName: "hz-2", state: ServerLogState.OFFLINE, message: "no agent" }],
    });
    const { logs, stream } = api(list);
    renderAt(URL, { logs });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({
          servers: [{ serverId: "srv_2", serverName: "hz-2", state: ServerLogState.OFFLINE, message: "no agent" }],
          caughtUp: true,
        }),
      ),
    );
    await screen.findByText("hz-2 is offline");
    await screen.findByRole("heading", { name: "Nothing to show until hz-2 is back" });
    expect(screen.queryByText(/no containers yet/)).toBeNull();
  });

  it("says which server a search didn't reach, rather than that nothing matched", async () => {
    const readLogs = vi.fn(() => ({
      servers: [{ serverId: "srv_2", serverName: "hz-2", state: ServerLogState.OFFLINE, message: "no agent" }],
    }));
    await withLines([line("here")], { readLogs }, `${URL}?q=far&deep=true`);
    await screen.findByRole("heading", { name: "hz-2 couldn't be searched" });
    expect(screen.queryByText(/Nothing in all the servers keep/)).toBeNull();
    expect(screen.queryByText(/no lines/)).toBeNull();
  });

  it("shows a search that failed as a failure, with a way to try again", async () => {
    let fail = true;
    const readLogs = vi.fn(() => {
      if (fail) throw new ConnectError("hz-2 can't read logs now", Code.Internal);
      return {};
    });
    const { logs, stream } = api(sourcesOf(second));
    const view = renderAt(`${URL}?q=far&deep=true`, { logs: { ...logs, readLogs } });
    await screen.findByRole("log");
    await act(() => stream.send(message({ sources: [second], caughtUp: true })));
    const user = userEvent.setup();
    void view;
    await screen.findByRole("heading", { name: "The search failed" });
    expect(screen.queryByText(/Nothing in all/)).toBeNull();
    // A machine's name stays as it was written, though it comes first.
    await screen.findByText("hz-2 can't read logs now.");
    fail = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(readLogs.mock.calls.length).toBe(2));
    await screen.findByText('Nothing in all the servers keep has "far".');
  });

  it("says once how far a search that found nothing reached", async () => {
    const readLogs = vi.fn(() => ({
      sources: [running],
      searchedLines: 1500n,
      searchedBackTo: { seconds: T0 - 86_400n, nanos: 0 },
    }));
    await withLines([line("here")], { readLogs }, `${URL}?q=far&deep=true`);
    await screen.findByRole("heading", { name: 'Nothing in all the servers keep has "far".' });
    expect(screen.getAllByText(/0 matches in 1,500 lines/)).toHaveLength(1);
  });

  it("leaves a machine's name as written when a server can't read logs", async () => {
    const { logs, stream } = api();
    renderAt(URL, { logs });
    await screen.findByRole("log");
    await act(() =>
      stream.send(
        message({
          servers: [
            {
              serverId: "srv_2",
              serverName: "hz-2",
              state: ServerLogState.UNAVAILABLE,
              message: "hz-2 can't read logs now: hz-2's Docker isn't running",
            },
          ],
        }),
      ),
    );
    const note = await screen.findByText("hz-2's Docker isn't running.");
    expect(note).toBeVisible();
    expect(document.body.textContent).not.toContain("Hz-2");
  });

  it("says how a search stopped, with what is true of it", async () => {
    const stopped = (fields: object) =>
      vi.fn(() => ({
        chunks: [{ sourceId: running.id, lines: [line("a far line")] }],
        sources: [running],
        searchedLines: 323n,
        searchCutShort: true,
        searchedBackTo: { seconds: T0, nanos: 0 },
        ...fields,
      }));
    const first = await withLines(
      [line("here")],
      { readLogs: stopped({ searchTimedOut: true }) },
      `${URL}?q=far&deep=true`,
    );
    await screen.findByText(/The search stopped after 30 s, at/);
    expect(screen.queryByText(/specific/)).toBeNull();
    first.unmount();
    await withLines([line("here")], { readLogs: stopped({}) }, `${URL}?q=far&deep=true`);
    await screen.findByText(/The search stopped at the newest 1,000,000 lines of a container/);
  });

  it("states the window of a time range, and reads it again on asking", async () => {
    const { requests, user } = await withLines([line("here")], {}, `${URL}?range=15m`);
    await screen.findByText(/Last 15 min, to \d\d:\d\d\. Refresh for newer lines\./);
    const before = requests.length;
    await user.click(screen.getByRole("button", { name: "Read this range again" }));
    await waitFor(() => expect(requests.length).toBe(before + 1));
  });

  it("says which zone its times are in", async () => {
    await withLines([line("here")]);
    expect(screen.getByRole("log").closest(".dw-log")?.querySelector(".dw-log-meta")).toHaveTextContent(/UTC/);
    await screen.findByText(/· UTC/);
  });
});

describe("searching the lines loaded", () => {
  const lines = [line("cart one"), line("GET /other", 1), line("cart two", 2), line("CART three", 3)];

  it("goes through matches with Enter and Shift+Enter, and says which", async () => {
    const { container, user } = await withLines(lines);
    const field = screen.getByRole("textbox", { name: "Search logs" });
    await user.type(field, "cart");
    await waitFor(() => expect(screen.queryByText("GET /other")).toBeNull());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(container.querySelector(".dw-log-line.is-current")).toHaveTextContent("CART three"));
    expect(screen.getByText("3 of 3")).toBeVisible();
    expect(container.querySelector("mark.is-current")).toHaveTextContent("CART");
    await user.keyboard("{Enter}");
    expect(screen.getByText("2 of 3")).toBeVisible();
    await user.keyboard("{Shift>}{Enter}{/Shift}");
    expect(screen.getByText("3 of 3")).toBeVisible();
    // On past either end, it comes round.
    await user.keyboard("{Shift>}{Enter}{/Shift}");
    expect(screen.getByText("1 of 3")).toBeVisible();
  });

  it("goes back to following once a search it went through is cleared", async () => {
    const { user } = await withLines(lines);
    const field = screen.getByRole("textbox", { name: "Search logs" });
    await user.type(field, "cart");
    await waitFor(() => expect(screen.queryByText("GET /other")).toBeNull());
    await user.keyboard("{Enter}");
    await screen.findByText("Paused");
    await user.keyboard("{Escape}");
    await screen.findByText("Following");
  });

  it("clears with Escape, or the button", async () => {
    const { user } = await withLines(lines);
    const field = screen.getByRole("textbox", { name: "Search logs" });
    await user.type(field, "cart");
    await user.keyboard("{Escape}");
    expect(field).toHaveValue("");
    await screen.findByText("GET /other");
    await user.type(field, "cart");
    await user.click(screen.getByRole("button", { name: "Clear the search" }));
    expect(field).toHaveValue("");
    await screen.findByText("GET /other");
  });

  it("shows a match among the lines around it, and leaves the search as it was", async () => {
    const readLogs = vi.fn(() => ({
      chunks: [{ sourceId: running.id, lines: [line("before it"), line("cart one"), line("after it", 1)] }],
    }));
    const { container, user } = await withLines(lines, { readLogs });
    const field = screen.getByRole("textbox", { name: "Search logs" });
    await user.type(field, "cart");
    await waitFor(() => expect(screen.queryByText("GET /other")).toBeNull());
    const row = Array.from(container.querySelectorAll<HTMLElement>(".dw-log-line")).find((el) =>
      el.textContent?.includes("cart one"),
    ) as HTMLElement;
    await user.hover(row);
    fireEvent.click(await within(row).findByRole("button", { name: "Show in context" }));
    const sheet = await screen.findByRole("dialog");
    await within(sheet).findByText("before it");
    const asked = readLogs.mock.calls[0] as unknown as [{ sourceIds: string[]; until: unknown; query: string }];
    expect(asked[0].sourceIds).toEqual([running.id]);
    expect(asked[0].until).toBeDefined();
    expect(asked[0].query).toBe("");
    await waitFor(() => expect(sheet.querySelector(".dw-log-line.is-current")).toHaveTextContent("cart one"));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(field).toHaveValue("cart");
  });

  it("says how many it found to a screen reader once, after typing stops", async () => {
    const { container, stream, user } = await withLines(lines);
    const live = () =>
      Array.from(container.querySelectorAll('[aria-live="polite"]'))
        .map((el) => el.textContent)
        .filter(Boolean);
    const said: string[] = [];
    const watch = new MutationObserver(() => said.push(...live()));
    watch.observe(container, { subtree: true, characterData: true, childList: true });
    await user.type(screen.getByRole("textbox", { name: "Search logs" }), "cart");
    // The count on screen goes on moving, as what comes does.
    for (let i = 0; i < 5; i++) {
      await act(() => stream.send(message({ chunks: [{ sourceId: running.id, lines: [line(`cart ${i}`, 4 + i)] }] })));
    }
    expect(said).toEqual([]);
    await waitFor(() => expect(said.length).toBeGreaterThan(0), { timeout: 2000 });
    watch.disconnect();
    expect(new Set(said).size).toBe(1);
  });
});

describe("levels", () => {
  it("shows a level alone on asking, and keeps the last one on", async () => {
    const { router, user } = await withLines([line("a")]);
    const errors = screen.getByRole("button", { name: "Errors" });
    await user.keyboard("{Shift>}");
    await user.click(errors);
    await user.keyboard("{/Shift}");
    await waitFor(() => expect(router.state.location.search).toMatchObject({ levels: "error" }));
    expect(errors).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "Info" })).toHaveAttribute("aria-pressed", "false");
    // The last one on stays on: it doesn't turn on all three.
    await user.click(errors);
    expect(errors).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "Info" })).toHaveAttribute("aria-pressed", "false");
    await user.dblClick(screen.getByRole("button", { name: "Warnings" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ levels: "warn" }));
  });

  it("calls them one thing on any layout", async () => {
    await withLines([line("a")]);
    expect(screen.getByRole("button", { name: "Warnings" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Warn" })).toBeNull();
  });
});

describe("taking lines away", () => {
  it("can't copy or download what isn't there", async () => {
    const { logs } = api();
    renderAt(URL, { logs });
    await screen.findByRole("log");
    expect(screen.getByRole("button", { name: "Copy these lines" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Download these lines" })).toBeDisabled();
  });

  it("copies with the time as the screen has it, and names a level once", async () => {
    {
      const { user } = await withLines([
        line("ERROR POST /cart", 0, LogLevel.ERROR),
        line("no word for it", 1, LogLevel.ERROR),
        line("a line", 2),
      ]);
      await waitFor(() => expect(screen.getByRole("button", { name: "Copy these lines" })).toBeEnabled());
      const writeText = vi.spyOn(navigator.clipboard, "writeText");
      await user.click(screen.getByRole("button", { name: "Copy these lines" }));
      await waitFor(() => expect(writeText).toHaveBeenCalled());
      const copied = (writeText.mock.calls[0] as unknown as [string])[0].split("\n");
      const when = new Date(Number(T0) * 1000);
      const pad = (n: number) => String(n).padStart(2, "0");
      expect(copied[0]).toContain(`T${pad(when.getHours())}:${pad(when.getMinutes())}:`);
      expect(copied[0]).toMatch(/(Z|[+-]\d\d:\d\d) hz-1 ERROR POST \/cart$|\+00:00 hz-1 ERROR POST \/cart$/);
      expect(copied[0]).not.toContain("ERROR ERROR");
      expect(copied[1]).toMatch(/hz-1 ERROR no word for it$/);
      expect(copied[2]).toMatch(/hz-1 a line$/);
    }
  });

  it("copies one line", async () => {
    {
      const { container, user } = await withLines([line("the one"), line("another", 1)]);
      await screen.findByText("the one");
      const writeText = vi.spyOn(navigator.clipboard, "writeText");
      const row = Array.from(container.querySelectorAll<HTMLElement>(".dw-log-line")).find((el) =>
        el.textContent?.includes("the one"),
      ) as HTMLElement;
      await user.hover(row);
      fireEvent.click(await within(row).findByRole("button", { name: "Copy this line" }));
      await waitFor(() => expect(writeText).toHaveBeenCalled());
      expect((writeText.mock.calls[0] as unknown as [string])[0]).toMatch(/hz-1 the one$/);
    }
  });
});

describe("a long line", () => {
  it("keeps wrapping in the URL, and follows it", async () => {
    const { container, router, user } = await withLines([line("a line")]);
    expect(container.querySelector(".dw-log")).not.toHaveClass("no-wrap");
    await user.click(screen.getByRole("button", { name: "Wrap long lines" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ nowrap: true }));
    expect(container.querySelector(".dw-log")).toHaveClass("no-wrap");
  });
});

describe("deploysOf", () => {
  it("groups containers by the deploy that made them", () => {
    const deploys = deploysOf([running, second, failed]);
    expect(deploys).toHaveLength(2);
    expect(deploys[0]?.current).toBe(true);
    expect(deploys[0]?.sources).toHaveLength(2);
    expect(deploys[1]?.failed).toBe(true);
    expect(deploys[1]?.current).toBe(false);
  });

  it("takes each container alone when nothing says which deploy made it", () => {
    const made = create(LogSourceSchema, { id: "dddddddddddd", state: LogSourceState.RUNNING });
    expect(deploysOf([made])[0]?.key).toBe("dddddddddddd");
  });
});

describe("withLabels", () => {
  const web = (id: string, n: number, state = LogSourceState.RUNNING, serverId = "srv_1", serverName = "hz-1") =>
    create(LogSourceSchema, { id, serverId, serverName, replica: 1, state, containerName: `env_01kat-web-${n}` });
  const of = (...sources: LogSource[]) => new Map(sources.map((s) => [s.id, s]));
  const named = (all: Map<string, LogSource>, read = all) =>
    [...withLabels(all, read).values()].map((s) => replicaLabel(s));

  it("names the server at rest, whatever number a deploy has reached", () => {
    expect(named(of(web("a", 7)))).toEqual(["hz-1"]);
  });
  it("names each container while more than one on a server is read, as through a rolling deploy", () => {
    expect(named(of(web("a", 2), web("b", 3)))).toEqual(["web-2", "web-3"]);
  });
  it("goes on naming what this view read of one that has gone, as it wrote those lines", () => {
    const all = of(web("a", 2, LogSourceState.KEPT), web("b", 3));
    expect(named(all)).toEqual(["web-2", "web-3"]);
  });
  it("leaves out what was only kept, and isn't being read", () => {
    const all = of(web("a", 2, LogSourceState.KEPT), web("b", 3));
    expect(named(all, of(web("b", 3)))).toEqual(["hz-1", "hz-1"]);
  });
  it("says the server too where there are several, and only of the one that has two", () => {
    const all = of(web("a", 2), web("b", 3), web("c", 1, LogSourceState.RUNNING, "srv_2", "hz-2"));
    expect(named(all)).toEqual(["hz-1/web-2", "hz-1/web-3", "hz-2"]);
  });
});

describe("replicaLabel", () => {
  it("goes by the name a person knows a container by", () => {
    expect(instanceName({ containerName: "env_01kat-web-3" })).toBe("web-3");
    expect(instanceName({ containerName: "env_01kat-my-api-12" })).toBe("my-api-12");
    expect(instanceName({ containerName: "" })).toBe("");
  });
  it("names a server, and which of its replicas past the first", () => {
    expect(replicaLabel({ serverName: "hz-1", replica: 1 })).toBe("hz-1");
    expect(replicaLabel({ serverName: "hz-1", replica: 2 })).toBe("hz-1-2");
  });
});
