// SPDX-License-Identifier: AGPL-3.0-only

/** A code from an authenticator app, or a recovery code. */
export type CodeKind = "totp" | "recovery";

/** Crockford's base32, which recovery codes are written in: no I, L, O or U. */
const RECOVERY = /^[0-9A-HJKMNP-TV-Z]{16}$/;

/**
 * A typed or pasted code as the control plane reads it, and which kind it
 * is, or undefined when it can be neither. Spaces of any width, dashes and
 * line breaks go, letters go up to capitals, and the letters Crockford's
 * base32 reads as digits become them: I and L as 1, O as 0. The control
 * plane does the same, though for fewer kinds of space and dash, so the
 * panel sends what it checked.
 */
export function normalizeCode(typed: string): { code: string; kind: CodeKind } | undefined {
  const code = typed
    .toUpperCase()
    .replace(/[\s\p{Pd}]/gu, "")
    .replace(/[IL]/g, "1")
    .replace(/O/g, "0");
  if (/^\d{6}$/.test(code)) return { code, kind: "totp" };
  if (RECOVERY.test(code)) return { code, kind: "recovery" };
  return undefined;
}
