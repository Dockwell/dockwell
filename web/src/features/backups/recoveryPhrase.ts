// SPDX-License-Identifier: AGPL-3.0-only

import commonPasswords from "../../../../internal/controlbackup/common-passwords.txt?raw";

// Keep the estimate in step with internal/controlbackup/phrase.go.
const trimSpace = (value: string) => value.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, "");
const commonSegments = commonPasswords
  .split("\n")
  .filter((s) => s && !s.startsWith("#"))
  .sort((a, b) => b.length - a.length);
const keyRows = ["1234567890-=", "qwertyuiop[]", "asdfghjkl;'", "zxcvbnm,./"];
const fromLeet = "@4301!5$7+";
const toLetter = "aaeoiisstt";

export function recoveryPhraseStrength(input: string): "format" | "weak" | "ready" {
  const code = trimSpace(input)
    .replace(/[- _]/g, "")
    .replace(/[a-z]/g, (c) => c.toUpperCase())
    .replace(/O/g, "0")
    .replace(/[IL]/g, "1");
  const phrase = /^[0-9A-HJKMNP-TV-Z]{32}$/.test(code) ? (code.match(/.{4}/g) ?? []).join("-") : input;
  if (
    new TextEncoder().encode(input).length > 256 ||
    Array.from(phrase).length < 20 ||
    trimSpace(phrase) !== phrase ||
    /\p{Cc}/u.test(input)
  ) {
    return "format";
  }
  return phraseBits(phrase) < 80 ? "weak" : "ready";
}

function phraseBits(phrase: string): number {
  const compact = phrase.replace(/[- _]/g, "");
  const alphabet = [
    [/[a-z]/, 26],
    [/[A-Z]/, 26],
    [/[0-9]/, 10],
    [/[\x20-\x2f\x3a-\x40\x5b-\x60\x7b-\x7e]/, 33],
    [/[\u0080-\u{10ffff}]/u, 26],
  ] as const;
  const size = alphabet.reduce((total, [pattern, count]) => total + (pattern.test(compact) ? count : 0), 0);
  const perChar = Math.log2(Math.max(1, size));
  const normal = Array.from(compact.replace(/[A-Z]/g, (c) => c.toLowerCase()));
  const folded = normal.map((c) => toLetter[fromLeet.indexOf(c)] ?? c);
  let bits = 0;
  for (let i = 0; i < normal.length; ) {
    const repeated = repeatLength(folded, i);
    if (repeated >= 3) {
      i += repeated;
      continue;
    }
    const common = commonSegments.find(
      (word) =>
        word[0] === folded[i] &&
        word.length <= folded.length - i &&
        Array.from(word).every((c, j) => c === folded[i + j]),
    );
    if (common) {
      bits += Math.min(10, perChar * common.length);
      i += common.length;
      continue;
    }
    if (i === 0 || !relatedCharacters(normal[i - 1] ?? "", normal[i] ?? "")) bits += perChar;
    i++;
  }
  return bits;
}

function repeatLength(text: string[], at: number): number {
  let longest = 0;
  for (let start = 0; start + 3 <= at; start++) {
    let n = 0;
    while (start + n < at && at + n < text.length && text[start + n] === text[at + n]) n++;
    longest = Math.max(longest, n);
  }
  return longest;
}

function relatedCharacters(a: string, b: string): boolean {
  if (a === b) return true;
  const delta = Math.abs(a.charCodeAt(0) - b.charCodeAt(0));
  if (a >= "0" && a <= "9" && b >= "0" && b <= "9" && (delta === 1 || delta === 9)) return true;
  if (a >= "a" && a <= "z" && b >= "a" && b <= "z" && delta === 1) return true;
  const [rowA, xA] = keyPosition(a);
  const [rowB, xB] = keyPosition(b);
  return rowA >= 0 && rowB >= 0 && Math.abs(rowA - rowB) <= 1 && Math.abs(xA - xB) <= 4;
}

function keyPosition(c: string): [number, number] {
  for (const [row, keys] of keyRows.entries()) {
    const column = keys.indexOf(c);
    if (column >= 0) return [row, 4 * column + row];
  }
  return [-1, 0];
}
