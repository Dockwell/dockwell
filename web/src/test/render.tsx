// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { createRouterTransport, type HandlerContext, type ServiceImpl } from "@connectrpc/connect";
import {
  AuthService,
  BackupService,
  CatalogService,
  ChangeService,
  ComposeService,
  ContainerService,
  DomainService,
  EventService,
  FileService,
  GetVersionResponseSchema,
  HealthService,
  LogService,
  ResourceService,
  ServerService,
  ServiceService,
  SettingsService,
  SystemService,
  TokenService,
  UpdateService,
  VariableService,
} from "@dockwell/api";
import { PHONE_QUERY } from "@dockwell/ui";
import { QueryClient } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { render, renderHook } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";
import { vi } from "vitest";

import { recheckAuth, watchSession } from "../app/api";
import { Providers } from "../app/Providers";
import { routeTree } from "../routeTree.gen";
import { authState } from "./auth";
import { track } from "./routers";

/** The parts of the API a test answers. Anything left out fails as unimplemented. */
export type Api = {
  system?: Partial<ServiceImpl<typeof SystemService>>;
  servers?: Partial<ServiceImpl<typeof ServerService>>;
  auth?: Partial<ServiceImpl<typeof AuthService>>;
  tokens?: Partial<ServiceImpl<typeof TokenService>>;
  changes?: Partial<ServiceImpl<typeof ChangeService>>;
  events?: Partial<ServiceImpl<typeof EventService>>;
  resources?: Partial<ServiceImpl<typeof ResourceService>>;
  variables?: Partial<ServiceImpl<typeof VariableService>>;
  domains?: Partial<ServiceImpl<typeof DomainService>>;
  services?: Partial<ServiceImpl<typeof ServiceService>>;
  compose?: Partial<ServiceImpl<typeof ComposeService>>;
  catalog?: Partial<ServiceImpl<typeof CatalogService>>;
  logs?: Partial<ServiceImpl<typeof LogService>>;
  health?: Partial<ServiceImpl<typeof HealthService>>;
  containers?: Partial<ServiceImpl<typeof ContainerService>>;
  files?: Partial<ServiceImpl<typeof FileService>>;
  backups?: Partial<ServiceImpl<typeof BackupService>>;
  settings?: Partial<ServiceImpl<typeof SettingsService>>;
  updates?: Partial<ServiceImpl<typeof UpdateService>>;
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

/** How a test's query cache behaves. */
export interface CacheOptions {
  /** How long what it read stays fresh: 0, unless a test needs the panel's 30 s, as createQueryClient has. */
  staleTime?: number;
}

/** The real providers, and the real session watch, around an in-memory API and a query cache that doesn't retry. */
function apiWrapper(api: Api, cache: CacheOptions = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: cache.staleTime ?? 0 } },
  });
  const transport = createRouterTransport(
    ({ service }) => {
      service(SystemService, api.system ?? {});
      service(ServerService, { watchServers: quietWatch, ...api.servers });
      service(AuthService, api.auth ?? {});
      service(TokenService, api.tokens ?? {});
      service(ChangeService, api.changes ?? {});
      // The command bar lists projects and recent changes; with none, it lists none.
      service(EventService, { listEvents: () => ({}), watchEvents: quietWatch, ...api.events });
      service(ResourceService, { listResources: () => ({}), ...api.resources });
      service(VariableService, api.variables ?? {});
      service(DomainService, { listDomains: () => ({}), ...api.domains });
      service(ServiceService, { listServices: () => ({}), ...api.services });
      service(ComposeService, api.compose ?? {});
      service(CatalogService, { listCatalogEntries: () => ({}), listCatalogInstalls: () => ({}), ...api.catalog });
      service(LogService, api.logs ?? {});
      service(HealthService, { listHealth: () => ({}), ...api.health });
      service(ContainerService, { listContainers: () => ({}), ...api.containers });
      service(FileService, api.files ?? {});
      service(SettingsService, { getSettings: () => ({ settings: { timeZone: "UTC" } }), ...api.settings });
      service(UpdateService, api.updates ?? {});
      service(BackupService, {
        listBackups: () => ({}),
        listBackupPolicies: () => ({}),
        listBackupDestinations: () => ({}),
        ...api.backups,
      });
    },
    { transport: { interceptors: [watchSession(() => recheckAuth(queryClient))] } },
  );
  return ({ children }: { children: ReactNode }) => (
    <Providers transport={transport} queryClient={queryClient}>
      {children}
    </Providers>
  );
}

/** Renders ui against api. */
export function renderWithApi(ui: ReactElement, api: Api, cache?: CacheOptions) {
  return render(ui, { wrapper: apiWrapper(api, cache) });
}

/** The control plane's build in tests that don't ask for another. */
export const version = () =>
  create(GetVersionResponseSchema, { build: { version: "v0.4.0", platform: "linux/amd64" } });

/**
 * Renders the whole panel at path, routes and all, against api, as the
 * browser would load it. Unless api says otherwise, the control plane
 * answers GetVersion with v0.4.0, and GetAuthState with Mira signed in.
 */
export function renderAt(path: string, api: Api = {}, cache?: CacheOptions) {
  const router = track(createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [path] }) }));
  const result = renderWithApi(
    <RouterProvider router={router} />,
    {
      ...api,
      system: { getVersion: version, ...api.system },
      auth: { getAuthState: () => authState(), ...api.auth },
    },
    cache,
  );
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

/** A wide screen where only this media query matches. */
export function onScreen(matching: string) {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: query === matching,
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
