// SPDX-License-Identifier: AGPL-3.0-only

import type { KeyboardEvent } from "react";

const STEP: Readonly<Record<string, number>> = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 };

/**
 * Roving focus for radio groups and tab lists (WAI-ARIA radiogroup and
 * tablist patterns): arrow keys move to the next or previous item and wrap,
 * Home and End jump to the ends. Selection follows focus. Only the selected
 * item is in the tab order; mark the group's items with data-roving-item.
 */
export function handleRovingKeyDown<V>(
  event: KeyboardEvent<HTMLElement>,
  values: readonly V[],
  current: V | undefined,
  select: (value: V) => void,
): void {
  if (event.altKey || event.ctrlKey || event.metaKey || values.length === 0) return;
  const step = STEP[event.key];
  if (step === undefined && event.key !== "Home" && event.key !== "End") return;
  event.preventDefault();
  const at = current === undefined ? -1 : values.indexOf(current);
  let next: number;
  if (event.key === "Home") next = 0;
  else if (event.key === "End") next = values.length - 1;
  else if (at === -1) next = step === 1 ? 0 : values.length - 1;
  else next = (at + (step ?? 0) + values.length) % values.length;
  const value = values[next];
  if (value === undefined) return;
  if (value !== current) select(value);
  event.currentTarget.querySelectorAll<HTMLElement>("[data-roving-item]")[next]?.focus();
}

/** The value that holds the tab stop: the selected one, or the first when nothing is selected. */
export function tabStop<V>(values: readonly V[], current: V | undefined): V | undefined {
  return current !== undefined && values.includes(current) ? current : values[0];
}
