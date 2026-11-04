// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AuthService } from "@dockwell/api";
import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";

import { isSignedOut } from "../lib/errors";

/**
 * The panel talks to the control plane that served it, over the Connect
 * protocol: binary in production, JSON in development so requests read well
 * in devtools. Read-only methods go out as GET, so they can be cached.
 */
export function createTransport(baseUrl: string = window.location.origin): Transport {
  return createConnectTransport({
    baseUrl,
    useBinaryFormat: import.meta.env.PROD,
    useHttpGet: true,
  });
}

/** Failures worth retrying: the server or the network, not the request. */
const transient = new Set([Code.Unavailable, Code.Unknown, Code.DeadlineExceeded, Code.Aborted]);

export function isTransient(error: unknown): boolean {
  return transient.has(ConnectError.from(error).code);
}

/** The key of the sign-in state that the auth gate reads. */
export const authStateKey = createConnectQueryKey({ schema: AuthService.method.getAuthState, cardinality: undefined });

/**
 * Caches that send the panel back to the gate when a session ends mid-use:
 * any call that fails as unauthenticated makes the gate check the sign-in
 * state again.
 */
export function signOutAwareCaches(client: () => QueryClient) {
  const recheck = (error: unknown) => {
    if (isSignedOut(error)) void client().invalidateQueries({ queryKey: authStateKey });
  };
  return { queryCache: new QueryCache({ onError: recheck }), mutationCache: new MutationCache({ onError: recheck }) };
}

export function createQueryClient(): QueryClient {
  const client: QueryClient = new QueryClient({
    ...signOutAwareCaches(() => client),
    defaultOptions: {
      queries: {
        staleTime: 30_000,
        retry: (failures, error) => failures < 2 && isTransient(error),
        retryDelay: (attempt) => Math.min(500 * 2 ** attempt, 4_000),
      },
    },
  });
  return client;
}
