// SPDX-License-Identifier: AGPL-3.0-only

import { createRouterTransport, type HandlerContext, type ServiceImpl } from "@connectrpc/connect";
import { ServerService, SystemService } from "@dockwell/api";
import { QueryClient } from "@tanstack/react-query";
import { render, renderHook } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";

import { Providers } from "../app/Providers";

/** The parts of the API a test answers. Anything left out fails as unimplemented. */
export type Api = {
  system?: Partial<ServiceImpl<typeof SystemService>>;
  servers?: Partial<ServiceImpl<typeof ServerService>>;
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
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
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
