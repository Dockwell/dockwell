// SPDX-License-Identifier: AGPL-3.0-only

import { Fragment, type ReactNode } from "react";

/** A Compose refusal, ready to show: the line it is about, and what to say, as a sentence. */
export interface RefusedAt {
  /** What it's about: the file, the .env beside it, or the directory it's in. */
  field: "file" | "env" | "directory";
  line: number;
  /** The last line it is about: the same, unless a parser can only say near which. */
  to: number;
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
export function composeRefusal(refusal: { message: string; line: number; endLine?: number }): RefusedAt {
  // What is wrong with an .env names a line of it, and never what is on it.
  const env = /^line (\d+) of the \.env file\b/i.exec(refusal.message);
  if (env) return { field: "env", line: Number(env[1]), to: Number(env[1]), text: sentenceOf(refusal.message) };
  if (/^the \.env file\b/i.test(refusal.message)) {
    return { field: "env", line: 0, to: 0, text: sentenceOf(refusal.message) };
  }
  if (/^the directory\b/i.test(refusal.message)) {
    return { field: "directory", line: 0, to: 0, text: sentenceOf(refusal.message) };
  }
  const near = /^near lines (\d+)\u2013(\d+): (.*)$/is.exec(refusal.message);
  if (near) return { field: "file", line: Number(near[1]), to: Number(near[2]), text: lineText(near[3] ?? "") };
  const own = /^line (\d+): (.*)$/is.exec(refusal.message);
  if (own) return { field: "file", line: Number(own[1]), to: Number(own[1]), text: lineText(own[2] ?? "") };
  // A path relative to the file means nothing until the directory is said, so that is where this is put right.
  const field = /\bPass the directory\b/.test(refusal.message) ? "directory" : "file";
  const text = refusal.line > 0 ? lineText(refusal.message) : sentenceOf(refusal.message);
  return { field, line: refusal.line, to: Math.max(refusal.endLine ?? 0, refusal.line), text };
}

/**
 * A refusal that only asks for a variable's value, which the form asks for itself,
 * and which is out of date the moment someone types one.
 */
export function waitsForValues(message: string): boolean {
  return (
    /\bwith no value\b|\bwhich has no value\b/.test(message) &&
    !/^the service .*'s variable /.test(message) &&
    // An .env's own value that names one is the .env's to put right.
    !/\bof the \.env file\b/.test(message)
  );
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
