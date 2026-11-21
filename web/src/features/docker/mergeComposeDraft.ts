// SPDX-License-Identifier: AGPL-3.0-only

import { diffArrays } from "diff";

interface LineEdit {
  start: number;
  end: number;
  lines: string[];
}

function changes(base: string[], next: string[]): LineEdit[] | undefined {
  const diff = diffArrays(base, next, { timeout: 60 });
  if (!diff) return undefined;
  const edits: LineEdit[] = [];
  let at = 0;
  let edit: LineEdit | undefined;
  for (const part of diff) {
    if (part.added || part.removed) {
      if (!edit) {
        edit = { start: at, end: at, lines: [] };
        edits.push(edit);
      }
      if (part.added) edit.lines = edit.lines.concat(part.value);
      else {
        at += part.value.length;
        edit.end = at;
      }
    } else {
      at += part.value.length;
      edit = undefined;
    }
  }
  return edits;
}

/** Merge disjoint line edits from the same base without guessing where repeated YAML belongs. */
export function mergeComposeDraft(base: string, draft: string, latest: string): string | undefined {
  if (latest === base || latest === draft) return draft;
  if (draft === base) return latest;
  const lines = (text: string) => text.match(/[^\n]*\n|[^\n]+$/g) ?? [];
  const original = lines(base);
  const mine = changes(original, lines(draft));
  const theirs = changes(original, lines(latest));
  if (!mine || !theirs) return undefined;
  const edits = [...mine, ...theirs].sort((a, b) => a.start - b.start);
  const merged: string[] = [];
  let previous: LineEdit | undefined;
  for (const edit of edits) {
    if (previous && (edit.start < previous.end || edit.start === previous.start)) {
      if (
        edit.start === previous.start &&
        edit.end === previous.end &&
        edit.lines.join("") === previous.lines.join("")
      ) {
        continue;
      }
      return undefined;
    }
    merged.push(original.slice(previous?.end ?? 0, edit.start).join(""), edit.lines.join(""));
    previous = edit;
  }
  merged.push(original.slice(previous?.end ?? 0).join(""));
  return merged.join("");
}
