// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { createRouterTransport, type HandlerContext, type ServiceImpl } from "@connectrpc/connect";
import { AuthService, GetVersionResponseSchema, ServerService, SystemService } from "@dockwell/api";
import { PHONE_QUERY } from "@dockwell/ui";
import { QueryClient } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { render, renderHook } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";
import { vi } from "vitest";

import { signOutAwareCaches } from "../app/api";
import { Providers } from "../app/Providers";
import { routeTree } from "../routeTree.gen";
import { authState } from "./auth";

/** The parts of the API a test answers. Anything left out fails as unimplemented. */
export type Api = {
  system?: Partial<ServiceImpl<typeof SystemService>>;
  servers?: Partial<ServiceImpl<typeof ServerService>>;
  auth?: Partial<ServiceImpl<typeof AuthService>>;
};

/** A server watch that stays open with nothing to report, so screens around it settle. */
export async function* quietWatch(_req: unknown, ctx: HandlerContext) {
  await untilAborted(ctx.signal);
  yield* [];
}

/** Resolves when the call is cancelled, which is how a stream handler waits for its client to leave. */
export function untilAborted(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) resolve();
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

/** The real providers around an in-memory API and a query cache that doesn't retry. */
function apiWrapper(api: Api) {
  const transport = createRouterTransport(({ service }) => {
    service(SystemService, api.system ?? {});
    service(ServerService, { watchServers: quietWatch, ...api.servers });
    service(AuthService, api.auth ?? {});
  });
  const queryClient: QueryClient = new QueryClient({
    ...signOutAwareCaches(() => queryClient),
    defaultOptions: { queries: { retry: false } },
  });
  return ({ children }: { children: ReactNode }) => (
    <Providers transport={transport} queryClient={queryClient}>
      {children}
    </Providers>
  );
}

/** Renders ui against api. */
export function renderWithApi(ui: ReactElement, api: Api) {
  return render(ui, { wrapper: apiWrapper(api) });
}

/** The control plane's build in tests that don't ask for another. */
export const version = () =>
  create(GetVersionResponseSchema, { build: { version: "v0.4.0", platform: "linux/amd64" } });

/**
 * Renders the whole panel at path, routes and all, against api, as the
 * browser would load it. Unless api says otherwise, the control plane
 * answers GetVersion with v0.4.0, and GetAuthState with Mira signed in.
 */
export function renderAt(path: string, api: Api = {}) {
  const router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [path] }) });
  const result = renderWithApi(<RouterProvider router={router} />, {
    ...api,
    system: { getVersion: version, ...api.system },
    auth: { getAuthState: () => authState(), ...api.auth },
  });
  return { ...result, router };
}

/** Makes the screen phone-sized from here on in the test: matchMedia matches the phone query. */
export function onPhone() {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: query === PHONE_QUERY,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
}

/** Renders a hook against api. */
export function renderHookWithApi<T>(hook: () => T, api: Api) {
  return renderHook(hook, { wrapper: apiWrapper(api) });
}

/**
 * A stream a test feeds by hand: push messages, then fail it or let the
 * client hang up. Pass stream as a server-streaming handler.
 */
export function feed<T>() {
  const queue: T[] = [];
  let failure: unknown;
  let wake = () => {};
  const changed = () => wake();
  return {
    push(...messages: T[]) {
      queue.push(...messages);
      changed();
    },
    fail(err: unknown) {
      failure = err;
      changed();
    },
    async *stream(_req: unknown, ctx: HandlerContext): AsyncGenerator<T> {
      for (;;) {
        const next = queue.shift();
        if (next !== undefined) {
          yield next;
          continue;
        }
        if (failure !== undefined) throw failure;
        if (ctx.signal.aborted) return;
        await Promise.race([new Promise<void>((resolve) => (wake = resolve)), untilAborted(ctx.signal)]);
      }
    },
  };
}
