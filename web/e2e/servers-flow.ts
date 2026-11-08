// SPDX-License-Identifier: AGPL-3.0-only
// Servers, as Servers-List lays them out: a reader down the list who chooses a
// server stays where they are, and the server's inspector is in view beside the
// list, held under the top bar as the page scrolls.
import type { Page } from "playwright-core";

import type { Flow, FlowRun } from "./flows.ts";

/** How far the page is scrolled, as the window scrolls it under the shell's sticky rail and top bar. */
const scrolled = (page: Page) => page.evaluate(() => (document.scrollingElement as HTMLElement).scrollTop);

/** Two frames, so what a choice changes has been laid out and drawn. */
const settled = (page: Page) =>
  page.evaluate(() => new Promise<void>((done) => requestAnimationFrame(() => requestAnimationFrame(() => done()))));

export async function serversFlow(page: Page, { origin, step }: Pick<FlowRun, "origin" | "step">): Promise<void> {
  step("servers: choosing a server down the page leaves it where it is, with the server's inspector in view");
  const was = page.viewportSize();
  // Short enough that even one server's page scrolls, wide enough for the inspector beside the list.
  const height = 320;
  await page.setViewportSize({ width: 1280, height });
  try {
    await page.goto(`${origin}/servers?closed=true`);
    const row = page.getByRole("table", { name: "Servers, sorted by name" }).getByRole("row").nth(1);
    await row.waitFor();
    // Down the page, as a reader of a long list is, with the row on the screen, as a click needs it.
    await page.evaluate(() => {
      const el = document.scrollingElement as HTMLElement;
      el.scrollTop = el.scrollHeight;
    });
    await row.scrollIntoViewIfNeeded();
    const before = await scrolled(page);
    if (before < 40) throw new Error(`at ${height} px the page scrolls ${before} px, so this measures nothing`);
    await row.click();
    const inspector = page.getByRole("complementary");
    await inspector.waitFor();
    await settled(page);
    const after = await scrolled(page);
    if (Math.abs(after - before) > 2) throw new Error(`choosing a server moved the page from ${before} to ${after} px`);
    const box = await inspector.boundingBox();
    // Under the top bar and on the screen, as it holds there however far the page scrolls.
    const bar = await page.evaluate(() =>
      Number.parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--scopebar-height")),
    );
    if (!box || box.y < bar - 1 || box.y >= height) {
      throw new Error(`the inspector isn't in view under the top bar: ${JSON.stringify(box)}, top bar ${bar} px`);
    }
    step(
      `  chosen at ${before} px down, the page stayed at ${after} px, with the inspector at ${Math.round(box.y)} px`,
    );
  } finally {
    if (was) await page.setViewportSize(was);
  }
}

/** Where sign-in.ts runs this among the flows, on the fake engine (flows.ts): after network, before settings. */
export const flow: Flow = { order: 82, run: (page, run) => serversFlow(page, run) };
