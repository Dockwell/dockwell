// SPDX-License-Identifier: AGPL-3.0-only

export interface FuzzyMatch {
  /** Higher is better: consecutive characters and starts of words count extra; shorter texts win ties. */
  score: number;
  /** Indexes of the matched characters in text. */
  indexes: number[];
}

const WORD_START = /[\s\-_/.]/;

/**
 * Matches the query's characters in order anywhere in text, ignoring case
 * and spaces in the query. Returns null when some character is missing.
 */
export function fuzzyMatch(query: string, text: string): FuzzyMatch | null {
  const q = query.toLowerCase().replace(/\s+/g, "");
  if (!q) return { score: 0, indexes: [] };
  const t = text.toLowerCase();
  const indexes: number[] = [];
  let j = 0;
  let score = 0;
  let last = -2;
  for (let i = 0; i < t.length && j < q.length; i++) {
    if (t[i] !== q[j]) continue;
    indexes.push(i);
    score += i === last + 1 ? 3 : 1;
    if (i === 0 || WORD_START.test(t[i - 1] ?? "")) score += 2;
    last = i;
    j++;
  }
  return j === q.length ? { score: score - t.length * 0.01, indexes } : null;
}

/** Splits text into runs, marking the runs whose characters matched. */
export function splitMatches(text: string, indexes: readonly number[]): { text: string; match: boolean }[] {
  const matched = new Set(indexes);
  const runs: { text: string; match: boolean }[] = [];
  for (let i = 0; i < text.length; i++) {
    const match = matched.has(i);
    const run = runs[runs.length - 1];
    if (run && run.match === match) run.text += text[i];
    else runs.push({ text: text[i] ?? "", match });
  }
  return runs;
}
