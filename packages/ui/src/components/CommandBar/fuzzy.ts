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
 * Of every way the characters line up, it takes the one that scores best,
 * so "web" lights up web in "Show what happened on web-1" rather than a w,
 * an e and a b strewn along the way, as the command bar's mock shows it.
 */
export function fuzzyMatch(query: string, text: string): FuzzyMatch | null {
  const q = query.toLowerCase().replace(/\s+/g, "");
  if (!q) return { score: 0, indexes: [] };
  const t = text.toLowerCase();
  const n = q.length;
  const m = t.length;
  if (n > m) return null;
  const startsWord = (i: number) => i === 0 || WORD_START.test(t[i - 1] ?? "");
  // best[j][i]: the best score with the query's jth character at text's ith; from[j][i]: where the one before sat.
  const best: number[][] = [];
  const from: number[][] = [];
  for (let j = 0; j < n; j++) {
    const row = new Array<number>(m).fill(Number.NEGATIVE_INFINITY);
    const back = new Array<number>(m).fill(-1);
    const prev = best[j - 1];
    // The best of the row before, up to two places back, and where it was: a match that isn't next to the last.
    let far = Number.NEGATIVE_INFINITY;
    let farAt = -1;
    for (let i = 0; i < m; i++) {
      if (prev && i >= 2 && (prev[i - 2] ?? Number.NEGATIVE_INFINITY) > far) {
        far = prev[i - 2] ?? far;
        farAt = i - 2;
      }
      if (t[i] !== q[j]) continue;
      const bonus = startsWord(i) ? 2 : 0;
      if (!prev) {
        row[i] = 1 + bonus;
        continue;
      }
      const near = i >= 1 ? (prev[i - 1] ?? Number.NEGATIVE_INFINITY) + 3 : Number.NEGATIVE_INFINITY;
      const apart = far + 1;
      if (near === Number.NEGATIVE_INFINITY && apart === Number.NEGATIVE_INFINITY) continue;
      if (near >= apart) {
        row[i] = near + bonus;
        back[i] = i - 1;
      } else {
        row[i] = apart + bonus;
        back[i] = farAt;
      }
    }
    best.push(row);
    from.push(back);
  }
  const last = best[n - 1] ?? [];
  let end = -1;
  for (let i = 0; i < m; i++)
    if ((last[i] ?? Number.NEGATIVE_INFINITY) > (last[end] ?? Number.NEGATIVE_INFINITY)) end = i;
  if (end < 0 || last[end] === Number.NEGATIVE_INFINITY) return null;
  const indexes: number[] = [];
  for (let j = n - 1, i = end; j >= 0 && i >= 0; j--) {
    indexes.unshift(i);
    i = from[j]?.[i] ?? -1;
  }
  return { score: (last[end] ?? 0) - m * 0.01, indexes };
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
