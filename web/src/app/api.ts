// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError, type Interceptor, type Transport } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AuthService } from "@dockwell/api";
import { QueryClient } from "@tanstack/react-query";

import { isSessionEnded } from "../lib/errors";

/**
 * The panel talks to the control plane that served it, over the Connect
 * protocol: binary in production, JSON in development so requests read well
 * in devtools. Read-only methods go out as GET, so they can be cached.
 */
export function createTransport(baseUrl: string = window.location.origin, interceptors: Interceptor[] = []): Transport {
  return createConnectTransport({
    baseUrl,
    useBinaryFormat: import.meta.env.PROD,
    useHttpGet: true,
    interceptors,
  });
}

/** The query cache and the transport the panel runs on, wired so an ended session sends it back to the gate. */
export function createApi(baseUrl: string = window.location.origin): {
  transport: Transport;
  queryClient: QueryClient;
} {
  const queryClient = createQueryClient();
  return { queryClient, transport: createTransport(baseUrl, [watchSession(() => recheckAuth(queryClient))]) };
}

/** Failures worth retrying: the server or the network, not the request. */
const transient = new Set([Code.Unavailable, Code.Unknown, Code.DeadlineExceeded, Code.Aborted]);

export function isTransient(error: unknown): boolean {
  return transient.has(ConnectError.from(error).code);
}

/** The key of the sign-in state that the auth gate reads. */
export const authStateKey = createConnectQueryKey({ schema: AuthService.method.getAuthState, cardinality: undefined });

/** The query clients with a recheck under way. */
const rechecking = new WeakSet<QueryClient>();

/**
 * Makes the gate read the sign-in state again, after a call found the
 * session ended. A read already under way may have left before the session
 * ended, and would say it hadn't, so this starts its own. Calls that find
 * the session ended while that read is under way share it: it left after
 * the session ended.
 */
export function recheckAuth(client: QueryClient): void {
  if (rechecking.has(client)) return;
  rechecking.add(client);
  void client.invalidateQueries({ queryKey: authStateKey }).finally(() => rechecking.delete(client));
}

/**
 * Sends the panel back to the gate when a session ends mid-use: any call
 * refused because its session ended, or there is none, calls onEnded. It
 * sits on the transport, so it sees every call, from queries and from
 * clients made by hand alike, and the errors that end a stream.
 */
export function watchSession(onEnded: () => void): Interceptor {
  const check = (error: unknown) => {
    if (isSessionEnded(error)) onEnded();
  };
  return (next) => async (req) => {
    try {
      const res = await next(req);
      return res.stream ? { ...res, message: watchStream(res.message, check) } : res;
    } catch (err) {
      check(err);
      throw err;
    }
  };
}

async function* watchStream<T>(messages: AsyncIterable<T>, check: (error: unknown) => void): AsyncIterable<T> {
  try {
    yield* messages;
  } catch (err) {
    check(err);
    throw err;
  }
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
