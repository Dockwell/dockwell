// SPDX-License-Identifier: AGPL-3.0-only

/** A colour a log line's codes can ask for, each drawn with a token that reads on any surface. */
export type LogColor = "red" | "green" | "yellow" | "blue" | "muted" | "ink";

/** A run of a log line's text, styled as its codes say. */
export interface LogSpan {
  text: string;
  color?: LogColor;
  bold?: boolean;
  dim?: boolean;
  underline?: boolean;
}

const ESC = "\u001b";
/** What can't be shown: controls and direction overrides, which could reorder what a reader sees. */
// biome-ignore lint/suspicious/noControlCharactersInRegex: finding them is the point
const UNSAFE = /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f‪-‮⁦-⁩]/g;

/**
 * Cuts a log line into runs of text, by its terminal codes. Only colour and
 * weight survive. Every other code, such as one that moves the cursor or sets a
 * window's title, goes, and so do other control characters, which show as a
 * replacement character where they stood in the text. The text only ever ends
 * up in text nodes, so nothing in it is ever read as markup.
 *
 * Colours come from a short list, whatever the code says: a line can ask for
 * black on a dark screen, or for a 24-bit shade of nothing, and it must still
 * read. Backgrounds are dropped for the same reason.
 */
export function parseAnsi(line: string): LogSpan[] {
  // Most lines have no codes, and this runs for each line drawn.
  const text = lastOverwrite(line);
  if (!text.includes(ESC)) return text === "" ? [] : [{ text: clean(text) }];
  const spans: LogSpan[] = [];
  let style: Omit<LogSpan, "text"> = {};
  let i = 0;
  let run = 0;
  const flush = (end: number) => {
    if (end > run) spans.push({ text: clean(text.slice(run, end)), ...style });
  };
  while (i < text.length) {
    if (text[i] !== ESC) {
      i++;
      continue;
    }
    flush(i);
    const seq = readEscape(text, i);
    if (seq.sgr !== undefined) style = applySgr(style, seq.sgr);
    i = seq.end;
    run = i;
  }
  flush(text.length);
  return spans;
}

/** The text of a log line as a reader sees it, for searching and copying. */
export function plainText(line: string): string {
  const text = lastOverwrite(line);
  if (!text.includes(ESC)) return clean(text);
  return parseAnsi(line)
    .map((span) => span.text)
    .join("");
}

/** What a terminal would show of a line that rewrites itself, as a progress bar does. */
function lastOverwrite(line: string): string {
  const end = line.endsWith("\r") ? line.length - 1 : line.length;
  const at = line.lastIndexOf("\r", end - 1);
  return at >= 0 ? line.slice(at + 1, end) : line.slice(0, end);
}

function clean(text: string): string {
  return text.replace(UNSAFE, "�");
}

/** Where an escape sequence at i ends, and its parameters if it is a colour code. */
function readEscape(text: string, i: number): { end: number; sgr?: string } {
  const kind = text[i + 1];
  if (kind === "[") {
    let j = i + 2;
    while (j < text.length && (text.charCodeAt(j) < 0x40 || text.charCodeAt(j) > 0x7e)) j++;
    if (j >= text.length) return { end: text.length };
    return text[j] === "m" ? { end: j + 1, sgr: text.slice(i + 2, j) } : { end: j + 1 };
  }
  if (kind === "]" || kind === "P" || kind === "X" || kind === "^" || kind === "_") {
    // A string, up to its terminator.
    for (let j = i + 2; j < text.length; j++) {
      if (text[j] === "\u0007") return { end: j + 1 };
      if (text[j] === ESC && text[j + 1] === "\\") return { end: j + 2 };
    }
    return { end: text.length };
  }
  return { end: Math.min(i + 2, text.length) };
}

const BASIC: readonly LogColor[] = ["muted", "red", "green", "yellow", "blue", "blue", "blue", "ink"];

function applySgr(style: Omit<LogSpan, "text">, params: string): Omit<LogSpan, "text"> {
  const codes = params === "" ? [0] : params.split(/[;:]/).map(Number);
  let next = style;
  for (let i = 0; i < codes.length; i++) {
    const c = codes[i] ?? 0;
    if (c === 0) next = {};
    else if (c === 1) next = { ...next, bold: true };
    else if (c === 2) next = { ...next, dim: true };
    else if (c === 4) next = { ...next, underline: true };
    else if (c === 21 || c === 22) {
      const { bold: _b, dim: _d, ...rest } = next;
      next = rest;
    } else if (c === 24) {
      const { underline: _u, ...rest } = next;
      next = rest;
    } else if (c === 39) {
      const { color: _c, ...rest } = next;
      next = rest;
    } else if ((c >= 30 && c <= 37) || (c >= 90 && c <= 97)) {
      next = { ...next, color: BASIC[c >= 90 ? c - 90 : c - 30] ?? "ink" };
    } else if (c === 38 || c === 48) {
      // 256 colours, or 24-bit: the nearest of the short list, and nothing for
      // a background, whose parameters are skipped all the same.
      const mode = codes[i + 1];
      const color =
        mode === 5
          ? fromIndexed(codes[i + 2] ?? 0)
          : mode === 2
            ? fromRgb(codes[i + 2] ?? 0, codes[i + 3] ?? 0, codes[i + 4] ?? 0)
            : undefined;
      i += mode === 5 ? 2 : mode === 2 ? 4 : 0;
      if (c === 38 && color) next = { ...next, color };
    }
  }
  return next;
}

function fromIndexed(n: number): LogColor {
  if (n < 8) return BASIC[n] ?? "ink";
  if (n < 16) return BASIC[n - 8] ?? "ink";
  if (n >= 232) return n < 244 ? "muted" : "ink";
  const v = n - 16;
  return fromRgb(Math.floor(v / 36) * 51, Math.floor((v % 36) / 6) * 51, (v % 6) * 51);
}

function fromRgb(r: number, g: number, b: number): LogColor {
  const high = Math.max(r, g, b);
  const low = Math.min(r, g, b);
  if (high - low < 40) return high < 128 ? "muted" : "ink";
  if (r >= g && r >= b) return g > r * 0.6 ? "yellow" : "red";
  if (g >= r && g >= b) return "green";
  return "blue";
}
