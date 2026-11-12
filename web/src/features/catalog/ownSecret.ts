// SPDX-License-Identifier: AGPL-3.0-only

import { Characters, GenerateAs, type Input } from "@dockwell/api";

/** How many characters a person's own value for a generated secret has at least, as catalog.OwnFloor says. */
export const OWN_FLOOR = 8;

/** What the app takes in place of a secret Dockwell would generate, as its input says. */
export type OwnRule =
  /** A key the app decodes: exactly size hex digits. */
  | { form: "hex"; size: number }
  /** A key the app decodes: exactly size bytes, in base64. */
  | { form: "base64"; size: number }
  /** Text of at least min characters, of those a URL carries alone where urlSafe. */
  | { form: "text"; min: number; urlSafe: boolean };

export function ownRule(input: Input): OwnRule {
  if (input.generateAs === GenerateAs.HEX) return { form: "hex", size: input.generate };
  if (input.generateAs === GenerateAs.BASE64) return { form: "base64", size: input.generate };
  return {
    form: "text",
    min: input.minLength || (input.generate > 0 ? OWN_FLOOR : 1),
    urlSafe: input.characters === Characters.URL_SAFE,
  };
}

/** What's wrong with a value for a rule, as the control plane would refuse it, and a space at either end, which it would drop. */
export type OwnFault = "ends" | "form" | "short" | "unprintable" | "url";

const PRINTABLE = /^[\p{L}\p{M}\p{N}\p{P}\p{S} ]*$/u;
const URL_SAFE = /^[A-Za-z0-9._~-]*$/;

export function ownFault(rule: OwnRule, value: string): OwnFault | undefined {
  if (value !== value.trim()) return "ends";
  if (rule.form === "hex") return value.length === rule.size && /^[0-9a-fA-F]*$/.test(value) ? undefined : "form";
  if (rule.form === "base64") return base64Bytes(value) === rule.size ? undefined : "form";
  if ([...value].length < rule.min) return "short";
  if (!PRINTABLE.test(value)) return "unprintable";
  if (rule.urlSafe && !URL_SAFE.test(value)) return "url";
  return undefined;
}

/** How many bytes a value in padded standard base64 decodes to, or -1 when it isn't that. */
function base64Bytes(value: string): number {
  if (value.length % 4 !== 0 || !/^[A-Za-z0-9+/]*={0,2}$/.test(value)) return -1;
  try {
    return atob(value).length;
  } catch {
    return -1;
  }
}

const LETTERS_AND_DIGITS = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789";

/**
 * A strong value the rule takes, made in the browser, as the control plane would make
 * one: letters and digits, which every app and URL takes, as many as the entry
 * generates; or the key's own form.
 */
export function strongValue(rule: OwnRule, generate: number): string {
  if (rule.form === "hex") {
    const bytes = crypto.getRandomValues(new Uint8Array(Math.ceil(rule.size / 2)));
    return Array.from(bytes, (b) => b.toString(16).padStart(2, "0"))
      .join("")
      .slice(0, rule.size);
  }
  if (rule.form === "base64") {
    const bytes = crypto.getRandomValues(new Uint8Array(rule.size));
    return btoa(String.fromCharCode(...bytes));
  }
  const length = Math.max(generate, rule.min, 32);
  let out = "";
  // Bytes past the largest multiple of 62 below 256 are drawn again, so each character is as likely.
  const limit = 256 - (256 % LETTERS_AND_DIGITS.length);
  while (out.length < length) {
    for (const b of crypto.getRandomValues(new Uint8Array(length * 2))) {
      if (b < limit && out.length < length) out += LETTERS_AND_DIGITS[b % LETTERS_AND_DIGITS.length];
    }
  }
  return out;
}
