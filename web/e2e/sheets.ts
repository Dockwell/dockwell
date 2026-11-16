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
 * The control server's name in a test, as long as a server's name may be (56) and
 * with nowhere to break, so a check that a page fits a phone can't pass on a short
 * one. Mostly hex, so no flow's check for a word finds it in the name. Every script
 * that starts dockwell sets it, through DOCKWELL_TEST_HOSTNAME (#738).
 */
export const LONG_HOSTNAME = "ip10x0x12x34a7f3c9d1e5b8a4f60d2c7e9b1a3f5d8c0b6e2a9f4d71";

/**
 * How far the page reaches past the viewport's width, and what reaches past it, so a
 * check that a page fits a phone names what doesn't, as a failure in another browser
 * than this machine's can't be looked at after. It waits first for the shell to take
 * the viewport's layout, as a page measured between a resize and the next frame still
 * has the wide one's rail and top bar, 182 px past a phone's edge (#738). What a
 * scrolling box clips isn't past the edge, and text that doesn't wrap is named by its
 * words.
 */
export async function pastTheEdge(page: Page): Promise<{ wider: number; what: string }> {
  await page.waitForFunction(
    () => {
      if (!document.querySelector(".shell")) return true;
      const phone = matchMedia("(max-width: 639.98px)").matches;
      return phone === !!document.querySelector(".tabbar") && phone === !document.querySelector(".topbar");
    },
    undefined,
    { timeout: 10_000 },
  );
  return page.evaluate(() => {
    const width = document.documentElement.clientWidth;
    const wider = document.documentElement.scrollWidth - width;
    if (wider <= 0) return { wider, what: "" };
    const clipped = (from: Element | null) => {
      for (let p = from; p && p !== document.body; p = p.parentElement) {
        if (getComputedStyle(p).overflowX !== "visible" && p.getBoundingClientRect().right <= width + 0.5) return true;
      }
      return false;
    };
    const named = (el: Element) => {
      const classes = typeof el.className === "string" ? el.className.trim().split(/\s+/).filter(Boolean) : [];
      return `${el.tagName.toLowerCase()}${classes.map((c) => `.${c}`).join("")}`;
    };
    const past: string[] = [];
    for (const el of Array.from(document.querySelectorAll("body *"))) {
      const box = el.getBoundingClientRect();
      if (box.width > 0 && box.right > width + 0.5 && !clipped(el.parentElement)) {
        past.push(`${named(el)} to ${Math.round(box.right)} px`);
      }
    }
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const range = document.createRange();
      range.selectNodeContents(n);
      const box = range.getBoundingClientRect();
      if (box.width > 0 && box.right > width + 0.5 && n.parentElement && !clipped(n.parentElement)) {
        past.push(
          `"${(n.textContent ?? "").trim().slice(0, 40)}" in ${named(n.parentElement)} to ${Math.round(box.right)} px`,
        );
      }
    }
    return { wider, what: past.slice(0, 10).join(", ") };
  });
}
