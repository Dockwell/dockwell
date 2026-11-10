// SPDX-License-Identifier: AGPL-3.0-only

/** The folder a path sits in: "/" for what's at the top. */
export function parentOf(path: string): string {
  const cut = path.replace(/\/+$/, "").lastIndexOf("/");
  return cut <= 0 ? "/" : path.slice(0, cut);
}

/** What a path is called: "/" for the top. */
export function nameOf(path: string): string {
  const trimmed = path.replace(/\/+$/, "");
  return trimmed.slice(trimmed.lastIndexOf("/") + 1) || "/";
}

/** name in the folder dir. */
export function join(dir: string, name: string): string {
  return dir === "/" ? `/${name}` : `${dir}/${name}`;
}

/** Each folder on the way to a path, with what it's called and where it is, from the top. */
export function crumbs(path: string): { name: string; path: string }[] {
  const out = [{ name: "/", path: "/" }];
  for (const name of path.split("/").filter(Boolean)) {
    out.push({ name, path: join(out[out.length - 1]?.path ?? "/", name) });
  }
  return out;
}

/**
 * Which line ending a text uses, for the editor's footer. "mixed" is a text with more
 * than one kind, or a lone CR, which withEndings keeps line by line.
 */
export function lineEnding(text: string): "LF" | "CRLF" | "mixed" {
  if (!text.includes("\r")) return "LF";
  return editable(text).replace(/\n/g, "\r\n") === text ? "CRLF" : "mixed";
}

/** How many lines a text has, as an editor numbers them: a last newline starts none. */
export function lineCount(text: string): number {
  if (text === "") return 1;
  let lines = text.endsWith("\n") ? 0 : 1;
  for (let at = text.indexOf("\n"); at !== -1; at = text.indexOf("\n", at + 1)) lines++;
  return lines;
}

/** A text as a field keeps it: a browser turns every line ending into LF as it takes it in. */
export function editable(text: string): string {
  return text.replace(/\r\n?/g, "\n");
}

/** The most edits withEndings looks through to match lines; past it, lines pair by place. */
const MATCH_EDITS = 500;

/**
 * A field's text as the file has it: each line keeps the ending it had in original,
 * a line typed over another takes that one's, lines joined into one take the last of
 * theirs, and a line added takes the one before it. So a file with more than one kind
 * of ending, or a lone CR, changes only on the lines someone touched.
 *
 * Undefined when that can't be told: an edit past what the diff looks through, on a
 * file with more than one kind of ending, where pairing lines by place would move the
 * endings of lines nobody touched. The person then picks one ending for every line.
 */
export function withEndings(text: string, original: string): string | undefined {
  const { lines: a, ends } = linesOf(original);
  if (!ends.some((e) => e === "\r\n" || e === "\r")) return text;
  const kinds = new Set(ends.filter((e) => e !== ""));
  const b = text.split("\n");
  // Which line of the original each line of text stands for, or -1 for one added.
  const from = new Array<number>(b.length).fill(-1);
  let start = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) {
    from[start] = start;
    start++;
  }
  let end = 0;
  while (end < a.length - start && end < b.length - start && a[a.length - 1 - end] === b[b.length - 1 - end]) {
    from[b.length - 1 - end] = a.length - 1 - end;
    end++;
  }
  const found = matching(a.slice(start, a.length - end), b.slice(start, b.length - end), MATCH_EDITS);
  // With one kind of ending, every line gets it whatever pairs with what.
  if (!found && kinds.size > 1) return undefined;
  const shared = found ?? [];
  let ai = start;
  let bi = start;
  // Between two lines that match, the lines typed take the places of those removed, in order,
  // and when fewer were typed than removed, as when lines are joined, the last takes the last's.
  const pair = (x: number, y: number) => {
    for (let t = 0; bi + t < y && ai + t < x; t++) from[bi + t] = ai + t;
    if (y > bi && x - ai > y - bi) from[y - 1] = x - 1;
  };
  for (const [x, y] of shared) {
    pair(x + start, y + start);
    from[y + start] = x + start;
    ai = x + start + 1;
    bi = y + start + 1;
  }
  pair(a.length - end, b.length - end);
  const usual = mostUsed(ends);
  let out = b[0] ?? "";
  let last = usual;
  for (let i = 1; i < b.length; i++) {
    const at = from[i - 1] ?? -1;
    // The original's last line had no ending, so one after it takes the line before's.
    const ending = (at >= 0 ? ends[at] : "") || last;
    out += ending + b[i];
    last = ending;
  }
  return out;
}

/** A text's lines and the ending after each: the last has none. */
function linesOf(text: string): { lines: string[]; ends: string[] } {
  const lines: string[] = [];
  const ends: string[] = [];
  const re = /\r\n|\r|\n/g;
  let at = 0;
  for (let m = re.exec(text); m; m = re.exec(text)) {
    lines.push(text.slice(at, m.index));
    ends.push(m[0]);
    at = m.index + m[0].length;
  }
  lines.push(text.slice(at));
  ends.push("");
  return { lines, ends };
}

/** The ending most lines have, for a line with nothing before it to go by. */
function mostUsed(ends: string[]): string {
  const count = new Map<string, number>();
  for (const e of ends) if (e) count.set(e, (count.get(e) ?? 0) + 1);
  let best = "\n";
  let most = 0;
  for (const [e, n] of count) {
    if (n > most) [best, most] = [e, n];
  }
  return best;
}

/**
 * The lines a and b share, in order, as pairs of their indexes: Myers' diff, which
 * takes time as the edits between them grow. Undefined past max edits.
 */
function matching(a: string[], b: string[], max: number): [number, number][] | undefined {
  const n = a.length;
  const m = b.length;
  const off = max + 1;
  const v = new Int32Array(2 * max + 3);
  const trace: Int32Array[] = [];
  for (let d = 0; d <= max; d++) {
    for (let k = -d; k <= d; k += 2) {
      const down = k === -d || (k !== d && (v[off + k - 1] ?? 0) < (v[off + k + 1] ?? 0));
      let x = down ? (v[off + k + 1] ?? 0) : (v[off + k - 1] ?? 0) + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x++;
        y++;
      }
      v[off + k] = x;
      if (x >= n && y >= m) {
        trace.push(v.slice());
        return backtrack(trace, n, m, off);
      }
    }
    trace.push(v.slice());
  }
  return undefined;
}

function backtrack(trace: Int32Array[], n: number, m: number, off: number): [number, number][] {
  const pairs: [number, number][] = [];
  let x = n;
  let y = m;
  for (let d = trace.length - 1; d > 0; d--) {
    const v = trace[d - 1] as Int32Array;
    const k = x - y;
    const down = k === -d || (k !== d && (v[off + k - 1] ?? 0) < (v[off + k + 1] ?? 0));
    const prevK = down ? k + 1 : k - 1;
    const prevX = v[off + prevK] ?? 0;
    const prevY = prevX - prevK;
    while (x > prevX && y > prevY) {
      x--;
      y--;
      pairs.push([x, y]);
    }
    x = prevX;
    y = prevY;
  }
  while (x > 0 && y > 0) {
    x--;
    y--;
    pairs.push([x, y]);
  }
  return pairs.reverse();
}

/** How many lines an edit touches: what's between the lines the two share at either end. */
export function changedLines(before: string, after: string): number {
  const a = before.split("\n");
  const b = after.split("\n");
  let start = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) start++;
  let end = 0;
  while (end < a.length - start && end < b.length - start && a[a.length - 1 - end] === b[b.length - 1 - end]) end++;
  return Math.max(a.length - start - end, b.length - start - end);
}
