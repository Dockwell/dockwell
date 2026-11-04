// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { AuthService } from "@dockwell/api";
import { useQueryClient } from "@tanstack/react-query";
import { useCallback } from "react";

import { authStateKey } from "../../app/api";

/** Whether the install has an owner, and who is signed in on this address. */
export function useAuthState() {
  return useQuery(AuthService.method.getAuthState, {});
}

/** Makes the gate read the sign-in state again, after signing in or out. */
export function useRefreshAuth() {
  const client = useQueryClient();
  return useCallback(async () => {
    // Everything cached belonged to the previous session.
    client.removeQueries({ predicate: (q) => q.queryKey[0] === "connect-query" && !matchesAuth(q.queryKey) });
    await client.invalidateQueries({ queryKey: authStateKey });
  }, [client]);
}

function matchesAuth(key: readonly unknown[]): boolean {
  const [, opts] = key as [string, { serviceName?: string; methodName?: string }];
  return opts?.serviceName === authStateKey[1].serviceName && opts?.methodName === "GetAuthState";
}

/** Where to go after signing in: a path on this panel, never another site. */
export function safeNext(next: unknown): string {
  return typeof next === "string" && next.startsWith("/") && !next.startsWith("//") && !next.startsWith("/\\")
    ? next
    : "/";
}
