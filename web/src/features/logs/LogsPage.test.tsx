// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ListLogSourcesResponseSchema,
  LogGapReason,
  LogKept,
  LogLevel,
  LogSourceSchema,
  LogSourceState,
  ServerLogState,
  type WatchLogsRequest,
  type WatchLogsResponse,
  WatchLogsResponseSchema,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { type Api, onPhone, renderAt, untilAborted } from "../../test/render";
import { deploysOf, replicaLabel } from "./LogsPage";

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
    expect(within(screen.getByRole("log").closest(".dw-log") as HTMLElement).getByText("Live")).toBeVisible();
    await expectAccessible(container);
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
    expect(screen.getByRole("radio", { name: "All replicas" })).toBeChecked();
    await userEvent.setup().click(screen.getByRole("radio", { name: "hz-2" }));
    await waitFor(() => expect(screen.queryByText("from one")).toBeNull());
    expect(container.querySelector(".dw-log-src")).toBeNull();
  });

  it("says when there are no containers yet, and when nothing was written", async () => {
    const empty = api(sourcesOf());
    const first = renderAt(URL, { logs: empty.logs });
    await screen.findByRole("log");
    await act(() => empty.stream.send(message({ caughtUp: true })));
    await screen.findByText(/web has no containers yet/);
    first.unmount();

    const quiet = api();
    renderAt(URL, { logs: quiet.logs });
    await screen.findByText("Reading logs");
    await act(() => quiet.stream.send(message({ sources: [running], caughtUp: true })));
    await screen.findByText(/No logs yet\. web hasn't written anything/);
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
    expect(screen.getByRole("alert")).toHaveTextContent("The lines from while it was away arrive when it returns");
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
    await screen.findByText("Nothing here matches these filters.");
    await user.click(screen.getByRole("button", { name: "Clear filters" }));
    await screen.findByText("GET /orders 200");
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
    await user.type(screen.getByRole("textbox", { name: "Search logs" }), "far");
    await user.click(await screen.findByRole("button", { name: "Search all the servers keep" }));
    // Marked where it matched, so split across elements.
    await waitFor(() => expect(screen.getByRole("log")).toHaveTextContent("found far back"));
    expect(readLogs.mock.calls.length).toBe(1);
    await screen.findByText(/Searched 1,500 lines, back to/);
    await screen.findByText(/as far as the servers keep logs/);
    await user.click(screen.getByRole("button", { name: "Back to the lines loaded" }));
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
    await screen.findByText(/1,204 lines from hz-1 not shown: it wrote faster than a live view carries/);
    await screen.findByText(/1,204 lines not shown so far/);
  });

  it("shows a deploy that failed, by the link Activity gives", async () => {
    const { logs, stream, requests } = api(sourcesOf(running, failed));
    renderAt(`${URL}?change=chg_2`, { logs });
    await screen.findByRole("log");
    await waitFor(() => expect(requests[0]?.changeId).toBe("chg_2"));
    expect(requests[0]?.once).toBe(true);
    await screen.findByText("This deploy failed and was rolled back.", { exact: false });
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
    expect(screen.queryByText("Live")).toBeNull();
    expect(screen.getByRole("radio", { name: /r2 · failed/ })).toBeChecked();
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
    await screen.findByText("Nothing was written in this time.");
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
    await screen.findByText("Reconnecting to the control plane");
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

describe("replicaLabel", () => {
  it("names a server, and which of its replicas past the first", () => {
    expect(replicaLabel({ serverName: "hz-1", replica: 1 })).toBe("hz-1");
    expect(replicaLabel({ serverName: "hz-1", replica: 2 })).toBe("hz-1-2");
  });
});
