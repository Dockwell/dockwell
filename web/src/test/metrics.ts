// SPDX-License-Identifier: AGPL-3.0-only

import { screen } from "@testing-library/react";
import { expect } from "vitest";

/** The Metric labelled label. */
export function metric(label: string): HTMLElement {
  const el = screen.getByText(label, { selector: ".dw-metric-label span" }).closest<HTMLElement>(".dw-metric");
  if (!el) throw new Error(`no metric ${label}`);
  return el;
}

/** Checks that the Metric labelled label shows each part, in order. */
export function expectMetric(label: string, ...parts: string[]) {
  const text = metric(label).textContent ?? "";
  let from = 0;
  for (const part of parts) {
    const at = text.indexOf(part, from);
    expect(at, `${label} shows “${part}” after position ${from} in “${text}”`).toBeGreaterThanOrEqual(0);
    from = at + part.length;
  }
}
