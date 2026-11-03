// SPDX-License-Identifier: AGPL-3.0-only

import axe from "axe-core";
import { expect } from "vitest";

/**
 * Fails the test on any axe violation in container. Colour contrast can't be
 * measured in jsdom; the token pairs are checked against the design system
 * instead (see tokens.test.ts), and the panel runs axe in a real browser.
 */
export async function expectAccessible(container: Element): Promise<void> {
  const results = await axe.run(container, { rules: { "color-contrast": { enabled: false } } });
  const summary = results.violations.map((v) => `${v.id}: ${v.help}\n  ${v.nodes.map((n) => n.html).join("\n  ")}`);
  expect(summary, summary.join("\n\n")).toEqual([]);
}
