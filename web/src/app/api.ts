// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { QueryClient } from "@tanstack/react-query";

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

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 30_000,
        retry: (failures, error) => failures < 2 && isTransient(error),
        retryDelay: (attempt) => Math.min(500 * 2 ** attempt, 4_000),
      },
    },
  });
}
