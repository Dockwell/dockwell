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

/**
 * How far the page reaches past the viewport's width, and what reaches past it, so a
 * check that a page fits a phone names what doesn't, as a failure in another browser
 * than this machine's can't be looked at after.
 */
export async function pastTheEdge(page: Page): Promise<{ wider: number; what: string }> {
  return page.evaluate(() => {
    const width = document.documentElement.clientWidth;
    const wider = document.documentElement.scrollWidth - width;
    if (wider <= 0) return { wider, what: "" };
    const past: string[] = [];
    for (const el of Array.from(document.querySelectorAll("body *"))) {
      const box = el.getBoundingClientRect();
      if (box.width > 0 && box.right > width + 0.5) {
        const classes = typeof el.className === "string" ? el.className.trim().split(/\s+/).filter(Boolean) : [];
        past.push(`${el.tagName.toLowerCase()}${classes.map((c) => `.${c}`).join("")} to ${Math.round(box.right)} px`);
      }
    }
    return { wider, what: past.slice(0, 8).join(", ") };
  });
}
