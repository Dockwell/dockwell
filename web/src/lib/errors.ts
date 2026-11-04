// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";

/** The metadata key the control plane names a failure's reason under (see AuthService). */
const REASON_KEY = "Dockwell-Error";

/** Why a call failed, as the control plane names it, such as "code_rejected"; "" when it doesn't say. */
export function reasonOf(error: unknown): string {
  return ConnectError.from(error).metadata.get(REASON_KEY) ?? "";
}

/** Seconds to wait before trying again, when the control plane says. */
export function retryAfter(error: unknown): number | undefined {
  const value = Number(ConnectError.from(error).metadata.get("Retry-After"));
  return Number.isFinite(value) && value > 0 ? value : undefined;
}

/** True when the call failed because there is no session, or it ended. */
export function isSignedOut(error: unknown): boolean {
  return ConnectError.from(error).code === Code.Unauthenticated;
}

/**
 * The control plane's own message as a sentence, for errors the panel has no
 * words of its own for. API messages start lowercase, as Go errors do.
 */
export function sentence(error: unknown): string {
  const err = ConnectError.from(error);
  const text = (err.rawMessage || err.message).trim();
  if (!text) return "";
  const capital = text.charAt(0).toUpperCase() + text.slice(1);
  return /[.!?]$/.test(capital) ? capital : `${capital}.`;
}
