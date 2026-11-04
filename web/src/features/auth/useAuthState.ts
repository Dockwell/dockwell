// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { AuthService } from "@dockwell/api";
import { type Query, type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useCallback } from "react";

import { authStateKey } from "../../app/api";

/** Whether the install has an owner, and who is signed in on this address. */
export function useAuthState() {
  return useQuery(AuthService.method.getAuthState, {});
}

/**
 * The sign-in state as the gate last read it, for pages behind the gate and
 * the parts of the shell that show who is signed in. They don't read it
 * again as they appear: the gate has an answer already, and keeps it
 * current.
 */
export function useSignedIn() {
  return useQuery(AuthService.method.getAuthState, {}, { refetchOnMount: false }).data;
}

/** Who is signed in, from the gate's answer. */
export function useSignedInUser() {
  return useSignedIn()?.user;
}

/** Makes the gate read the sign-in state again, after signing in or out. */
export function useRefreshAuth() {
  const client = useQueryClient();
  return useCallback(async () => {
    forgetSession(client);
    await client.invalidateQueries({ queryKey: authStateKey });
  }, [client]);
}

/**
 * Forgets what the cache holds from the session before, other than the
 * sign-in state itself: what nothing on screen shows goes now, and what a
 * page still shows goes as the page leaves. Taken from under a page still on
 * screen, its queries would empty and ask again without the session they
 * belonged to: the profile flashing "Loading" as you sign out, or the claim
 * page checking its spent link on its way to the overview.
 */
function forgetSession(client: QueryClient) {
  const cache = client.getQueryCache();
  const shown = new Set<Query>();
  for (const query of cache.findAll({
    predicate: (q) => q.queryKey[0] === "connect-query" && !matchesAuth(q.queryKey),
  })) {
    if (query.getObserversCount() === 0) cache.remove(query);
    else shown.add(query);
  }
  if (shown.size === 0) return;
  const unsubscribe = cache.subscribe((event) => {
    if (!shown.has(event.query)) return;
    const left = event.type === "observerRemoved" && event.query.getObserversCount() === 0;
    if (!left && event.type !== "removed") return;
    shown.delete(event.query);
    if (left) cache.remove(event.query);
    if (shown.size === 0) unsubscribe();
  });
}

function matchesAuth(key: readonly unknown[]): boolean {
  const [, opts] = key as [string, { serviceName?: string; methodName?: string }];
  const [, auth] = authStateKey;
  return opts?.serviceName === auth.serviceName && opts?.methodName === auth.methodName;
}

/** Where to go after signing in: a path on this panel, never another site. */
export function safeNext(next: unknown): string {
  return typeof next === "string" && next.startsWith("/") && !next.startsWith("//") && !next.startsWith("/\\")
    ? next
    : "/";
}
