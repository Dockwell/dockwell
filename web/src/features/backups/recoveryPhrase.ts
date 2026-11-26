// SPDX-License-Identifier: AGPL-3.0-only

// Keep these checks in step with internal/controlbackup/phrase.go.
const trimSpace = (value: string) => value.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, "");

export function recoveryPhraseStrength(input: string): "format" | "weak" | "ready" {
  const code = trimSpace(input)
    .replace(/[- _]/g, "")
    .replace(/[a-z]/g, (c) => c.toUpperCase())
    .replace(/O/g, "0")
    .replace(/[IL]/g, "1");
  const phrase = /^[0-9A-HJKMNP-TV-Z]{32}$/.test(code) ? (code.match(/.{4}/g) ?? []).join("-") : input;
  const length = Array.from(phrase).length;
  if (
    length < 20 ||
    new TextEncoder().encode(phrase).length > 256 ||
    phrase.trim() !== phrase ||
    /[\r\n\0]/.test(phrase)
  ) {
    return "format";
  }
  const alphabet = [
    [/[a-z]/, 26],
    [/[A-Z]/, 26],
    [/[0-9]/, 10],
    [/[\x20-\x2f\x3a-\x40\x5b-\x60\x7b-\x7e]/, 33],
    [/[\u0080-\u{10ffff}]/u, 26],
  ] as const;
  const size = alphabet.reduce((total, [pattern, count]) => total + (pattern.test(phrase) ? count : 0), 0);
  if (length * Math.log2(size) < 80) return "weak";
  const compact = phrase.toLowerCase().replace(/[- _]/g, "");
  const letters = compact.replace(/[0-9!@#$%^&*()+.?]+$/, "");
  const common = [
    "password",
    "passphrase",
    "letmein",
    "admin",
    "welcome",
    "iloveyou",
    "changeme",
    "qwerty",
    "passwordpassword",
    "correcthorsebatterystaple",
  ];
  const sequences = [
    "abcdefghijklmnopqrstuvwxyz",
    "zyxwvutsrqponmlkjihgfedcba",
    "01234567890123456789012345678901",
    "qwertyuiopasdfghjklzxcvbnm",
  ];
  const runes = Array.from(compact);
  if (runes.length < 2 || common.includes(letters) || sequences.some((s) => s.includes(compact))) return "weak";
  for (let period = 1; period <= runes.length / 2; period++) {
    if (runes.slice(period).every((c, i) => c === runes[i])) return "weak";
  }
  return "ready";
}
