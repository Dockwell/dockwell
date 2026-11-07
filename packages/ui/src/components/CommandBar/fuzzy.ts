// SPDX-License-Identifier: AGPL-3.0-only

export interface FuzzyMatch {
  /** Higher is better: consecutive characters and starts of words count extra; shorter texts win ties. */
  score: number;
  /** Indexes in text of the characters that matched, each where its character starts. */
  indexes: number[];
}

/** A text as the matcher reads it, worked out once so each keystroke only matches. */
export interface FoldedText {
  readonly text: string;
  /** The text in lower case, with accents dropped and full-width letters made plain. */
  readonly folded: string;
  /** Where in text each of folded's characters came from. */
  readonly from: Int32Array;
  /** Which of folded's characters start a word. */
  readonly starts: Uint8Array;
}

const WORD_START = /[\s\-_/.:,()]/;
const MARKS = /\p{M}/gu;
const PLAIN = /^[\x20-\x7e]*$/;

/** A character as the matcher compares it: lower case, without accents, and full-width letters made plain. */
function foldChar(ch: string): string {
  return ch.normalize("NFKD").replace(MARKS, "").toLowerCase();
}

/**
 * Works out how the matcher reads text. "café" reads as "cafe", "ｗｅｂ" as
 * "web", and "İ" as "i", each still pointing at its own character in text.
 */
export function foldText(text: string): FoldedText {
  let folded: string;
  let from: Int32Array;
  if (PLAIN.test(text)) {
    folded = text.toLowerCase();
    from = new Int32Array(folded.length);
    for (let i = 0; i < from.length; i++) from[i] = i;
  } else {
    folded = "";
    const at: number[] = [];
    let i = 0;
    for (const ch of text) {
      const plain = foldChar(ch);
      folded += plain;
      for (let k = 0; k < plain.length; k++) at.push(i);
      i += ch.length;
    }
    from = Int32Array.from(at);
  }
  const starts = new Uint8Array(folded.length);
  for (let i = 0; i < folded.length; i++) starts[i] = i === 0 || WORD_START.test(folded[i - 1] ?? "") ? 1 : 0;
  return { text, folded, from, starts };
}

let lastQuery = "";
let lastFolded = "";

/** The query as the matcher reads it, without its spaces. A bar asks the same query of every option, so the last is kept. */
function foldQuery(query: string): string {
  if (query !== lastQuery) {
    lastQuery = query;
    lastFolded = (PLAIN.test(query) ? query.toLowerCase() : Array.from(query, foldChar).join("")).replace(/\s+/g, "");
  }
  return lastFolded;
}

// Room the matcher reuses from one text to the next, grown as needed.
let rowA = new Float64Array(64);
let rowB = new Float64Array(64);
let back = new Int32Array(256);
let lo = new Int32Array(16);
let hi = new Int32Array(16);

function room(n: number, m: number): void {
  if (rowA.length < m) {
    rowA = new Float64Array(m * 2);
    rowB = new Float64Array(m * 2);
  }
  if (back.length < n * m) back = new Int32Array(n * m * 2);
  if (lo.length < n) {
    lo = new Int32Array(n * 2);
    hi = new Int32Array(n * 2);
  }
}

const NONE = Number.NEGATIVE_INFINITY;

/**
 * Matches the query's characters in order anywhere in text, ignoring case,
 * accents and spaces in the query. Returns null when some character is
 * missing. Of every way the characters line up, it takes the one that scores
 * best, so "web" lights up web in "Show what happened on web-1" rather than
 * a w, an e and a b strewn along the way, as the command bar's mock shows it.
 * The first character counts double at the start of a word, so the name an
 * item acts on comes before its verb: "pg" finds "Open postgres" before
 * "Open ghost".
 */
export function fuzzyMatch(query: string, text: string | FoldedText): FuzzyMatch | null {
  const q = foldQuery(query);
  if (!q) return { score: 0, indexes: [] };
  const { folded: t, from, starts } = typeof text === "string" ? foldText(text) : text;
  const n = q.length;
  const m = t.length;
  if (n > m) return null;
  room(n, m);

  // The earliest place each character can sit, which is also the check that every one is there.
  for (let i = 0, j = 0; j < n; i++) {
    if (i >= m) return null;
    if (t.charCodeAt(i) === q.charCodeAt(j)) lo[j++] = i;
  }
  // And the latest, so each row only scores the places that can still lead to a whole match.
  for (let i = m - 1, j = n - 1; j >= 0; i--) if (t.charCodeAt(i) === q.charCodeAt(j)) hi[j--] = i;

  // A row holds, for the query's jth character at text's ith, the best score of a line-up ending there.
  let prev = rowA;
  let row = rowB;
  for (let j = 0; j < n; j++) {
    const start = lo[j] ?? 0;
    const end = hi[j] ?? 0;
    const code = q.charCodeAt(j);
    const pStart = j > 0 ? (lo[j - 1] ?? 0) : 0;
    const pEnd = j > 0 ? (hi[j - 1] ?? 0) : -1;
    const at = (i: number) => (i >= pStart && i <= pEnd ? (prev[i] ?? NONE) : NONE);
    // The best of the row before, two or more places back, and where it was: a match that isn't next to the last.
    let far = NONE;
    let farAt = -1;
    for (let i = pStart; i <= start - 2 && i <= pEnd; i++) {
      if ((prev[i] ?? NONE) > far) {
        far = prev[i] ?? NONE;
        farAt = i;
      }
    }
    for (let i = start; i <= end; i++) {
      if (i >= 2 && at(i - 2) > far) {
        far = at(i - 2);
        farAt = i - 2;
      }
      if (t.charCodeAt(i) !== code) {
        row[i] = NONE;
        continue;
      }
      const bonus = starts[i] ? 2 : 0;
      if (j === 0) {
        row[i] = 1 + bonus * 2;
        continue;
      }
      const near = at(i - 1) + 3;
      const apart = far + 1;
      if (near === NONE && apart === NONE) {
        row[i] = NONE;
      } else if (near >= apart) {
        row[i] = near + bonus;
        back[j * m + i] = i - 1;
      } else {
        row[i] = apart + bonus;
        back[j * m + i] = farAt;
      }
    }
    [prev, row] = [row, prev];
  }

  let best = NONE;
  let last = -1;
  for (let i = lo[n - 1] ?? 0; i <= (hi[n - 1] ?? -1); i++) {
    if ((prev[i] ?? NONE) > best) {
      best = prev[i] ?? NONE;
      last = i;
    }
  }
  if (last < 0) return null;
  const indexes: number[] = [];
  for (let j = n - 1, i = last; j >= 0; j--) {
    const at = from[i] ?? 0;
    if (indexes[0] !== at) indexes.unshift(at);
    i = j > 0 ? (back[j * m + i] ?? 0) : i;
  }
  return { score: best - m * 0.01, indexes };
}

const MARK = /^\p{M}$/u;

/**
 * Splits text into runs, marking the runs whose characters matched. A
 * character's accents go with it, and a character outside the basic plane
 * stays whole.
 */
export function splitMatches(text: string, indexes: readonly number[]): { text: string; match: boolean }[] {
  const matched = new Set(indexes);
  const runs: { text: string; match: boolean }[] = [];
  let i = 0;
  let match = false;
  for (const ch of text) {
    if (!MARK.test(ch) || runs.length === 0) match = matched.has(i);
    const run = runs[runs.length - 1];
    if (run && run.match === match) run.text += ch;
    else runs.push({ text: ch, match });
    i += ch.length;
  }
  return runs;
}
