// SPDX-License-Identifier: AGPL-3.0-only

// How a flow reads a sheet that one view covers with another, as a plan covers the
// form that asked for it. The covered view stays in the sheet: it's marked away at
// once, then fades for 120 ms before it's hidden (packages/ui's Sheet.css). So a
// check on the whole sheet, or the whole page, can find the form's words: on a
// fast plan, while the form is still seen, and after, as hidden.

import type { Locator, Page } from "playwright-core";

/** The view a sheet shows on top, such as a plan, without the form it covers or a view on its way out. */
export function onTop(sheet: Locator): Locator {
  return sheet.locator(".dw-sheet-view:not(.is-away)");
}

/** Waits until no view a sheet covers, or one on its way out, can still be seen, for a check that reads the whole page. */
export async function coveredHidden(page: Page): Promise<void> {
  await page.waitForFunction(() =>
    [...document.querySelectorAll(".dw-sheet-view.is-away")].every((v) => getComputedStyle(v).visibility === "hidden"),
  );
}
