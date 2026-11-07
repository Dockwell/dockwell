// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import type { HandlerContext } from "@connectrpc/connect";
import { HealthState, ListLogSourcesResponseSchema, LogSourceSchema, LogSourceState } from "@dockwell/api";
import { act, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { HIDDEN_CLOSE_MS } from "../features/activity/EventsHub";

import { aHealth, anApp, places } from "../test/apps";
import { snapshot } from "../test/changes";
import { type Api, renderAt, untilAborted } from "../test/render";
import { aServer, changed, snapshotComplete } from "../test/servers";

/**
 * Over plain HTTP, as through the SSH tunnel, a browser keeps six connections to
 * the panel for all its tabs, and each open stream holds one. A screen holds the
 * fleet's watch and the tab's one events stream, so two tabs fit in six with
 * room for their reads. Logs hold one more, their own.
 */
const BUDGET = 2;

function counting() {
  let open = 0;
  let most = 0;
  const held = <T,>(stream: (ctx: HandlerContext) => AsyncGenerator<T>) =>
    async function* (_req: unknown, ctx: HandlerContext): AsyncGenerator<T> {
      open += 1;
      most = Math.max(most, open);
      try {
        yield* stream(ctx);
      } finally {
        open -= 1;
      }
    };
  const web = anApp("web", "ghcr.io/example/web:1.5.0");
  const api: Api = {
    servers: {
      watchServers: held(async function* (ctx) {
        yield changed(aServer({ id: "srv_1", name: "hz-1" }));
        yield snapshotComplete;
        await untilAborted(ctx.signal);
      }),
    },
    events: {
      watchEvents: held(async function* (ctx) {
        yield snapshot([]);
        await untilAborted(ctx.signal);
      }),
      listEvents: () => ({}),
    },
    logs: {
      listLogSources: () =>
        create(ListLogSourcesResponseSchema, {
          sources: [
            create(LogSourceSchema, {
              id: "aaaaaaaaaaaa",
              serverId: "srv_1",
              serverName: "hz-1",
              replica: 1,
              revision: 3n,
              changeId: "chg_web",
              state: LogSourceState.RUNNING,
              containerName: "env-web-1",
            }),
          ],
        }),
      watchLogs: held(async function* (ctx) {
        await untilAborted(ctx.signal);
        yield* [];
      }),
    },
    services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({}) },
    health: { listHealth: () => ({ services: [aHealth(web.id, { state: HealthState.HEALTHY })] }) },
    resources: places(),
    domains: { listDomains: () => ({}) },
    backups: { listBackups: () => ({}), listBackupPolicies: () => ({}), listBackupDestinations: () => ({}) },
    variables: { listVariables: () => ({}) },
    catalog: { listCatalogEntries: () => ({}), listCatalogInstalls: () => ({}) },
    containers: { listContainers: () => ({}) },
    changes: { listChanges: () => ({}) },
  };
  return { api, most: () => most, open: () => open };
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 400));

describe("the streams a screen holds", () => {
  it.each([
    "/",
    "/apps",
    "/apps/svc_web",
    "/apps/svc_web/deployments",
    "/apps/svc_web/environment",
    "/activity",
    "/activity?kind=changes",
    "/domains",
    "/backups",
    "/variables",
    "/servers",
    "/servers/srv_1",
    "/catalog",
    "/profile",
    "/more",
  ])(`at most ${BUDGET} on %s, so two tabs fit in a browser's six`, async (path) => {
    const { api, most } = counting();
    const { unmount } = renderAt(path, api);
    await screen.findByRole("heading", { level: 1 });
    await settle();
    expect(most()).toBeLessThanOrEqual(BUDGET);
    // The tab's events stream is the one the screen's timeline and the services' re-read share.
    expect(most()).toBeGreaterThanOrEqual(1);
    unmount();
  });

  it("holds one more on an app's logs, the log's own stream", async () => {
    const { api, most } = counting();
    const { unmount } = renderAt("/apps/svc_web/logs", api);
    await screen.findByRole("heading", { level: 1 });
    await settle();
    expect(most()).toBe(BUDGET + 1);
    unmount();
  });
});

describe("a tab out of sight", () => {
  const seen = (state: "hidden" | "visible") => {
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
    document.dispatchEvent(new Event("visibilitychange"));
  };

  afterEach(() => {
    vi.useRealTimers();
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" });
  });

  it("closes its events stream after a while, to free the connection, and opens it again on coming back", async () => {
    const { api, open } = counting();
    const { unmount } = renderAt("/", api);
    await screen.findByRole("heading", { level: 1 });
    await waitFor(() => expect(open()).toBe(2));
    vi.useFakeTimers({ shouldAdvanceTime: true });

    act(() => seen("hidden"));
    await act(() => vi.advanceTimersByTimeAsync(HIDDEN_CLOSE_MS - 1_000));
    expect(open()).toBe(2);
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    await waitFor(() => expect(open()).toBe(1));

    act(() => seen("visible"));
    await waitFor(() => expect(open()).toBe(2));
    unmount();
  });
});
