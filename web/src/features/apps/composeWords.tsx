// SPDX-License-Identifier: AGPL-3.0-only

import { Fragment, type ReactNode } from "react";

/** A Compose refusal, ready to show: the line it is about, and what to say, as a sentence. */
export interface RefusedAt {
  line: number;
  text: string;
}

function sentenceOf(text: string): string {
  const out = text.charAt(0).toUpperCase() + text.slice(1);
  return /[.!?]$/.test(out) ? out : `${out}.`;
}

/**
 * Reads a refusal the API wrote for every surface. Where it says the line
 * itself, as a file that isn't YAML does, the field says that once. A word of
 * the file's own comes in backticks, for withCode to set in mono.
 */
export function composeRefusal(refusal: { message: string; line: number }): RefusedAt {
  const own = /^line (\d+): (.*)$/is.exec(refusal.message);
  if (own) return { line: Number(own[1]), text: lineText(own[2] ?? "") };
  return { line: refusal.line, text: refusal.line > 0 ? lineText(refusal.message) : sentenceOf(refusal.message) };
}

/** What follows "Line 6:", which is still the sentence's start, so it stays as the API wrote it. */
function lineText(text: string): string {
  return /[.!?]$/.test(text) ? text : `${text}.`;
}

/** Sets what a message has in backticks in mono: "name it with `image`". */
export function withCode(message: string): ReactNode {
  const parts = message.split("`");
  if (parts.length < 3) return message;
  return parts.map((part, i) =>
    // biome-ignore lint/suspicious/noArrayIndexKey: the parts of one message, which never move
    i % 2 === 1 ? <code key={i}>{part}</code> : <Fragment key={i}>{part}</Fragment>,
  );
}
