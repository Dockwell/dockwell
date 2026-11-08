// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";
import { t } from "@lingui/core/macro";

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

/**
 * The reasons the control plane gives for refusing a call for want of a
 * session: none came with it, or the one that did has ended.
 */
const SIGNED_OUT = new Set(["unauthenticated", "session_ended"]);

/**
 * True when the call failed because there is no session, or it ended. A
 * wrong code or passkey is unauthenticated too, but says so with its own
 * reason, and doesn't count.
 */
export function isSessionEnded(error: unknown): boolean {
  return ConnectError.from(error).code === Code.Unauthenticated && SIGNED_OUT.has(reasonOf(error));
}

/**
 * True when the call was refused because the caller's role doesn't cover it.
 * Trying again won't help: someone has to change the role first.
 */
export function isRoleRefusal(error: unknown): boolean {
  return reasonOf(error) === "needs_role";
}

/**
 * The control plane's own message as a sentence, for errors the panel has no
 * words of its own for. API messages start lowercase, as Go errors do. A role
 * refusal can come from any call, so it gets the panel's words here, in the
 * panel's language, wherever it shows.
 */
export function sentence(error: unknown): string {
  if (isRoleRefusal(error)) return t`Your role doesn't let you do this here. Ask an admin to change your role.`;
  const err = ConnectError.from(error);
  const text = (err.rawMessage || err.message).trim();
  if (!text) return "";
  // A domain, an address or a name leads as it's written, and what's in it tells it
  // from a word: bad-db-1, legacy_api, shop2.
  const capital = /^\S*[.:/_\d-]\S*\s/.test(text) ? text : text.charAt(0).toUpperCase() + text.slice(1);
  return /[.!?]$/.test(capital) ? capital : `${capital}.`;
}
