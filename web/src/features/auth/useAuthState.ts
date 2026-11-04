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

/**
 * Who is signed in, for the parts of the shell that show it. The gate has
 * read the sign-in state already, so they don't read it again as they
 * appear: such a read could answer from just before a session ended, and the
 * gate's recheck would wait for it rather than ask again.
 */
export function useSignedInUser() {
  return useQuery(AuthService.method.getAuthState, {}, { refetchOnMount: false }).data?.user;
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
  const [, auth] = authStateKey;
  return opts?.serviceName === auth.serviceName && opts?.methodName === auth.methodName;
}

/** Where to go after signing in: a path on this panel, never another site. */
export function safeNext(next: unknown): string {
  return typeof next === "string" && next.startsWith("/") && !next.startsWith("//") && !next.startsWith("/\\")
    ? next
    : "/";
}
